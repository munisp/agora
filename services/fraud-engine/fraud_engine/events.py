"""CloudEvents for fraud alerts (SPEC-W30 §3).

``com.opendesk.fraud.AlertRaised`` -> topic ``opendesk.fraud.alerts.v1``:
id ``tenant:alert:alert_id``, extension ``tenantid``,
data ``{alert_id, type, severity, person_id?, agent_id?}``.

The publisher is a thin seam: ``KafkaPublisher`` (confluent-kafka, imported
lazily) in production; ``InMemoryPublisher`` in tests/dev.
"""

from __future__ import annotations

import json
import math
from datetime import UTC, datetime
from typing import TYPE_CHECKING, Any, Protocol

if TYPE_CHECKING:
    from .alerts import AlertRecord

SOURCE = "fraud-engine"
ALERT_RAISED_TYPE = "com.opendesk.fraud.AlertRaised"
DEFAULT_ALERTS_TOPIC = "opendesk.fraud.alerts.v1"


def strict_json_safe(value: Any) -> Any:
    """Deep-sanitize a payload so it is valid STRICT JSON (SPEC-W34 GF9).

    Python's ``json.dumps`` emits bare ``NaN``/``Infinity`` tokens by
    default, which strict parsers (``parse_constant``-rejecting consumers,
    most non-Python deserializers) refuse. Any non-finite float anywhere in
    the structure is replaced with ``None``; every other value passes
    through unchanged.
    """
    if isinstance(value, float):
        return value if math.isfinite(value) else None
    if isinstance(value, dict):
        return {k: strict_json_safe(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [strict_json_safe(v) for v in value]
    return value


def alert_raised_event(tenant_id: str, alert: "AlertRecord") -> dict[str, Any]:
    """CloudEvents 1.0 envelope (SPEC-W30 §3 shape).

    The whole envelope is strict-JSON sanitized (SPEC-W34 GF9): no bare
    NaN/Infinity token can ever reach the wire, even if upstream evidence
    was poisoned.
    """
    return strict_json_safe(
        {
            "specversion": "1.0",
            "id": f"{tenant_id}:alert:{alert.alert_id}",  # SPEC: id = tenant:alert:alert_id
            "source": SOURCE,
            "type": ALERT_RAISED_TYPE,
            "subject": alert.alert_id,
            "time": datetime.now(UTC).isoformat(),
            "tenantid": tenant_id,  # extension
            "data": alert.event_data(),
        }
    )


class EventPublisher(Protocol):
    def publish(self, topic: str, key: str, event: dict[str, Any]) -> None: ...


class InMemoryPublisher:
    """Tests/dev: records published events; assertions inspect `.published`."""

    def __init__(self) -> None:
        self.published: list[tuple[str, str, dict[str, Any]]] = []

    def publish(self, topic: str, key: str, event: dict[str, Any]) -> None:
        self.published.append((topic, key, event))


class KafkaPublisher:
    """Production publisher (confluent-kafka, lazy import)."""

    def __init__(self, bootstrap_servers: str) -> None:
        try:
            from confluent_kafka import Producer  # lazy: not needed in tests
        except ImportError as exc:  # pragma: no cover
            raise RuntimeError("confluent-kafka package not installed") from exc
        self._producer = Producer({"bootstrap.servers": bootstrap_servers})

    def publish(self, topic: str, key: str, event: dict[str, Any]) -> None:
        # SPEC-W34 GF9: sanitize + allow_nan=False — the emitted payload is
        # always valid strict JSON (no bare NaN/Infinity), and any sanitizer
        # gap fails loudly here instead of shipping invalid JSON.
        payload = strict_json_safe(event)
        self._producer.produce(
            topic,
            key=key.encode(),
            value=json.dumps(payload, sort_keys=True, default=str, allow_nan=False).encode(),
        )
        self._producer.poll(0)

    def flush(self, timeout: float = 5.0) -> None:
        self._producer.flush(timeout)


def publisher_from_settings(settings: Any) -> EventPublisher:
    if settings.kafka_enabled:
        return KafkaPublisher(settings.kafka_bootstrap_servers)
    return InMemoryPublisher()
