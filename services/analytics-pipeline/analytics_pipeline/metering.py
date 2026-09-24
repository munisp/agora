"""GET /v1/metering (STRATEGY §3, Wave 5 #9 — monetization hook §2 item 2).

Aggregates raw usage records from the Iceberg table `bronze.usage_events`
(written by this service's consumer from opendesk.usage.events) into
{tenant_id, date, metric, total_value} rows for one tenant and an optional
[inclusive] date range — the same shape as the dbt gold.usage_daily mart, so
the API answers consistently whether or not dbt has run yet.

Sparse data is the norm (v1 emits booking + call-minute metrics only, and
payments/voice emission is deferred — see README): a missing table, a tenant
without rows, or an empty range all return an empty list, never an error.

The scan pushes the tenant predicate down (row_filter) and projects only
the needed fields (W46-F P6) — same pattern as recommendations.py.
"""

from __future__ import annotations

from datetime import date, datetime
from typing import Any

import structlog
from pyiceberg.exceptions import NoSuchTableError
from pyiceberg.expressions import EqualTo

from .config import Settings
from .iceberg_tables import load_rest_catalog

log = structlog.get_logger()

USAGE_TABLE = "bronze.usage_events"

_USAGE_FIELDS = ("tenant_id", "metric", "value", "occurred_at")


def fetch_usage(
    settings: Settings,
    tenant: str,
    date_from: date | None = None,
    date_to: date | None = None,
) -> list[dict[str, Any]]:
    """Aggregated usage rows for one tenant (empty if the table is absent
    or the tenant has no rows in range). Blocking pyiceberg I/O — call via
    asyncio.to_thread."""
    catalog = load_rest_catalog(settings)
    try:
        table = catalog.load_table(USAGE_TABLE)
    except NoSuchTableError:
        log.info("metering.table_absent", table=USAGE_TABLE)
        return []

    # W46-F P6: push the tenant predicate down to the scan (same fix as
    # recommendations.py) — no more full bronze.usage_events scan per
    # request; the in-Python check stays as defense-in-depth.
    rows = (
        table.scan(
            row_filter=EqualTo("tenant_id", tenant),
            selected_fields=_USAGE_FIELDS,
        )
        .to_arrow()
        .to_pylist()
    )

    totals: dict[tuple[date, str], float] = {}
    for row in rows:
        if row.get("tenant_id") != tenant:
            continue
        ts = row.get("occurred_at")
        day = ts.date() if isinstance(ts, datetime) else None
        if day is None:
            continue
        if date_from is not None and day < date_from:
            continue
        if date_to is not None and day > date_to:
            continue
        value = row.get("value")
        if value is None:
            continue
        key = (day, str(row.get("metric") or "unknown"))
        totals[key] = totals.get(key, 0.0) + float(value)

    return [
        {"tenant_id": tenant, "date": day.isoformat(), "metric": metric,
         "total_value": total}
        for (day, metric), total in sorted(totals.items())
    ]
