"""SPEC-W62 tests: bronze receipt ingest (G1) + ReceiptReconFinding routing (G2).

G1 (PII exclusion): kyc_* DocumentExtracted => zero bronze rows; only
kind='receipt' lands; extracted_fields whitelist is EXACTLY
{amount_minor, currency, receipt_date} — no vendor/note/items/free text.
G2 (routing fix): ReceiptReconFinding-typed messages on
opendesk.booking.events route to bronze.receipt_recon_findings with the
full payload; map_booking_event SKIPS them (never bronze.booking_events,
no tenant-slug-in-booking_id stuffing); normal booking events unchanged.

Pure mapper/router tests need no Kafka/Iceberg; the pyiceberg schema test
and the consumer flush test run when those deps are importable (they are
in the dev install), otherwise they skip honestly.
"""

from __future__ import annotations

import asyncio
import os
import sys
from datetime import datetime

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from analytics_pipeline.mapping import (  # noqa: E402
    RECEIPT_DOCUMENT_COLUMNS,
    RECEIPT_RECON_FINDING_COLUMNS,
    is_receipt_recon_finding,
    map_booking_event,
    map_ocr_document,
    map_receipt_recon_finding,
    route_booking_event,
)

T1 = "11111111-1111-1111-1111-111111111111"


def _receipt_event(extracted_fields=None, kind="receipt", **data_overrides):
    data = {
        "document_id": "doc-1",
        "tenant_id": T1,
        "kind": kind,
        "status": "extracted",
        "page_count": 2,
        "sha256": "a" * 64,
        "phash": "f" * 16,
        "model_version": "ocr-v3",
        "storage_bucket": "ocr",
        "storage_key": "t/doc-1.pdf",
        "extracted_fields": extracted_fields if extracted_fields is not None else {},
    }
    data.update(data_overrides)
    return {
        "specversion": "1.0",
        "id": "evt-ocr-1",
        "source": "ocr-service",
        "type": "com.opendesk.ocr.DocumentExtracted",
        "subject": "doc-1",
        "time": "2026-02-01T10:00:00Z",
        "tenantid": T1,
        "data": data,
    }


def _finding_event(**data_overrides):
    data = {
        "finding_id": "find-1",
        "tenant_id": T1,
        "kind": "amount_mismatch",
        "confidence": "exact",
        "capture_client_id": "cap-1",
        "booking_id": "bk-9",
        "attempt_tx_ref": "tx-1",
        "expected_amount_minor": 5000,
        "captured_amount_minor": 4500,
        "receipt_amount_major": 45.0,
        "run_id": "run-1",
        "detail": {"receipt_currency": "NGN"},
    }
    data.update(data_overrides)
    return {
        "specversion": "1.0",
        "id": "evt-find-1",
        "source": "booking-service",
        "type": "com.opendesk.booking.ReceiptReconFinding",
        "subject": "acme-co",  # TENANT SLUG, not a booking id (the G2 bug)
        "time": "2026-02-01T11:00:00Z",
        "tenantid": T1,
        "data": data,
    }


# --- G1: receipt mapper ------------------------------------------------------


def test_receipt_maps_full_row():
    row = map_ocr_document(
        _receipt_event(
            {
                "amount_minor": 123400,
                "currency": "ngn",
                "receipt_date": "2026-01-31",
            }
        )
    )
    assert row is not None
    assert list(row.keys()) == list(RECEIPT_DOCUMENT_COLUMNS)
    assert row["event_id"] == "evt-ocr-1"
    assert row["tenant_id"] == T1
    assert row["document_id"] == "doc-1"
    assert row["kind"] == "receipt"
    assert row["status"] == "extracted"
    assert row["page_count"] == 2
    assert row["sha256"] == "a" * 64
    assert row["phash"] == "f" * 16
    assert row["model_version"] == "ocr-v3"
    assert row["amount_minor"] == 123400
    assert row["currency"] == "NGN"  # normalised ISO-3 upper-case
    assert row["receipt_date"] == "2026-01-31"
    assert row["occurred_at"] == datetime(2026, 2, 1, 10, 0, 0)


def test_g1_kyc_kinds_dropped_entirely():
    # W58 R1 already emits extracted_fields={} for kyc_*; the mapper must
    # drop them EVEN IF fields were present (belt-and-braces): zero rows.
    for kind in ("kyc_passport", "kyc_id_card", "kyc_selfie"):
        assert map_ocr_document(_receipt_event(kind=kind)) is None
        assert (
            map_ocr_document(
                _receipt_event(
                    kind=kind,
                    extracted_fields={
                        "full_name": "Ada Lovelace",
                        "id_number": "A1234567",
                        "amount_minor": 100,
                    },
                )
            )
            is None
        ), "kyc_* kinds must NEVER land in bronze, even with fields present"


def test_g1_non_receipt_kinds_dropped():
    for kind in ("document", "knowledge_scan", ""):
        assert map_ocr_document(_receipt_event(kind=kind)) is None
    # No kind at all -> dropped (no table for unidentified documents).
    msg = _receipt_event()
    del msg["data"]["kind"]
    assert map_ocr_document(msg) is None


def test_g1_extracted_fields_whitelist_exact():
    row = map_ocr_document(
        _receipt_event(
            {
                "amount_minor": 5000,
                "currency": "USD",
                "receipt_date": "2026-01-15",
                # everything below is PII/free text and must NEVER land:
                "vendor": "Mama Nkechi Provisions",
                "note": "paid by Chidi, +2348012345678",
                "items": ["rice", "beans"],
                "raw_text": "THANK YOU FOR SHOPPING...",
                "confidence_score": 0.98,
            }
        )
    )
    assert row is not None
    assert set(row.keys()) == set(RECEIPT_DOCUMENT_COLUMNS)
    assert row["amount_minor"] == 5000
    assert row["currency"] == "USD"
    assert row["receipt_date"] == "2026-01-15"
    # No free text / non-whitelisted value appears anywhere in the row.
    haystack = {str(v) for v in row.values()}
    for leaked in ("Mama Nkechi Provisions", "paid by Chidi, +2348012345678",
                   "THANK YOU FOR SHOPPING...", "0.98"):
        assert leaked not in haystack
    assert not any(isinstance(v, (list, dict)) for v in row.values())


def test_g1_whitelist_fields_validated():
    row = map_ocr_document(
        _receipt_event(
            {
                "amount_minor": "7500",       # string int coerced
                "currency": "US",             # not ISO-3 -> NULL
                "receipt_date": "yesterday",  # not YYYY-MM-DD -> NULL
            }
        )
    )
    assert row["amount_minor"] == 7500
    assert row["currency"] is None
    assert row["receipt_date"] is None
    row2 = map_ocr_document(
        _receipt_event({"currency": "usd!", "receipt_date": "2026-13-40"})
    )
    assert row2["currency"] is None
    assert row2["receipt_date"] is None


def test_receipt_missing_extracted_fields_tolerated():
    msg = _receipt_event()
    del msg["data"]["extracted_fields"]
    row = map_ocr_document(msg)
    assert row is not None
    assert row["amount_minor"] is None
    assert row["currency"] is None
    assert row["receipt_date"] is None


def test_receipt_bare_payload_tolerated():
    row = map_ocr_document(
        {
            "documentId": "doc-2",
            "tenantId": T1,
            "kind": "receipt",
            "status": "extracted",
            "pageCount": 1,
            "sha256": "b" * 64,
            "extractedFields": {"amountMinor": 100, "currency": "gbp"},
        }
    )
    assert row is not None
    assert row["document_id"] == "doc-2"
    assert row["amount_minor"] == 100
    assert row["currency"] == "GBP"


# --- G2: ReceiptReconFinding routing ----------------------------------------


def test_g2_is_receipt_recon_finding():
    assert is_receipt_recon_finding(_finding_event()) is True
    assert is_receipt_recon_finding(_receipt_event()) is False
    assert is_receipt_recon_finding({"type": "ReceiptReconFinding", "data": {}}) is False
    # ^ no specversion => bare payload; bare payloads need data.event_type.
    assert (
        is_receipt_recon_finding({"event_type": "com.opendesk.booking.ReceiptReconFinding"})
        is True
    )


def test_g2_map_booking_event_skips_findings():
    # The finding has subject='acme-co' (tenant slug) and data.booking_id —
    # it must produce NO booking_events row at all.
    assert map_booking_event(_finding_event()) is None
    # Even without data.booking_id: previously the tenant-slug subject was
    # stuffed into booking_id; now still skipped.
    msg = _finding_event()
    del msg["data"]["booking_id"]
    assert map_booking_event(msg) is None


def test_g2_finding_maps_full_payload():
    table, row = route_booking_event(_finding_event())
    assert table == "receipt_recon_findings"
    assert list(row.keys()) == list(RECEIPT_RECON_FINDING_COLUMNS)
    assert row["event_id"] == "evt-find-1"
    assert row["tenant_id"] == T1
    assert row["run_id"] == "run-1"
    assert row["finding_kind"] == "amount_mismatch"
    assert row["confidence"] == "exact"
    assert row["capture_client_id"] == "cap-1"
    assert row["booking_id"] == "bk-9"  # from data, NOT the slug subject
    assert row["attempt_tx_ref"] == "tx-1"
    assert row["expected_amount_minor"] == 5000
    assert row["captured_amount_minor"] == 4500
    assert row["receipt_amount_major"] == 45.0
    assert row["receipt_currency"] == "NGN"  # via detail fallback
    assert row["occurred_at"] == datetime(2026, 2, 1, 11, 0, 0)


def test_g2_finding_nullable_fields_absent():
    # payment_without_receipt: no capture/receipt side at all.
    msg = _finding_event(
        kind="payment_without_receipt",
        confidence="none",
    )
    for key in ("capture_client_id", "booking_id", "attempt_tx_ref",
                "captured_amount_minor", "receipt_amount_major", "run_id",
                "detail"):
        msg["data"].pop(key, None)
    table, row = route_booking_event(msg)
    assert table == "receipt_recon_findings"
    assert row["finding_kind"] == "payment_without_receipt"
    assert row["confidence"] == "none"
    assert row["expected_amount_minor"] == 5000
    assert row["booking_id"] is None  # NULL — never the tenant-slug subject
    assert row["capture_client_id"] is None
    assert row["run_id"] is None
    assert row["receipt_currency"] is None


def test_g2_map_receipt_recon_finding_direct():
    row = map_receipt_recon_finding(_finding_event())
    assert row["finding_kind"] == "amount_mismatch"


def test_g2_regression_normal_booking_event_unchanged():
    msg = {
        "specversion": "1.0",
        "id": "evt-bk-1",
        "source": "booking-service",
        "type": "com.opendesk.booking.BookingCreated",
        "subject": "bk-9",
        "time": "2026-02-01T09:00:00Z",
        "tenantid": T1,
        "data": {
            "bookingId": "bk-9",
            "status": "CONFIRMED",
            "source": "voice",
            "priceCents": 5000,
            "currency": "usd",
            "startsAt": "2026-02-03T10:00:00Z",
        },
    }
    table, row = route_booking_event(msg)
    assert table == "booking_events"
    assert row["event_id"] == "evt-bk-1"
    assert row["event_type"] == "BookingCreated"
    assert row["booking_id"] == "bk-9"
    assert row["price_cents"] == 5000
    assert row["occurred_at"] == datetime(2026, 2, 1, 9, 0, 0)
    # Legacy subject fallback for REAL booking events is preserved.
    msg2 = dict(msg, data={"status": "CONFIRMED"})
    table2, row2 = route_booking_event(msg2)
    assert table2 == "booking_events"
    assert row2["booking_id"] == "bk-9"


# --- registry / consumer wiring ----------------------------------------------


def test_topic_registry_includes_ocr_and_booking_default():
    consumer_mod = pytest.importorskip(
        "analytics_pipeline.consumer", reason="aiokafka/structlog not installed"
    )
    from analytics_pipeline.config import load_settings

    registry = consumer_mod.topic_registry(load_settings())
    assert registry["opendesk.ocr.events"][0] == "receipt_documents"
    assert registry["opendesk.booking.events"][0] == "booking_events"
    # ocr router drops kyc, maps receipts
    router = registry["opendesk.ocr.events"][1]
    assert router(_receipt_event(kind="kyc_passport")) is None
    assert router(_receipt_event()) == (
        "receipt_documents",
        map_ocr_document(_receipt_event()),
    )


class _FakeSink:
    def __init__(self):
        self.appends: list[tuple[str, list[dict]]] = []

    def append(self, table, rows):
        rows = list(rows)
        self.appends.append((table, rows))
        return len(rows)


def test_consumer_flush_routes_booking_topic_to_two_tables():
    consumer_mod = pytest.importorskip(
        "analytics_pipeline.consumer", reason="aiokafka/structlog not installed"
    )
    from analytics_pipeline.config import load_settings

    consumer = consumer_mod.BronzeConsumer(load_settings(), _FakeSink())
    topic = "opendesk.booking.events"
    consumer._buffers[topic] = [
        _finding_event(),
        {
            "specversion": "1.0",
            "id": "evt-bk-2",
            "type": "com.opendesk.booking.BookingCreated",
            "subject": "bk-1",
            "time": "2026-02-01T09:00:00Z",
            "tenantid": T1,
            "data": {"bookingId": "bk-1", "status": "CONFIRMED"},
        },
    ]
    asyncio.run(consumer._flush(topic))
    by_table = {t: rows for t, rows in consumer._sink.appends}
    assert set(by_table) == {"booking_events", "receipt_recon_findings"}
    assert len(by_table["booking_events"]) == 1
    assert by_table["booking_events"][0]["event_type"] == "BookingCreated"
    assert len(by_table["receipt_recon_findings"]) == 1
    assert by_table["receipt_recon_findings"][0]["finding_kind"] == "amount_mismatch"
    assert consumer.last_error is None


def test_consumer_flush_drops_kyc_documents():
    consumer_mod = pytest.importorskip(
        "analytics_pipeline.consumer", reason="aiokafka/structlog not installed"
    )
    from analytics_pipeline.config import load_settings

    sink = _FakeSink()
    consumer = consumer_mod.BronzeConsumer(load_settings(), sink)
    topic = "opendesk.ocr.events"
    consumer._buffers[topic] = [
        _receipt_event(kind="kyc_passport"),
        _receipt_event(kind="document"),
        _receipt_event(),
    ]
    asyncio.run(consumer._flush(topic))
    assert sink.appends == [("receipt_documents", [map_ocr_document(_receipt_event())])]


# --- iceberg schema contract ---------------------------------------------------


def test_iceberg_schemas_match_spec_columns():
    it = pytest.importorskip(
        "analytics_pipeline.iceberg_tables", reason="pyiceberg/pyarrow not installed"
    )
    from pyiceberg.types import DoubleType, IntegerType, LongType, StringType, TimestampType

    spec = {
        "receipt_documents": [
            ("event_id", StringType), ("tenant_id", StringType),
            ("document_id", StringType), ("kind", StringType),
            ("status", StringType), ("page_count", IntegerType),
            ("sha256", StringType), ("phash", StringType),
            ("model_version", StringType), ("amount_minor", LongType),
            ("currency", StringType), ("receipt_date", StringType),
            ("occurred_at", TimestampType),
        ],
        "receipt_recon_findings": [
            ("event_id", StringType), ("tenant_id", StringType),
            ("run_id", StringType), ("finding_kind", StringType),
            ("confidence", StringType), ("capture_client_id", StringType),
            ("booking_id", StringType), ("attempt_tx_ref", StringType),
            ("expected_amount_minor", LongType),
            ("captured_amount_minor", LongType),
            ("receipt_amount_major", DoubleType),
            ("receipt_currency", StringType), ("occurred_at", TimestampType),
            # SPEC-W64 appended column (join key to recon_resolutions).
            ("finding_id", StringType),
        ],
        # SPEC-W64 §6 (G-B): the two new booking.events bronze tables.
        "receipt_ingest": [
            ("event_id", StringType), ("tenant_id", StringType),
            ("channel", StringType), ("document_id", StringType),
            ("capture_client_id", StringType), ("media_inbox_id", StringType),
            ("booking_id", StringType), ("mime", StringType),
            ("bytes", LongType), ("sha256_prefix", StringType),
            ("state", StringType), ("occurred_at", TimestampType),
        ],
        "recon_resolutions": [
            ("event_id", StringType), ("tenant_id", StringType),
            ("finding_id", StringType), ("run_id", StringType),
            ("kind", StringType), ("resolved_at", TimestampType),
            ("resolved_by", StringType), ("occurred_at", TimestampType),
        ],
    }
    for table, wanted in spec.items():
        schema = it.iceberg_schema(table)
        assert [(f.name, type(f.field_type)) for f in schema.fields] == [
            (name, cls) for name, cls in wanted
        ], table
        assert [f.field_id for f in schema.fields] == list(
            range(1, len(wanted) + 1)
        ), "field ids must be sequential/stable"
        # arrow append schema must carry the same column names in order
        assert it.arrow_schema(table).names == [name for name, _ in wanted]


def test_sink_dedupes_on_event_id_within_append():
    it = pytest.importorskip(
        "analytics_pipeline.iceberg_tables", reason="pyiceberg/pyarrow not installed"
    )
    rows = [
        {"event_id": "e1", "tenant_id": T1},
        {"event_id": "e1", "tenant_id": T1},   # redelivered dup -> dropped
        {"event_id": "e2", "tenant_id": T1},
        {"event_id": None, "tenant_id": T1},   # null ids never deduped
        {"event_id": None, "tenant_id": T1},
    ]
    out = it._dedupe_on_event_id("receipt_documents", rows)
    assert [r["event_id"] for r in out] == ["e1", "e2", None, None]
    # transcripts has no event_id column -> untouched
    assert it._dedupe_on_event_id("transcripts", rows) is rows
