"""CacStore tests: idempotent record_event (idempotency_key dedupe), rollup
upsert deltas, RLS tenant-tx idiom (set_config app.tenant_id LOCAL) — offline,
fake asyncpg pool (no live Postgres in this environment)."""

from __future__ import annotations

import asyncio
from datetime import UTC, datetime
from decimal import Decimal

from analytics_pipeline.cac_events import FunnelEvent
from analytics_pipeline.cac_store import CacStore, _database_dsn
from analytics_pipeline.config import load_settings

T1 = "11111111-1111-1111-1111-111111111111"
CAMP = "33333333-3333-3333-3333-333333333333"


class FakeConn:
    """Emulates the asyncpg surface CacStore uses + the processed-events
    ON CONFLICT DO NOTHING semantics (authoritative idempotency guard)."""

    def __init__(self):
        self.processed: set[tuple[str, str]] = set()
        self.executed: list[tuple[str, tuple]] = []
        self.set_config_calls: list[str] = []

    async def execute(self, sql, *args):
        if "set_config" in sql:
            self.set_config_calls.append(args[0])
        else:
            self.executed.append((sql, args))

    async def fetchval(self, sql, *args):
        if "cac_processed_events" in sql:
            key = (str(args[0]), args[1])
            if key in self.processed:
                return None
            self.processed.add(key)
            return 1
        raise AssertionError(f"unexpected fetchval: {sql}")

    def transaction(self):
        conn = self

        class _Tx:
            async def __aenter__(self):
                return conn

            async def __aexit__(self, *exc):
                return False

        return _Tx()


class FakePool:
    def __init__(self, conn):
        self._conn = conn

    def acquire(self):
        conn = self._conn

        class _Acq:
            async def __aenter__(self):
                return conn

            async def __aexit__(self, *exc):
                return False

        return _Acq()


def _store_with_fake():
    settings = load_settings()
    store = CacStore(settings)
    conn = FakeConn()
    store._pool = FakePool(conn)
    return store, conn


def _event(name="lead_created", idem="idem-1", channel="whatsapp",
           campaign=CAMP, lga=42, amount=None):
    return FunnelEvent(
        event_id="ev-1",
        tenant_id=T1,
        entity_type="lead",
        entity_id="lead-9",
        event_name=name,
        event_ts=datetime(2026, 1, 15, 10, 0, tzinfo=UTC),
        channel=channel,
        campaign_id=campaign,
        lga_id=lga,
        amount_ngn=Decimal(str(amount)) if amount is not None else None,
        idempotency_key=idem,
    )


def _rollup_deltas(conn, table):
    for sql, args in conn.executed:
        if f"INSERT INTO {table}" in sql:
            return args[3], args[4], args[5]  # leads, conversions, revenue
    raise AssertionError(f"no upsert into {table}")


def test_record_event_applies_rollups_in_tenant_tx():
    store, conn = _store_with_fake()
    applied = asyncio.run(store.record_event(_event()))
    assert applied is True
    # RLS idiom: app.tenant_id set LOCAL before any tenant statement.
    assert conn.set_config_calls == [T1]
    leads, conv, rev = _rollup_deltas(conn, "cac_rollup_channel")
    assert (leads, conv, rev) == (1, 0, Decimal("0"))
    leads, conv, rev = _rollup_deltas(conn, "cac_rollup_lga")
    assert (leads, conv, rev) == (1, 0, Decimal("0"))
    # campaign -> channel first-touch mapping recorded
    assert any("cac_campaign_channel" in sql for sql, _ in conn.executed)


def test_record_event_replay_is_dropped_idempotently():
    store, conn = _store_with_fake()
    assert asyncio.run(store.record_event(_event())) is True
    conn.executed.clear()
    # same tenant + idempotency_key -> replay, no rollup writes at all
    assert asyncio.run(store.record_event(_event())) is False
    assert conn.executed == []
    # same key under another tenant is NOT a replay (key scoped per tenant)
    other = _event()
    object.__setattr__(other, "tenant_id", "22222222-2222-2222-2222-222222222222")
    assert asyncio.run(store.record_event(other)) is True


def test_record_event_conversion_and_revenue_deltas():
    store, conn = _store_with_fake()
    applied = asyncio.run(
        store.record_event(_event(name="converted", idem="idem-2", amount=15000.5))
    )
    assert applied is True
    leads, conv, rev = _rollup_deltas(conn, "cac_rollup_channel")
    assert (leads, conv, rev) == (0, 1, Decimal("15000.5"))


def test_record_event_first_txn_revenue_only():
    store, conn = _store_with_fake()
    assert asyncio.run(
        store.record_event(_event(name="first_txn", idem="idem-3", amount=9000))
    ) is True
    leads, conv, rev = _rollup_deltas(conn, "cac_rollup_channel")
    assert (leads, conv, rev) == (0, 0, Decimal("9000"))


def test_record_event_non_counting_names_still_mark_processed():
    store, conn = _store_with_fake()
    assert asyncio.run(store.record_event(_event(name="qualified", idem="idem-4"))) is True
    # no rollup deltas (but campaign mapping insert may happen)
    assert not any("cac_rollup_" in sql for sql, _ in conn.executed)
    # replay still deduped
    assert asyncio.run(store.record_event(_event(name="qualified", idem="idem-4"))) is False


def test_record_event_without_channel_skips_channel_rollup_but_keeps_lga():
    store, conn = _store_with_fake()
    assert asyncio.run(
        store.record_event(_event(idem="idem-5", channel=None, campaign=None))
    ) is True
    assert not any("cac_rollup_channel" in sql for sql, _ in conn.executed)
    assert any("cac_rollup_lga" in sql for sql, _ in conn.executed)


def test_record_event_without_lga_skips_lga_rollup():
    store, conn = _store_with_fake()
    assert asyncio.run(store.record_event(_event(idem="idem-6", lga=None))) is True
    assert any("cac_rollup_channel" in sql for sql, _ in conn.executed)
    assert not any("cac_rollup_lga" in sql for sql, _ in conn.executed)


def test_dsn_database_override():
    settings = load_settings()
    assert _database_dsn(settings).endswith("/analytics_meta")
    object.__setattr__(settings, "pg_dsn", "postgres://u:p@pg:5432/other")
    assert _database_dsn(settings) == "postgres://u:p@pg:5432/analytics_meta"
