"""Alert publishing seam (SPEC-W33 §4 C2/C5: topic `opendesk.ops.alerts`).

I1 honest degradation: the Kafka producer is IMPORT-GUARDED. kafka-python is
deliberately NOT in requirements.txt (I5 slim image); install it to enable
real publishing. Without it — or with the broker down — every publish falls
back to log-and-continue and NEVER raises into the scheduler.

Tests inject ``FakePublisher`` and assert on emitted payloads.
"""

from __future__ import annotations

import json
import logging
from typing import Any, Protocol

log = logging.getLogger(__name__)


class AlertPublisher(Protocol):
    def publish(self, topic: str, payload: dict[str, Any]) -> None:
        """Emit one alert payload; must not raise (I1)."""
        ...


class LogOnlyPublisher:
    """Default when Kafka is unavailable/disabled: log-and-continue (I1)."""

    def __init__(self) -> None:
        self.published: list[dict[str, Any]] = []  # ops-visible breadcrumb

    def publish(self, topic: str, payload: dict[str, Any]) -> None:
        self.published.append({"topic": topic, "payload": payload})
        log.warning("alert (log-only, kafka unavailable): topic=%s payload=%s",
                    topic, json.dumps(payload, sort_keys=True, default=str))


class KafkaPublisher:
    """Real publisher over kafka-python, lazily imported (import-guarded)."""

    def __init__(self, bootstrap_servers: str) -> None:
        from kafka import KafkaProducer  # noqa: PLC0415 lazy: optional dep

        self._producer = KafkaProducer(
            bootstrap_servers=bootstrap_servers,
            value_serializer=lambda v: json.dumps(v, default=str).encode("utf-8"),
            request_timeout_ms=5000,
            max_block_ms=5000,
        )

    def publish(self, topic: str, payload: dict[str, Any]) -> None:
        try:
            self._producer.send(topic, payload).get(timeout=5)
        except Exception:  # noqa: BLE001 — I1: never crash the scheduler
            log.exception("kafka publish failed (topic=%s); alert dropped to logs. "
                          "payload=%s", topic, json.dumps(payload, default=str))


def build_publisher(*, kafka_enabled: bool, bootstrap_servers: str) -> AlertPublisher:
    """Factory: real Kafka publisher if enabled and importable, else log-only."""
    if not kafka_enabled:
        log.info("KAFKA_ENABLED=false → log-only alert publisher")
        return LogOnlyPublisher()
    try:
        return KafkaPublisher(bootstrap_servers)
    except Exception as exc:  # noqa: BLE001 — import error OR broker down
        log.warning("kafka publisher unavailable (%s); falling back to log-only", exc)
        return LogOnlyPublisher()
