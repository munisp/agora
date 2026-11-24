"""Ends text conversations (web chat, WhatsApp, Telegram) so they reach the CRM like calls do (2026-11-24).

A phone or browser call ends when the caller hangs up, and its SessionEnded event (with the `quality` block) is what
conversation-service turns into the dashboard's Call details and crm-sync turns into a CRM note. Text chats never
end: there is no hang-up, so no SessionEnded was ever sent and no chat or WhatsApp conversation reached the CRM.

ChatEndTracker remembers each open text conversation as its turns happen and, once it has been quiet for
`idle_s` (CHAT_END_IDLE_S, default 600 s), publishes ONE SessionEnded for it with the same quality shape a call
carries: channel ("chat" | "whatsapp" | "telegram"), the customer's number and name, message count, tools used and
the stored conversation id. A conversation that resumes later is tracked afresh and ends again (crm-sync keeps one
note per stored conversation). State is in memory (voice runs one replica): conversations open during a restart end
without a note.
"""

from __future__ import annotations

import asyncio
import time
from collections import Counter
from typing import Any

from .events import new_cloudevent, session_lifecycle_data
from .logging import get_logger

log = get_logger("chat-end")

SESSION_ENDED = "com.opendesk.conversation.SessionEnded"


class ChatEndTracker:
    def __init__(self, *, dapr: Any, settings: Any, idle_s: float = 600.0, clock=time.monotonic) -> None:
        self._dapr = dapr
        self._settings = settings
        self._idle_s = idle_s
        self._clock = clock
        self._open: dict[str, dict[str, Any]] = {}
        self._task: asyncio.Task | None = None

    def touch(self, session: Any, tenant_ctx: Any, trace: list[dict[str, Any]] | None) -> None:
        """Record one finished turn of a text conversation (never raises)."""
        try:
            cid = str(session.conversation_id)
            lookup = getattr(session, "stored_id_lookup", None)
            if (getattr(session, "channel", None) or "web") in ("whatsapp", "telegram", "email") and callable(lookup):
                # The messaging gateway names its stored conversation (one per customer) on every message; the
                # runtime session id may differ per message, so the stored id keys the conversation.
                cid = str(lookup() or cid)
            now = self._clock()
            rec = self._open.get(cid)
            if rec is None:
                rec = self._open[cid] = {"started": now, "turns": 0, "tools": Counter()}
            rec["last"] = now
            rec["turns"] += 1
            for entry in trace or []:
                tool = entry.get("tool") if isinstance(entry, dict) else None
                if tool:
                    rec["tools"][tool] += 1
            rec["channel"] = (getattr(session, "channel", None) or "web")
            rec["phone"] = (getattr(session, "verified_phone", None) or getattr(session, "confirmed_phone", None)
                            or getattr(session, "claimed_phone", None))
            rec["name"] = getattr(session, "caller_name", None)
            rec["email"] = getattr(session, "caller_email", None)
            rec["language"] = getattr(session, "active_language", None)
            rec["escalated"] = bool(getattr(session, "escalation_room", None))
            rec["agent_id"] = getattr(session, "resolved_agent_id", None)
            rec["lookup"] = lookup if callable(lookup) else None
            if tenant_ctx is not None:
                rec["tenant_id"] = str(getattr(tenant_ctx, "tenant_id", "") or "")
                rec["tenant_slug"] = getattr(tenant_ctx, "tenant_slug", None)
                rec["site_slug"] = getattr(tenant_ctx, "site_slug", None) or getattr(session, "site_slug", None)
        except Exception as exc:  # noqa: BLE001 - tracking never breaks a chat turn
            log.warning("chat end tracking failed", error=str(exc)[:200])

    @staticmethod
    def quality(rec: dict[str, Any]) -> dict[str, Any]:
        channel = "chat" if rec["channel"] == "web" else rec["channel"]
        q: dict[str, Any] = {
            "duration_s": round(max(0.0, rec["last"] - rec["started"]), 1),
            "turn_count": rec["turns"],
            "tool_calls": dict(rec["tools"]),
            "stt_calls": 0,
            "tts_calls": 0,
            "llm_fallback_used": False,
            "escalated": rec["escalated"],
            "confirmed_phone": rec.get("phone") or "",
            "direction": "inbound",
            "channel": channel,
        }
        for key, field in (("name", "contact_name"), ("email", "contact_email"), ("language", "language")):
            if rec.get(key):
                q[field] = rec[key]
        return q

    async def end(self, cid: str, rec: dict[str, Any]) -> None:
        if not rec.get("tenant_id"):
            log.info("chat ended without a business; not published", conversation_id=cid)
            return
        stored = None
        if rec.get("lookup") is not None:
            try:
                stored = rec["lookup"]()
            except Exception:  # noqa: BLE001
                stored = None
        channel = "chat" if rec["channel"] == "web" else rec["channel"]
        event = new_cloudevent(
            type_=SESSION_ENDED,
            subject=rec.get("tenant_slug") or rec.get("site_slug") or "",
            tenant_uuid=rec["tenant_id"],
            data=session_lifecycle_data(
                conversation_id=cid,
                channel=channel,
                site_slug=rec.get("site_slug") or "",
                quality=self.quality(rec),
                agent_id=rec.get("agent_id"),
                stored_conversation_id=str(stored) if stored else None,
                direction="inbound",
            ),
        )
        await self._dapr.publish_best_effort(
            self._settings.dapr_pubsub, self._settings.conversation_events_topic, event, kind=SESSION_ENDED
        )
        log.info("text conversation ended", conversation_id=cid, channel=channel, turns=rec["turns"],
                 has_phone=bool(rec.get("phone")), stored_conversation_id=stored)

    async def sweep_once(self) -> int:
        now = self._clock()
        idle = [cid for cid, rec in self._open.items() if now - rec["last"] >= self._idle_s]
        for cid in idle:
            rec = self._open.pop(cid)
            try:
                await self.end(cid, rec)
            except Exception as exc:  # noqa: BLE001
                log.warning("chat end publish failed", conversation_id=cid, error=str(exc)[:200])
        return len(idle)

    async def _run(self) -> None:
        period = max(5.0, min(60.0, self._idle_s / 5))
        while True:
            await asyncio.sleep(period)
            await self.sweep_once()

    def start(self) -> None:
        if self._task is None:
            self._task = asyncio.create_task(self._run(), name="chat-end-sweeper")

    async def stop(self) -> None:
        if self._task is not None:
            self._task.cancel()
            try:
                await self._task
            except (asyncio.CancelledError, Exception):  # noqa: BLE001
                pass
            self._task = None
