"""aiokafka consumer for the CAC funnel stream (SPEC-W13 contract §2/§7).

Topic ``cac.events`` (CAC_EVENTS_TOPIC), consumer group ``analytics-cac``
(CAC_EVENTS_GROUP), ``enable_auto_commit=False``: offsets are committed only
AFTER the rollup upsert + idempotency marker commit in Postgres, so delivery
is at-least-once and CacStore.record_event dedupes replays by
idempotency_key (effectively-once rollups).

Unlike the bronze sink this stream is low-volume and per-event latency
matters (realtime dashboard), so events are applied one at a time instead of
micro-batched into Iceberg.
"""

from __future__ import annotations

import asyncio
import json
from typing import Any, Mapping

import structlog
from aiokafka import AIOKafkaConsumer

from . import metrics
from .cac_events import parse_funnel_event
from .cac_store import CacStore
from .config import Settings

log = structlog.get_logger()

RETRY_SECONDS = 5.0


def _decode(raw: bytes | None) -> Mapping[str, Any]:
    if not raw:
        return {}
    try:
        value = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        log.warning("cac.consumer.bad_message_dropped")
        return {}
    return value if isinstance(value, Mapping) else {}


class CacConsumer:
    def __init__(self, settings: Settings, store: CacStore):
        self._settings = settings
        self._store = store
        self._consumer: AIOKafkaConsumer | None = None
        self.running = False
        self.last_error: str | None = None
        self.processed = 0
        self.replays = 0
        self.dropped = 0

    async def start(self) -> None:
        self._consumer = AIOKafkaConsumer(
            self._settings.cac_events_topic,
            bootstrap_servers=self._settings.kafka_bootstrap_servers,
            group_id=self._settings.cac_events_group,
            enable_auto_commit=False,
            auto_offset_reset="earliest",
            value_deserializer=None,  # raw bytes; JSON handled per-message
        )
        await self._consumer.start()
        self.running = True
        log.info(
            "cac.consumer.started",
            topic=self._settings.cac_events_topic,
            group=self._settings.cac_events_group,
        )

    async def stop(self) -> None:
        self.running = False
        if self._consumer is not None:
            await self._consumer.stop()
        log.info("cac.consumer.stopped")

    async def process_value(self, value: Mapping[str, Any]) -> bool:
        """Apply one decoded Kafka value. Returns True when the offset may be
        committed (applied, replay, or intentionally dropped)."""
        event = parse_funnel_event(value)
        if event is None:
            self.dropped += 1
            log.warning("cac.consumer.event_dropped")
            return True  # poison pill: commit past it, bronze keeps the raw copy
        applied = await self._store.record_event(event)
        if applied:
            self.processed += 1
        else:
            self.replays += 1
        metrics.CAC_EVENTS_PROCESSED.labels(
            outcome="applied" if applied else "replay"
        ).inc()
        return True

    async def run(self, stop_event: asyncio.Event) -> None:
        assert self._consumer is not None, "start() must be called first"
        async for msg in self._consumer:
            if stop_event.is_set():
                break
            value = _decode(msg.value)
            if not value:
                continue
            try:
                await self.process_value(value)
            except Exception as exc:  # noqa: BLE001 — PG outage mid-stream
                # Do NOT commit: the message is redelivered after rebalance/
                # restart; the idempotency marker keeps replays exact.
                self.last_error = f"{type(exc).__name__}: {exc}"
                log.error("cac.consumer.process_failed", error=self.last_error)
                await asyncio.sleep(RETRY_SECONDS)
                continue
            self.last_error = None
            metrics.MESSAGES_CONSUMED.labels(topic=msg.topic).inc()
            await self._consumer.commit()
