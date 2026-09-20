"""Helpdesk channel automation (SPEC-W45, UC helpdesk): when a USER turn
carries a human-escalation intent, conversation-service opens a real
helpdesk ticket via booking-service POST /v1/helpdesk/tickets
(X-Internal-Token = CONVERSATION_BOOKING_INTERNAL_TOKEN, tenant-bound via
X-Tenant-Slug resolved from the tenant_slugs projection) — so the
channel→ticket path is real instead of aspirational.

Detection is deterministic: a curated multi-word escalation phrase lexicon
(always on) plus the optional LLM NER intent names (INTEL_LLM=on). Ticket
creation is a non-blocking background task (same posture as the incident
IDP emission): failures are logged, never raised into the turn path. One
ticket per conversation per process lifetime (in-memory dedupe; a restart
may open a follow-up ticket for a repeat escalation — acceptable and
preferable to silently dropping the escalation).
"""

from __future__ import annotations

import asyncio
import re
import uuid
from typing import Any

from .config import Config
from .dapr_client import DaprClient, DaprError
from .logging import get_logger

log = get_logger(__name__)

# Multi-word escalation phrases (EN + common service phrasings). Matched
# against normalize_text() output (lowercase, punctuation-stripped) with
# word boundaries so single benign words never trigger a ticket.
_ESCALATION_PHRASES = tuple(
    sorted(
        {
            "speak to a human", "speak with a human", "talk to a human",
            "speak to a person", "talk to a person", "real person",
            "human agent", "live agent", "real agent",
            "speak to an agent", "talk to an agent", "speak with an agent",
            "speak to someone", "talk to someone",
            "speak to a manager", "talk to a manager", "speak with a manager",
            "customer service", "customer care", "support agent",
            "lodge a complaint", "file a complaint", "make a complaint",
            "formal complaint", "escalate this", "escalate my",
            "want a human", "need a human",
        },
        key=len,
        reverse=True,
    )
)

# LLM NER intent names (INTEL_LLM=on) that count as human escalation.
_ESCALATION_INTENTS = frozenset(
    {
        "human_escalation",
        "escalate_to_human",
        "escalate_to_agent",
        "speak_to_agent",
        "request_human",
        "request_human_agent",
        "talk_to_human",
        "complaint_escalation",
    }
)

_WORD_RE = re.compile(r"[a-z']+")


def _normalize(text: str) -> str:
    return " ".join(_WORD_RE.findall(text.lower()))


def is_escalation(text: str, intent: str | None = None) -> tuple[bool, str]:
    """Human-escalation detector. Returns (hit, matched_signal)."""
    if intent and intent.strip().lower() in _ESCALATION_INTENTS:
        return True, f"intent:{intent.strip().lower()}"
    norm = _normalize(text or "")
    for phrase in _ESCALATION_PHRASES:
        if re.search(rf"\b{re.escape(phrase)}\b", norm):
            return True, f"phrase:{phrase}"
    return False, ""


class HelpdeskAutomation:
    """Opens helpdesk tickets for escalated conversations (one per
    conversation per process lifetime)."""

    def __init__(self, cfg: Config, dapr: DaprClient, agent_store: Any) -> None:
        self._cfg = cfg
        self._dapr = dapr
        self._agents = agent_store
        self._ticketed: set[uuid.UUID] = set()

    def maybe_escalate(
        self,
        *,
        tenant_id: uuid.UUID,
        conversation_id: uuid.UUID,
        turn_id: uuid.UUID,
        text: str,
        intent: str | None,
        channel: str,
        contact_phone: str | None,
        background_tasks: set[asyncio.Task] | None = None,
    ) -> bool:
        """Schedule ticket creation when the turn escalates. Returns True
        when a ticket task was scheduled (never raises)."""
        if not self._cfg.helpdesk_enabled:
            return False
        hit, signal = is_escalation(text, intent)
        if not hit:
            return False
        if conversation_id in self._ticketed:
            log.info(
                "helpdesk ticket already opened for conversation; skipping",
                conversation_id=str(conversation_id),
            )
            return False
        self._ticketed.add(conversation_id)
        task = asyncio.create_task(
            self._emit_ticket(
                tenant_id=tenant_id,
                conversation_id=conversation_id,
                turn_id=turn_id,
                text=text,
                channel=channel,
                contact_phone=contact_phone,
                signal=signal,
            ),
            name=f"helpdesk-ticket-{conversation_id}",
        )
        if background_tasks is not None:
            background_tasks.add(task)
            task.add_done_callback(background_tasks.discard)
        return True

    async def _tenant_slug(self, tenant_id: uuid.UUID) -> str | None:
        """tenant_id -> slug via the tenant_slugs projection (J-14 exempt
        mapping table; populated by the TenantResolver write-through)."""
        try:
            return await self._agents.tenant_slug_for(tenant_id)
        except Exception as exc:  # noqa: BLE001
            log.error("tenant slug projection lookup failed", error=str(exc),
                      tenant_id=str(tenant_id))
            return None

    async def _emit_ticket(
        self,
        *,
        tenant_id: uuid.UUID,
        conversation_id: uuid.UUID,
        turn_id: uuid.UUID,
        text: str,
        channel: str,
        contact_phone: str | None,
        signal: str,
    ) -> None:
        """POST the ticket via Dapr service invocation. Logs and never
        raises (the turn path must not fail on helpdesk outages)."""
        if not self._cfg.booking_internal_token:
            log.error(
                "CONVERSATION_BOOKING_INTERNAL_TOKEN unset; helpdesk ticket "
                "NOT created (booking would fail closed)",
                conversation_id=str(conversation_id),
            )
            return
        slug = await self._tenant_slug(tenant_id)
        if not slug:
            log.error(
                "tenant slug unknown (tenant_slugs projection empty); "
                "helpdesk ticket NOT created",
                tenant_id=str(tenant_id), conversation_id=str(conversation_id),
            )
            return
        excerpt = (text or "").strip().replace("\n", " ")
        if len(excerpt) > 120:
            excerpt = excerpt[:117] + "..."
        payload: dict[str, Any] = {
            "subject": f"Human escalation requested ({channel}): {excerpt}",
            "channel": channel or "web",
            "priority": "high",
            "conversation_id": str(conversation_id),
        }
        headers = {
            "X-Internal-Token": self._cfg.booking_internal_token,
            "X-Tenant-Slug": slug,
        }
        try:
            await self._dapr.invoke_post(
                self._cfg.booking_app_id,
                "v1/helpdesk/tickets",
                json_body=payload,
                headers=headers,
            )
        except DaprError as exc:
            log.error(
                "helpdesk ticket creation failed",
                error=str(exc), status=exc.status_code,
                tenant_id=str(tenant_id), conversation_id=str(conversation_id),
            )
            return
        except Exception as exc:  # noqa: BLE001
            log.error(
                "helpdesk ticket creation failed",
                error=str(exc), tenant_id=str(tenant_id),
                conversation_id=str(conversation_id),
            )
            return
        log.info(
            "helpdesk ticket opened for escalated conversation",
            tenant_id=str(tenant_id), tenant_slug=slug,
            conversation_id=str(conversation_id), turn_id=str(turn_id),
            channel=channel, contact=bool(contact_phone), signal=signal,
        )
