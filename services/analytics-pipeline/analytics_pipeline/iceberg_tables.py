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
from pyiceberg.types import DoubleType, LongType, NestedField, StringType, TimestampType

from .config import Settings
from .mapping import (
    BOOKING_EVENT_COLUMNS,
    CAC_EVENT_COLUMNS,
    PAYMENT_EVENT_COLUMNS,
    TRANSCRIPT_COLUMNS,
    USAGE_EVENT_COLUMNS,
)

BRONZE_NAMESPACE = "bronze"

_STR = StringType()
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
}

TABLE_COLUMNS: dict[str, tuple[str, ...]] = {
    "booking_events": BOOKING_EVENT_COLUMNS,
    "payment_events": PAYMENT_EVENT_COLUMNS,
    "transcripts": TRANSCRIPT_COLUMNS,
    "usage_events": USAGE_EVENT_COLUMNS,
    # SPEC-W33 §2 A2: closes the cac_analytics.py TODO producer.
    "cac_events": CAC_EVENT_COLUMNS,
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
        rows = list(rows)
        if not rows:
            return 0
        arrow = pa.Table.from_pylist(list(rows), schema=arrow_schema(table))
        self._table(table).append(arrow)
        return len(rows)
