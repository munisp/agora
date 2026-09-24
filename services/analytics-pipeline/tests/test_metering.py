"""Usage metering tests (Wave 5 #9): map_usage_event contract and
fetch_usage aggregation — offline, REST catalog faked (no Iceberg/MinIO)."""

from __future__ import annotations

from datetime import UTC, date, datetime

import pyarrow as pa
import pytest
from pyiceberg.exceptions import NoSuchTableError

from analytics_pipeline import metering
from analytics_pipeline.config import load_settings
from analytics_pipeline.mapping import USAGE_EVENT_COLUMNS, map_usage_event

T1 = "11111111-1111-1111-1111-111111111111"
T2 = "22222222-2222-2222-2222-222222222222"


class _FakeScan:
    def __init__(self, rows):
        self._rows = rows

    def to_arrow(self):
        return pa.Table.from_pylist(self._rows)


class _FakeTable:
    last_scan: dict | None = None

    def __init__(self, rows):
        self._rows = rows

    def scan(self, selected_fields, row_filter=None):
        # W46-F P6: record the pushed-down predicate for assertion (the
        # fake returns all rows so the in-Python reduction is exercised).
        _FakeTable.last_scan = {
            "selected_fields": selected_fields,
            "row_filter": row_filter,
        }
        return _FakeScan(self._rows)


class _FakeCatalog:
    def __init__(self, table):
        self._table = table

    def load_table(self, identifier):
        if self._table is None:
            raise NoSuchTableError(identifier)
        return _FakeTable(self._table)


@pytest.fixture()
def settings():
    return load_settings()


# ------------------------------------------------------------- mapping -----
def test_map_usage_event_bare_payload():
    row = map_usage_event({
        "tenant_id": T1,
        "metric": "booking",
        "value": 1,
        "ts": "2026-03-01T10:00:00Z",
        "meta": {"booking_id": "b-1", "source": "voice"},
    })
    assert list(row.keys()) == list(USAGE_EVENT_COLUMNS)
    assert row["tenant_id"] == T1
    assert row["metric"] == "booking"
    assert row["value"] == 1.0
    assert row["occurred_at"] == datetime(2026, 3, 1, 10, 0, 0)  # naive UTC
    assert row["meta"] == '{"booking_id": "b-1", "source": "voice"}'
    assert row["event_id"] is None


def test_map_usage_event_fractional_value_and_epoch_ts():
    row = map_usage_event({"tenant_id": T1, "metric": "call_minutes",
                           "value": 4.5, "ts": 1774454400})  # 2026-03-25T16:00:00Z
    assert row["value"] == 4.5
    assert row["occurred_at"] == datetime(2026, 3, 25, 16, 0, 0)
    assert row["meta"] is None


def test_map_usage_event_cloudevent_envelope():
    row = map_usage_event({
        "specversion": "1.0",
        "id": "evt-9",
        "type": "com.opendesk.usage.UsageRecorded",
        "time": "2026-03-02T08:30:00Z",
        "data": {"tenant_id": T1, "metric": "ai_tokens", "value": 820},
    })
    assert row["event_id"] == "evt-9"
    assert row["tenant_id"] == T1
    assert row["value"] == 820.0
    assert row["occurred_at"] == datetime(2026, 3, 2, 8, 30, 0)


def test_map_usage_event_sparse_and_bad_values():
    row = map_usage_event({})
    assert row == {"event_id": None, "tenant_id": None, "metric": None,
                   "value": None, "occurred_at": None, "meta": None}
    row = map_usage_event({"tenant_id": T1, "metric": "x", "value": "not-a-number"})
    assert row["value"] is None  # unparseable value drops to NULL, never crashes


# ------------------------------------------------------------ fetch_usage --
def _rows():
    return [
        {"tenant_id": T1, "metric": "booking", "value": 1.0,
         "occurred_at": datetime(2026, 3, 1, 9, tzinfo=UTC)},
        {"tenant_id": T1, "metric": "booking", "value": 2.0,
         "occurred_at": datetime(2026, 3, 1, 18, tzinfo=UTC)},
        {"tenant_id": T1, "metric": "call_minutes", "value": 4.5,
         "occurred_at": datetime(2026, 3, 1, 12, tzinfo=UTC)},
        {"tenant_id": T1, "metric": "call_minutes", "value": 3.0,
         "occurred_at": datetime(2026, 3, 2, 12, tzinfo=UTC)},
        # other tenant — filtered out
        {"tenant_id": T2, "metric": "booking", "value": 99.0,
         "occurred_at": datetime(2026, 3, 1, 12, tzinfo=UTC)},
        # sparse rows — skipped gracefully
        {"tenant_id": T1, "metric": "booking", "value": None,
         "occurred_at": datetime(2026, 3, 1, 12, tzinfo=UTC)},
        {"tenant_id": T1, "metric": None, "value": 5.0, "occurred_at": None},
    ]


def test_fetch_usage_aggregates_per_day_metric(monkeypatch, settings):
    monkeypatch.setattr(metering, "load_rest_catalog", lambda s: _FakeCatalog(_rows()))
    out = metering.fetch_usage(settings, T1)
    assert out == [
        {"tenant_id": T1, "date": "2026-03-01", "metric": "booking", "total_value": 3.0},
        {"tenant_id": T1, "date": "2026-03-01", "metric": "call_minutes", "total_value": 4.5},
        {"tenant_id": T1, "date": "2026-03-02", "metric": "call_minutes", "total_value": 3.0},
    ]


def test_fetch_usage_date_range_filter(monkeypatch, settings):
    monkeypatch.setattr(metering, "load_rest_catalog", lambda s: _FakeCatalog(_rows()))
    out = metering.fetch_usage(settings, T1, date(2026, 3, 2), date(2026, 3, 2))
    assert out == [
        {"tenant_id": T1, "date": "2026-03-02", "metric": "call_minutes", "total_value": 3.0},
    ]


def test_fetch_usage_missing_table_returns_empty(monkeypatch, settings):
    monkeypatch.setattr(metering, "load_rest_catalog", lambda s: _FakeCatalog(None))
    assert metering.fetch_usage(settings, T1) == []


def test_fetch_usage_tenant_without_rows_returns_empty(monkeypatch, settings):
    monkeypatch.setattr(metering, "load_rest_catalog", lambda s: _FakeCatalog(_rows()))
    assert metering.fetch_usage(settings, "33333333-3333-3333-3333-333333333333") == []


def test_scan_pushes_down_tenant_filter(monkeypatch, settings):
    """W46-F P6: the Iceberg scan carries row_filter (tenant predicate) +
    projected fields — no full-table scan per request."""
    from pyiceberg.expressions import EqualTo

    monkeypatch.setattr(metering, "load_rest_catalog", lambda s: _FakeCatalog(_rows()))
    metering.fetch_usage(settings, T1)
    scan = _FakeTable.last_scan
    assert scan is not None
    assert scan["selected_fields"] == metering._USAGE_FIELDS
    assert scan["row_filter"] == EqualTo("tenant_id", T1)
