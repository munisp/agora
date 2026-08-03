"""GET /v1/cac/summary assembly (SPEC-W13 contract §5).

Response::

    {by_channel: [{channel, spend_ngn, leads, conversions, cac_ngn}],
     by_lga: [{lga_id, leads, conversions, spend_ngn, cac_ngn, geom}],
     blended_cac_ngn, ltv_ngn, payback_days_estimate, data_quality}

Spend join: campaign spend lives in booking-service (contract §4,
POST /v1/campaigns/{id}/spend), so the summary reads it back per campaign
via Dapr invoke ``GET /internal/campaigns/{id}/spend-sum?from&to`` (built by
Agent A in parallel — CONTRACT ASSUMPTION on the response shape, see
BookingSpendClient below). The join is RESILIENT: a 404/unreachable
booking-service never fails the summary — that campaign's spend counts as 0
and the response is marked ``data_quality: "spend_unavailable"``.

Payback: ``payback_days_estimate = blended_cac_ngn / (avg daily gross margin
per converted lead)``. v1 has no COGS signal, so gross margin is approximated
by the converted/first_txn ``amount_ngn`` payloads (ASSUMPTION — documented
in docs/cac-analytics-api.md). Null-safe: no amounts / no conversions / no
period -> null.
"""

from __future__ import annotations

import asyncio
from datetime import date
from decimal import Decimal, InvalidOperation
from typing import Any, Protocol

import structlog

from . import metrics
from .cac_store import CacStore, decimal_or_none
from .config import Settings
from .dapr_client import DaprClient

log = structlog.get_logger()


class SpendFetcher(Protocol):
    """Per-campaign spend lookup (booking-service internal endpoint)."""

    async def spend_sum(
        self, campaign_id: str, date_from: date | None, date_to: date | None
    ) -> Decimal:
        ...


class SpendUnavailable(Exception):
    """booking-service spend endpoint unreachable/404 — spend counts as 0."""


class BookingSpendClient:
    """Dapr-invoke spend lookup against booking-service.

    CONTRACT ASSUMPTION (flagged to Agent A): expects
    ``GET /internal/campaigns/{id}/spend-sum?from&to`` to answer
    ``{"campaign_id": ..., "spend_ngn": <number>}`` — the parser also
    tolerates ``total_spend_ngn`` / ``amount_ngn`` keys and a bare numeric
    body so a minor shape drift degrades gracefully instead of zeroing spend.
    """

    def __init__(self, dapr: DaprClient, app_id: str = "booking"):
        self._dapr = dapr
        self._app_id = app_id

    async def spend_sum(
        self, campaign_id: str, date_from: date | None, date_to: date | None
    ) -> Decimal:
        params: dict[str, str] = {}
        if date_from is not None:
            params["from"] = date_from.isoformat()
        if date_to is not None:
            params["to"] = date_to.isoformat()
        try:
            payload = await self._dapr.invoke(
                self._app_id,
                f"internal/campaigns/{campaign_id}/spend-sum",
                params=params,
                http_method="GET",
            )
        except Exception as exc:  # noqa: BLE001 — daprd/booking outage or 404
            raise SpendUnavailable(
                f"spend-sum {campaign_id}: {type(exc).__name__}: {exc}"
            ) from exc
        return _parse_spend(payload)


def _parse_spend(payload: Any) -> Decimal:
    if payload is None:
        return Decimal("0")
    if isinstance(payload, (int, float, str, Decimal)):
        try:
            return Decimal(str(payload))
        except InvalidOperation:
            return Decimal("0")
    if isinstance(payload, dict):
        for key in ("spend_ngn", "total_spend_ngn", "amount_ngn", "spend"):
            if key in payload and payload[key] is not None:
                try:
                    return Decimal(str(payload[key]))
                except InvalidOperation:
                    return Decimal("0")
    return Decimal("0")


def _money(value: Decimal | None) -> float | None:
    return None if value is None else float(round(value, 2))


def _cac(spend: Decimal, conversions: int) -> Decimal | None:
    if conversions <= 0:
        return None
    return spend / conversions


def build_summary(
    *,
    tenant_id: str,
    date_from: date | None,
    date_to: date | None,
    channel_rows: list[dict[str, Any]],
    lga_rows: list[dict[str, Any]],
    spend_by_channel: dict[str, Decimal],
    spend_unavailable: bool,
    day_bounds: tuple[date | None, date | None],
) -> dict[str, Any]:
    """Pure rollup math — unit-tested without Postgres/Dapr."""
    by_channel: list[dict[str, Any]] = []
    total_spend = Decimal("0")
    total_leads = 0
    total_conversions = 0
    total_revenue = Decimal("0")
    for row in channel_rows:
        channel = row["channel"]
        spend = spend_by_channel.get(channel, Decimal("0"))
        leads = int(row["leads"])
        conversions = int(row["conversions"])
        revenue = decimal_or_none(row.get("revenue_ngn")) or Decimal("0")
        total_spend += spend
        total_leads += leads
        total_conversions += conversions
        total_revenue += revenue
        by_channel.append(
            {
                "channel": channel,
                "spend_ngn": _money(spend),
                "leads": leads,
                "conversions": conversions,
                "cac_ngn": _money(_cac(spend, conversions)),
            }
        )

    # Spend with no matching events in range (channel seen only via
    # out-of-range events) still surfaces as its own row — honest CAC.
    seen = {row["channel"] for row in channel_rows}
    for channel, spend in sorted(spend_by_channel.items()):
        if channel in seen:
            continue  # spend already counted in the first loop
        total_spend += spend
        if spend == 0:
            continue
        by_channel.append(
            {
                "channel": channel,
                "spend_ngn": _money(spend),
                "leads": 0,
                "conversions": 0,
                "cac_ngn": None,
            }
        )

    by_lga = [
        {
            "lga_id": int(row["lga_id"]),
            "leads": int(row["leads"]),
            "conversions": int(row["conversions"]),
            # Spend is booked per campaign/channel, not per LGA — no honest
            # allocation exists yet (nightly lakehouse reconcile may add one).
            "spend_ngn": None,
            "cac_ngn": None,
            # No LGA boundary endpoint exists in booking-service this wave
            # (checked internal/store/geolocations.go + httpapi routes) —
            # geom stays null; the dashboard renders these rows in a table.
            "geom": None,
        }
        for row in lga_rows
    ]

    blended = _cac(total_spend, total_conversions)
    ltv = (
        (total_revenue / total_conversions)
        if total_conversions > 0 and total_revenue > 0
        else None
    )

    # Payback period length: explicit [from, to] wins; else the data span.
    period_days: int | None = None
    if date_from is not None and date_to is not None:
        period_days = (date_to - date_from).days + 1
    elif day_bounds[0] is not None and day_bounds[1] is not None:
        period_days = (day_bounds[1] - day_bounds[0]).days + 1
    payback: float | None = None
    if blended is not None and ltv is not None and period_days and period_days > 0:
        avg_daily_margin = ltv / period_days
        if avg_daily_margin > 0:
            payback = float(round(blended / avg_daily_margin, 1))

    return {
        "tenant": tenant_id,
        "from": date_from.isoformat() if date_from else None,
        "to": date_to.isoformat() if date_to else None,
        "by_channel": by_channel,
        "by_lga": by_lga,
        "blended_cac_ngn": _money(blended),
        "ltv_ngn": _money(ltv),
        "payback_days_estimate": payback,
        "data_quality": "spend_unavailable" if spend_unavailable else "ok",
        # totals are additive context for the dashboard cards (not in the
        # contract but harmless — contract fields above are authoritative).
        "totals": {
            "spend_ngn": _money(total_spend),
            "leads": total_leads,
            "conversions": total_conversions,
            "revenue_ngn": _money(total_revenue),
        },
    }


async def fetch_summary(
    settings: Settings,
    store: CacStore,
    spend_client: SpendFetcher,
    tenant_id: str,
    date_from: date | None,
    date_to: date | None,
) -> dict[str, Any]:
    """Orchestrate store reads + the resilient per-campaign spend join."""
    channel_recs, lga_recs, campaign_recs, day_bounds = await asyncio.gather(
        store.fetch_channel_rollup(tenant_id, date_from, date_to),
        store.fetch_lga_rollup(tenant_id, date_from, date_to),
        store.list_campaign_channels(tenant_id),
        store.rollup_day_bounds(tenant_id),
    )

    spend_by_channel: dict[str, Decimal] = {}
    spend_unavailable = False

    async def _lookup(campaign_id: str, channel: str) -> None:
        nonlocal spend_unavailable
        try:
            spend = await spend_client.spend_sum(campaign_id, date_from, date_to)
        except SpendUnavailable as exc:
            # Contract requirement: never fail the summary on spend lookup.
            spend_unavailable = True
            metrics.CAC_SPEND_LOOKUPS.labels(outcome="unavailable").inc()
            log.warning("cac.spend_unavailable", campaign_id=campaign_id, error=str(exc))
            return
        metrics.CAC_SPEND_LOOKUPS.labels(outcome="ok").inc()
        spend_by_channel[channel] = spend_by_channel.get(channel, Decimal("0")) + spend

    await asyncio.gather(
        *(
            _lookup(str(rec["campaign_id"]), str(rec["channel"]))
            for rec in campaign_recs
        )
    )

    return build_summary(
        tenant_id=tenant_id,
        date_from=date_from,
        date_to=date_to,
        channel_rows=[dict(rec) for rec in channel_recs],
        lga_rows=[dict(rec) for rec in lga_recs],
        spend_by_channel=spend_by_channel,
        spend_unavailable=spend_unavailable,
        day_bounds=day_bounds,
    )
