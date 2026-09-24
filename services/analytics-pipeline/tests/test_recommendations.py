"""fetch_recommendations unit tests (SPEC-W3 §3 innovation 9) — offline:
the REST catalog is faked; no Iceberg/MinIO needed."""

from __future__ import annotations

from datetime import UTC, datetime

import pyarrow as pa
import pytest
from pyiceberg.exceptions import NoSuchTableError

from analytics_pipeline import recommendations
from analytics_pipeline.config import load_settings

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
        # W46-F P6: record the pushed-down predicate for assertion; the
        # fake returns all rows so the in-Python reduction is still
        # exercised (row_filter honored by real pyiceberg).
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


def _rows():
    return [
        # two runs for offering A — the later computed_at must win
        {"tenant_id": T1, "offering_id": "A", "computed_at": datetime(2025, 1, 1, tzinfo=UTC),
         "bookings_30d": 10, "net_revenue_cents_30d": 5000, "peak_hour": 9,
         "peak_share": 0.3, "no_show_rate": 0.1, "suggested_peak_multiplier": 1.25,
         "suggested_deposit_pct": 10},
        {"tenant_id": T1, "offering_id": "A", "computed_at": datetime(2025, 1, 2, tzinfo=UTC),
         "bookings_30d": 12, "net_revenue_cents_30d": 6000, "peak_hour": 10,
         "peak_share": 0.5, "no_show_rate": 0.35, "suggested_peak_multiplier": 1.5,
         "suggested_deposit_pct": 30},
        {"tenant_id": T1, "offering_id": "B", "computed_at": datetime(2025, 1, 1, tzinfo=UTC),
         "bookings_30d": 3, "net_revenue_cents_30d": 900, "peak_hour": 14,
         "peak_share": 0.2, "no_show_rate": 0.0, "suggested_peak_multiplier": 1.0,
         "suggested_deposit_pct": 0},
        # another tenant — must be filtered out
        {"tenant_id": T2, "offering_id": "A", "computed_at": datetime(2025, 1, 3, tzinfo=UTC),
         "bookings_30d": 99, "net_revenue_cents_30d": 1, "peak_hour": 1,
         "peak_share": 1.0, "no_show_rate": 1.0, "suggested_peak_multiplier": 1.5,
         "suggested_deposit_pct": 30},
    ]


@pytest.fixture()
def settings():
    return load_settings()


def test_latest_per_offering_and_tenant_filter(monkeypatch, settings):
    monkeypatch.setattr(recommendations, "load_rest_catalog", lambda s: _FakeCatalog(_rows()))
    out = recommendations.fetch_recommendations(settings, T1)
    assert [r["offering_id"] for r in out] == ["A", "B"]
    a = out[0]
    assert a["computed_at"] == "2025-01-02T00:00:00+00:00"  # latest run won
    assert a["peak_hour"] == 10
    assert a["suggested_peak_multiplier"] == 1.5
    assert a["suggested_deposit_pct"] == 30
    assert all(r["offering_id"] != "A" or r["bookings_30d"] == 12 for r in out)


def test_scan_pushes_down_tenant_filter(monkeypatch, settings):
    """W46-F P6: the Iceberg scan carries row_filter (tenant predicate) +
    projected fields — no full-table scan per request."""
    from pyiceberg.expressions import EqualTo

    monkeypatch.setattr(recommendations, "load_rest_catalog", lambda s: _FakeCatalog(_rows()))
    recommendations.fetch_recommendations(settings, T1)
    scan = _FakeTable.last_scan
    assert scan is not None
    assert scan["selected_fields"] == recommendations._RECO_FIELDS
    assert scan["row_filter"] == EqualTo("tenant_id", T1)


def test_missing_table_returns_empty_list(monkeypatch, settings):
    monkeypatch.setattr(recommendations, "load_rest_catalog", lambda s: _FakeCatalog(None))
    assert recommendations.fetch_recommendations(settings, T1) == []


def test_tenant_without_rows_returns_empty_list(monkeypatch, settings):
    monkeypatch.setattr(recommendations, "load_rest_catalog", lambda s: _FakeCatalog(_rows()))
    assert recommendations.fetch_recommendations(settings, "33333333-3333-3333-3333-333333333333") == []
