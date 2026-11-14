"""SPEC-W64 tests (G-B): ReceiptIngested + ReceiptReconResolved routing.

G-B (consumer ordering / safety): com.opendesk.booking.ReceiptIngested and
com.opendesk.booking.ReceiptReconResolved ride opendesk.booking.events but
are NOT booking events. They route to bronze.receipt_ingest and
bronze.recon_resolutions respectively and MUST NEVER fall through to
bronze.booking_events (the dbt stg_booking_events.event_type
accepted_values test goes red otherwise — the W62 G2 bug class).

Also covered: full-payload row mapping (SPEC §6 columns, in order), the
PII posture (sender_phone NEVER lands, even when hostile payloads carry
it), booking_id from data only (never the tenant-slug subject — the G2
lesson), bare-payload tolerance, occurred_at fallback to resolved_at, and
event_id in-batch dedupe for the two new tables.
"""

from __future__ import annotations

import asyncio
import os
import sys
from datetime import datetime

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from analytics_pipeline.mapping import (  # noqa: E402
    RECEIPT_INGEST_COLUMNS,
    RECON_RESOLUTION_COLUMNS,
    is_receipt_ingested,
    is_receipt_recon_finding,
    is_receipt_recon_resolved,
    map_booking_event,
    map_receipt_ingest,
    map_recon_resolution,
    route_booking_event,
)

T1 = "11111111-1111-1111-1111-111111111111"


def _ingest_event(**data_overrides):
    data = {
        "tenant_id": T1,
        "channel": "whatsapp",
        "document_id": "doc-1",
        "media_inbox_id": "mi-1",
        "sha256_prefix": "ab12cd34",
        "mime": "image/jpeg",
        "bytes": 123456,
        "state": "received",
        "booking_id": "bk-9",
    }
    data.update(data_overrides)
    return {
        "specversion": "1.0",
        "id": "evt-ing-1",
        "source": "booking-service",
        "type": "com.opendesk.booking.ReceiptIngested",
        "subject": "acme-co",  # TENANT SLUG, not a booking id (G2 lesson)
        "time": "2026-02-05T08:30:00Z",
        "tenantid": T1,
        "data": data,
    }


def _resolved_event(**data_overrides):
    data = {
        "finding_id": "find-1",
        "tenant_id": T1,
        "run_id": "run-1",
        "kind": "amount_mismatch",
        "resolved_at": "2026-02-05T09:15:00Z",
        "resolved_by": "ops-user-7",
    }
    data.update(data_overrides)
    return {
        "specversion": "1.0",
        "id": "evt-res-1",
        "source": "booking-service",
        "type": "com.opendesk.booking.ReceiptReconResolved",
        "subject": "acme-co",  # TENANT SLUG
        "time": "2026-02-05T09:15:01Z",
        "tenantid": T1,
        "data": data,
    }


def _booking_event():
    return {
        "specversion": "1.0",
        "id": "evt-bk-1",
        "source": "booking-service",
        "type": "com.opendesk.booking.BookingCreated",
        "subject": "bk-1",
        "time": "2026-02-05T07:00:00Z",
        "tenantid": T1,
        "data": {"bookingId": "bk-1", "status": "CONFIRMED", "source": "web"},
    }


# --- type predicates ---------------------------------------------------------


def test_type_predicates():
    assert is_receipt_ingested(_ingest_event()) is True
    assert is_receipt_recon_resolved(_resolved_event()) is True
    # Cross-talk: the predicates are exact short-name matches.
    assert is_receipt_ingested(_resolved_event()) is False
    assert is_receipt_recon_resolved(_ingest_event()) is False
    assert is_receipt_recon_finding(_ingest_event()) is False
    assert is_receipt_recon_finding(_resolved_event()) is False
    assert is_receipt_ingested(_booking_event()) is False
    assert is_receipt_recon_resolved(_booking_event()) is False
    # Bare payloads via data.event_type, case-insensitive short name.
    assert is_receipt_ingested({"event_type": "receiptingested"}) is True
    assert (
        is_receipt_recon_resolved(
            {"event_type": "com.opendesk.booking.ReceiptReconResolved"}
        )
        is True
    )


# --- G-B: NEVER bronze.booking_events ----------------------------------------


def test_gb_new_types_never_map_as_booking_events():
    assert map_booking_event(_ingest_event()) is None
    assert map_booking_event(_resolved_event()) is None


def test_gb_router_arms_route_to_own_tables():
    table, row = route_booking_event(_ingest_event())
    assert table == "receipt_ingest"
    table, row = route_booking_event(_resolved_event())
    assert table == "recon_resolutions"


def test_gb_consumer_flush_keeps_booking_events_clean():
    """A mixed booking.events batch: the new types land in their own tables;
    bronze.booking_events gets ONLY the real BookingCreated (G-B proven at
    the consumer flush layer, not just the pure router)."""
    consumer_mod = pytest.importorskip(
        "analytics_pipeline.consumer", reason="aiokafka/structlog not installed"
    )
    from analytics_pipeline.config import load_settings

    class _FakeSink:
        def __init__(self):
            self.appends = []

        def append(self, table, rows):
            rows = list(rows)
            self.appends.append((table, rows))
            return len(rows)

    sink = _FakeSink()
    consumer = consumer_mod.BronzeConsumer(load_settings(), sink)
    topic = "opendesk.booking.events"
    consumer._buffers[topic] = [_booking_event(), _ingest_event(), _resolved_event()]
    asyncio.run(consumer._flush(topic))
    by_table = {t: rows for t, rows in sink.appends}
    assert set(by_table) == {"booking_events", "receipt_ingest", "recon_resolutions"}
    assert [r["event_type"] for r in by_table["booking_events"]] == ["BookingCreated"]
    assert by_table["receipt_ingest"][0]["channel"] == "whatsapp"
    assert by_table["recon_resolutions"][0]["finding_id"] == "find-1"
    assert consumer.last_error is None


# --- ReceiptIngested row mapping ----------------------------------------------


def test_ingest_maps_full_row():
    table, row = route_booking_event(_ingest_event())
    assert table == "receipt_ingest"
    assert list(row.keys()) == list(RECEIPT_INGEST_COLUMNS)
    assert row["event_id"] == "evt-ing-1"
    assert row["tenant_id"] == T1
    assert row["channel"] == "whatsapp"
    assert row["document_id"] == "doc-1"
    assert row["media_inbox_id"] == "mi-1"
    assert row["booking_id"] == "bk-9"  # from data, NOT the slug subject
    assert row["mime"] == "image/jpeg"
    assert row["bytes"] == 123456
    assert row["sha256_prefix"] == "ab12cd34"
    assert row["state"] == "received"
    assert row["occurred_at"] == datetime(2026, 2, 5, 8, 30, 0)


def test_ingest_fieldcapture_shape():
    """fieldcapture AttachReceipt payload: channel field_pwa, capture_client_id,
    no media_inbox/mime/bytes/state keys — all nullable."""
    table, row = route_booking_event(
        _ingest_event(
            channel="field_pwa",
            capture_client_id="cap-1",
            media_inbox_id=None,
            sha256_prefix=None,
            mime=None,
            bytes=None,
            state=None,
        )
    )
    assert table == "receipt_ingest"
    assert row["channel"] == "field_pwa"
    assert row["capture_client_id"] == "cap-1"
    assert row["media_inbox_id"] is None
    assert row["mime"] is None
    assert row["bytes"] is None
    assert row["state"] is None


def test_ingest_never_emits_sender_phone():
    """PII posture: even if a hostile/buggy producer payload carried
    sender_phone / sender_phone_digits, the mapper reads ONLY whitelisted
    keys — the phone never lands in bronze."""
    msg = _ingest_event(
        sender_phone="+2348012345678",
        sender_phone_digits="2348012345678",
    )
    table, row = route_booking_event(msg)
    assert table == "receipt_ingest"
    assert set(row.keys()) == set(RECEIPT_INGEST_COLUMNS)
    haystack = {str(v) for v in row.values()}
    assert "+2348012345678" not in haystack
    assert "2348012345678" not in haystack


def test_ingest_booking_id_never_subject_slug():
    msg = _ingest_event()
    del msg["data"]["booking_id"]
    _, row = route_booking_event(msg)
    assert row["booking_id"] is None  # NULL — never "acme-co" (the G2 bug)


def test_ingest_bare_payload_tolerated():
    row = map_receipt_ingest(
        {
            "eventType": "com.opendesk.booking.ReceiptIngested",
            "tenantId": T1,
            "channel": "whatsapp",
            "documentId": "doc-2",
            "mediaInboxId": "mi-2",
            "sha256Prefix": "ff00",
            "mime": "application/pdf",
            "bytes": "4321",
            "state": "received",
        }
    )
    assert row["document_id"] == "doc-2"
    assert row["media_inbox_id"] == "mi-2"
    assert row["sha256_prefix"] == "ff00"
    assert row["bytes"] == 4321


# --- ReceiptReconResolved row mapping ------------------------------------------


def test_resolved_maps_full_row():
    table, row = route_booking_event(_resolved_event())
    assert table == "recon_resolutions"
    assert list(row.keys()) == list(RECON_RESOLUTION_COLUMNS)
    assert row["event_id"] == "evt-res-1"
    assert row["tenant_id"] == T1
    assert row["finding_id"] == "find-1"
    assert row["run_id"] == "run-1"
    assert row["kind"] == "amount_mismatch"
    assert row["resolved_at"] == datetime(2026, 2, 5, 9, 15, 0)
    assert row["resolved_by"] == "ops-user-7"
    assert row["occurred_at"] == datetime(2026, 2, 5, 9, 15, 1)


def test_resolved_bare_payload_falls_back_to_resolved_at():
    row = map_recon_resolution(
        {
            "eventType": "com.opendesk.booking.ReceiptReconResolved",
            "findingId": "find-2",
            "tenantId": T1,
            "runId": "run-2",
            "kind": "payment_without_receipt",
            "resolvedAt": "2026-02-05T10:00:00Z",
            "resolvedBy": "ops-user-8",
        }
    )
    assert row["event_id"] == "find-2"  # finding_id is the event_id fallback
    assert row["kind"] == "payment_without_receipt"
    assert row["resolved_at"] == datetime(2026, 2, 5, 10, 0, 0)
    # occurred_at falls back to resolved_at so the row is never timeless.
    assert row["occurred_at"] == datetime(2026, 2, 5, 10, 0, 0)


# --- iceberg schema contract + dedupe ------------------------------------------


def test_iceberg_schemas_cover_new_tables():
    """The auto-create idiom picks up both new tables (ensure_bronze iterates
    TABLE_COLUMNS) with event_id first (dedupe key) and sequential ids."""
    it = pytest.importorskip(
        "analytics_pipeline.iceberg_tables", reason="pyiceberg/pyarrow not installed"
    )
    from analytics_pipeline.mapping import (
        RECEIPT_INGEST_COLUMNS as INGEST,
        RECON_RESOLUTION_COLUMNS as RES,
    )

    for table, cols in (("receipt_ingest", INGEST), ("recon_resolutions", RES)):
        assert it.TABLE_COLUMNS[table] == cols
        schema = it.iceberg_schema(table)
        assert [f.name for f in schema.fields] == list(cols)
        assert [f.field_id for f in schema.fields] == list(range(1, len(cols) + 1))
        assert it.arrow_schema(table).names == list(cols)
        assert cols[0] == "event_id"


def test_sink_dedupes_new_tables_on_event_id():
    it = pytest.importorskip(
        "analytics_pipeline.iceberg_tables", reason="pyiceberg/pyarrow not installed"
    )
    rows = [
        {"event_id": "e1", "tenant_id": T1},
        {"event_id": "e1", "tenant_id": T1},  # redelivered dup -> dropped
        {"event_id": "e2", "tenant_id": T1},
    ]
    for table in ("receipt_ingest", "recon_resolutions"):
        out = it._dedupe_on_event_id(table, rows)
        assert [r["event_id"] for r in out] == ["e1", "e2"], table
