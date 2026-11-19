"""DTMF keypad capture for PSTN (SIP) voice calls (SPEC-W86).

LiveKit relays SIP INFO DTMF as room data-channel messages (topic "dtmf",
payload = the digit[s]). This module is livekit-free and duck-typed — the
same pattern as app/sip.py — so the unit tests drive it with plain fakes:

- ``parse_dtmf_payload`` — pure payload parser: topic "dtmf" with a plain
  digit payload, or the JSON forms {"digit": ...} / {"dtmf": ...} on any
  topic (relay variants that wrap the digit).
- ``DtmfBuffer`` — per-session asyncio buffer: digits accumulate
  (bounded), ``*`` clears, ``#`` submits; ``wait_for_entry`` auto-submits
  once ``expected_length`` digits have accumulated (6-digit portal codes
  need no ``#``).
- ``attach_dtmf_listener`` — registers a duck-typed
  ``room.on("data_received")`` handler that feeds the buffer ONLY from
  SIP-kind participants (a web widget can never inject keypad digits into
  a PSTN session).

Security invariants (SPEC-W86): the buffer is per-session, bounded
(MAX_DIGITS), and every wait is timeout-bounded by the caller (the tool
layer clamps to 5-60s). The digits never leave the runtime except as the
ToolLayer.read_keypad_entry result for the session's own LLM loop — they
are never logged and never emitted in events.
"""

from __future__ import annotations

import asyncio
import json
from typing import Any, Callable

from . import metrics, sip
from .logging import get_logger

log = get_logger("dtmf")

DTMF_TOPIC = "dtmf"
DTMF_DIGITS = frozenset("0123456789")
DTMF_CONTROLS = frozenset("*#")
MAX_DIGITS = 32


def _valid_keypad_text(text: str) -> bool:
    return bool(text) and all(
        ch in DTMF_DIGITS or ch in DTMF_CONTROLS for ch in text
    )


def parse_dtmf_payload(data: bytes, topic: str) -> str | None:
    """Extract DTMF keypad text from a data-channel payload.

    Accepted forms:

    - topic "dtmf" with a plain keypad payload (one or more of 0-9 * #,
      e.g. b"5" or b"53#") — the LiveKit SIP relay shape;
    - a JSON object {"digit": "5"} / {"dtmf": "5"} on ANY topic (relay
      variants that wrap the digit; integer values 0-9 accepted).

    Everything else (plain payloads on non-DTMF topics, garbage bytes,
    non-object JSON, wrong value shapes, empty payloads) returns None.
    """
    if not data:
        return None
    if topic == DTMF_TOPIC:
        try:
            text = data.decode("utf-8", errors="strict").strip()
        except (UnicodeDecodeError, ValueError):
            text = ""
        if _valid_keypad_text(text):
            return text
    try:
        payload = json.loads(data.decode("utf-8", errors="strict"))
    except (UnicodeDecodeError, ValueError):
        return None
    if not isinstance(payload, dict):
        return None
    for key in ("digit", "dtmf"):
        value = payload.get(key)
        if isinstance(value, str):
            text = value.strip()
            if _valid_keypad_text(text):
                return text
        elif isinstance(value, int) and not isinstance(value, bool):
            if 0 <= value <= 9:
                return str(value)
    return None


class DtmfBuffer:
    """Per-session DTMF accumulator (one PSTN call = one buffer).

    ``feed`` runs from the room data-received handler (event-loop
    context); ``wait_for_entry`` is awaited from the tool path. A wait is
    one-shot: a successful wait CONSUMES the entry (a later wait starts
    fresh), and a timeout discards any partial digits so a retry never
    mixes digits from two attempts.
    """

    def __init__(self) -> None:
        self._digits: list[str] = []
        self._submitted: str | None = None
        self._event = asyncio.Event()
        self._expected_length: int | None = None
        # SPEC-W96: armed IVR menu (digit -> callback) + wait-active flag.
        # A menu digit is intercepted ONLY while no wait is active and no
        # partial entry is buffered — OTP/code capture is sacred.
        self._menu_digits: frozenset[str] = frozenset()
        self._on_menu_digit: Callable[[str], None] | None = None
        self._wait_active = False

    def arm_menu(
        self, digits: frozenset[str], on_menu_digit: Callable[[str], None]
    ) -> None:
        """Arm IVR-A menu interception (SPEC-W96): ``digits`` are the menu
        keys; ``on_menu_digit`` fires instead of buffering when a menu key
        arrives outside any keypad wait. Called once per session by
        ivr_menu.attach_ivr_menu."""
        self._menu_digits = digits
        self._on_menu_digit = on_menu_digit

    @property
    def pending(self) -> str:
        """Digits accumulated so far (tests / debug observability)."""
        return "".join(self._digits)

    def feed(self, text: str) -> None:
        """Accumulate keypad input: 0-9 append (bounded at MAX_DIGITS),
        ``*`` clears, ``#`` submits. Auto-submits the moment the armed
        expected_length is reached (6-digit portal codes need no ``#``).
        Characters after a submit in the same payload are ignored."""
        for ch in text:
            if ch in DTMF_DIGITS:
                # SPEC-W96 IVR-A: an armed menu key pressed outside any
                # keypad wait and with no partial entry buffered is a menu
                # choice, not entry input — fire the callback, never
                # buffer it. Inside a wait (OTP/code capture) or mid-entry
                # the digit buffers exactly as before.
                if (
                    self._on_menu_digit is not None
                    and ch in self._menu_digits
                    and not self._wait_active
                    and not self._digits
                ):
                    try:
                        self._on_menu_digit(ch)
                    except Exception:  # noqa: BLE001 - menu never breaks capture
                        pass
                    continue
                if len(self._digits) >= MAX_DIGITS:
                    continue
                self._digits.append(ch)
                metrics.dtmf_digit()
                if (
                    self._expected_length is not None
                    and len(self._digits) >= self._expected_length
                ):
                    self._submit()
                    return
            elif ch == "*":
                self._digits.clear()
            elif ch == "#":
                self._submit()
                return

    def _submit(self) -> None:
        if self._submitted is None and self._digits:
            self._submitted = "".join(self._digits)
            self._digits.clear()
            self._event.set()
            metrics.dtmf_entry()

    async def wait_for_entry(
        self, timeout_s: float, expected_length: int | None = None
    ) -> str | None:
        """Await the next submitted entry; None on timeout.

        Arms expected_length auto-submit first — digits already buffered
        (typed while the agent was speaking) can satisfy the wait
        immediately. The consumed entry rearms the buffer for a later
        wait.
        """
        if expected_length is not None:
            self._expected_length = expected_length
        # SPEC-W96: mark the wait active so the IVR menu never intercepts
        # digits while the tool layer is collecting (OTP capture is
        # sacred), whatever the expected_length.
        self._wait_active = True
        try:
            if (
                self._submitted is None
                and self._expected_length is not None
                and len(self._digits) >= self._expected_length
            ):
                self._submit()
            if self._submitted is None:
                try:
                    await asyncio.wait_for(self._event.wait(), timeout_s)
                except (asyncio.TimeoutError, TimeoutError):
                    metrics.dtmf_timeout()
                    self._digits.clear()  # never mix partials across attempts
                    self._expected_length = None
                    return None
            entry, self._submitted = self._submitted, None
            self._event.clear()
            self._expected_length = None
            return entry
        finally:
            self._wait_active = False


def attach_dtmf_listener(room: Any, buffer: DtmfBuffer) -> Any:
    """Register the room data-received handler that feeds ``buffer``.

    Duck-typed (no livekit imports): the room only needs an ``on`` method
    accepting ("data_received", callback) — the decorator form is the
    fallback — and each event packet only needs ``data`` / ``topic`` /
    ``participant`` attributes (or dict keys). Only SIP-kind participants
    feed the buffer: web/data-channel traffic (chat widget, UI actions,
    copilot suggestions) never does. Returns the registered handler (the
    room drops it on disconnect).
    """

    def _on_data_received(*args: Any, **kwargs: Any) -> None:
        packet = args[0] if args else kwargs.get("packet", kwargs)
        data = getattr(packet, "data", None)
        topic = getattr(packet, "topic", "") or ""
        participant = getattr(packet, "participant", None)
        if isinstance(packet, dict):
            data = packet.get("data", data)
            topic = packet.get("topic", topic) or ""
            participant = packet.get("participant", participant)
        if not isinstance(data, (bytes, bytearray)):
            return
        if not sip.is_sip_participant(participant):
            return
        text = parse_dtmf_payload(bytes(data), topic)
        if text is None:
            return
        buffer.feed(text)

    try:
        room.on("data_received", _on_data_received)
    except TypeError:
        room.on("data_received")(_on_data_received)
    return _on_data_received
