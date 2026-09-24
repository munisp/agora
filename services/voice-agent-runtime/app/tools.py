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
"""

from __future__ import annotations

import asyncio
import uuid
from datetime import datetime, timedelta, timezone
from typing import Any, Coroutine, Optional

from . import metrics, sip, ui_actions
from .config import Settings
from .dapr_client import DaprClient
from .escalation import LiveKitEscalation, escalation_room_name
from .events import new_cloudevent
from .logging import get_logger
from .plugin_tools import PluginTool
from .session_state import PhoneConfirmationRequired, SessionState
from .tenant_context import TenantContext
from .verification import BookingPortalVerifier, hash_phone

log = get_logger("tools")

BOOK = "com.opendesk.booking.command.BookAppointment"
RESCHEDULE = "com.opendesk.booking.command.RescheduleAppointment"
CANCEL = "com.opendesk.booking.command.CancelAppointment"
ESCALATION_REQUESTED = "com.opendesk.conversation.EscalationRequested"

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
                "human, is distressed, or the request cannot be resolved."
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

    def _require_verified(self, phone: str | None) -> str | dict[str, Any]:
        """K15(c): mutating tools (lookup/reschedule/cancel) require a
        VERIFIED session (OTP or channel-pinned identity).

        Returns the verified phone to use for the operation, or the refusal
        payload to hand back to the model. The verified number ALWAYS wins
        over a model-supplied one; a mismatch is rejected (no silent
        re-targeting at another customer's bookings)."""
        verified = self._session.verified_phone
        if not verified:
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
        if supplied and supplied != verified:
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

    async def _refuse_gate(self, tool: str, gate: dict[str, Any]) -> dict[str, Any]:
        """Emit the ToolInvoked event for a verification-gate refusal and
        return the refusal payload to the model."""
        if gate.get("error") == "verification_unavailable":
            status = "verification_unavailable"
        elif gate.get("error") == "phone_mismatch":
            status = "phone_mismatch"
        else:
            status = "verification_required"
        await self._emit_tool_event(tool, status, {})
        return gate

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

    async def get_availability(
        self, offering_id: str, team_member_id: str, from_iso: str, to_iso: str
    ) -> dict[str, Any]:
        resp = await self._dapr.invoke_get(
            self._settings.booking_app_id,
            f"public/sites/{self._ctx.site_slug}/availability",
            params={
                "offering_id": offering_id,
                "team_member_id": team_member_id,
                "from": from_iso,
                "to": to_iso,
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
        try:
            confirmed = self._require_phone(phone)
        except PhoneConfirmationRequired as pcr:
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
        await self._emit_tool_event(
            "book_appointment", "accepted", {"offering_id": offering_id, "starts_at": starts_at}
        )
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
        phone = sip.normalize_phone(phone)
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
        phone = sip.normalize_phone(phone)
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

    async def lookup_appointment(self, phone: str) -> dict[str, Any]:
        gate = self._require_verified(phone)
        if isinstance(gate, dict):
            return await self._refuse_gate("lookup_appointment", gate)
        confirmed = gate

        now = datetime.now(timezone.utc)
        resp = await self._dapr.invoke_get(
            self._settings.booking_app_id,
            "v1/bookings",
            params={
                "from": (now - timedelta(days=1)).isoformat(),
                "to": (now + timedelta(days=180)).isoformat(),
            },
            headers={"X-Tenant-Slug": self._ctx.tenant_slug},
        )
        bookings = resp if isinstance(resp, list) else (resp or {}).get("bookings") or []
        mine = [b for b in bookings if b.get("contact_phone") == confirmed]
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
        gate = self._require_verified(phone)
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
        gate = self._require_verified(phone)
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

            headers = {"X-Tenant-Slug": self._ctx.tenant_slug}
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
            "message": (
                "I'm connecting you with a member of our team right now. "
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
