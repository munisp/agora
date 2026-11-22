"""SPEC-W101 C2: outbound PSTN calls over LiveKit SIP (the dial path).

Inbound (Wave 5 #1, app/sip.py) hands PSTN calls to the receptionist; this
module is the reverse direction: the platform dials OUT through a LiveKit
SIP OUTBOUND trunk (deploy/livekit-sip/outbound-trunk-config.example.yaml),
creating a room per call that the agents worker then serves like any other
voice session (entrypoint recognizes the ``outcall-`` room prefix, C4).

Carrier discipline (VOICE-SCALING, docs/telephony.md "Outbound calls"):
**CPS — the carrier's call-START rate — is the binding constraint.** Every
dial passes through :class:`OutboundPacer`, a token bucket mirroring the
notification-worker ``OutboundPacer`` (OUTBOUND_CPS, default 1/s) so
campaign callers (waitlist backfill, no-show rebook) can loop this endpoint
and stay under the carrier's start-rate limit without re-implementing
pacing.

The livekit-api import + request construction is isolated in
:func:`_create_sip_participant` — if a livekit-api upgrade reshapes the
proto, the fix lives in that one function (the WorkerOptions TypeError
fallback philosophy). Tests drive fakes through the same seam.
"""

from __future__ import annotations

import asyncio
import time
import uuid
from dataclasses import dataclass
from typing import Any, Awaitable, Callable

from . import sip
from .logging import get_logger

log = get_logger("outbound")

OUTBOUND_ROOM_PREFIX = "outcall-"


class OutboundError(Exception):
    """Base for honest outbound-call failures."""

    status_code: int = 502


class TrunkNotConfigured(OutboundError):
    """OUTBOUND_TRUNK_ID is unset — the deploy step has not been run."""

    status_code = 503


class InvalidNumber(OutboundError):
    """The callee number is not a plausible E.164 MSISDN."""

    status_code = 422


class OutboundDialError(OutboundError):
    """The LiveKit SIP API rejected/failed the participant create."""

    status_code = 502


@dataclass
class OutboundCall:
    """Result of an accepted dial (status is always "dialing" — the SIP
    outcome arrives asynchronously via the room lifecycle)."""

    room: str
    call_id: str
    site_slug: str
    phone_number: str


def outbound_room_name(site_slug: str, call_id: str) -> str:
    return f"{OUTBOUND_ROOM_PREFIX}{site_slug}-{call_id}"


def site_slug_from_outbound_room(room_name: str) -> str | None:
    """`outcall-{slug}-{32 hex}` -> slug; None for non-outbound rooms."""
    if not room_name.startswith(OUTBOUND_ROOM_PREFIX):
        return None
    base = room_name[len(OUTBOUND_ROOM_PREFIX):]
    slug, sep, suffix = base.rpartition("-")
    if not sep or not slug or len(suffix) != 32:
        return None
    hexdigits = set("0123456789abcdef")
    if any(c not in hexdigits for c in suffix.lower()):
        return None
    return slug


def validate_callee(number: str) -> str:
    """Normalize to E.164 or raise InvalidNumber (never dials garbage)."""
    normalized = sip.normalize_phone(number)
    # normalize_phone strips formatting; require + and 8..15 digits (E.164).
    digits = normalized.lstrip("+")
    if not normalized.startswith("+") or not (8 <= len(digits) <= 15) or not digits.isdigit():
        raise InvalidNumber(
            f"callee '{number}' is not a valid E.164 number (expected +<country><number>)"
        )
    return normalized


class OutboundPacer:
    """Token bucket pacing outbound call STARTS (capacity 1, refill at
    ``cps``/s). Mirrors notification-worker OutboundPacer: the bucket never
    exceeds one start, so bursts serialize at the carrier's CPS limit.

    Clock and sleep are injectable — tests advance a fake clock instead of
    sleeping real seconds (no mocks on the production path: the default is
    the real monotonic clock + asyncio.sleep).
    """

    def __init__(
        self,
        cps: float = 1.0,
        *,
        clock: Callable[[], float] = time.monotonic,
        sleep: Callable[[float], Awaitable[None]] = asyncio.sleep,
    ) -> None:
        self._interval = 1.0 / cps if cps > 0 else 0.0
        self._next_at = 0.0
        self._clock = clock
        self._sleep = sleep
        self._lock = asyncio.Lock()

    @property
    def cps(self) -> float:
        return 1.0 / self._interval if self._interval > 0 else 0.0

    async def acquire(self) -> float:
        """Wait until the next call start is allowed; returns seconds waited."""
        async with self._lock:
            now = self._clock()
            wait = max(0.0, self._next_at - now)
            start = max(now, self._next_at)
            self._next_at = start + self._interval
        if wait > 0:
            await self._sleep(wait)
        return wait


def build_outbound_call_context(
    room_name: str, participants: Any = ()
) -> "sip.InboundCallContext | None":
    """C4: context for an outcall- room, or None for non-outbound rooms.

    The tenant binds from the ROOM NAME (`outcall-{slug}-{call_id}`) — the
    callee's number is the CUSTOMER's, never a key in the agents registry /
    TENANT_PHONE_MAP (those map OUR DIDs for inbound). The SIP callee
    participant attaches as the confirmed phone (we dialed them);
    tenant_source="outbound" marks the provenance in logs/metrics.
    """
    slug = site_slug_from_outbound_room(room_name)
    if slug is None:
        return None
    callee, _, attrs = sip.extract_call_info(room_name, participants)
    return sip.InboundCallContext(
        site_slug=slug,
        caller_phone=callee,
        dialed_number="",
        tenant_source="outbound",
        attributes=attrs,
    )


async def _create_sip_participant(api: Any, request: Any) -> Any:
    """The single livekit-api touchpoint: api.sip.create_sip_participant."""
    return await api.sip.create_sip_participant(request)


def _build_participant_request(trunk_id: str, room: str, call_to: str, identity: str) -> Any:
    """Construct the CreateSIPParticipant request for the INSTALLED
    livekit-api (lazy import — tests stub livekit.protocol.sip)."""
    from livekit.protocol import sip as proto_sip  # noqa: PLC0415 - lazy by design

    for cls_name in ("CreateSIPParticipantRequest", "SIPParticipantCreate"):
        cls = getattr(proto_sip, cls_name, None)
        if cls is None:
            continue
        kwargs = {
            "sip_trunk_id": trunk_id,
            "sip_call_to": call_to,
            "room_name": room,
            "participant_identity": identity,
        }
        try:
            return cls(**kwargs)
        except TypeError:
            # A future proto may rename fields; drop Nones and retry once.
            return cls(**{k: v for k, v in kwargs.items() if v is not None})
    raise OutboundDialError(
        "installed livekit-api exposes no CreateSIPParticipant request type "
        "(livekit.protocol.sip) — upgrade/align the livekit-api pin"
    )


async def place_outbound_call(
    settings: Any,
    site_slug: str,
    phone_number: str,
    *,
    api: Any | None = None,
) -> OutboundCall:
    """Dial ``phone_number`` (E.164) on behalf of ``site_slug``.

    Creates one room per call (``outcall-{slug}-{call_id}``) and places the
    SIP participant into it through the configured outbound trunk; the
    agents worker joins the room and runs the receptionist session. The
    returned status is honestly "dialing" — answer/decline/voicemail is an
    asynchronous SIP outcome surfaced through the room lifecycle.
    """
    trunk_id = (getattr(settings, "outbound_trunk_id", "") or "").strip()
    if not trunk_id:
        raise TrunkNotConfigured(
            "OUTBOUND_TRUNK_ID is not set — create the outbound trunk "
            "(deploy/livekit-sip/setup.sh) and configure the id first"
        )
    callee = validate_callee(phone_number)
    slug = (site_slug or "").strip()
    if not slug or any(c in slug for c in "/\\") or ".." in slug:
        raise InvalidNumber(f"site_slug '{site_slug}' is not a safe slug")
    call_id = uuid.uuid4().hex
    room = outbound_room_name(slug, call_id)

    close_api = False
    if api is None:
        from livekit.api import LiveKitAPI  # noqa: PLC0415 - lazy by design

        api = LiveKitAPI(
            settings.livekit_url, settings.livekit_api_key, settings.livekit_api_secret
        )
        close_api = True
    try:
        request = _build_participant_request(
            trunk_id, room, callee, identity=f"out-{callee}"
        )
        await _create_sip_participant(api, request)
    except OutboundError:
        raise
    except Exception as exc:  # noqa: BLE001 - provider/SDK failure surfaces as 502
        log.error("outbound dial failed", room=room, error=str(exc)[:300])
        raise OutboundDialError(f"livekit SIP dial failed: {exc}") from exc
    finally:
        if close_api and hasattr(api, "aclose"):
            await api.aclose()

    log.info(
        "outbound call dialing",
        room=room,
        site_slug=slug,
        callee=callee,
        trunk_id=trunk_id,
    )
    return OutboundCall(room=room, call_id=call_id, site_slug=slug, phone_number=callee)
