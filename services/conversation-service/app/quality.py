"""Call-quality sentiment enrichment (STRATEGY §3, Wave 5 innovation 2).

Consumes SessionEnded CloudEvents from opendesk.conversation.events (own
consumer group `conversation-sentiment`, so offsets are independent of
crm-sync-service), computes the average per-turn sentiment for the
conversation from the turns table (the `sentiment` column written by
app/intel.py), and republishes the quality payload — enriched with
avg_sentiment — as CallQualityEnriched on the NEW topic
opendesk.conversation.quality.

A separate topic (not opendesk.conversation.events) is deliberate: the
enriched event must not retrigger SessionEnded consumers.

Skip+ack cases (never retried, none are errors):
  - the event is not a SessionEnded, or carries no quality object;
  - quality.confirmed_phone is empty (the CRM note path would skip anyway,
    so enrichment would be wasted work);
  - the conversation has no turns with a sentiment score (e.g. intel ran
    before the columns existed) — there is nothing to enrich with.
"""

from __future__ import annotations

import asyncio
import json
import uuid
from typing import Any

from .config import Config
from .db import Database
from .events import cloud_event
from .logging import get_logger

log = get_logger(__name__)

EVENT_TYPE_SESSION_ENDED = "com.opendesk.conversation.SessionEnded"
EVENT_TYPE_CALL_QUALITY_ENRICHED = "com.opendesk.conversation.CallQualityEnriched"


def build_enriched_event(
    env: dict[str, Any], avg_sentiment: float, turn_sentiment_count: int
) -> dict[str, Any]:
    """Pure builder for the CallQualityEnriched CloudEvent (tested directly).

    The incoming SessionEnded quality payload is copied and extended with
    avg_sentiment; the top-level data also carries avg_sentiment +
    turn_sentiment_count so consumers can use them without re-deriving.
    """
    data = env.get("data") or {}
    quality = dict(data.get("quality") or {})
    quality["avg_sentiment"] = avg_sentiment
    payload: dict[str, Any] = {
        "conversationId": data.get("conversationId"),
        "channel": data.get("channel"),
        "siteSlug": data.get("siteSlug"),
        "quality": quality,
        "avg_sentiment": avg_sentiment,
        "turn_sentiment_count": turn_sentiment_count,
    }
    # 2026-11-23: the stored transcript's id (the runtime session id differs) so crm-sync can summarise the call
    if data.get("storedConversationId"):
        payload["storedConversationId"] = data.get("storedConversationId")
    return cloud_event(
        EVENT_TYPE_CALL_QUALITY_ENRICHED,
        subject=str(env.get("subject") or data.get("siteSlug") or ""),
        tenant_id=str(env.get("tenantid") or ""),
        data=payload,
    )


class CallQualityEnricher:
    """Background task: SessionEnded -> sentiment -> CallQualityEnriched.

    Direct-broker aiokafka consumer (same pattern as the privacy erase
    consumer) + a TranscriptSink producer for the quality topic.
    """

    def __init__(self, cfg: Config, db: Database, sink: Any) -> None:
        self._cfg = cfg
        self._db = db
        self._sink = sink
        self._task: asyncio.Task | None = None
        self._consumer: Any = None

    def start(self) -> None:
        self._task = asyncio.create_task(self._run(), name="call-quality-enricher")
        log.info(
            "call-quality enricher started",
            topic=self._cfg.conversation_events_topic,
            group=self._cfg.sentiment_group,
            publish_topic=self._cfg.quality_topic,
        )

    async def stop(self) -> None:
        if self._task is not None:
            self._task.cancel()
            try:
                await self._task
            except (asyncio.CancelledError, Exception):  # noqa: BLE001
                pass
        if self._consumer is not None:
            try:
                await self._consumer.stop()
            except Exception:  # noqa: BLE001
                pass

    async def _run(self) -> None:
        from aiokafka import AIOKafkaConsumer

        backoff = 2.0
        while True:
            try:
                self._consumer = AIOKafkaConsumer(
                    self._cfg.conversation_events_topic,
                    bootstrap_servers=self._cfg.kafka_brokers,
                    group_id=self._cfg.sentiment_group,
                    enable_auto_commit=False,
                    auto_offset_reset="earliest",
                )
                await self._consumer.start()
                backoff = 2.0
                async for msg in self._consumer:
                    if await self._process(msg.value):
                        await self._consumer.commit()
            except asyncio.CancelledError:
                raise
            except Exception as exc:  # noqa: BLE001 — keep the consumer alive
                log.error("quality enricher error; retrying", error=str(exc))
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, 30.0)
            finally:
                if self._consumer is not None:
                    try:
                        await self._consumer.stop()
                    except Exception:  # noqa: BLE001
                        pass
                    self._consumer = None

    async def _save_call_details(self, env: dict, data: dict, quality: dict) -> None:
        """2026-11-23: keep the call's facts on the stored conversation (the dashboard shows them; the CRM note
        carries the same). Best effort: a failure here never blocks the enrichment."""
        stored = data.get("storedConversationId") or data.get("conversationId")
        try:
            conversation_id = uuid.UUID(str(stored))
            tenant_id = uuid.UUID(str(env.get("tenantid")))
        except (ValueError, AttributeError, TypeError):
            return
        tools = quality.get("tool_calls") or {}
        outcome = ("booked" if tools.get("book_appointment") else "rescheduled" if tools.get("reschedule_appointment")
                   else "cancelled" if tools.get("cancel_appointment") else "none")
        details = {k: v for k, v in {
            "direction": quality.get("direction") or data.get("direction") or "inbound",
            "purpose": quality.get("purpose"),
            "contact_name": quality.get("contact_name"),
            "channel": quality.get("channel") or data.get("channel"),
            "duration_s": quality.get("duration_s"),
            "outcome": outcome,
            "escalated": bool(quality.get("escalated")),
            "confirmed_phone": quality.get("confirmed_phone") or None,
            "whatsapp_sent": bool(tools.get("send_whatsapp_confirmation")),
        }.items() if v is not None}
        try:
            await self._db.merge_call_details(conversation_id, tenant_id, details, ended=True)
        except Exception as exc:  # noqa: BLE001
            log.warning("call details not saved", error=str(exc)[:200])

    async def _process(self, value: bytes) -> bool:
        """Handle one event. Returns True when the offset may be committed
        (processed, enriched or deliberately skipped); False to retry later
        (transient DB/broker failures)."""
        try:
            env = json.loads(value)
        except (ValueError, UnicodeDecodeError):
            log.error("malformed conversation event; skipping")
            return True  # poison payload — never heals
        if not isinstance(env, dict) or env.get("type") != EVENT_TYPE_SESSION_ENDED:
            return True  # other conversation events: acknowledge and skip
        data = env.get("data") or {}
        quality = data.get("quality")
        if not isinstance(quality, dict) or not quality:
            return True  # session recorded no signals — nothing to enrich
        await self._save_call_details(env, data, quality)
        phone = str(quality.get("confirmed_phone") or "").strip()
        if not phone:
            return True  # CRM note path skips too; enrichment would be wasted
        try:
            conversation_id = uuid.UUID(str(data.get("conversationId")))
            tenant_id = uuid.UUID(str(env.get("tenantid")))
        except (ValueError, AttributeError, TypeError):
            log.error("SessionEnded with bad conversationId/tenantid; skipping")
            return True

        for attempt in range(1, 4):
            try:
                avg, count = await self._db.sentiment_summary(conversation_id, tenant_id)
                if count == 0 or avg is None:
                    log.info(
                        "no sentiment turns for conversation; enrichment skipped",
                        conversation_id=str(conversation_id),
                    )
                    return True
                await self._sink.publish(build_enriched_event(env, avg, count))
                try:
                    stored = data.get("storedConversationId") or data.get("conversationId")
                    await self._db.merge_call_details(uuid.UUID(str(stored)), tenant_id, {"avg_sentiment": avg})
                except Exception as exc:  # noqa: BLE001
                    log.warning("call mood not saved", error=str(exc)[:200])
                log.info(
                    "call quality enriched with sentiment",
                    conversation_id=str(conversation_id),
                    avg_sentiment=round(avg, 4),
                    turn_sentiment_count=count,
                )
                return True
            except Exception as exc:  # noqa: BLE001
                log.error("quality enrichment failed", error=str(exc), attempt=attempt)
                await asyncio.sleep(attempt * 0.5)
        return False
