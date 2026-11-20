"""IVR-A DTMF accelerator: keypad menu with LLM intent hints (SPEC-W96).

User-approved design: the PSTN greeting announces ~3 keypad shortcuts
(1 = book, 2 = reschedule, 3 = human); a digit press injects an INTENT
HINT into the LLM turn — deliberately NO state machine (every menu digit
always maps to the same ack + hint, mid-conversation included), and
multilingual-safe by construction (digits are language-neutral; the LLM
composes the actual reply in the caller's language).

Backend reality (recon-established):

- LiveKit path (SPEC-W86 buffer): digits transit this runtime, so the full
  surface applies — greeting menu line, DtmfBuffer menu interception,
  spoken ack, chat_ctx system hint.
- ElevenLabs native path (SPEC-W93): digits never transit this runtime
  (ElevenLabs relays them to the hosted agent as user turns); the honest
  contract surface is the read_keypad_entry ``keypad_native`` tool
  message, which W96 extends with the menu mapping (ToolLayer
  ``ivr_menu_hint``). The hosted greeting/first message is provisioned
  per-tenant on ElevenLabs' side (documented SPEC-W96 non-goal).

This module is livekit-free and duck-typed (same pattern as app/dtmf.py):
``attach_ivr_menu`` only needs ``agent.say`` (coroutine) and
``agent.chat_ctx.append`` — unit tests drive it with plain fakes.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
from typing import Any, Callable

from . import metrics
from .logging import get_logger

log = get_logger("ivr-menu")

MAX_MENU_ENTRIES = 8


@dataclass(frozen=True)
class IvrEntry:
    """One keypad shortcut: digit -> intent key + spoken phrase."""

    digit: str
    intent: str
    spoken: str


# The user-approved default menu (IVR-A): ~3 shortcuts covering the three
# dominant call drivers.
DEFAULT_MENU: tuple[IvrEntry, ...] = (
    IvrEntry("1", "book", "book an appointment"),
    IvrEntry("2", "reschedule", "reschedule an appointment"),
    IvrEntry("3", "human", "speak to a team member"),
)

_MENU_DIGITS = frozenset("0123456789")


def parse_ivr_menu(raw: str) -> tuple[IvrEntry, ...]:
    """Parse the IVR_MENU env value into menu entries.

    Format: ``1=book:book an appointment,2=reschedule:reschedule an
    appointment,3=human:speak to a team member``. ``""`` selects the
    DEFAULT_MENU; ``"none"`` disables (no entries). Malformed entries are
    skipped individually (bad digit, missing colon, empty intent/spoken,
    duplicate digit) and the menu is capped at MAX_MENU_ENTRIES — a bad
    env value degrades the menu, never the call.
    """
    raw = (raw or "").strip()
    if not raw:
        return DEFAULT_MENU
    if raw.lower() == "none":
        return ()
    entries: list[IvrEntry] = []
    seen: set[str] = set()
    skipped = 0
    for chunk in raw.split(","):
        chunk = chunk.strip()
        if not chunk:
            continue
        head, sep, spoken = chunk.partition("=")
        digit = head.strip()
        intent, sep2, phrase = spoken.partition(":")
        intent, phrase = intent.strip(), phrase.strip()
        if (
            not sep
            or not sep2
            or len(digit) != 1
            or digit not in _MENU_DIGITS
            or not intent
            or not phrase
            or digit in seen
        ):
            skipped += 1
            continue
        seen.add(digit)
        entries.append(IvrEntry(digit, intent, phrase))
        if len(entries) >= MAX_MENU_ENTRIES:
            break
    if skipped:
        log.warning("ivr menu entries skipped", skipped=skipped)
    return tuple(entries)


def menu_line(menu: tuple[IvrEntry, ...]) -> str:
    """The greeting announcement (empty menu -> "").

    Default rendering: "You can press 1 to book an appointment, 2 to
    reschedule an appointment, or 3 to speak to a team member at any
    time." Deterministic per menu — one TTS render per deployment, then
    an LRU hit on every call.
    """
    if not menu:
        return ""
    parts = [f"{e.digit} to {e.spoken}" for e in menu]
    if len(parts) == 1:
        listing = parts[0]
    elif len(parts) == 2:
        listing = f"{parts[0]} or {parts[1]}"
    else:
        listing = ", ".join(parts[:-1]) + f", or {parts[-1]}"
    return f"You can press {listing} at any time."


# Deterministic per-intent acks (TTS-LRU-friendly constants). The ack is
# the immediate steering response; the chat_ctx hint keeps the following
# LLM turn on the same rails.
_ACKS = {
    "book": "Sure — I can help you book an appointment. What day works for you?",
    "reschedule": (
        "Sure — I can help with rescheduling. "
        "What day was your appointment originally booked for?"
    ),
    "human": "Of course — let me get a team member for you. One moment.",
}


def ack_phrase(entry: IvrEntry) -> str:
    """The spoken ack for a menu press (deterministic per intent)."""
    return _ACKS.get(entry.intent, f"Sure — one moment while I help you {entry.spoken}.")


def intent_hint(entry: IvrEntry) -> str:
    """The chat_ctx system addendum injected on a menu press."""
    hint = (
        f"The caller just pressed {entry.digit} on the phone keypad, "
        f"choosing to {entry.spoken}. Treat this as their current intent "
        "and continue in the caller's language."
    )
    if entry.intent == "human":
        hint += " Use the request_human tool to bring in a team member."
    return hint


def prompt_addendum(menu: tuple[IvrEntry, ...]) -> str:
    """The system-prompt KEYPAD MENU block (empty menu -> "").

    Rides build_system_prompt's keypad_menu_hint kwarg so the mapping
    survives every per-turn rebuild (locale switches re-render the
    prompt). Multilingual-safe: the mapping is digits; the LLM composes
    the reply in the caller's language.
    """
    if not menu:
        return ""
    mapping = "; ".join(f"{e.digit} = {e.spoken}" for e in menu)
    lines = [
        "KEYPAD MENU",
        f"- The caller was offered a keypad menu: {mapping}.",
        "- A user message that is just one of these digits means the "
        "caller chose that option — act on it directly, in the caller's "
        "language.",
    ]
    if any(e.intent == "human" for e in menu):
        lines.append("- If the choice is to speak to a team member, use the request_human tool.")
    return "\n".join(lines) + "\n"


def native_menu_sentence(menu: tuple[IvrEntry, ...]) -> str:
    """The menu-mapping sentence appended to the keypad_native tool
    message (SPEC-W96 native path; empty menu -> "")."""
    if not menu:
        return ""
    mapping = "; ".join(f"{e.digit} = {e.spoken}" for e in menu)
    return (
        f" The caller was also offered a keypad menu: {mapping} — a bare "
        "digit as the caller's message means they chose that option."
    )


def attach_ivr_menu(
    buffer: Any,
    menu: tuple[IvrEntry, ...],
    agent: Any,
) -> None:
    """Arm the DTMF buffer's menu interception for this session.

    Duck-typed: ``buffer`` needs ``arm_menu`` (app/dtmf.py), ``agent``
    needs ``say`` (coroutine) and ``chat_ctx.append``. On a menu press the
    callback (fired from the room data-received handler — sync event-loop
    context) schedules the ack via ``asyncio.ensure_future`` and appends
    the intent hint to the chat context (the emergency-addendum idiom).
    Never raises — a broken menu must never break keypad capture.
    """
    if not menu:
        return
    entries = {e.digit: e for e in menu}

    def _on_menu_digit(digit: str) -> None:
        entry = entries.get(digit)
        if entry is None:
            return
        try:
            metrics.ivr_menu_digit(entry.intent)
        except Exception:  # noqa: BLE001 - metrics must never break the call
            pass
        try:
            asyncio.ensure_future(
                agent.say(ack_phrase(entry), allow_interruptions=True)
            )
        except Exception as exc:  # noqa: BLE001 - ack is best-effort
            log.warning("ivr menu ack failed", error=str(exc)[:200])
        try:
            chat_ctx = getattr(agent, "chat_ctx", None)
            if chat_ctx is not None:
                chat_ctx.append(role="system", text=intent_hint(entry))
        except Exception as exc:  # noqa: BLE001 - hint is best-effort
            log.warning("ivr menu hint injection failed", error=str(exc)[:200])

    buffer.arm_menu(frozenset(entries), _on_menu_digit)
