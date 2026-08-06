"""SPEC-W33 §2 A2: cac.events -> iceberg.bronze.cac_events bronze sink.

Covers the fifth bronze topic wiring, closing the cac_analytics.py TODO
producer: mapper contract (CloudEvent envelope + bare payload tolerance,
matching the Spark job's documented bronze input contract), Iceberg schema
registration + auto-create, topic registry wiring (same consumer group and
commit-after-append semantics as the other four bronze topics), and a
pyiceberg (sql-sqlite) round-trip proving rows appended by the sink are
visible on read-back (GA5 unit-level).

Run:
    python -m pytest tests/test_bronze_cac.py
"""

from __future__ import annotations

import asyncio
import os
import sys
from datetime import datetime

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from analytics_pipeline.config import load_settings  # noqa: E402
from analytics_pipeline.consumer import BronzeConsumer, topic_registry  # noqa: E402
from analytics_pipeline.iceberg_tables import (  # noqa: E402
    IcebergSink,
    arrow_schema,
    ensure_bronze,
    iceberg_schema,
)
from analytics_pipeline.mapping import CAC_EVENT_COLUMNS, map_cac_event  # noqa: E402

T1 = "11111111-1111-1111-1111-111111111111"


def _cloudevent(**data_overrides):
    data = {
        "event_id": "ev-1",
        "tenant_id": T1,
        "entity_type": "lead",
        "entity_id": "lead-9",
        "event_name": "lead_created",
        "event_ts": "2026-01-15T10:00:00Z",
        "channel": "web",
        "campaign_id": "camp-1",
        "lga_id": 42,
        "amount_ngn": None,
        "idempotency_key": "idem-1",
    }
    data.update(data_overrides)
    return {
        "specversion": "1.0",
        "id": "env-1",
        "source": "//booking",
        "type": "com.opendesk.cac.FunnelEvent",
        "time": "2026-01-15T10:00:01Z",
        "tenantid": T1,
        "data": data,
    }


# ------------------------------------------------------------------ mapper
def test_mapper_flattens_cloudevent_to_spark_contract():
    row = map_cac_event(_cloudevent())
    # exact column set/order of the bronze contract (cac_analytics.py header)
    assert list(row.keys()) == list(CAC_EVENT_COLUMNS) == [
        "event_id", "tenant_id", "entity_type", "entity_id", "event_name",
        "event_ts", "channel", "campaign_id", "lga_id", "amount_ngn",
        "idempotency_key",
    ]
    # envelope id wins over data.event_id (same precedence as the other
    # bronze mappers — map_booking_event/map_payment_event convention)
    assert row["event_id"] == "env-1"
    assert row["tenant_id"] == T1
    assert row["entity_type"] == "lead"
    assert row["entity_id"] == "lead-9"
    assert row["event_name"] == "lead_created"
    assert row["event_ts"] == datetime(2026, 1, 15, 10, 0, 0)  # naive UTC
    assert row["channel"] == "web"
    assert row["campaign_id"] == "camp-1"
    assert row["lga_id"] == 42 and isinstance(row["lga_id"], int)
    assert row["amount_ngn"] is None
    assert row["idempotency_key"] == "idem-1"


def test_mapper_tolerates_bare_payload_camel_case_and_ts_fallback():
    row = map_cac_event({
        "tenantId": T1,
        "entityType": "customer",
        "entityId": "cust-1",
        "eventName": "converted",
        "eventTs": 1768476000,  # epoch seconds
        "channel": "USSD",
        "amountNgn": "12500.50",
        "lgaId": "7",
        "idempotencyKey": "idem-2",
    })
    assert row["tenant_id"] == T1
    assert row["entity_type"] == "customer"
    assert row["event_name"] == "converted"
    assert row["event_ts"] == datetime(2026, 1, 15, 11, 20)  # naive UTC
    assert row["lga_id"] == 7
    assert row["amount_ngn"] == 12500.50 and isinstance(row["amount_ngn"], float)
    # envelope time fallback when event_ts is absent
    row2 = map_cac_event({"specversion": "1.0", "time": "2026-02-01T00:00:00Z",
                          "type": "com.opendesk.cac.FunnelEvent",
                          "data": {"tenant_id": T1, "event_name": "lost"}})
    assert row2["event_ts"] == datetime(2026, 2, 1, 0, 0, 0)
    assert row2["channel"] is None and row2["idempotency_key"] is None


# ------------------------------------------------------------------ schema
def test_cac_events_registered_in_bronze_tables():
    from analytics_pipeline.iceberg_tables import TABLE_COLUMNS

    assert TABLE_COLUMNS["cac_events"] == CAC_EVENT_COLUMNS
    schema = iceberg_schema("cac_events")
    names = [f.name for f in schema.fields]
    assert names == list(CAC_EVENT_COLUMNS)
    # field ids assigned sequentially from 1 (stable, like the other tables)
    assert [f.field_id for f in schema.fields] == list(
        range(1, len(CAC_EVENT_COLUMNS) + 1)
    )
    arrow = arrow_schema("cac_events")
    assert arrow.names == list(CAC_EVENT_COLUMNS)
    # a mapped row must be appendable under the arrow schema
    import pyarrow as pa

    table = pa.Table.from_pylist([map_cac_event(_cloudevent())], schema=arrow)
    assert table.num_rows == 1


# ------------------------------------------------------------ registry wiring
def test_topic_registry_maps_cac_events_topic():
    settings = load_settings()
    registry = topic_registry(settings)
    table, mapper = registry["cac.events"]
    assert table == "cac_events"
    assert mapper is map_cac_event
    # the four pre-existing bronze topics are untouched
    assert registry["opendesk.booking.events"][0] == "booking_events"
    assert registry["opendesk.payments.events"][0][0:7] == "payment"
    assert len(registry) == 5


class _FakeSink:
    def __init__(self, fail=False):
        self.appended: list[tuple[str, list[dict]]] = []
        self.fail = fail

    def append(self, table, rows):
        if self.fail:
            raise ConnectionError("iceberg-rest down")
        rows = list(rows)
        self.appended.append((table, rows))
        return len(rows)


def _bare(**data_overrides):
    """Bare (non-enveloped) FunnelEvent data payload."""
    data = {
        "event_id": "ev-1",
        "tenant_id": T1,
        "entity_type": "lead",
        "entity_id": "lead-9",
        "event_name": "lead_created",
        "event_ts": "2026-01-15T10:00:00Z",
        "idempotency_key": "idem-1",
    }
    data.update(data_overrides)
    return data


def test_bronze_consumer_flushes_cac_batch_to_cac_events_table():
    sink = _FakeSink()
    consumer = BronzeConsumer(load_settings(), sink)
    consumer._buffers["cac.events"].append(_bare())
    consumer._buffers["cac.events"].append(_bare(event_id="ev-2"))
    asyncio.run(consumer._flush("cac.events"))
    assert len(sink.appended) == 1
    table, rows = sink.appended[0]
    assert table == "cac_events"
    assert [r["event_id"] for r in rows] == ["ev-1", "ev-2"]
    assert consumer._buffers["cac.events"] == []
    assert consumer.last_error is None


def test_bronze_consumer_keeps_batch_on_sink_failure():
    sink = _FakeSink(fail=True)
    consumer = BronzeConsumer(load_settings(), sink)
    consumer._buffers["cac.events"].append(_bare())
    asyncio.run(consumer._flush("cac.events"))  # no offset commit, batch kept
    assert len(consumer._buffers["cac.events"]) == 1
    assert consumer.last_error is not None
    # retry after recovery appends exactly the same row (at-least-once)
    sink.fail = False
    asyncio.run(consumer._flush("cac.events"))
    assert len(sink.appended) == 1
    assert sink.appended[0][1][0]["event_id"] == "ev-1"


# ------------------------------------------- pyiceberg round-trip (GA5 unit)
def test_ensure_bronze_and_sink_round_trip(tmp_path):
    """Auto-create bronze.cac_events like the other 4 tables, append a
    mapped row through IcebergSink, read it back (pyiceberg sql catalog)."""
    from pyiceberg.catalog import load_catalog

    catalog = load_catalog(
        "w33a2-test",
        **{
            "type": "sql",
            "uri": f"sqlite:///{tmp_path}/catalog.db",
            "warehouse": f"file://{tmp_path}/warehouse",
        },
    )
    ensure_bronze(catalog)
    identifiers = [t[1] for t in catalog.list_tables("bronze")]
    for expected in (
        "booking_events", "payment_events", "transcripts", "usage_events",
        "cac_events",
    ):
        assert expected in identifiers
    # idempotent re-run (existing tables -> schema-evolution no-op)
    ensure_bronze(catalog)

    sink = IcebergSink(catalog)
    written = sink.append("cac_events", [map_cac_event(_bare(lga_id=42))])
    assert written == 1
    table = catalog.load_table("bronze.cac_events")
    rows = table.scan().to_arrow().to_pylist()
    assert len(rows) == 1
    assert rows[0]["event_id"] == "ev-1"
    assert rows[0]["tenant_id"] == T1
    assert rows[0]["event_name"] == "lead_created"
    assert rows[0]["lga_id"] == 42
