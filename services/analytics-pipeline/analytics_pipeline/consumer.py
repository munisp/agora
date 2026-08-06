"""aiokafka consumer with per-topic micro-batching -> Iceberg sink.

Delivery semantics: at-least-once. Offsets are committed only AFTER a
successful Iceberg append; on append failure the batch is kept, the error is
counted, and the consumer retries on the next flush tick. Duplicates on
redelivery are expected and removed downstream by the Spark silver jobs
(infra/lakehouse/spark/jobs).
"""

from __future__ import annotations

import asyncio
import json
import time
from typing import Any, Awaitable, Callable, Mapping

import structlog
from aiokafka import AIOKafkaConsumer, TopicPartition

from . import metrics
from .config import Settings
from .iceberg_tables import IcebergSink
from .mapping import (
    map_booking_event,
    map_cac_event,
    map_payment_event,
    map_transcript,
    map_usage_event,
)

log = structlog.get_logger()

# topic -> (bronze table, row mapper)
Mapper = Callable[[Mapping[str, Any]], dict[str, Any]]


def topic_registry(settings: Settings) -> dict[str, tuple[str, Mapper]]:
    return {
        settings.topic_booking_events: ("booking_events", map_booking_event),
        settings.topic_payment_events: ("payment_events", map_payment_event),
        settings.topic_transcripts: ("transcripts", map_transcript),
        # Wave 5 #9: usage metering records -> bronze.usage_events.
        settings.topic_usage_events: ("usage_events", map_usage_event),
        # SPEC-W33 §2 A2: CAC funnel events -> bronze.cac_events. Same
        # consumer group (kafka_group_id) and commit-after-append semantics
        # as the other bronze topics; the realtime rollup path keeps its own
        # group (cac_events_group) in cac_consumer.py.
        settings.cac_events_topic: ("cac_events", map_cac_event),
    }


def _decode(raw: bytes | None) -> Mapping[str, Any]:
    if not raw:
        return {}
    try:
        value = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        log.warning("consumer.bad_message_dropped")
        return {}
    return value if isinstance(value, Mapping) else {}


class BronzeConsumer:
    def __init__(self, settings: Settings, sink: IcebergSink):
        self._settings = settings
        self._sink = sink
        self._registry = topic_registry(settings)
        self._buffers: dict[str, list[Mapping[str, Any]]] = {t: [] for t in self._registry}
        self._buffer_since: dict[str, float] = {}
        self._consumer: AIOKafkaConsumer | None = None
        self.running = False
        self.last_error: str | None = None

    # -- lifecycle ---------------------------------------------------------
    async def start(self) -> None:
        self._consumer = AIOKafkaConsumer(
            *self._registry.keys(),
            bootstrap_servers=self._settings.kafka_bootstrap_servers,
            group_id=self._settings.kafka_group_id,
            enable_auto_commit=False,
            auto_offset_reset="earliest",
            value_deserializer=None,  # raw bytes; JSON handled per-message
        )
        await self._consumer.start()
        self.running = True
        metrics.CONSUMER_UP.set(1)
        log.info("consumer.started", topics=list(self._registry))

    async def stop(self) -> None:
        self.running = False
        if self._consumer is not None:
            await self._flush_all()
            await self._consumer.stop()
        metrics.CONSUMER_UP.set(0)
        log.info("consumer.stopped")

    # -- main loop ---------------------------------------------------------
    async def run(self, stop_event: asyncio.Event) -> None:
        assert self._consumer is not None, "start() must be called first"
        flusher = asyncio.create_task(self._flush_ticker(stop_event))
        try:
            async for msg in self._consumer:
                if stop_event.is_set():
                    break
                value = _decode(msg.value)
                if not value:
                    continue
                buffer = self._buffers[msg.topic]
                if not buffer:
                    self._buffer_since[msg.topic] = time.monotonic()
                buffer.append(value)
                metrics.MESSAGES_CONSUMED.labels(topic=msg.topic).inc()
                metrics.BUFFER_SIZE.labels(topic=msg.topic).set(len(buffer))
                await self._update_lag_metric(msg.topic, msg.partition)
                if len(buffer) >= self._settings.batch_size:
                    await self._flush(msg.topic)
        finally:
            flusher.cancel()
            try:
                await flusher
            except asyncio.CancelledError:
                pass

    # -- flushing ----------------------------------------------------------
    async def _flush_ticker(self, stop_event: asyncio.Event) -> None:
        while not stop_event.is_set():
            await asyncio.sleep(1.0)
            now = time.monotonic()
            for topic, buffer in self._buffers.items():
                if not buffer:
                    continue
                age = now - self._buffer_since.get(topic, now)
                if age >= self._settings.flush_interval_seconds:
                    await self._flush(topic)

    async def _flush_all(self) -> None:
        for topic in list(self._buffers):
            await self._flush(topic)

    async def _flush(self, topic: str) -> None:
        buffer = self._buffers[topic]
        if not buffer:
            return
        table, mapper = self._registry[topic]
        batch, self._buffers[topic] = buffer, []
        metrics.BUFFER_SIZE.labels(topic=topic).set(0)
        started = time.monotonic()
        try:
            rows = [mapper(value) for value in batch]
            written = await asyncio.to_thread(self._sink.append, table, rows)
        except Exception as exc:  # keep the batch; retry next tick
            self._buffers[topic] = batch + self._buffers[topic]
            self._buffer_since[topic] = time.monotonic()
            metrics.BUFFER_SIZE.labels(topic=topic).set(len(self._buffers[topic]))
            metrics.FLUSHES.labels(table=table, outcome="error").inc()
            self.last_error = f"{type(exc).__name__}: {exc}"
            log.error("sink.flush_failed", table=table, error=self.last_error)
            return
        elapsed = time.monotonic() - started
        metrics.FLUSHES.labels(table=table, outcome="ok").inc()
        metrics.FLUSH_DURATION.labels(table=table).observe(elapsed)
        metrics.ROWS_WRITTEN.labels(table=table).inc(written)
        self.last_error = None
        if self._consumer is not None:
            await self._consumer.commit()  # at-least-once: commit only after append
        log.info("sink.flushed", table=table, rows=written, seconds=round(elapsed, 3))

    # -- health ------------------------------------------------------------
    def _update_lag_metric(self, topic: str, partition: int) -> None:
        if self._consumer is None:
            return
        tp = TopicPartition(topic, partition)
        try:
            highwater = self._consumer.highwater(tp)
            position = self._consumer.position(tp)
        except Exception:
            return
        if highwater is not None and position is not None:
            metrics.CONSUMER_LAG.labels(topic=topic, partition=str(partition)).set(
                max(highwater - position, 0)
            )

    async def lag_report(self) -> dict[str, dict[str, Any]]:
        """Per-topic lag + buffer depth for GET /healthz."""
        report: dict[str, dict[str, Any]] = {}
        assignment = self._consumer.assignment() if self._consumer else set()
        for topic in self._registry:
            lag = 0
            partitions: dict[str, int] = {}
            for tp in assignment:
                if tp.topic != topic or self._consumer is None:
                    continue
                try:
                    highwater = self._consumer.highwater(tp)
                    position = self._consumer.position(tp)
                except Exception:
                    continue
                if highwater is not None and position is not None:
                    tp_lag = max(highwater - position, 0)
                    partitions[str(tp.partition)] = tp_lag
                    lag += tp_lag
            report[topic] = {
                "lag": lag,
                "partitions": partitions,
                "buffered": len(self._buffers[topic]),
                "table": f"bronze.{self._registry[topic][0]}",
            }
        return report
