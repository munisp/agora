"""Raw transcript sinks (SPEC §5).

Every accepted turn is streamed as a raw record
{conversationId, tenantId, role, text, ts} to topic
`opendesk.transcripts-raw` for high-throughput edge/telephony ingestion
(PII redaction happens downstream in the Fluvio smart module).

Two implementations behind the TranscriptSink protocol, selected by env
TRANSCRIPT_SINK=fluvio|kafka (default kafka):
  - FluvioSink  — official fluvio python client (import guarded; the sync
                  client is invoked via asyncio.to_thread). W48-B: connects
                  with an explicit FluvioConfig built from FLUVIO_ENDPOINT
                  (default fluvio:9003) instead of the ~/.fluvio profile,
                  which does not exist inside the container image.
  - KafkaSink   — aiokafka producer fallback writing the same topic on Kafka
"""

from __future__ import annotations

import asyncio
import json
from typing import Any, Protocol

from .config import Config
from .logging import get_logger

log = get_logger(__name__)


class TranscriptSink(Protocol):
    async def start(self) -> None: ...
    async def publish(self, record: dict[str, Any]) -> None: ...
    async def close(self) -> None: ...


class FluvioSink:
    """Publishes records via the fluvio python client.

    The import is guarded so the service runs without the optional
    `fluvio` package; construction then raises RuntimeError and the
    factory falls back to KafkaSink.

    W48-B: the connection uses an explicit endpoint config
    (`FluvioConfig.new(endpoint)` + `Fluvio.connect_with_config`) so the
    broker address comes from FLUVIO_ENDPOINT — never the default
    `~/.fluvio/config` profile.
    """

    def __init__(self, topic: str, endpoint: str = "fluvio:9003") -> None:
        try:
            from fluvio import Fluvio, FluvioConfig  # type: ignore[import-not-found]
        except ImportError as exc:  # pragma: no cover - env dependent
            raise RuntimeError(
                "TRANSCRIPT_SINK=fluvio requires the optional 'fluvio' package"
            ) from exc
        self._topic = topic
        self._endpoint = endpoint
        self._fluvio = Fluvio
        self._fluvio_config = FluvioConfig
        self._producer: Any = None

    async def start(self) -> None:
        def _connect() -> Any:
            config = self._fluvio_config.new(self._endpoint)
            client = self._fluvio.connect_with_config(config)
            return client.topic_producer(self._topic)

        self._producer = await asyncio.to_thread(_connect)
        log.info("fluvio sink connected", topic=self._topic, endpoint=self._endpoint)

    async def publish(self, record: dict[str, Any]) -> None:
        assert self._producer is not None, "FluvioSink.start() not called"
        payload = json.dumps(record)

        def _send() -> None:
            self._producer.send_string(payload)

        await asyncio.to_thread(_send)

    async def close(self) -> None:
        self._producer = None


class KafkaSink:
    """aiokafka fallback sink for the raw transcript topic."""

    def __init__(self, brokers: list[str], topic: str) -> None:
        self._brokers = brokers
        self._topic = topic
        self._producer: Any = None

    async def start(self) -> None:
        from aiokafka import AIOKafkaProducer

        # construct inside the running loop (aiokafka requirement)
        self._producer = AIOKafkaProducer(bootstrap_servers=self._brokers)
        await self._producer.start()
        log.info("kafka sink connected", topic=self._topic)

    async def publish(self, record: dict[str, Any]) -> None:
        assert self._producer is not None, "KafkaSink.start() not called"
        key = str(record.get("conversationId", "")).encode()
        await self._producer.send_and_wait(
            self._topic, json.dumps(record).encode(), key=key or None
        )

    async def close(self) -> None:
        if self._producer is not None:
            await self._producer.stop()
            self._producer = None


def build_sink(cfg: Config, topic: str | None = None) -> TranscriptSink:
    """Select the sink per TRANSCRIPT_SINK (default kafka) for one topic.

    W48-B: shared by all three sink topics — the raw transcript topic
    (default, cfg.fluvio_topic), the intel enriched topic
    (cfg.enriched_topic) and the call-quality topic (cfg.quality_topic) —
    so TRANSCRIPT_SINK=fluvio is honored identically everywhere, on the
    same per-sink topic names the Kafka path uses.

    Falls back to KafkaSink when the fluvio client is unavailable.
    """
    topic = topic or cfg.fluvio_topic
    if cfg.transcript_sink == "fluvio":
        try:
            return FluvioSink(topic, cfg.fluvio_endpoint)
        except RuntimeError as exc:
            log.warning("fluvio sink unavailable, falling back to kafka", error=str(exc))
    return KafkaSink(cfg.kafka_brokers, topic)
