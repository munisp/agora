"""CacConsumer tests: idempotency_key dedupe at the consumer level, poison
pill handling, and store-failure propagation (no offset commit on error)."""

from __future__ import annotations

import asyncio

from analytics_pipeline.cac_consumer import CacConsumer
from analytics_pipeline.config import load_settings

T1 = "11111111-1111-1111-1111-111111111111"


class FakeStore:
    """In-memory record_event with real idempotency semantics."""

    def __init__(self):
        self.processed_keys: set[str] = set()
        self.events = []
        self.fail_next = False

    async def record_event(self, event):
        if self.fail_next:
            self.fail_next = False
            raise ConnectionError("postgres down")
        key = (event.tenant_id, event.idempotency_key)
        if key in self.processed_keys:
            return False
        self.processed_keys.add(key)
        self.events.append(event)
        return True


def _payload(idem="idem-1", name="lead_created"):
    return {
        "specversion": "1.0",
        "id": "env-1",
        "type": "com.opendesk.cac.FunnelEvent",
        "time": "2026-01-15T10:00:00Z",
        "data": {
            "event_id": "ev-1",
            "tenant_id": T1,
            "entity_type": "lead",
            "entity_id": "lead-9",
            "event_name": name,
            "event_ts": "2026-01-15T10:00:00Z",
            "channel": "web",
            "campaign_id": None,
            "lga_id": None,
            "amount_ngn": None,
            "idempotency_key": idem,
        },
    }


def test_consumer_applies_then_dedupes_replay():
    store = FakeStore()
    consumer = CacConsumer(load_settings(), store)
    assert asyncio.run(consumer.process_value(_payload())) is True
    assert consumer.processed == 1 and consumer.replays == 0
    # at-least-once redelivery of the same idempotency_key -> replay
    assert asyncio.run(consumer.process_value(_payload())) is True
    assert consumer.processed == 1 and consumer.replays == 1
    assert len(store.events) == 1  # rolled up exactly once


def test_consumer_drops_poison_pill_but_commits():
    store = FakeStore()
    consumer = CacConsumer(load_settings(), store)
    # garbage that fails contract parsing: commit past it (bronze keeps raw)
    assert asyncio.run(consumer.process_value({"type": "com.opendesk.cac.FunnelEvent"})) is True
    assert consumer.dropped == 1
    assert store.events == []


def test_consumer_store_failure_propagates_without_commit():
    store = FakeStore()
    store.fail_next = True
    consumer = CacConsumer(load_settings(), store)
    try:
        asyncio.run(consumer.process_value(_payload()))
    except ConnectionError:
        pass
    else:
        raise AssertionError("expected store error to propagate (no commit)")
    # retry after recovery applies exactly once
    assert asyncio.run(consumer.process_value(_payload())) is True
    assert len(store.events) == 1
