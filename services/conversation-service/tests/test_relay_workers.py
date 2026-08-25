"""SPEC-W43 Y-08: conversation_outbox relay — unit tests with fakes.

Covers: unsent-row publish + mark sent, publish failure -> backoff mark +
row stays unsent, per-row isolation (one failure does not stop others),
backoff schedule monotonicity/cap.
"""

from __future__ import annotations

import sys
import uuid

import pytest

sys.path.insert(0, ".")

from app.config import Config  # noqa: E402
from app.outbox import OutboxRelay, backoff_seconds  # noqa: E402

pytestmark = pytest.mark.asyncio

TENANT = uuid.uuid4()


class _FakeDB:
    def __init__(self, rows, fail_mark=False):
        self.rows = rows  # list of dicts (unsent)
        self.marked_sent: list[tuple] = []
        self.marked_failed: list[tuple] = []
        self.fail_mark = fail_mark

    async def outbox_unsent(self, limit=100):
        return self.rows[:limit]

    async def outbox_mark_sent(self, outbox_id, tenant_id):
        if self.fail_mark:
            raise RuntimeError("db down")
        self.marked_sent.append((outbox_id, tenant_id))
        self.rows = [r for r in self.rows if r["id"] != outbox_id]

    async def outbox_mark_failed(self, outbox_id, tenant_id, delay):
        self.marked_failed.append((outbox_id, tenant_id, delay))


class _FakeDapr:
    def __init__(self, fail_topics=()):
        self.published = []
        self.fail_topics = set(fail_topics)

    async def publish_event(self, topic, event):
        if topic in self.fail_topics:
            raise RuntimeError("broker down")
        self.published.append((topic, event))


def _row(topic="opendesk.conversation.transcripts", attempts=0):
    return {
        "id": uuid.uuid4(),
        "tenant_id": TENANT,
        "topic": topic,
        "payload": {"specversion": "1.0", "data": {}},
        "attempts": attempts,
    }


async def test_relay_publishes_unsent_and_marks_sent():
    row = _row()
    db = _FakeDB([row])
    dapr = _FakeDapr()
    relay = OutboxRelay(Config(), db, dapr)
    assert await relay.run_once() == 1
    assert dapr.published == [(row["topic"], row["payload"])]
    assert db.marked_sent == [(row["id"], TENANT)]
    assert db.rows == []


async def test_relay_publish_failure_marks_backoff_and_keeps_row():
    row = _row(topic="dead-topic", attempts=2)
    db = _FakeDB([row])
    dapr = _FakeDapr(fail_topics={"dead-topic"})
    relay = OutboxRelay(Config(), db, dapr)
    assert await relay.run_once() == 0
    assert dapr.published == []
    assert db.marked_sent == []
    # backoff mark recorded with the exponential delay for attempt #3
    assert db.marked_failed == [(row["id"], TENANT, backoff_seconds(2))]
    assert db.rows == [row]  # row stays unsent — never silent


async def test_relay_row_failure_does_not_stop_others():
    bad = _row(topic="dead-topic")
    good = _row()
    db = _FakeDB([bad, good])
    dapr = _FakeDapr(fail_topics={"dead-topic"})
    relay = OutboxRelay(Config(), db, dapr)
    assert await relay.run_once() == 1
    assert db.marked_sent == [(good["id"], TENANT)]
    assert db.marked_failed == [(bad["id"], TENANT, backoff_seconds(0))]


async def test_relay_empty_outbox_noops():
    relay = OutboxRelay(Config(), _FakeDB([]), _FakeDapr())
    assert await relay.run_once() == 0


def test_backoff_schedule_exponential_and_capped():
    assert backoff_seconds(0) == 2.0
    assert backoff_seconds(1) == 4.0
    assert backoff_seconds(3) == 16.0
    assert backoff_seconds(100) == 300.0  # capped
