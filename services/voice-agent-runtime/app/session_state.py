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

SPEC-W45 K15 — two hardening layers on top of that:

- K15(b) session resume: every session is issued a ``session_secret`` (uuid4)
  at creation, returned to the caller once. Resuming a session (recovering
  confirmed_phone / history / escalation state) requires BOTH the
  conversation_id AND the matching secret; a client-supplied conversation_id
  alone NEVER resumes state — the store starts a fresh session instead.
- K15(c) verified sessions: lookup/reschedule/cancel additionally require a
  VERIFIED phone — OTP via the booking customer portal
  (app/verification.py) or a channel-verified identity pinned by the
  messaging-gateway (e.g. WhatsApp wa_id). ``confirmed_phone`` (read-back)
  is NOT sufficient for those tools anymore.

State is in-memory by default (dev-grade); SPEC-W97 G8 adds
`DaprSessionStore`, persisting sessions + chat history through the Dapr
state store (`statestore`, Redis) behind SESSION_STORE_BACKEND=dapr.
"""

from __future__ import annotations

import hmac
import inspect
import time
import uuid
from dataclasses import dataclass, field
from typing import Any

from . import metrics
from .logging import get_logger

log = get_logger("session-state")


class PhoneConfirmationRequired(RuntimeError):
    """Raised by the tool layer when a mutation lacks a confirmed phone."""

    def __init__(self, pending_phone: str) -> None:
        super().__init__("phone confirmation required")
        self.pending_phone = pending_phone


@dataclass
class SessionState:
    conversation_id: str
    site_slug: str
    # K15(b): resume credential, minted once at creation (uuid4) and
    # returned to the caller; required to resume this session's state.
    session_secret: str = field(default_factory=lambda: str(uuid.uuid4()))
    pending_phone: str | None = None
    confirmed_phone: str | None = None
    # K15(c): OTP/channel-verified caller phone — required by the mutating
    # tools (lookup/reschedule/cancel). Set ONLY by verify_caller_code
    # (booking-portal OTP) or by an internal-token-authorized channel
    # identity pin (WhatsApp wa_id); never by the model or the SIP carrier.
    verified_phone: str | None = None
    # K15(c): a phone the channel ASSERTED but which is not verified (SIP
    # caller ID, web self-claim). Usable as a prompt hint and for the
    # emergency location-capture contact lookup; NEVER authorizes a
    # mutation. Replaces the removed SIP carrier-asserted pre-confirmation
    # bypass (OOS-03).
    claimed_phone: str | None = None
    caller_name: str | None = None
    # SPEC-W96 IVR-A: the session's parsed IVR menu (ivr_menu.IvrEntry
    # tuple), set by build_voice_agent for PSTN sessions when the menu is
    # enabled; None everywhere else. Typed Any to keep this module free of
    # the ivr_menu import (duck-typed, same discipline as other bags).
    ivr_menu: Any = None
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

    def mark_verified(self, phone: str) -> str:
        """K15(c): pin an OTP/channel-verified caller phone.

        A verified number also satisfies the (weaker) read-back
        confirmation, so book_appointment does not re-ask for it. Returns
        the stored (already normalized by the caller) phone."""
        self.verified_phone = phone
        self.confirmed_phone = phone
        self.pending_phone = None
        self.touch()
        return phone

    def check_secret(self, session_secret: str | None) -> bool:
        """Constant-time resume-credential check (K15(b))."""
        if not session_secret:
            return False
        return hmac.compare_digest(session_secret, self.session_secret)

    # ------------------------------------------------------------ SPEC-W97
    _STATE_VERSION = 1

    def to_dict(self) -> dict[str, Any]:
        """Serialize the resume-relevant state (SPEC-W97 G8).

        ``ivr_menu`` is deliberately NOT persisted: it is derived
        per-call from settings (parse_ivr_menu is deterministic), so a
        resumed session re-parses it in build_voice_agent.
        """
        return {
            "v": self._STATE_VERSION,
            "conversation_id": self.conversation_id,
            "site_slug": self.site_slug,
            "session_secret": self.session_secret,
            "pending_phone": self.pending_phone,
            "confirmed_phone": self.confirmed_phone,
            "verified_phone": self.verified_phone,
            "claimed_phone": self.claimed_phone,
            "caller_name": self.caller_name,
            "last_booking_ids": list(self.last_booking_ids),
            "active_agent": self.active_agent,
            "escalation_room": self.escalation_room,
            "active_language": self.active_language,
            "channel": self.channel,
            "resolved_agent_id": self.resolved_agent_id,
            "created_at": self.created_at,
            "touched_at": self.touched_at,
        }

    @classmethod
    def from_dict(cls, data: Any) -> "SessionState | None":
        """Inverse of to_dict; defensive — junk/foreign envelopes return
        None (the store treats them as a miss, never a crash)."""
        if not isinstance(data, dict):
            return None
        if data.get("v") != cls._STATE_VERSION:
            return None
        cid = data.get("conversation_id")
        slug = data.get("site_slug")
        secret = data.get("session_secret")
        if not all(isinstance(x, str) and x for x in (cid, slug, secret)):
            return None
        session = cls(conversation_id=cid, site_slug=slug, session_secret=secret)

        def _str(key: str) -> "str | None":
            value = data.get(key)
            return value if isinstance(value, str) and value else None

        session.pending_phone = _str("pending_phone")
        session.confirmed_phone = _str("confirmed_phone")
        session.verified_phone = _str("verified_phone")
        session.claimed_phone = _str("claimed_phone")
        session.caller_name = _str("caller_name")
        ids = data.get("last_booking_ids")
        session.last_booking_ids = (
            [x for x in ids if isinstance(x, str)] if isinstance(ids, list) else []
        )
        session.active_agent = _str("active_agent")
        session.escalation_room = _str("escalation_room")
        session.active_language = _str("active_language")
        channel = data.get("channel")
        session.channel = channel if isinstance(channel, str) and channel else "web"
        session.resolved_agent_id = _str("resolved_agent_id")
        for key in ("created_at", "touched_at"):
            value = data.get(key)
            if isinstance(value, (int, float)):
                setattr(session, key, float(value))
        return session


@dataclass
class SessionStore:
    """In-memory session store with idle expiry."""

    ttl_s: int = 3600
    _sessions: dict[str, SessionState] = field(default_factory=dict)

    def get_or_create(
        self,
        conversation_id: str | None,
        site_slug: str,
        session_secret: str | None = None,
    ) -> SessionState:
        """Resume an existing session or start a fresh one.

        K15(b): resuming requires the session_secret issued at creation. A
        conversation_id presented WITHOUT the matching secret NEVER resumes
        confirmed_phone/history/escalation state — a brand-new session (new
        conversation_id) is created instead, leaving the targeted session
        untouched."""
        self._gc()
        if conversation_id and conversation_id in self._sessions:
            session = self._sessions[conversation_id]
            if session.check_secret(session_secret):
                session.touch()
                return session
            # Secret missing/wrong: fall through to a fresh session below.
        cid = conversation_id or str(uuid.uuid4())
        if cid in self._sessions:
            # Existing session, wrong/absent secret: never adopt its id.
            cid = str(uuid.uuid4())
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

    # -- SPEC-W97 persistence hooks (memory backend: documented no-ops) --
    def save(self, session: SessionState) -> None:
        """Memory sessions are shared by reference; nothing to write."""

    def load_history(self, conversation_id: str) -> None:
        """Memory backend: ChatHistory owns the history — returns None."""
        return None

    def save_history(self, conversation_id: str, messages: list[dict[str, Any]]) -> None:
        """Memory backend: ChatHistory owns the history — nothing to write."""


class DaprSessionStore:
    """SPEC-W97 G8: session + chat-history persistence through the Dapr
    state store (``statestore``, Redis — component + voice scope already
    exist in infra/dapr/components/statestore.redis.yaml).

    No in-memory cache: every get_or_create is one state GET, every save
    one state PUT (single writer per conversation; last-writer-wins —
    ETag concurrency is a documented non-goal). Fail-open: a GET error
    degrades to a fresh session (resume lost, calls unaffected); save
    errors propagate to the caller's fail-open wrapper. Both directions
    are counted on voice_session_store_ops_total{op,result}.

    K15(b) wire parity: resuming STILL requires the session_secret —
    the envelope is loaded but state is adopted only when the secret
    matches (constant-time); otherwise a fresh session under a NEW
    conversation_id is created, the targeted envelope untouched.
    """

    KEY_PREFIX = "voice-session:"
    HISTORY_PREFIX = "voice-history:"
    HISTORY_MAX = 40  # ChatHistory.max_messages parity

    def __init__(
        self,
        dapr: Any,
        *,
        store_name: str = "statestore",
        ttl_s: int = 3600,
    ) -> None:
        self._dapr = dapr
        self._store = store_name
        self.ttl_s = ttl_s

    def _key(self, conversation_id: str) -> str:
        return f"{self.KEY_PREFIX}{conversation_id}"

    def _history_key(self, conversation_id: str) -> str:
        return f"{self.HISTORY_PREFIX}{conversation_id}"

    async def get(self, conversation_id: str) -> SessionState | None:
        """Load a session envelope; None on miss, foreign junk, or a
        state-store error (fail-open, counted)."""
        try:
            data = await self._dapr.get_state(self._store, self._key(conversation_id))
        except Exception as exc:  # noqa: BLE001 - fail-open degradation
            metrics.session_store_op("load", "error")
            log.warning(
                "session state load failed", conversation_id=conversation_id,
                error=str(exc)[:200],
            )
            return None
        if data is None:
            metrics.session_store_op("load", "miss")
            return None
        session = SessionState.from_dict(data)
        metrics.session_store_op("load", "ok" if session is not None else "miss")
        return session

    async def get_or_create(
        self,
        conversation_id: str | None,
        site_slug: str,
        session_secret: str | None = None,
    ) -> SessionState:
        """Resume (secret-gated) or start fresh — K15(b) over the wire."""
        if conversation_id:
            session = await self.get(conversation_id)
            if session is not None and session.check_secret(session_secret):
                session.touch()
                return session
            if session is not None:
                # Wrong/absent secret: NEVER adopt the targeted id — a
                # fresh session under a NEW conversation_id, the targeted
                # envelope untouched (K15(b) wire parity).
                return SessionState(
                    conversation_id=str(uuid.uuid4()), site_slug=site_slug
                )
            # Miss: a caller-chosen fresh id is fine (same as memory).
        cid = conversation_id or str(uuid.uuid4())
        return SessionState(conversation_id=cid, site_slug=site_slug)

    async def save(self, session: SessionState) -> None:
        """Persist the envelope with TTL. Raises DaprError — callers wrap
        fail-open; the metric counts both outcomes."""
        try:
            await self._dapr.save_state(
                self._store, self._key(session.conversation_id),
                session.to_dict(), ttl_s=self.ttl_s,
            )
        except Exception:
            metrics.session_store_op("save", "error")
            raise
        metrics.session_store_op("save", "ok")

    async def load_history(self, conversation_id: str) -> "list[dict[str, Any]] | None":
        """The persisted chat history (None on miss — NOT an empty list —
        so the caller can distinguish 'no record' from 'empty history')."""
        try:
            data = await self._dapr.get_state(
                self._store, self._history_key(conversation_id)
            )
        except Exception as exc:  # noqa: BLE001 - fail-open degradation
            metrics.session_store_op("history_load", "error")
            log.warning(
                "chat history load failed", conversation_id=conversation_id,
                error=str(exc)[:200],
            )
            return None
        if not isinstance(data, list):
            metrics.session_store_op("history_load", "miss")
            return None
        metrics.session_store_op("history_load", "ok")
        return [m for m in data if isinstance(m, dict)]

    async def save_history(
        self, conversation_id: str, messages: list[dict[str, Any]]
    ) -> None:
        """Persist the bounded history (last HISTORY_MAX messages)."""
        bounded = list(messages)[-self.HISTORY_MAX :]
        try:
            await self._dapr.save_state(
                self._store, self._history_key(conversation_id),
                bounded, ttl_s=self.ttl_s,
            )
        except Exception:
            metrics.session_store_op("history_save", "error")
            raise
        metrics.session_store_op("history_save", "ok")


# ---------------------------------------------------------------------------
# SPEC-W97 backend-agnostic helpers (maybe-await idiom; the memory backend's
# hooks are sync no-ops, the Dapr backend's are async).


async def store_get_or_create(
    store: Any,
    conversation_id: str | None,
    site_slug: str,
    session_secret: str | None = None,
) -> SessionState:
    result = store.get_or_create(
        conversation_id, site_slug, session_secret=session_secret
    )
    if inspect.isawaitable(result):
        return await result
    return result


async def store_get(store: Any, conversation_id: str) -> SessionState | None:
    result = store.get(conversation_id)
    if inspect.isawaitable(result):
        return await result
    return result


async def store_save_session(store: Any, session: SessionState) -> None:
    """Persist the session if the backend has a save hook. Raises whatever
    the backend raises — callers wrap fail-open (never breaks a turn)."""
    save = getattr(store, "save", None)
    if save is None:
        return
    result = save(session)
    if inspect.isawaitable(result):
        await result


async def store_load_history(
    store: Any, conversation_id: str
) -> "list[dict[str, Any]] | None":
    """None = the backend keeps no history record (memory: ChatHistory
    owns it); a list hydrates the caller's in-turn bag."""
    load = getattr(store, "load_history", None)
    if load is None:
        return None
    result = load(conversation_id)
    if inspect.isawaitable(result):
        return await result
    return result


async def store_save_history(
    store: Any, conversation_id: str, messages: list[dict[str, Any]]
) -> None:
    save = getattr(store, "save_history", None)
    if save is None:
        return
    result = save(conversation_id, messages)
    if inspect.isawaitable(result):
        await result
