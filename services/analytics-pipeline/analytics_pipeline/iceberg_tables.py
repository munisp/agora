"""Iceberg (pyiceberg) side: catalog factory, namespace/table bootstrap, appends.

Catalog: REST at ICEBERG_REST_URI (apache/iceberg-rest-fixture in dev), warehouse
s3://lake/warehouse on MinIO. Bronze tables are auto-created with explicit schemas
matching the dbt sources contract (see mapping.py header).
"""

from __future__ import annotations

from typing import Any, Iterable, Mapping

import pyarrow as pa
from pyiceberg.catalog import Catalog, load_catalog
from pyiceberg.exceptions import NamespaceAlreadyExistsError, TableAlreadyExistsError
from pyiceberg.schema import Schema
from pyiceberg.types import (
    DoubleType,
    IntegerType,
    LongType,
    NestedField,
    StringType,
    TimestampType,
)

from .config import Settings
from .mapping import (
    BOOKING_EVENT_COLUMNS,
    CAC_EVENT_COLUMNS,
    PAYMENT_EVENT_COLUMNS,
    RECEIPT_DOCUMENT_COLUMNS,
    RECEIPT_INGEST_COLUMNS,
    RECEIPT_RECON_FINDING_COLUMNS,
    RECON_RESOLUTION_COLUMNS,
    TRANSCRIPT_COLUMNS,
    USAGE_EVENT_COLUMNS,
)

BRONZE_NAMESPACE = "bronze"

_STR = StringType()
_INT = IntegerType()
_LONG = LongType()
_DOUBLE = DoubleType()
_TS = TimestampType()  # timestamp WITHOUT timezone (naive UTC) — see mapping.py

_COLUMN_TYPES: dict[str, Any] = {
    "event_id": _STR,
    "event_type": _STR,
    "tenant_id": _STR,
    "booking_id": _STR,
    "status": _STR,
    "source": _STR,
    "price_cents": _LONG,
    "currency": _STR,
    "starts_at": _TS,
    "occurred_at": _TS,
    "offering_id": _STR,
    "amount_cents": _LONG,
    "transfer_code": _LONG,
    "ledger_ref": _STR,
    "conversation_id": _STR,
    "role": _STR,
    "text": _STR,
    "ts": _TS,
    "audio_url": _STR,
    "metric": _STR,
    "value": _DOUBLE,
    "meta": _STR,
    # SPEC-W33 §2 A2: cac.events -> bronze.cac_events (CAC_EVENT_COLUMNS).
    "entity_type": _STR,
    "entity_id": _STR,
    "event_name": _STR,
    "event_ts": _TS,
    "channel": _STR,
    "campaign_id": _STR,
    "lga_id": _LONG,
    "amount_ngn": _DOUBLE,
    "idempotency_key": _STR,
    # SPEC-W62 §bronze.receipt_documents (G1: kind='receipt' only, kyc_*
    # dropped, extracted_fields whitelist = amount_minor/currency/receipt_date).
    "document_id": _STR,
    "kind": _STR,
    "page_count": _INT,
    "sha256": _STR,
    "phash": _STR,
    "model_version": _STR,
    "amount_minor": _LONG,
    "receipt_date": _STR,
    # SPEC-W62 §bronze.receipt_recon_findings (G2: ReceiptReconFinding events
    # routed off booking.events into their own table, full finding payload).
    "run_id": _STR,
    "finding_kind": _STR,
    "confidence": _STR,
    "capture_client_id": _STR,
    "attempt_tx_ref": _STR,
    "expected_amount_minor": _LONG,
    "captured_amount_minor": _LONG,
    "receipt_amount_major": _DOUBLE,
    "receipt_currency": _STR,
    # SPEC-W64 §6: bronze.receipt_ingest (ReceiptIngested, G-B router arm)
    # + bronze.recon_resolutions (ReceiptReconResolved). finding_id is ALSO
    # appended to bronze.receipt_recon_findings (W62 table extension —
    # _evolve_schema ADD COLUMNs it onto pre-existing tables) as the join
    # key for the gold resolved_count.
    "finding_id": _STR,
    "media_inbox_id": _STR,
    "mime": _STR,
    "bytes": _LONG,
    "sha256_prefix": _STR,
    "state": _STR,
    "resolved_at": _TS,
    "resolved_by": _STR,
}

TABLE_COLUMNS: dict[str, tuple[str, ...]] = {
    "booking_events": BOOKING_EVENT_COLUMNS,
    "payment_events": PAYMENT_EVENT_COLUMNS,
    "transcripts": TRANSCRIPT_COLUMNS,
    "usage_events": USAGE_EVENT_COLUMNS,
    # SPEC-W33 §2 A2: closes the cac_analytics.py TODO producer.
    "cac_events": CAC_EVENT_COLUMNS,
    # SPEC-W62: opendesk.ocr.events DocumentExtracted (receipts only).
    "receipt_documents": RECEIPT_DOCUMENT_COLUMNS,
    # SPEC-W62 G2: ReceiptReconFinding routed off opendesk.booking.events.
    "receipt_recon_findings": RECEIPT_RECON_FINDING_COLUMNS,
    # SPEC-W64 G-B: ReceiptIngested + ReceiptReconResolved routed off
    # opendesk.booking.events into their own tables (never booking_events).
    "receipt_ingest": RECEIPT_INGEST_COLUMNS,
    "recon_resolutions": RECON_RESOLUTION_COLUMNS,
}


def iceberg_schema(table: str) -> Schema:
    """Explicit pyiceberg schema; field ids assigned sequentially (stable)."""
    return Schema(
        *[
            NestedField(field_id=i + 1, name=name, field_type=_COLUMN_TYPES[name], required=False)
            for i, name in enumerate(TABLE_COLUMNS[table])
        ]
    )


def _pa_type(iceberg_type: Any) -> pa.DataType:
    if isinstance(iceberg_type, LongType):
        return pa.int64()
    if isinstance(iceberg_type, IntegerType):
        return pa.int32()
    if isinstance(iceberg_type, DoubleType):
        return pa.float64()
    if isinstance(iceberg_type, TimestampType):
        return pa.timestamp("us")
    return pa.string()


def arrow_schema(table: str) -> pa.Schema:
    """pyarrow schema matching the Iceberg schema (for append payloads)."""
    return pa.schema(
        [pa.field(name, _pa_type(_COLUMN_TYPES[name]), nullable=True)
         for name in TABLE_COLUMNS[table]]
    )


def load_rest_catalog(settings: Settings) -> Catalog:
    """REST catalog over the fixture endpoint with PyArrow S3 FileIO (MinIO)."""
    return load_catalog(
        "opendesk",
        **{
            "type": "rest",
            "uri": settings.iceberg_rest_uri,
            "warehouse": settings.iceberg_warehouse,
            "py-io-impl": "pyiceberg.io.pyarrow.PyArrowFileIO",
            "s3.endpoint": settings.aws_endpoint_url,
            "s3.access-key-id": settings.aws_access_key_id,
            "s3.secret-access-key": settings.aws_secret_access_key,
            "s3.region": settings.aws_region,
        },
    )


def ensure_bronze(catalog: Catalog) -> None:
    """Create namespace `bronze` and the raw tables if missing (idempotent)."""
    try:
        catalog.create_namespace(BRONZE_NAMESPACE)
    except NamespaceAlreadyExistsError:
        pass
    for table in TABLE_COLUMNS:
        identifier = f"{BRONZE_NAMESPACE}.{table}"
        try:
            catalog.create_table(identifier, schema=iceberg_schema(table))
        except TableAlreadyExistsError:
            _evolve_schema(catalog, identifier, table)


def _evolve_schema(catalog: Catalog, identifier: str, table: str) -> None:
    """Idempotent ADD COLUMN for tables created before a column was added
    (e.g. booking_events.offering_id, SPEC-W3 §3) — Iceberg schema evolution
    keeps existing files valid; NULLs are read for old data."""
    existing = catalog.load_table(identifier)
    present = {f.name for f in existing.schema().fields}
    wanted = [c for c in TABLE_COLUMNS[table] if c not in present]
    if not wanted:
        return
    with existing.update_schema() as update:
        for col in wanted:
            update.add_column(col, _COLUMN_TYPES[col])


class IcebergSink:
    """Thin appender: rows (dicts) -> pyarrow micro-batch -> Iceberg append."""

    def __init__(self, catalog: Catalog):
        self._catalog = catalog
        self._tables: dict[str, Any] = {}

    def _table(self, name: str):
        if name not in self._tables:
            self._tables[name] = self._catalog.load_table(f"{BRONZE_NAMESPACE}.{name}")
        return self._tables[name]

    def append(self, table: str, rows: Iterable[Mapping[str, Any]]) -> int:
        rows = _dedupe_on_event_id(table, list(rows))
        if not rows:
            return 0
        arrow = pa.Table.from_pylist(list(rows), schema=arrow_schema(table))
        self._table(table).append(arrow)
        return len(rows)


def _dedupe_on_event_id(table: str, rows: list[Mapping[str, Any]]) -> list[Mapping[str, Any]]:
    """In-batch idempotency (SPEC-W62: event_id dedupe on append): for tables
    keyed by event_id, keep the FIRST row per non-null event_id within the
    append batch (first wins — redelivered duplicates are byte-identical).
    Cross-batch duplicates from at-least-once redelivery are still removed
    downstream by the Spark silver jobs, as before. Tables without an
    event_id column (transcripts) are untouched."""
    if "event_id" not in TABLE_COLUMNS.get(table, ()):
        return rows
    seen: set[Any] = set()
    out: list[Mapping[str, Any]] = []
    for row in rows:
        event_id = row.get("event_id")
        if event_id is not None:
            if event_id in seen:
                continue
            seen.add(event_id)
        out.append(row)
    return out
