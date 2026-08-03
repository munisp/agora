"""FastAPI sidecar: GET /healthz (consumer lag per topic), GET /metrics,
GET /v1/recommendations (SPEC-W3 §3), GET /v1/metering (Wave 5 #9) and
GET /v1/cac/summary (SPEC-W13 contract §5)."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
from datetime import date
from typing import Any

import structlog
from fastapi import FastAPI, Header, HTTPException, Query
from fastapi.responses import JSONResponse, PlainTextResponse
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest

from .cac_summary import SpendFetcher, fetch_summary
from .cac_store import CacStore
from .config import Settings
from .consumer import BronzeConsumer
from .metering import fetch_usage
from .recommendations import fetch_recommendations
from .tenants import TenantNotFoundError, TenantResolutionError, TenantResolver

log = structlog.get_logger()


def _parse_date(raw: str | None, param: str) -> date | None:
    if raw in (None, ""):
        return None
    try:
        return date.fromisoformat(raw)
    except ValueError:
        raise HTTPException(
            status_code=400, detail=f"{param} must be an ISO date (YYYY-MM-DD)"
        ) from None


@dataclass
class CacDeps:
    """SPEC-W13 wiring for GET /v1/cac/summary. None when the CAC module is
    disabled (CAC_CONSUMER_ENABLED=false) — the route then answers 503."""

    store: CacStore
    spend: SpendFetcher
    tenants: TenantResolver


def create_app(
    consumer: BronzeConsumer,
    ready_flag: dict[str, bool],
    settings: Settings,
    cac: CacDeps | None = None,
) -> FastAPI:
    app = FastAPI(title="opendesk-analytics-pipeline", version="0.1.0")

    @app.get("/healthz")
    async def healthz() -> JSONResponse:
        lag = await consumer.lag_report()
        ready = ready_flag.get("ready", False) and consumer.running
        body: dict[str, Any] = {
            "status": "ok" if ready else "starting",
            "consumer_running": consumer.running,
            "last_error": consumer.last_error,
            "topics": lag,
        }
        # 503 while still starting so compose healthchecks gate dependents;
        # flush errors alone do not fail health (retry loop is by design).
        return JSONResponse(body, status_code=200 if ready else 503)

    @app.get("/metrics")
    async def metrics_endpoint() -> PlainTextResponse:
        return PlainTextResponse(generate_latest().decode("utf-8"),
                                 media_type=CONTENT_TYPE_LATEST)

    @app.get("/v1/recommendations")
    async def recommendations(tenant: str = Query(min_length=1)) -> dict[str, Any]:
        """Latest pricing recommendations per offering for a tenant
        (SPEC-W3 §3 innovation 9). `tenant` is the tenant UUID as stored in
        the lakehouse. Empty list when gold.reco_pricing does not exist yet."""
        try:
            items = await asyncio.to_thread(fetch_recommendations, settings, tenant)
        except Exception as exc:  # noqa: BLE001 — iceberg/MinIO outage
            log.warning("recommendations.failed", error=str(exc))
            raise HTTPException(status_code=502, detail=f"lakehouse error: {exc}") from exc
        return {"tenant": tenant, "recommendations": items}

    @app.get("/v1/metering")
    async def metering(
        tenant: str = Query(min_length=1),
        from_: str | None = Query(default=None, alias="from"),
        to: str | None = None,
    ) -> dict[str, Any]:
        """Aggregated usage for a tenant (Wave 5 #9): rows of
        {tenant_id, date, metric, total_value} over bronze.usage_events,
        optionally bounded by [from, to] ISO dates (inclusive). Empty list
        when no usage exists yet — sparse data is normal in v1."""
        date_from = _parse_date(from_, "from")
        date_to = _parse_date(to, "to")
        if date_from is not None and date_to is not None and date_from > date_to:
            raise HTTPException(status_code=400, detail="from must be <= to")
        try:
            items = await asyncio.to_thread(
                fetch_usage, settings, tenant, date_from, date_to
            )
        except Exception as exc:  # noqa: BLE001 — iceberg/MinIO outage
            log.warning("metering.failed", error=str(exc))
            raise HTTPException(status_code=502, detail=f"lakehouse error: {exc}") from exc
        return {"tenant": tenant, "usage": items}

    @app.get("/v1/cac/summary")
    async def cac_summary(
        from_: str | None = Query(default=None, alias="from"),
        to: str | None = None,
        tenant: str | None = Query(default=None),
        x_tenant_slug: str | None = Header(default=None),
    ) -> dict[str, Any]:
        """CAC dashboard summary (SPEC-W13 contract §5): realtime rollups
        from cac.events + resilient campaign-spend join against
        booking-service. Tenant comes from the X-Tenant-Slug header (resolved
        via identity-service, same as booking-service) or, for parity with
        the other sidecar routes, a ?tenant=<uuid> query param.
        `from`/`to` are optional inclusive ISO dates."""
        if cac is None:
            raise HTTPException(status_code=503, detail="cac module disabled")
        date_from = _parse_date(from_, "from")
        date_to = _parse_date(to, "to")
        if date_from is not None and date_to is not None and date_from > date_to:
            raise HTTPException(status_code=400, detail="from must be <= to")

        tenant_id: str | None = None
        if x_tenant_slug:
            try:
                tenant_id = (await cac.tenants.by_slug(x_tenant_slug)).id
            except TenantNotFoundError as exc:
                raise HTTPException(status_code=404, detail="tenant not found") from exc
            except TenantResolutionError as exc:
                raise HTTPException(status_code=502, detail=str(exc)) from exc
        elif tenant:
            tenant_id = tenant
        if not tenant_id:
            raise HTTPException(
                status_code=400,
                detail="X-Tenant-Slug header (or ?tenant=<uuid>) is required",
            )
        try:
            return await fetch_summary(
                settings, cac.store, cac.spend, tenant_id, date_from, date_to
            )
        except HTTPException:
            raise
        except Exception as exc:  # noqa: BLE001 — postgres outage
            log.warning("cac.summary_failed", error=str(exc))
            raise HTTPException(status_code=502, detail=f"rollup store error: {exc}") from exc

    return app
