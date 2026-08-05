"""Audit event seam (SPEC-W30 §4 WS-C).

Alert resolution emits a CloudEvent (``com.opendesk.fraud.AlertResolved``)
to topic ``opendesk.fraud.alerts.v1``. The publisher is a thin abstraction
so tests inject a fake; production uses Kafka when KAFKA_BOOTSTRAP_SERVERS
is set, otherwise a structured-log no-op (dev tiers run without Kafka).
"""

from __future__ import annotations

import json
from datetime import datetime, timezone
from typing import Any, Protocol

import structlog

log = structlog.get_logger("graph-service.events")

ALERT_RESOLVED_TYPE = "com.opendesk.fraud.AlertResolved"


class AlertEventPublisher(Protocol):
    async def publish(self, topic: str, event: dict[str, Any]) -> None: ...


class NoopAlertEventPublisher:
    """Default when Kafka is not configured: structured-log the event."""

    def __init__(self) -> None:
        self.published: list[tuple[str, dict[str, Any]]] = []

    async def publish(self, topic: str, event: dict[str, Any]) -> None:
        self.published.append((topic, event))
        log.info("alert_event", topic=topic, event_type=event.get("type"), id=event.get("id"))


class KafkaAlertEventPublisher:
    """confluent-kafka producer (lazy import; best-effort delivery — a Kafka
    outage logs an error but never fails the resolve request)."""

    def __init__(self, bootstrap_servers: str) -> None:
        from confluent_kafka import Producer  # lazy: not needed for tests

        self._producer = Producer({"bootstrap.servers": bootstrap_servers})

    async def publish(self, topic: str, event: dict[str, Any]) -> None:
        import asyncio

        payload = json.dumps(event, sort_keys=True).encode("utf-8")

        def _send() -> None:
            self._producer.produce(
                topic, key=str(event.get("id", "")).encode(), value=payload
            )
            self._producer.flush(timeout=10)

        try:
            await asyncio.to_thread(_send)
        except Exception as exc:  # noqa: BLE001 — never fail the API call
            log.error("alert_event_publish_failed", topic=topic, error=str(exc))


def build_alert_resolved_event(
    *,
    tenant_id: str,
    alert: dict[str, Any],
    decision: str,
    reason: str,
    resolved_by: str,
    resolved_at: str,
) -> dict[str, Any]:
    """CloudEvents 1.0 envelope, mirroring fraud-engine's AlertRaised shape
    (id ``tenant:alert-resolution:alert_id``, ``tenantid`` extension)."""
    alert_id = str(alert.get("alert_id"))
    return {
        "specversion": "1.0",
        "id": f"{tenant_id}:alert-resolution:{alert_id}",
        "source": "graph-service",
        "type": ALERT_RESOLVED_TYPE,
        "subject": alert_id,
        "time": resolved_at
        or datetime.now(timezone.utc).isoformat(),
        "tenantid": tenant_id,
        "data": {
            "alert_id": alert_id,
            "tenant_id": tenant_id,
            "type": alert.get("type"),
            "severity": alert.get("severity"),
            "person_id": alert.get("person_id"),
            "agent_id": alert.get("agent_id"),
            "decision": decision,
            "reason": reason,
            "resolved_by": resolved_by,
            "resolved_at": resolved_at,
        },
    }
