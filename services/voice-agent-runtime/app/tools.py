"""The six receptionist tools (SPEC §11), shared by the LiveKit voice worker,
the text chat endpoint and the ElevenLabs webhook.

EXACT tool names: get_business_info, get_availability, book_appointment,
lookup_appointment, reschedule_appointment, cancel_appointment.

- Read-only tools call booking-service public endpoints via Dapr service
  invocation (app-id `booking`).
- Mutating tools publish CloudEvents commands to Kafka topic
  `opendesk.booking.commands` via Dapr pubsub component `pubsub-kafka`;
  the CloudEvent id is reused as the idempotency key (`data.idempotency_key`).
- Phone-confirmation policy (SPEC §1/§11): book_appointment refuses without
  a confirmed phone in session state (see session_state.py).
- SPEC-W45 K15(c): lookup/reschedule/cancel additionally require a VERIFIED
  session — OTP via the booking customer portal (request_verification_code
  -> verify_caller_code) or a channel-pinned identity (WhatsApp wa_id).
  Fail-closed (verification_unavailable) when BOOKING_URL /
  VOICE_BOOKING_INTERNAL_TOKEN are unset; the SIP carrier-asserted bypass
  is removed (OOS-03).
- SPEC-W45 K15(e): the escalation staff LiveKit token never rides the
  events topic (internal mint endpoint / targeted in-room delivery).
- SPEC-W45 K15(f): caller phones in ToolInvoked/capture_location events
  are HMAC-hashed (PHONE_HASH_SALT, W28 scheme) or omitted.
- SPEC-W81 A2: explicit risk-tier map (VOICE_TOOL_TIERS, app/config.py)
  enforced at dispatch() as the single choke point — tier 0 pass / tier 1
  confirmed-or-verified / tier 2 verified; the map formalizes the existing
  gates (never weakens them) and hard-gates pack plugin tools (default
  tier 1).
- SPEC-W86: read_keypad_entry — DTMF keypad capture for PSTN calls (the
  app/dtmf.py per-session buffer fed by the LiveKit room data channel;
  tier 0 passive listener; keypad_unavailable on non-PSTN channels — an
  honest channel-capability report, never a mock).
"""

from __future__ import annotations

import asyncio
import uuid
from datetime import datetime, timedelta, timezone
from typing import Any, Coroutine, Optional

from . import dtmf, metrics, sip, ui_actions
from .config import (
    DEFAULT_VOICE_TOOL_TIERS,
    TIER_HIGH,
    TIER_LOW,
    Settings,
)
from .dapr_client import DaprClient
from .escalation import LiveKitEscalation, escalation_room_name
from .events import new_cloudevent
from .logging import get_logger
from .plugin_tools import PluginTool
from .session_state import PhoneConfirmationRequired, SessionState
from .tenant_context import TenantContext
from .verification import BookingPortalVerifier, canonical_e164, hash_phone, normalize_digits

log = get_logger("tools")

BOOK = "com.opendesk.booking.command.BookAppointment"
RESCHEDULE = "com.opendesk.booking.command.RescheduleAppointment"
CANCEL = "com.opendesk.booking.command.CancelAppointment"
ESCALATION_REQUESTED = "com.opendesk.conversation.EscalationRequested"

# SPEC-W86: server-side bounds for the read_keypad_entry tool. The model
# may ask for anything; the tool clamps before arming the DTMF buffer so a
# call can never wedge the session on an unbounded keypad wait.
KEYPAD_TIMEOUT_MIN_S = 5
KEYPAD_TIMEOUT_MAX_S = 60
KEYPAD_LENGTH_MIN = 4
KEYPAD_LENGTH_MAX = 8

# OpenAI-format tool schemas, used by the chat path and the ElevenLabs
# webhook (the LiveKit worker derives its schema from the FunctionContext
# docstrings/signatures with the same names).
TOOL_SCHEMAS: list[dict[str, Any]] = [
    {
        "type": "function",
        "function": {
            "name": "get_business_info",
            "description": "Get business information: catalog (offerings with ids, durations, prices), team members, timezone, currency and terminology.",
            "parameters": {"type": "object", "properties": {}, "required": []},
        },
    },
    {
        "type": "function",
        "function": {
            "name": "get_availability",
            "description": "Get open appointment slots for an offering with a team member in a time range.",
            "parameters": {
                "type": "object",
                "properties": {
                    "offering_id": {"type": "string", "description": "Offering UUID"},
                    "team_member_id": {"type": "string", "description": "Team member UUID"},
                    "from_iso": {"type": "string", "description": "Range start, RFC3339"},
                    "to_iso": {"type": "string", "description": "Range end, RFC3339 (max 62 days)"},
                },
                "required": ["offering_id", "team_member_id", "from_iso", "to_iso"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "book_appointment",
            "description": "Book an appointment. Requires the caller's phone number (phone-confirmation policy).",
            "parameters": {
                "type": "object",
                "properties": {
                    "offering_id": {"type": "string"},
                    "team_member_id": {"type": "string"},
                    "starts_at": {"type": "string", "description": "RFC3339 start time"},
                    "phone": {"type": "string", "description": "Caller phone number"},
                    "contact_name": {"type": "string"},
                    "email": {"type": "string"},
                },
                "required": ["offering_id", "team_member_id", "starts_at", "phone"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "lookup_appointment",
            "description": "Look up the caller's upcoming appointments by phone number.",
            "parameters": {
                "type": "object",
                "properties": {
                    "phone": {"type": "string", "description": "Caller phone number"},
                },
                "required": ["phone"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "reschedule_appointment",
            "description": "Reschedule an existing booking to a new start time.",
            "parameters": {
                "type": "object",
                "properties": {
                    "booking_id": {"type": "string", "description": "Booking UUID"},
                    "starts_at": {"type": "string", "description": "New start, RFC3339"},
                    "phone": {"type": "string", "description": "Caller phone number"},
                },
                "required": ["booking_id", "starts_at", "phone"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "cancel_appointment",
            "description": "Cancel an existing booking.",
            "parameters": {
                "type": "object",
                "properties": {
                    "booking_id": {"type": "string", "description": "Booking UUID"},
                    "phone": {"type": "string", "description": "Caller phone number"},
                    "reason": {"type": "string"},
                },
                "required": ["booking_id", "phone"],
            },
        },
    },
    # SPEC-W45 K15(c): caller verification (OTP). The agent sends a
    # one-time code to the caller's phone via the booking customer portal,
    # then verifies the code the caller reads back. Only a session verified
    # this way (or via a channel-pinned identity) may run the mutating
    # tools (lookup/reschedule/cancel).
    {
        "type": "function",
        "function": {
            "name": "request_verification_code",
            "description": (
                "Send a one-time verification code to the caller's phone "
                "number (SMS/email via the booking portal). REQUIRED before "
                "looking up, rescheduling or cancelling an existing booking. "
                "After calling this, ask the caller to read you the code and "
                "submit it with verify_caller_code."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "phone": {"type": "string", "description": "Caller phone number"},
                },
                "required": ["phone"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "verify_caller_code",
            "description": (
                "Verify the one-time code the caller read back. On success "
                "the session is verified and lookup/reschedule/cancel may run."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "phone": {"type": "string", "description": "Caller phone number"},
                    "code": {"type": "string", "description": "One-time code read back by the caller"},
                },
                "required": ["phone", "code"],
            },
        },
    },
    # SPEC-W86: DTMF keypad capture (PSTN calls only). The caller types
    # the OTP on the phone keypad instead of reading it aloud — digit ASR
    # in noisy environments is error-prone, and reading a security code
    # aloud in public is a privacy leak. On non-PSTN channels the tool
    # answers keypad_unavailable (honest channel-capability report).
    {
        "type": "function",
        "function": {
            "name": "read_keypad_entry",
            "description": (
                "Read digits the caller types on their phone keypad "
                "(phone calls only). Use after request_verification_code "
                "so the caller can type the 6-digit code instead of "
                "reading it aloud: call with expected_length 6, then "
                "submit the returned digits with verify_caller_code. The "
                "caller presses # to submit early or * to clear."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "timeout_s": {
                        "type": "integer",
                        "description": "Seconds to wait for the entry (clamped 5-60, default 30)",
                    },
                    "expected_length": {
                        "type": "integer",
                        "description": "Optional expected digit count (clamped 4-8); auto-submits when reached, e.g. 6 for portal codes",
                    },
                },
                "required": [],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "send_whatsapp_confirmation",
            "description": (
                "Send the person a WhatsApp message confirming their appointment (service, with whom, when). Call it "
                "once, right after they confirm an appointment or you book one. Mention the WhatsApp only if this "
                "returns status sent."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "offering": {"type": "string", "description": "The service, e.g. 'RSA Registration and Onboarding Call'"},
                    "team_member": {"type": "string", "description": "Who it is with, e.g. 'Aisha Bello' (empty if unknown)"},
                    "when": {"type": "string", "description": "Day and time as you said it, e.g. 'Tuesday 24 November at 9:00 am'"},
                },
                "required": ["offering", "when"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "send_document_upload_link",
            "description": (
                "When the person needs to send you a document (a pay stub, a receipt, an ID, a letter): on a web chat this "
                "tells you to point them to the paperclip; on a phone call it WhatsApps them a secure upload link. Say only "
                "what the result's message tells you."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "document_type": {"type": "string", "enum": ["pay_stub", "receipt", "document"],
                                      "description": "What they will send: pay_stub, receipt, or document for anything else"},
                },
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "check_shared_documents",
            "description": (
                "List the documents the person has shared in this conversation and where each check stands (received, "
                "still being checked, passed, or needs a person to review). Use it when they say they have sent one."
            ),
            "parameters": {"type": "object", "properties": {}},
        },
    },
    {
        "type": "function",
        "function": {
            "name": "save_caller_details",
            "description": (
                "Save the caller's name, email and phone number to the CRM as soon as they have CONFIRMED the "
                "read-back at the start of the call, so the business can always reach them, even if they never "
                "book. Call it once, right after the caller says the details are correct."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "name": {"type": "string", "description": "Caller's full name, e.g. 'Ngozi Okafor'"},
                    "phone": {"type": "string", "description": "Caller's phone number as digits, e.g. '08065553108'"},
                    "email": {"type": "string", "description": "Caller's email, e.g. 'ngozi.okafor@gmail.com' (empty if none)"},
                },
                "required": ["name", "phone"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "capture_location",
            "description": (
                "Save the caller's location to their contact record "
                "(emergency / location-first flow). Resolve the address the "
                "caller gave into address_text; pass lat and lng only when "
                "the caller explicitly provided coordinates."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "address_text": {
                        "type": "string",
                        "description": "Spoken address / landmark, e.g. '12 Allen Avenue, Ikeja, Lagos'",
                    },
                    "lat": {"type": "number", "description": "Latitude (only with lng)"},
                    "lng": {"type": "number", "description": "Longitude (only with lat)"},
                },
                "required": [],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "request_human",
            "description": (
                "Escalate the conversation to a human staff member (warm "
                "handoff). Creates a LiveKit escalation room, notifies staff "
                "and confirms to the caller. Use when the caller asks for a "
                "human, is distressed, or the request cannot be resolved. "
                "Tell the customer exactly what the result's message says "
                "(in a typed chat staff reply in the chat; never mention a call)."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "reason": {
                        "type": "string",
                        "description": "Short reason for the escalation",
                    },
                },
                "required": [],
            },
        },
    },
    # SPEC-W9 Part B: agent-driven UI actions. These tools never execute
    # anything server-side — a validated action is attached to the chat
    # turn's outgoing payload and applied by the web widget (embed.js).
    {
        "type": "function",
        "function": {
            "name": "navigate_to_page",
            "description": (
                "Show the visitor a page on this website (web chat only). "
                "The path must be same-origin: it starts with '/' and has no "
                "scheme or host."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {
                        "type": "string",
                        "description": "Same-origin path, e.g. '/rooms' or '/#booking'",
                    },
                },
                "required": ["path"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "highlight_element",
            "description": (
                "Highlight an element on the visitor's page (web chat only): "
                "it scrolls into view and pulses briefly. Use a simple CSS "
                "selector such as '#booking-form' or '.offerings'."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "selector": {
                        "type": "string",
                        "description": "CSS selector (letters, digits, - _ # . : [ ] = \" ' > only, max 120 chars)",
                    },
                },
                "required": ["selector"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "prefill_booking",
            "description": (
                "Pre-select an offering in the visitor's booking form (web "
                "chat only). Use an offering id from get_business_info."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "offering_id": {
                        "type": "string",
                        "description": "Offering UUID",
                    },
                },
                "required": ["offering_id"],
            },
        },
    },
]

# SPEC-W46 P9: best-effort publishes (ToolInvoked events) are scheduled
# fire-and-forget so the tool path never awaits a daprd RTT before the LLM
# resumes. Tasks are strongly referenced until completion and failures are
# logged from the done-callback (the publish helper itself never raises).
_BACKGROUND_TASKS: "set[asyncio.Task[Any]]" = set()


def _log_background_failure(task: "asyncio.Task[Any]") -> None:
    _BACKGROUND_TASKS.discard(task)
    if task.cancelled():
        return
    exc = task.exception()
    if exc is not None:
        log.warning("background publish task failed", error=str(exc)[:200])


def _fire_and_forget(coro: Coroutine[Any, Any, Any]) -> None:
    """Schedule `coro` on the running loop; exceptions are logged, never raised."""
    try:
        task = asyncio.create_task(coro)
    except RuntimeError as exc:  # no running loop — must not leak the coroutine
        coro.close()
        log.warning("background publish skipped: no running event loop", error=str(exc))
        return
    _BACKGROUND_TASKS.add(task)
    task.add_done_callback(_log_background_failure)


TOOL_NAMES = [t["function"]["name"] for t in TOOL_SCHEMAS]

# SPEC-W9 Part B: names of the agent-driven UI action tools (validated
# client-side actions attached to the chat turn's outgoing payload).
UI_ACTION_TOOL_NAMES = ["navigate_to_page", "highlight_element", "prefill_booking"]


def _clamp_int(value: Any, minimum: int, maximum: int, default: "int | None") -> "int | None":
    """Coerce to int and clamp into [minimum, maximum]; un-coercible -> default
    (SPEC-W86: server-side bounds for model-supplied tool arguments)."""
    try:
        number = int(value)
    except (TypeError, ValueError):
        return default
    return max(minimum, min(maximum, number))


def _confirmation_payload(pending_phone: str) -> dict[str, Any]:
    return {
        "status": "confirmation_required",
        "message": (
            "Phone-confirmation policy: read the phone number "
            f"{pending_phone or '(not provided)'} back to the caller and ask "
            "them to confirm it, then call this tool again with the same number."
        ),
        "pending_phone": pending_phone,
    }


def _phones_match(a: str | None, b: str | None) -> bool:
    """SPEC-W88: form-independent phone equality for the verification
    gates. Both sides reduce to digits (normalize_digits) and must be
    NON-EMPTY and equal. ``+234…`` and ``234…`` are the same subscriber;
    national vs international forms never fold (trunk-0 kept — the W61
    G3 ruling), so two genuinely different numbers cannot collide. The
    gates therefore stay fail-closed: only the "+"-vs-no-"+" divergence
    (WhatsApp wa_id pin vs SIP/OTP "+"-forms) stops false-mismatching."""
    da, db = normalize_digits(a or ""), normalize_digits(b or "")
    return bool(da) and da == db


def _verification_required_payload() -> dict[str, Any]:
    """K15(c): mutating tools refuse unverified sessions — the agent must
    drive the OTP flow (request_verification_code -> verify_caller_code)."""
    return {
        "status": "verification_required",
        "message": (
            "For the caller's security, this phone number must be verified "
            "before bookings can be looked up or changed. Call "
            "request_verification_code with the caller's number to send a "
            "one-time code, ask the caller to read it back, then submit it "
            "with verify_caller_code."
        ),
    }


def _tier_confirmation_required_payload(tool: str) -> dict[str, Any]:
    """SPEC-W81 A2: tier-1 refusal for tools WITHOUT their own confirmation
    flow (pack plugin tools). Built-in tier-1 tools keep their existing
    in-method read-back flow and the byte-compatible _confirmation_payload;
    this shape is used ONLY at the dispatch choke point for plugin tools
    (and never for the built-ins, so the existing refusal payloads are
    untouched)."""
    return {
        "status": "confirmation_required",
        "error": "confirmed_phone_required",
        "tool": tool,
        "tier": 1,
        "message": (
            "This tool requires a confirmed caller identity. Collect the "
            "caller's phone number and have it confirmed (the "
            "book_appointment read-back flow, or verify_caller_code for a "
            "verified identity) before calling this tool again."
        ),
    }


def _verification_unavailable_payload() -> dict[str, Any]:
    """Honest degrade (503-equivalent on the tool path): verification is
    not configured (BOOKING_URL / VOICE_BOOKING_INTERNAL_TOKEN unset), so
    NO session can be verified and mutating tools must refuse rather than
    fall back to self-asserted numbers."""
    return {
        "status": "error",
        "error": "verification_unavailable",
        "message": (
            "Phone verification is temporarily unavailable, so I can't look "
            "up or change existing bookings right now. Please try again "
            "later or contact the business directly."
        ),
    }



def known_id(value: str, candidates: list[str]) -> str:
    """The real id the model meant. It copies long UUIDs out of the prompt and sometimes slips a character
    ("4a5dca9a-3663-…" for "…-3963-…"): the booking service then 400s and the caller hears "trouble reaching our
    booking system". An id that is not known exactly but is ≥ 0.9 similar to exactly one known id is that id."""
    import difflib

    v = str(value or "").strip()
    if not v or v in candidates or not candidates:
        return v
    scored = sorted(((difflib.SequenceMatcher(None, v, c).ratio(), c) for c in candidates), reverse=True)
    best = scored[0]
    if best[0] >= 0.9 and (len(scored) == 1 or scored[1][0] < best[0] - 0.05):
        return best[1]
    return v


def _rfc3339(value: str, tz_name: str | None) -> str:
    """The booking API needs RFC3339 (with an offset). The model often sends a bare local time ("2026-11-18T08:00:00"),
    which booking rejected with 400 "from must be RFC3339" and the caller heard "trouble reaching our booking system".
    A bare date/time is read in the business's time zone; anything unparseable is passed through unchanged."""
    from zoneinfo import ZoneInfo

    raw = str(value or "").strip()
    try:
        dt = datetime.fromisoformat(raw.replace("Z", "+00:00"))
    except ValueError:
        return raw
    if dt.tzinfo is not None:
        # A UTC time from the model is a local time with the wrong marker: a receptionist in Lagos asked for "12 noon"
        # sent "...T12:00:00Z" and booked 1pm. Read UTC wall-clock times in the business's zone; real offsets
        # ("+01:00") pass through unchanged.
        if dt.utcoffset() == timedelta(0) and (tz_name or "UTC") not in ("UTC", "Etc/UTC", "GMT"):
            try:
                return dt.replace(tzinfo=ZoneInfo(tz_name)).isoformat()
            except Exception:  # noqa: BLE001
                return raw
        return raw
    if dt.tzinfo is None:
        try:
            dt = dt.replace(tzinfo=ZoneInfo(tz_name or "UTC"))
        except Exception:  # noqa: BLE001
            dt = dt.replace(tzinfo=timezone.utc)
    return dt.isoformat()


def _past_window_note(to_iso: str, tz_name: str | None, now: datetime | None = None) -> str | None:
    """A note when the requested window ends before today in the business's time zone, else None."""
    from zoneinfo import ZoneInfo

    try:
        end = datetime.fromisoformat(str(to_iso).replace("Z", "+00:00"))
    except ValueError:
        return None
    try:
        tz = ZoneInfo(tz_name or "UTC")
    except Exception:  # noqa: BLE001
        tz = ZoneInfo("UTC")
    if end.tzinfo is None:
        end = end.replace(tzinfo=tz)
    today = (now or datetime.now(timezone.utc)).astimezone(tz).date()
    if end.astimezone(tz).date() >= today:
        return None
    return (f"The requested dates ({end.astimezone(tz).date().isoformat()}) are in the past. Today is "
            f"{today.strftime('%A')} {today.isoformat()}. Check again with a date from today onwards.")

class ToolLayer:
    """Implements the six tools against Dapr. One instance per session."""

    def __init__(
        self,
        *,
        dapr: DaprClient,
        settings: Settings,
        ctx: TenantContext,
        session: SessionState,
        escalation: LiveKitEscalation | None = None,
        plugin_tools: list[PluginTool] | None = None,
        ui_action_sink: list[dict[str, Any]] | None = None,
        verifier_factory: "Any | None" = None,
        dtmf_buffer: "dtmf.DtmfBuffer | None" = None,
        native_keypad: bool = False,
        ivr_menu_hint: str = "",
    ) -> None:
        self._dapr = dapr
        self._settings = settings
        self._ctx = ctx
        self._session = session
        self._escalation = escalation or LiveKitEscalation(settings)
        self._plugin_tools = {t.name: t for t in (plugin_tools or [])}
        # Injectable BookingPortalVerifier factory (tests; same pattern as
        # the injectable agents-registry client in app/sip.py). None = the
        # real client built from settings on every OTP call.
        self._verifier_factory = verifier_factory
        # SPEC-W86: per-session DTMF keypad buffer, attached by the LiveKit
        # worker for PSTN (SIP) calls only; None on every other channel —
        # read_keypad_entry then answers keypad_unavailable.
        self._dtmf_buffer = dtmf_buffer
        # SPEC-W93: ElevenLabs hosted backend — ElevenLabs collects keypad
        # digits natively (out-of-band) and relays them to the hosted agent
        # as a user turn; read_keypad_entry reports keypad_native instead
        # of keypad_unavailable. The LiveKit buffer always wins when set.
        self._native_keypad = native_keypad
        # SPEC-W96 IVR-A: the IVR menu sentence appended to the
        # keypad_native tool answer so the hosted agent knows the keypad
        # shortcuts even though digits never transit this runtime. Empty
        # when the menu is disabled or absent.
        self._ivr_menu_hint = ivr_menu_hint
        # SPEC-W9 Part B: per-turn collector for validated UI actions. The
        # chat path injects a fresh list per turn; other callers (voice
        # worker, ElevenLabs) get a private one that is simply never read —
        # the tools still ack so the model gets a sensible answer.
        self._ui_actions = ui_action_sink if ui_action_sink is not None else []

    @property
    def tenant_context(self) -> TenantContext:
        return self._ctx

    @property
    def collected_ui_actions(self) -> list[dict[str, Any]]:
        """Validated UI actions queued by tools during this turn (SPEC-W9 B)."""
        return self._ui_actions

    def _tool_allowlist(self) -> set[str]:
        """SPEC-W38 F2 ``tool_allowlist`` from the agent definition merged
        onto the tenant context (app/agent_definition.py). Empty/absent =
        all tools allowed (legacy behaviour)."""
        definition = getattr(self._ctx, "agent_definition", None)
        if definition is None:
            return set()
        return {str(n) for n in (definition.tool_allowlist or [])}

    def schemas(self) -> list[dict[str, Any]]:
        """Built-in tool schemas plus any pack plugin tool schemas.

        SPEC-W38 F2: a non-empty definition tool_allowlist filters the
        merged list to the named tools only."""
        merged = TOOL_SCHEMAS + [t.schema() for t in self._plugin_tools.values()]
        allowlist = self._tool_allowlist()
        if not allowlist:
            return merged
        return [s for s in merged if s["function"]["name"] in allowlist]

    # ------------------------------------------------------------------ util
    async def _emit_tool_event(self, tool: str, status: str, detail: dict[str, Any]) -> None:
        """Record + publish the ToolInvoked event.

        SPEC-W46 P9: the daprd publish is best-effort (publish_best_effort
        already never raises) so it is scheduled fire-and-forget — the tool
        path returns to the LLM immediately instead of awaiting a sidecar
        RTT. The per-session quality accumulator stays synchronous.
        """
        # Per-session quality accumulator (every tool invocation lands here,
        # on every path: LiveKit worker, chat tool loop, ElevenLabs webhook).
        metrics.session_tool_call(tool)
        event = new_cloudevent(
            type_="com.opendesk.conversation.ToolInvoked",
            subject=self._ctx.tenant_slug,
            tenant_uuid=self._ctx.tenant_id,
            data={
                "conversationId": self._session.conversation_id,
                "tool": tool,
                "status": status,
                "detail": detail,
            },
        )
        _fire_and_forget(
            self._dapr.publish_best_effort(
                self._settings.dapr_pubsub,
                self._settings.conversation_events_topic,
                event,
                kind="ToolInvoked",
            )
        )

    def _require_phone(self, phone: str | None) -> str:
        if not self._settings.phone_confirmation_required:
            return (phone or self._session.confirmed_phone or "").strip()
        return self._session.require_confirmed_phone(phone)

    # ------------------------------------------------------ K15(c) OTP gate
    def _verifier(self) -> BookingPortalVerifier:
        if self._verifier_factory is not None:
            return self._verifier_factory()
        return BookingPortalVerifier(
            base_url=self._settings.booking_url,
            internal_token=self._settings.voice_booking_internal_token,
            site_slug=self._ctx.site_slug,
            timeout_s=self._settings.http_timeout_s,
        )

    async def _require_verified(self, phone: str | None) -> str | dict[str, Any]:
        """K15(c): mutating tools (lookup/reschedule/cancel) require a
        VERIFIED session (OTP or channel-pinned identity).

        Returns the verified phone to use for the operation, or the refusal
        payload to hand back to the model. The verified number ALWAYS wins
        over a model-supplied one; a mismatch is rejected (no silent
        re-targeting at another customer's bookings).

        SPEC-W87: before refusing an unverified PSTN caller, honor an
        active booking-side verification grant under the strict anchor
        rule (see _honor_verification_grant)."""
        verified = self._session.verified_phone
        if not verified:
            granted = await self._honor_verification_grant(phone)
            if granted is not None:
                return granted
            if not (
                self._settings.booking_url
                and self._settings.voice_booking_internal_token
            ):
                log.error(
                    "mutating tool refused: caller verification not "
                    "configured (BOOKING_URL / VOICE_BOOKING_INTERNAL_TOKEN "
                    "unset) — failing closed"
                )
                return _verification_unavailable_payload()
            return _verification_required_payload()
        supplied = sip.normalize_phone(phone)
        if supplied and not _phones_match(supplied, verified):
            return {
                "status": "error",
                "error": "phone_mismatch",
                "message": (
                    "That number does not match the verified caller number "
                    "for this session. Use the verified number, or verify "
                    "the new number first."
                ),
            }
        return verified

    async def _honor_verification_grant(self, phone: str | None) -> str | None:
        """SPEC-W87: honor a durable booking-side verification grant.

        Returns the verified phone when the grant applies, else None (the
        caller falls through to the byte-identical OTP refusal payloads).
        ALL of the anchor rule must hold:
          a. grants enabled (settings.verification_grants_enabled),
          b. session.channel == "phone" — the SIP carrier-asserted anchor
             ONLY; web self-claims NEVER honor grants (whatsapp/telegram
             identities are already per-turn pinned and never reach here),
          c. session.claimed_phone present, and the model-supplied phone
             absent or equal to the claimed one (no re-targeting),
          d. the booking check answers verified for the claimed phone.
        On honor: mark_verified(claimed) + stepup_verified("grant") + log
        — mirroring the chat.py channel-pin path (NO ToolInvoked event).
        A check error/unavailable is treated as no-grant (fail-safe)."""
        if not getattr(self._settings, "verification_grants_enabled", False):
            return None
        if self._session.channel != "phone":
            return None
        claimed = self._session.claimed_phone
        if not claimed:
            return None
        supplied = sip.normalize_phone(phone)
        if supplied and not _phones_match(supplied, claimed):
            return None
        verifier = self._verifier()
        try:
            result = await verifier.check_grant(claimed)
        finally:
            await verifier.aclose()
        if not result.ok:
            if result.status not in ("no_grant",):
                log.warning(
                    "verification grant check failed — falling through to OTP",
                    status=result.status,
                    conversation_id=self._session.conversation_id,
                )
            return None
        verified = self._session.mark_verified(claimed)
        metrics.stepup_verified("grant")
        log.info(
            "verification grant honored",
            conversation_id=self._session.conversation_id,
            channel=self._session.channel,
        )
        return verified

    async def _refuse_gate(self, tool: str, gate: dict[str, Any]) -> dict[str, Any]:
        """Emit the ToolInvoked event for a verification-gate refusal and
        return the refusal payload to the model."""
        if gate.get("error") == "verification_unavailable":
            status = "verification_unavailable"
        elif gate.get("error") == "phone_mismatch":
            status = "phone_mismatch"
        else:
            status = "verification_required"
        metrics.stepup_refused(status)
        await self._emit_tool_event(tool, status, {})
        return gate

    # ------------------------------------------- SPEC-W81 A2: tier policy
    def _tool_tier(self, name: str) -> int:
        """Effective risk tier for ``name`` (VOICE_TOOL_TIERS map).

        Tools absent from the map — pack plugin tools included — DEFAULT to
        tier 1 (confirmed identity), never silently tier 0. The loader
        (app/config.py parse_voice_tool_tiers) already rejected invalid
        values at startup; the int coercion here is belt-and-braces for
        programmatically built Settings."""
        tiers = getattr(self._settings, "voice_tool_tiers", None) or {}
        try:
            base = int(tiers.get(name, 1))
        except (TypeError, ValueError):
            base = 1
        # SPEC-W84: per-tenant pack stepUpPolicy.tools overrides the env map
        # (pack = tenant refinement; env = operator global). The pack parser
        # (config.parse_pack_tool_tiers) already floored built-ins, so a pack
        # entry can never sit below DEFAULT_VOICE_TOOL_TIERS.
        ctx = getattr(self, "_ctx", None)
        pack_tiers = getattr(ctx, "pack_tool_tiers", None) or {}
        if name in pack_tiers:
            try:
                return int(pack_tiers[name])
            except (TypeError, ValueError):
                return base
        return base

    async def _enforce_tool_tier(
        self, name: str, arguments: dict[str, Any]
    ) -> dict[str, Any] | None:
        """SPEC-W81 A2: the SINGLE tier-policy choke point (dispatch).

        Returns the refusal payload to hand back to the model, or None to
        proceed. The map FORMALIZES the existing ladder and never weakens
        it: built-in tools at their default tier self-enforce inside their
        methods on EVERY path (book_appointment: read-back confirmation;
        lookup/reschedule/cancel: _require_verified) with byte-compatible
        refusal payloads, so the choke point deliberately does NOT
        double-gate them. It hard-gates:
        - pack plugin tools (no internal gate — default tier 1), and
        - any tool whose configured tier was RAISED above its built-in
          floor (the loader refuses lowering).
        """
        tier = self._tool_tier(name)
        if tier <= TIER_LOW:
            return None
        metrics.stepup_required(name, tier)
        floor = DEFAULT_VOICE_TOOL_TIERS.get(name)
        if floor is not None and tier <= floor:
            # Built-in at its (un-lowerable) default tier: the method's own
            # gate enforces it on every path — no double refusal here.
            return None
        if tier >= TIER_HIGH:
            gate = await self._require_verified(arguments.get("phone"))
            if isinstance(gate, dict):
                return await self._refuse_gate(name, gate)
            return None
        # Tier 1 (MEDIUM): confirmed (read-back) or verified identity.
        if self._session.confirmed_phone or self._session.verified_phone:
            return None
        metrics.stepup_refused("confirmation_required")
        await self._emit_tool_event(name, "confirmation_required", {})
        return _tier_confirmation_required_payload(name)

    def _event_phone_detail(self, phone: str) -> dict[str, Any]:
        """K15(f): phone detail for ToolInvoked/capture_location events —
        HMAC-hashed (W28 scheme) when PHONE_HASH_SALT is set, OMITTED
        entirely otherwise (never plaintext)."""
        if not self._settings.phone_hash_salt:
            log.warning(
                "PHONE_HASH_SALT unset — omitting caller phone from event "
                "payload (fail closed, no plaintext)"
            )
            return {}
        return {
            "phone_hash": hash_phone(
                self._settings.phone_hash_salt, self._ctx.tenant_id, phone
            )
        }

    async def _publish_command(self, type_: str, data: dict[str, Any]) -> str:
        """Publish a booking command; returns the CloudEvent id (idempotency key)."""
        event_id = str(uuid.uuid4())
        data = {**data, "idempotency_key": event_id}
        event = new_cloudevent(
            type_=type_,
            subject=self._ctx.tenant_slug,  # tenant slug
            tenant_uuid=self._ctx.tenant_id,  # tenant UUID (consumer parses it)
            data=data,
            event_id=event_id,
        )
        await self._dapr.publish(
            self._settings.dapr_pubsub, self._settings.booking_commands_topic, event
        )
        log.info("booking command published", type=type_, event_id=event_id)
        return event_id

    # ------------------------------------------------------------- read-only
    async def get_business_info(self) -> dict[str, Any]:
        ctx = self._ctx
        result = {
            "business": ctx.display_name,
            "timezone": ctx.timezone,
            "currency": ctx.currency,
            "locale": ctx.locale,
            "terminology": ctx.terminology,
            "offerings": [
                {
                    "id": o.get("id"),
                    "name": o.get("name"),
                    "description": o.get("description"),
                    "duration_min": o.get("duration_min"),
                    "price_cents": o.get("price_cents"),
                    "currency": ctx.currency,
                }
                for o in ctx.offerings
            ],
            "team_members": [
                {"id": m.get("id"), "name": m.get("name"), "role": m.get("role")}
                for m in ctx.team_members
            ],
        }
        await self._emit_tool_event("get_business_info", "ok", {})
        return result

    def _real_ids(self, offering_id: str, team_member_id: str) -> tuple[str, str]:
        offerings = [str(o.get("id")) for o in (self._ctx.offerings or []) if o.get("id")]
        team = [str(m.get("id")) for m in (self._ctx.team_members or []) if m.get("id")]
        return known_id(offering_id, offerings), known_id(team_member_id, team)

    async def get_availability(
        self, offering_id: str, team_member_id: str, from_iso: str, to_iso: str
    ) -> dict[str, Any]:
        offering_id, team_member_id = self._real_ids(offering_id, team_member_id)
        # A window that ends before today cannot have free slots; the model sometimes guesses the wrong year. Say so
        # (with today's date) instead of returning an empty list it reads as "fully booked".
        past = _past_window_note(to_iso, self._ctx.timezone)
        if past is not None:
            await self._emit_tool_event("get_availability", "past_date", {"offering_id": offering_id})
            return {"offering_id": offering_id, "team_member_id": team_member_id, "timezone": self._ctx.timezone,
                    "slots": [], "note": past}
        resp = await self._dapr.invoke_get(
            self._settings.booking_app_id,
            f"public/sites/{self._ctx.site_slug}/availability",
            params={
                "offering_id": offering_id,
                "team_member_id": team_member_id,
                "from": _rfc3339(from_iso, self._ctx.timezone),
                "to": _rfc3339(to_iso, self._ctx.timezone),
            },
        )
        slots = (resp or {}).get("slots") or []
        await self._emit_tool_event(
            "get_availability", "ok", {"slots": len(slots), "offering_id": offering_id}
        )
        return {
            "offering_id": offering_id,
            "team_member_id": team_member_id,
            "timezone": self._ctx.timezone,
            "slots": slots,
        }

    # -------------------------------------------------------------- mutating
    async def book_appointment(
        self,
        offering_id: str,
        team_member_id: str,
        starts_at: str,
        phone: str,
        contact_name: str | None = None,
        email: str | None = None,
    ) -> dict[str, Any]:
        offering_id, team_member_id = self._real_ids(offering_id, team_member_id)
        # The model often sends a bare local time; booking dead-lettered "bad starts_at" and the caller had been told
        # the visit was booked. Read it in the business's time zone, as for availability.
        starts_at = _rfc3339(starts_at, self._ctx.timezone)
        try:
            confirmed = self._require_phone(phone)
        except PhoneConfirmationRequired as pcr:
            metrics.stepup_refused("confirmation_required")
            await self._emit_tool_event("book_appointment", "confirmation_required", {})
            return _confirmation_payload(pcr.pending_phone)

        event_id = await self._publish_command(
            BOOK,
            {
                "offering_id": offering_id,
                "team_member_id": team_member_id,
                "starts_at": starts_at,
                "phone": confirmed,
                "contact_name": contact_name or self._session.caller_name or "",
                "email": email or "",
                "source": "voice",
                "conversation_id": self._session.conversation_id,
            },
        )
        # SPEC-W80 C5: include the confirmed contact identifiers so
        # crm-sync's ToolInvoked note path (detail.phone / detail.email)
        # activates. Guarded: only keys actually present are included.
        detail: dict[str, Any] = {"offering_id": offering_id, "starts_at": starts_at}
        if confirmed:
            detail["phone"] = confirmed
        if email:
            detail["email"] = email
        await self._emit_tool_event("book_appointment", "accepted", detail)
        return {
            "status": "accepted",
            "message": "Booking request accepted and queued for confirmation.",
            "command_id": event_id,
            "offering_id": offering_id,
            "team_member_id": team_member_id,
            "starts_at": starts_at,
        }

    # ------------------------------------------- K15(c) verification tools
    async def request_verification_code(self, phone: str) -> dict[str, Any]:
        """Send a one-time verification code to the claimed caller phone."""
        # SPEC-W88: best-effort canonical E.164 (a conforming number is
        # sent/stored in ONE form across channels); national read-outs
        # fall back to the legacy normalize_phone form, never rejected.
        phone = canonical_e164(phone) or sip.normalize_phone(phone)
        if not phone:
            return {
                "status": "error",
                "message": "request_verification_code needs the caller's phone number.",
            }
        verifier = self._verifier()
        try:
            result = await verifier.request_code(phone)
        finally:
            await verifier.aclose()
        await self._emit_tool_event(
            "request_verification_code",
            result.status,
            self._event_phone_detail(phone),
        )
        if result.ok:
            return {
                "status": "code_sent",
                "message": (
                    "I've sent a one-time verification code to the number "
                    f"ending in {phone[-4:]}. Ask the caller to read it back "
                    "and submit it with verify_caller_code."
                ),
            }
        if result.status == "unavailable":
            return _verification_unavailable_payload()
        if result.status == "rate_limited":
            return {
                "status": "error",
                "error": "rate_limited",
                "message": (
                    "Too many verification codes were requested for this "
                    "number. Ask the caller to wait a moment and try again."
                ),
            }
        return {
            "status": "error",
            "message": (
                "The verification code could not be sent just now; please "
                "try again in a moment."
            ),
        }

    async def verify_caller_code(self, phone: str, code: str) -> dict[str, Any]:
        """Verify the caller-read-back code; success marks the session
        verified (K15(c)) so lookup/reschedule/cancel may run."""
        # SPEC-W88: same best-effort canonical form as
        # request_verification_code, so the OTP pair and the resulting
        # verified_phone agree (and match the channel-pin form).
        phone = canonical_e164(phone) or sip.normalize_phone(phone)
        if not phone:
            return {
                "status": "error",
                "message": "verify_caller_code needs the caller's phone number.",
            }
        verifier = self._verifier()
        try:
            result = await verifier.verify_code(phone, code)
        finally:
            await verifier.aclose()
        if result.ok:
            self._session.mark_verified(phone)
            metrics.stepup_verified("otp")
            await self._emit_tool_event(
                "verify_caller_code", "verified", self._event_phone_detail(phone)
            )
            log.info(
                "caller phone verified",
                conversation_id=self._session.conversation_id,
                channel=self._session.channel,
            )
            return {
                "status": "verified",
                "message": (
                    "Thank you — the number is verified. You may now look "
                    "up, reschedule or cancel the caller's bookings."
                ),
            }
        await self._emit_tool_event("verify_caller_code", result.status, {})
        if result.status == "unavailable":
            return _verification_unavailable_payload()
        if result.status == "rate_limited":
            return {
                "status": "error",
                "error": "rate_limited",
                "message": (
                    "Too many failed attempts. Ask the caller to request a "
                    "new code with request_verification_code."
                ),
            }
        if result.status == "invalid_code":
            return {
                "status": "invalid_code",
                "message": (
                    "That code didn't match. Ask the caller to double-check "
                    "the code and try again, or send a new one."
                ),
            }
        return {
            "status": "error",
            "message": "The code could not be verified just now; please try again.",
        }

    async def read_keypad_entry(
        self, timeout_s: int = 30, expected_length: "int | None" = None
    ) -> dict[str, Any]:
        """Collect keypad (DTMF) digits from the caller (SPEC-W86).

        PSTN calls only: the buffer is attached by the LiveKit worker for
        SIP sessions; every other channel answers keypad_unavailable — an
        honest capability report, never a mock. Bounds are clamped
        server-side (the model may request anything). The digits are
        returned ONLY to this session's own LLM loop — they are never
        logged and never emitted in events (the ToolInvoked event carries
        the outcome status only, no digits).
        """
        timeout = _clamp_int(timeout_s, KEYPAD_TIMEOUT_MIN_S, KEYPAD_TIMEOUT_MAX_S, 30)
        length = (
            _clamp_int(expected_length, KEYPAD_LENGTH_MIN, KEYPAD_LENGTH_MAX, None)
            if expected_length is not None
            else None
        )
        buffer = self._dtmf_buffer
        if buffer is None:
            if self._native_keypad:
                # SPEC-W93: ElevenLabs backend — keypad digits are collected
                # natively by ElevenLabs and arrive as the caller's NEXT
                # MESSAGE (a completed user turn); they never transit this
                # runtime, so there is nothing to wait for here. Tell the
                # hosted agent the honest relay contract.
                await self._emit_tool_event("read_keypad_entry", "keypad_native", {})
                message = (
                    "On this phone platform keypad entry is collected "
                    "natively: ask the caller to type the digits on "
                    "their keypad (pressing # submits early). The "
                    "digits arrive as the caller's next message — "
                    "take them from there and pass them as the code "
                    "to verify_caller_code."
                )
                # SPEC-W96 IVR-A: append the menu sentence so the hosted
                # agent can speak/route the keypad shortcuts itself.
                if self._ivr_menu_hint:
                    message = f"{message} {self._ivr_menu_hint}"
                return {
                    "status": "keypad_native",
                    "message": message,
                    "channel": self._session.channel or "",
                    "digits": "",
                    "timeout_s": timeout,
                    "expected_length": length,
                }
            await self._emit_tool_event("read_keypad_entry", "keypad_unavailable", {})
            return {
                "status": "keypad_unavailable",
                "error": "keypad_unavailable",
                "channel": self._session.channel or "",
                "digits": "",
                "timeout_s": timeout,
                "expected_length": length,
            }
        digits = await buffer.wait_for_entry(
            timeout_s=float(timeout), expected_length=length
        )
        if digits is None:
            await self._emit_tool_event("read_keypad_entry", "keypad_timeout", {})
            return {
                "status": "keypad_timeout",
                "digits": "",
                "timeout_s": timeout,
                "expected_length": length,
            }
        await self._emit_tool_event("read_keypad_entry", "keypad_entry", {})
        return {
            "status": "keypad_entry",
            "digits": digits,
            "timeout_s": timeout,
            "expected_length": length,
        }

    async def lookup_appointment(self, phone: str) -> dict[str, Any]:
        gate = await self._require_verified(phone)
        if isinstance(gate, dict):
            return await self._refuse_gate("lookup_appointment", gate)
        confirmed = gate

        # SPEC-W47 (UIMOB-05): the Booking JSON has NO contact_phone field
        # (bookings carry contact_id), so the old client-side
        # ``b.get("contact_phone")`` filter matched zero rows for every
        # verified caller. The lookup now uses the booking-service
        # server-side filter ``GET /v1/bookings?contact_phone=`` (W47-A:
        # internal-token-gated, same internauth guard as the other internal
        # routes) over the W46 P-02 direct BOOKING_BASE_URL client
        # (dapr_client direct_bases, daprd fallback on transport failure).
        # FAIL-CLOSED: without the internal token the filter route would
        # 401 — refuse honestly instead of returning an empty list that
        # reads as "you have no bookings".
        if not self._settings.voice_booking_internal_token:
            log.error(
                "lookup_appointment refused: VOICE_BOOKING_INTERNAL_TOKEN "
                "unset (the contact_phone filter route is internal-token-"
                "gated) — failing closed"
            )
            await self._emit_tool_event(
                "lookup_appointment", "unavailable", {}
            )
            return _verification_unavailable_payload()

        now = datetime.now(timezone.utc)
        resp = await self._dapr.invoke_get(
            self._settings.booking_app_id,
            "v1/bookings",
            params={
                # K15(f): the verified phone goes ONLY to the internal
                # booking route — never into logs/events.
                "contact_phone": confirmed,
                "from": (now - timedelta(days=1)).isoformat(),
                "to": (now + timedelta(days=180)).isoformat(),
            },
            headers={
                "X-Tenant-Slug": self._ctx.tenant_slug,
                "X-Internal-Token": self._settings.voice_booking_internal_token,
            },
        )
        # Contract (W47-A item 4): a JSON list of
        # {id, status, starts_at, offering, ...}; tolerate the legacy
        # {"bookings": [...]} envelope while the backend rolls out.
        mine = resp if isinstance(resp, list) else (resp or {}).get("bookings") or []
        for b in mine:
            bid = b.get("id")
            if bid and bid not in self._session.last_booking_ids:
                self._session.last_booking_ids.append(str(bid))
        await self._emit_tool_event(
            "lookup_appointment", "ok", {"found": len(mine)}
        )
        return {"phone": confirmed, "bookings": mine, "count": len(mine)}

    async def reschedule_appointment(
        self, booking_id: str, starts_at: str, phone: str
    ) -> dict[str, Any]:
        starts_at = _rfc3339(starts_at, self._ctx.timezone)
        gate = await self._require_verified(phone)
        if isinstance(gate, dict):
            return await self._refuse_gate("reschedule_appointment", gate)
        confirmed = gate

        event_id = await self._publish_command(
            RESCHEDULE,
            {
                "booking_id": booking_id,
                "starts_at": starts_at,
                "phone": confirmed,
                "source": "voice",
                "conversation_id": self._session.conversation_id,
            },
        )
        await self._emit_tool_event(
            "reschedule_appointment", "accepted", {"booking_id": booking_id}
        )
        return {
            "status": "accepted",
            "message": "Reschedule request accepted and queued.",
            "command_id": event_id,
            "booking_id": booking_id,
            "starts_at": starts_at,
        }

    async def cancel_appointment(
        self, booking_id: str, phone: str, reason: str | None = None
    ) -> dict[str, Any]:
        gate = await self._require_verified(phone)
        if isinstance(gate, dict):
            return await self._refuse_gate("cancel_appointment", gate)
        confirmed = gate

        event_id = await self._publish_command(
            CANCEL,
            {
                "booking_id": booking_id,
                "phone": confirmed,
                "reason": reason or "voice_command",
                "source": "voice",
                "conversation_id": self._session.conversation_id,
            },
        )
        await self._emit_tool_event(
            "cancel_appointment", "accepted", {"booking_id": booking_id}
        )
        return {
            "status": "accepted",
            "message": "Cancellation request accepted and queued.",
            "command_id": event_id,
            "booking_id": booking_id,
        }

    # ------------------------------------- caller details to the CRM (2026-11-21)
    async def save_caller_details(
        self, name: str, phone: str, email: str | None = None
    ) -> dict[str, Any]:
        """Save the details the caller gave (and confirmed) at the start of the call as a contact, so the
        business can reach them even without a booking. booking-service ``POST /internal/contacts/upsert``
        with ``capture_channel``: it matches an existing contact by phone or e-mail and, for these
        self-reported details, only FILLS blank fields (a caller can claim any number, so an existing
        contact's name/e-mail is never overwritten); it then publishes ContactCaptured so crm-sync adds the
        person to the CRM. Also remembers the details on the session for the rest of the call.

        NEVER raises: failures resolve to a payload the model can carry on from (the call must go on)."""
        name = (name or "").strip()
        email = (email or "").strip()
        # the same form book_appointment sends (the model's digits, stripped), so a later booking for this
        # caller matches the contact saved here instead of creating a second one
        phone_n = (phone or "").strip()
        if not name or not phone_n:
            return {"status": "error", "message": "save_caller_details needs the caller's name and phone number."}
        self._session.caller_name = name
        if email:
            self._session.caller_email = email
        if not self._session.claimed_phone:
            self._session.claimed_phone = phone_n
        headers = {"X-Tenant-Slug": self._ctx.tenant_slug}
        if self._settings.voice_booking_internal_token:
            headers["X-Internal-Token"] = self._settings.voice_booking_internal_token
        try:
            res = await self._dapr.invoke_post(
                self._settings.booking_app_id,
                "internal/contacts/upsert",
                payload={"name": name, "phone": phone_n, "email": email,
                         "capture_channel": self._session.channel or "voice"},
                headers=headers,
            )
            contact = (res or {}).get("contact") or {}
            contact_id = str(contact.get("id") or "")
            await self._emit_tool_event(
                "save_caller_details", "ok",
                {"contact_id": contact_id, "created": bool((res or {}).get("created")), **self._event_phone_detail(phone_n)},
            )
            log.info("caller details saved", conversation_id=self._session.conversation_id, contact_id=contact_id)
            return {"status": "ok", "contact_id": contact_id,
                    "message": "Saved. Now ask how you can help them today."}
        except Exception as exc:  # noqa: BLE001 - surfaced to the model
            log.warning("save_caller_details failed", error=str(exc)[:200])
            await self._emit_tool_event("save_caller_details", "error", {"error": str(exc)[:200]})
            return {"status": "error", "message": "The details could not be saved just now; carry on helping the caller."}

    # ------------------------------------ WhatsApp confirmation (2026-11-22)
    async def send_whatsapp_confirmation(
        self, offering: str, when: str, team_member: str | None = None
    ) -> dict[str, Any]:
        """WhatsApp the person their appointment details via the messaging-gateway (POST /v1/whatsapp/send). The text is
        composed HERE from the arguments (no free-form model text reaches the customer). Sent to the session's WhatsApp
        number (session.whatsapp_to: the demo phone's chosen number, or the callee when WHATSAPP_CONFIRMATIONS=on).
        NEVER raises: a failure tells the model to carry on (the call must go on)."""
        # only the number the call set up for this (demo phone's number, or the callee when enabled) — never a guess
        to = (self._session.whatsapp_to or "").strip()
        offering, when = " ".join((offering or "").split())[:120], " ".join((when or "").split())[:80]
        team_member = " ".join((team_member or "").split())[:80]
        if not to or not offering or not when:
            return {"status": "error", "message": "No WhatsApp number for this call; do not mention WhatsApp, carry on."}
        from .whatsapp_messages import appointment_text

        text = appointment_text("confirmation", self._ctx.display_name or self._ctx.site_slug, self._session.caller_name,
                                offering, when, team_member)
        try:
            import httpx

            async with httpx.AsyncClient(timeout=self._settings.http_timeout_s) as client:
                r = await client.post(f"{self._settings.messaging_gateway_url.rstrip('/')}/v1/whatsapp/send",
                                      json={"to": to, "message": text})
            if r.status_code >= 400:
                raise RuntimeError(f"messaging-gateway {r.status_code}: {r.text[:160]}")
            await self._emit_tool_event("send_whatsapp_confirmation", "ok", self._event_phone_detail(to))
            log.info("whatsapp confirmation sent", conversation_id=self._session.conversation_id)
            return {"status": "sent", "message": "Sent. Tell them a WhatsApp confirmation is on its way."}
        except Exception as exc:  # noqa: BLE001 - surfaced to the model
            log.warning("whatsapp confirmation failed", error=str(exc)[:200])
            await self._emit_tool_event("send_whatsapp_confirmation", "error", {"error": str(exc)[:200]})
            return {"status": "error", "message": "The WhatsApp message could not be sent just now; do not mention it."}

    # ------------------------------------------------ caller documents (2026-11-26)
    async def send_document_upload_link(self, document_type: str | None = None) -> dict[str, Any]:
        """Point the person to where a document goes: the paperclip on a web chat, a photo on WhatsApp, and on a phone
        call a signed upload link WhatsApped to the number the call set up (session.whatsapp_to, the same rule as
        send_whatsapp_confirmation). NEVER raises: a failure tells the model what to say instead."""
        kind = (document_type or "").strip().lower()
        kind = kind if kind in ("pay_stub", "receipt", "document") else "document"
        label = {"pay_stub": "pay stub", "receipt": "receipt"}.get(kind, "document")
        if getattr(self._session, "text_chat", False) and self._session.channel in ("web", ""):
            return {"status": "use_paperclip", "message": f"Tell them to tap the paperclip below the chat, pick the {label} "
                    "and press Upload; it is checked in about two minutes and you will see the result."}
        if self._session.channel == "whatsapp":
            return {"status": "use_whatsapp", "message": f"Tell them to send a clear photo of the {label} here in WhatsApp."}
        secret, base = self._settings.document_link_secret, self._settings.document_link_base_url
        to = (self._session.whatsapp_to or "").strip()
        if not (secret and base):
            return {"status": "error", "message": "Document sharing is not set up; do not offer to receive documents."}
        if not to:
            return {"status": "error", "message": "There is no WhatsApp number for this call, so no link can be sent. Tell "
                    "them they can share it from the chat on the website instead."}
        from .documents import mint_document_link

        token, _exp = mint_document_link(secret=secret, session_id=self._session.conversation_id,
                                         tenant_id=self._ctx.tenant_id, ttl_s=self._settings.document_link_ttl_s)
        minutes = max(1, int(self._settings.document_link_ttl_s // 60))
        business = self._ctx.display_name or self._ctx.site_slug
        text = (f"{business}: here is your secure link to upload your {label}: {base.rstrip('/')}/{token}?kind={kind} "
                f"(it works for {minutes} minutes).")
        try:
            import httpx

            async with httpx.AsyncClient(timeout=self._settings.http_timeout_s) as client:
                r = await client.post(f"{self._settings.messaging_gateway_url.rstrip('/')}/v1/whatsapp/send",
                                      json={"to": to, "message": text})
            if r.status_code >= 400:
                raise RuntimeError(f"messaging-gateway {r.status_code}: {r.text[:160]}")
            await self._emit_tool_event("send_document_upload_link", "ok", self._event_phone_detail(to))
            return {"status": "sent", "message": "Sent. Tell them a WhatsApp with a secure upload link is on its way; once "
                    "they have uploaded it, use check_shared_documents to see it."}
        except Exception as exc:  # noqa: BLE001 - surfaced to the model
            log.warning("document link failed", error=str(exc)[:200])
            await self._emit_tool_event("send_document_upload_link", "error", {"error": str(exc)[:200]})
            return {"status": "error", "message": "The link could not be sent just now. Tell them they can share it from "
                    "the chat on the website instead."}

    async def check_shared_documents(self) -> dict[str, Any]:
        """The documents shared in this conversation, as their kind and check result (never the details read from
        them). A web chat's are on its session; a call's are kept by the control plane, read with a signed request."""
        from .documents import document_lookup_sig, documents_in_words

        docs = list(getattr(self._session, "shared_documents", None) or [])
        secret = self._settings.document_link_secret
        if not getattr(self._session, "text_chat", False) and secret:
            try:
                import httpx

                sid = self._session.conversation_id
                async with httpx.AsyncClient(timeout=self._settings.http_timeout_s) as client:
                    r = await client.get(f"{self._settings.voice_control_plane_url.rstrip('/')}/voice/internal/documents/{sid}",
                                         headers={"X-Document-Sig": document_lookup_sig(secret, sid)})
                if r.status_code == 200:
                    docs = r.json().get("documents") or docs
            except Exception as exc:  # noqa: BLE001 - an empty answer, never a broken call
                log.warning("shared documents lookup failed", error=str(exc)[:200])
        if not docs:
            return {"status": "none", "message": "Nothing has been shared in this conversation yet."}
        return {"status": "ok", "documents": documents_in_words(docs),
                "message": "Tell them, in a sentence, what you have and where each check stands. Never read out details "
                "from the documents."}

    # -------------------------------------------- location capture (W11 C)
    async def capture_location(
        self,
        address_text: str | None = None,
        lat: Optional[float] = None,
        lng: Optional[float] = None,
    ) -> dict[str, Any]:
        """Save the caller's location on their contact record (SPEC-W11 C §4).

        Contact resolution: the session's confirmed phone (read-back
        confirmed or OTP/channel verified) selects the contact via
        booking-service ``GET /internal/contacts?phone=``; when no phone is
        confirmed the channel-ASSERTED (unverified) claimed phone is used —
        location capture is a safety-critical WRITE for the emergency lane,
        never a data read, and K15(c) removed the carrier-asserted bypass
        only for the mutating tools. The location is then upserted through
        the Wave-8 contract ``PUT /v1/contacts/{id}/location`` with
        ``{lat, lng}`` when coordinates were given, else
        ``{address: address_text}`` (server-side geocoding per
        GEOCODE_ENABLED).

        NEVER raises: every failure resolves to an error payload the model
        can speak (the emergency flow must not break the call).
        """
        address_text = (address_text or "").strip()
        try:
            if lat is not None and lng is not None:
                payload: dict[str, Any] = {
                    "lat": float(lat),
                    "lng": float(lng),
                    "source": "manual",
                }
            elif address_text:
                payload = {"address": address_text}
            else:
                return {
                    "status": "error",
                    "message": (
                        "capture_location needs an address (address_text) or "
                        "both lat and lng."
                    ),
                }

            phone = (
                self._session.confirmed_phone or self._session.claimed_phone or ""
            ).strip()
            if not phone:
                return {
                    "status": "error",
                    "message": (
                        "No caller phone number is known for this "
                        "session, so the location cannot be attached to a "
                        "contact. Ask the caller for their number first."
                    ),
                }

            # SPEC-W80 B7 (G9): /internal/contacts is internAuth-gated —
            # forward X-Internal-Token, same token mechanism as v1/bookings
            # (lookup_appointment): VOICE_BOOKING_INTERNAL_TOKEN. Empty token
            # sends no header and the peer fails closed (internalHeaders
            # idiom) — existing header shape is unchanged when unset.
            headers = {"X-Tenant-Slug": self._ctx.tenant_slug}
            if self._settings.voice_booking_internal_token:
                headers["X-Internal-Token"] = self._settings.voice_booking_internal_token
            contact = await self._dapr.invoke_get(
                self._settings.booking_app_id,
                "internal/contacts",
                params={"phone": phone},
                headers=headers,
            )
            contact_id = str((contact or {}).get("id") or "").strip()
            if not contact_id:
                # K15(f): hashed (or omitted) — never the plaintext number.
                await self._emit_tool_event(
                    "capture_location", "no_contact", self._event_phone_detail(phone)
                )
                return {
                    "status": "error",
                    "message": (
                        "No contact record exists for the caller's number, "
                        "so the location could not be saved."
                    ),
                }

            await self._dapr.invoke_put(
                self._settings.booking_app_id,
                f"v1/contacts/{contact_id}/location",
                payload=payload,
                headers=headers,
            )
            await self._emit_tool_event(
                "capture_location", "ok", {"contact_id": contact_id}
            )
            log.info(
                "caller location captured",
                conversation_id=self._session.conversation_id,
                contact_id=contact_id,
                kind="latlng" if "lat" in payload else "address",
            )
            return {
                "status": "ok",
                "contact_id": contact_id,
                "message": (
                    "I've saved that location. Stay on the line — help is "
                    "being notified."
                ),
            }
        except Exception as exc:  # noqa: BLE001 - surfaced to the model
            log.warning("capture_location failed", error=str(exc)[:200])
            await self._emit_tool_event(
                "capture_location", "error", {"error": str(exc)[:200]}
            )
            return {
                "status": "error",
                "message": (
                    "The location could not be saved just now; keep the "
                    "caller on the line and note the address verbally."
                ),
            }

    # ------------------------------------------------------- warm handoff
    async def request_human(self, reason: str | None = None) -> dict[str, Any]:
        """Escalate to a human operator (SPEC-W3 §4, innovation 1).

        Creates LiveKit room ``escalation-{conversation_id}`` and publishes
        an EscalationRequested CloudEvent to ``opendesk.conversation.events``.

        SPEC-W45 K15(e): the staff LiveKit join token is NO LONGER on the
        event (the events topic is fan-out — any consumer could hijack the
        escalation room and listen to the caller). The event carries the
        room name only; the staff token is delivered to the staff
        participant only, either minted on demand via the internal
        ``POST /voice-admin/escalations/{conversation_id}/staff-token``
        endpoint (X-Internal-Token, app/control_plane.py) or pushed in-room
        via targeted LiveKit data (LiveKitEscalation.deliver_staff_token).

        Degrades gracefully when LiveKit is unreachable: the event still
        goes out (staff see the banner) and the caller still gets a spoken
        confirmation.
        """
        room = escalation_room_name(self._session.conversation_id)
        room_created = await self._escalation.create_room(room)

        self._session.escalation_room = room
        self._session.touch()

        event = new_cloudevent(
            type_=ESCALATION_REQUESTED,
            subject=self._ctx.tenant_slug,
            tenant_uuid=self._ctx.tenant_id,
            data={
                "conversation_id": self._session.conversation_id,
                "tenant_id": self._ctx.tenant_id,
                "site_slug": self._ctx.site_slug,
                "room": room,
                "reason": reason or "caller_requested",
                # 2026-11-23: the stored transcript's id, so the human inbox opens the right conversation.
                "stored_conversation_id": (self._session.stored_id_lookup() if callable(
                    getattr(self._session, "stored_id_lookup", None)) else None),
                # K15(e): staff join via the internal staff-token endpoint.
                "staff_token_endpoint": (
                    f"/voice-admin/escalations/{self._session.conversation_id}"
                    "/staff-token"
                ),
            },
        )
        await self._dapr.publish(
            self._settings.dapr_pubsub,
            self._settings.conversation_events_topic,
            event,
        )
        await self._emit_tool_event(
            "request_human", "escalated", {"room": room, "room_created": room_created}
        )
        log.info(
            "escalation requested",
            conversation_id=self._session.conversation_id,
            room=room,
            room_created=room_created,
        )
        return {
            "status": "escalated",
            "room": room,
            "room_created": room_created,
            # 2026-11-23: a typed chat is answered in the same chat (human inbox), not on a call.
            "message": (
                "I've passed this conversation to a member of our team. They will reply right here in this "
                "chat shortly, so please keep this chat open."
                if getattr(self._session, "text_chat", False)
                else "I'm connecting you with a member of our team right now. "
                "They've been notified and will join shortly; I'll stay on "
                "the line to help in the meantime."
            ),
        }

    # ---------------------------------------------------- UI actions (W9 B)
    def _queue_ui_action(self, tool: str, action: dict[str, Any] | None, hint: str) -> dict[str, Any] | None:
        """Shared validation path for the UI action tools.

        Returns the error payload to hand back to the model when ``action``
        is None (invalid input — dropped, never reaches the client); queues
        the action on the per-turn sink otherwise and returns None.
        """
        if action is None:
            log.info("ui action rejected", tool=tool)
            return {"status": "error", "message": f"Invalid {tool} argument: {hint}"}
        self._ui_actions.append(action)
        return None

    async def navigate_to_page(self, path: str) -> dict[str, Any]:
        action = ui_actions.validate_navigate(path)
        error = self._queue_ui_action(
            "navigate_to_page",
            action,
            "path must start with '/' and contain no scheme or host (same-origin only)",
        )
        if error is not None:
            await self._emit_tool_event("navigate_to_page", "rejected", {})
            return error
        assert action is not None
        await self._emit_tool_event("navigate_to_page", "ok", {"path": action["path"]})
        return {
            "status": "ok",
            "message": f"The visitor is being shown {action['path']}.",
            "ui_action": action,
        }

    async def highlight_element(self, selector: str) -> dict[str, Any]:
        action = ui_actions.validate_highlight(selector)
        error = self._queue_ui_action(
            "highlight_element",
            action,
            "selector may only contain letters, digits and - _ # . : [ ] = \" ' > (max 120 chars)",
        )
        if error is not None:
            await self._emit_tool_event("highlight_element", "rejected", {})
            return error
        assert action is not None
        await self._emit_tool_event("highlight_element", "ok", {"selector": action["selector"]})
        return {
            "status": "ok",
            "message": "The element is being highlighted for the visitor.",
            "ui_action": action,
        }

    async def prefill_booking(self, offering_id: str) -> dict[str, Any]:
        action = ui_actions.validate_prefill_booking(offering_id)
        error = self._queue_ui_action(
            "prefill_booking",
            action,
            "offering_id must be a UUID (see get_business_info for the catalog)",
        )
        if error is not None:
            await self._emit_tool_event("prefill_booking", "rejected", {})
            return error
        assert action is not None
        await self._emit_tool_event(
            "prefill_booking", "ok", {"offering_id": action["offering_id"]}
        )
        return {
            "status": "ok",
            "message": "The offering is being pre-selected in the visitor's booking form.",
            "ui_action": action,
        }

    # ----------------------------------------------------------- dispatching
    async def dispatch(self, name: str, arguments: dict[str, Any]) -> dict[str, Any]:
        """Dispatch a tool call by name (chat path / ElevenLabs webhook)."""
        # SPEC-W38 F2: a non-empty definition tool_allowlist also BLOCKS
        # dispatch of non-allowlisted tools (defense in depth — the model
        # never saw the schema, but a leaked/retried call must not run).
        allowlist = self._tool_allowlist()
        if allowlist and name not in allowlist:
            log.info("tool blocked by agent definition allowlist", tool=name)
            await self._emit_tool_event(name, "blocked", {})
            return {
                "status": "error",
                "message": f"tool {name!r} is not enabled for this agent",
            }
        handler = {
            "get_business_info": lambda: self.get_business_info(),
            "get_availability": lambda: self.get_availability(
                offering_id=str(arguments.get("offering_id", "")),
                team_member_id=str(arguments.get("team_member_id", "")),
                from_iso=str(arguments.get("from_iso", "")),
                to_iso=str(arguments.get("to_iso", "")),
            ),
            "book_appointment": lambda: self.book_appointment(
                offering_id=str(arguments.get("offering_id", "")),
                team_member_id=str(arguments.get("team_member_id", "")),
                starts_at=str(arguments.get("starts_at", "")),
                phone=str(arguments.get("phone", "")),
                contact_name=arguments.get("contact_name"),
                email=arguments.get("email"),
            ),
            "lookup_appointment": lambda: self.lookup_appointment(
                phone=str(arguments.get("phone", ""))
            ),
            "request_verification_code": lambda: self.request_verification_code(
                phone=str(arguments.get("phone", ""))
            ),
            "verify_caller_code": lambda: self.verify_caller_code(
                phone=str(arguments.get("phone", "")),
                code=str(arguments.get("code", "")),
            ),
            "read_keypad_entry": lambda: self.read_keypad_entry(
                timeout_s=arguments.get("timeout_s", 30),
                expected_length=arguments.get("expected_length"),
            ),
            "reschedule_appointment": lambda: self.reschedule_appointment(
                booking_id=str(arguments.get("booking_id", "")),
                starts_at=str(arguments.get("starts_at", "")),
                phone=str(arguments.get("phone", "")),
            ),
            "cancel_appointment": lambda: self.cancel_appointment(
                booking_id=str(arguments.get("booking_id", "")),
                phone=str(arguments.get("phone", "")),
                reason=arguments.get("reason"),
            ),
            "send_whatsapp_confirmation": lambda: self.send_whatsapp_confirmation(
                offering=str(arguments.get("offering", "")),
                when=str(arguments.get("when", "")),
                team_member=arguments.get("team_member"),
            ),
            "send_document_upload_link": lambda: self.send_document_upload_link(
                document_type=arguments.get("document_type"),
            ),
            "check_shared_documents": lambda: self.check_shared_documents(),
            "save_caller_details": lambda: self.save_caller_details(
                name=str(arguments.get("name", "")),
                phone=str(arguments.get("phone", "")),
                email=arguments.get("email"),
            ),
            "capture_location": lambda: self.capture_location(
                address_text=arguments.get("address_text"),
                lat=arguments.get("lat"),
                lng=arguments.get("lng"),
            ),
            "request_human": lambda: self.request_human(
                reason=arguments.get("reason"),
            ),
            "navigate_to_page": lambda: self.navigate_to_page(
                path=str(arguments.get("path", "")),
            ),
            "highlight_element": lambda: self.highlight_element(
                selector=str(arguments.get("selector", "")),
            ),
            "prefill_booking": lambda: self.prefill_booking(
                offering_id=str(arguments.get("offering_id", "")),
            ),
        }.get(name)
        # SPEC-W81 A2: risk-tier policy — the single choke point. Runs after
        # the allowlist block, before execution; unknown tools fall through
        # to the unchanged "unknown tool" error below.
        if handler is not None or name in self._plugin_tools:
            tier_gate = await self._enforce_tool_tier(name, arguments)
            if tier_gate is not None:
                return tier_gate
        if handler is None:
            plugin = self._plugin_tools.get(name)
            if plugin is not None:
                try:
                    result = await plugin.execute(arguments)
                    await self._emit_tool_event(name, result.get("status", "ok"), {})
                    return result
                except Exception as exc:  # noqa: BLE001 - surfaced to the model
                    log.warning("plugin tool failed", tool=name, error=str(exc))
                    await self._emit_tool_event(name, "error", {"error": str(exc)[:200]})
                    return {"status": "error", "message": f"{name} failed: {exc}"}
            return {"status": "error", "message": f"unknown tool {name!r}"}
        try:
            return await handler()
        except Exception as exc:  # noqa: BLE001 - surfaced to the model
            log.warning("tool call failed", tool=name, error=str(exc))
            await self._emit_tool_event(name, "error", {"error": str(exc)[:200]})
            return {"status": "error", "message": f"{name} failed: {exc}"}
