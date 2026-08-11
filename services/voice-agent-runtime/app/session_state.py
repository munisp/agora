"""Per-conversation session state + the phone-confirmation policy (SPEC §11).

Policy: mutating tools (book/lookup/reschedule/cancel) refuse to run without a
*confirmed* contact phone in session state. Confirmation is two-step and
server-enforced (never delegated to the model):

1. First mutating call carrying a phone number -> the phone becomes
   `pending_phone` and the tool returns `confirmation_required` so the agent
   reads the number back to the caller.
2. A subsequent mutating call with the *same* phone confirms it (the model
   re-issues the call after the caller said "yes"); the phone becomes
   `confirmed_phone` and the call proceeds.

State is in-memory (dev-grade; swap `SessionStore` for the Dapr state store
`statestore.redis` in production — the interface is tiny).
"""

from __future__ import annotations

import time
import uuid
from dataclasses import dataclass, field


class PhoneConfirmationRequired(RuntimeError):
    """Raised by the tool layer when a mutation lacks a confirmed phone."""

    def __init__(self, pending_phone: str) -> None:
        super().__init__("phone confirmation required")
        self.pending_phone = pending_phone


@dataclass
class SessionState:
    conversation_id: str
    site_slug: str
    pending_phone: str | None = None
    confirmed_phone: str | None = None
    caller_name: str | None = None
    last_booking_ids: list[str] = field(default_factory=list)
    # Multi-agent crews (SPEC-W3 §4, innovation 6): id of the specialist
    # agent currently steering the persona, or None for the base persona.
    active_agent: str | None = None
    # Warm handoff (SPEC-W3 §4, innovation 1): set once request_human ran;
    # copilot mode posts suggested replies into this room's data channel.
    escalation_room: str | None = None
    # Multilingual receptionist (Wave 5 #3): the language the caller is
    # currently speaking (whisper auto-detect, app/multilang.py); None = the
    # tenant default language applies.
    active_language: str | None = None
    # Omnichannel inbound (SPEC-W6 Part A): the channel the current message
    # arrived on ("web" | "whatsapp" | "telegram"). Metadata only.
    channel: str = "web"
    # SPEC-W38 F1/F3: id of the agents-registry record resolved during SIP
    # inbound bootstrap (resolve_agent_for_dialed). Carried on the session
    # so the SessionEnded lifecycle event can emit it as `agentId`; None for
    # web sessions and legacy TENANT_PHONE_MAP calls.
    resolved_agent_id: str | None = None
    created_at: float = field(default_factory=time.time)
    touched_at: float = field(default_factory=time.time)

    def touch(self) -> None:
        self.touched_at = time.time()

    def require_confirmed_phone(self, phone: str | None) -> str:
        """Enforce the phone-confirmation policy.

        Returns the confirmed phone to use for the mutation, or raises
        PhoneConfirmationRequired (carrying the pending phone) when the
        caller still needs to confirm.
        """
        phone = (phone or "").strip() or None
        if self.confirmed_phone:
            # Already confirmed this session; a different phone starts a new
            # confirmation cycle.
            if phone is None or phone == self.confirmed_phone:
                self.touch()
                return self.confirmed_phone
            self.pending_phone = phone
            self.confirmed_phone = None
            self.touch()
            raise PhoneConfirmationRequired(phone)
        if phone is None:
            raise PhoneConfirmationRequired("")
        if self.pending_phone == phone:
            # Second call with the same number => caller confirmed.
            self.confirmed_phone = phone
            self.touch()
            return phone
        self.pending_phone = phone
        self.touch()
        raise PhoneConfirmationRequired(phone)


@dataclass
class SessionStore:
    """In-memory session store with idle expiry."""

    ttl_s: int = 3600
    _sessions: dict[str, SessionState] = field(default_factory=dict)

    def get_or_create(self, conversation_id: str | None, site_slug: str) -> SessionState:
        self._gc()
        if conversation_id and conversation_id in self._sessions:
            session = self._sessions[conversation_id]
            session.touch()
            return session
        cid = conversation_id or str(uuid.uuid4())
        session = SessionState(conversation_id=cid, site_slug=site_slug)
        self._sessions[cid] = session
        return session

    def get(self, conversation_id: str) -> SessionState | None:
        self._gc()
        return self._sessions.get(conversation_id)

    def _gc(self) -> None:
        now = time.time()
        expired = [
            cid
            for cid, s in self._sessions.items()
            if now - s.touched_at > self.ttl_s
        ]
        for cid in expired:
            del self._sessions[cid]
