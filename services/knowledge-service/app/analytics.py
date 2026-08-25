"""Text-to-SQL analytics (SPEC-W3 §3, innovation 8).

POST /v1/analytics/query {tenant, question}:
  1. an OpenAI-compatible LLM (LLM_BASE_URL/LLM_MODEL/LLM_API_KEY, default
     qwen3:8b via Ollama) translates the question into one SELECT against
     the gold dbt models (schema embedded in the system prompt below);
  2. app.sqlguard validates + tenant-scopes + LIMIT-caps the statement;
  3. it executes via Trino's HTTP API (TRINO_URL, X-Trino-User) with a 20s
     budget and returns {sql, columns, rows, truncated}.

Failure contract (SPEC-W43 Y-02): every failure (LLM error, guardrail
rejection, Trino error) surfaces as a GENERIC 400 — the bound SQL and
backend error text are NEVER returned to clients (they leak schema and
infrastructure detail); the detail is logged server-side. The success
response likewise no longer echoes the bound SQL.
"""

from __future__ import annotations

from typing import Any

import httpx
from fastapi import APIRouter, HTTPException, Request
from pydantic import BaseModel, Field

from . import sqlguard
from .config import settings
from .db import bind_tenant_value, parse_tenant_slugs
from .logging import get_logger

log = get_logger("knowledge-service.analytics")

router = APIRouter()

STATEMENT_TIMEOUT_SECONDS = 20.0

# Gold schema (dbt infra/lakehouse/dbt/models/gold) embedded in the prompt.
GOLD_SCHEMA_PROMPT = """You translate natural-language analytics questions into ONE Trino SQL
SELECT statement against the gold layer of a booking-platform lakehouse.

Available tables (Trino catalog `iceberg`, schema `gold`; reference them as gold.<name>):

gold.daily_bookings_per_tenant(
  tenant_id varchar, day date,
  bookings_created bigint, bookings_confirmed bigint, bookings_rescheduled bigint,
  bookings_cancelled bigint, no_shows bigint,
  bookings_created_voice bigint, bookings_created_web bigint, bookings_created_chat bigint)

gold.revenue_daily(
  tenant_id varchar, day date, currency char(3),
  captured_revenue_cents bigint, refunded_cents bigint, no_show_fees_cents bigint,
  net_revenue_cents bigint)

gold.no_show_rate(
  tenant_id varchar, day date, bookings_confirmed bigint, no_shows bigint, no_show_rate double)

gold.agent_containment_rate(
  tenant_id varchar, day date, conversations_total bigint,
  contained_conversations bigint, containment_rate double)

Rules:
- Output ONLY the SQL, no markdown fences, no explanation, no semicolon.
- Exactly ONE SELECT statement; never INSERT/UPDATE/DELETE/DDL, never WITH.
- Only the four tables above; joins between them on (tenant_id, day) are fine.
- Do NOT filter on tenant_id yourself; the platform injects the tenant predicate.
- Monetary amounts are integer cents; day is a DATE (use date_trunc, current_date, interval).
"""


class AnalyticsQueryRequest(BaseModel):
    tenant: str = Field(..., description="tenant slug or UUID")
    question: str = Field(..., min_length=1)


class AnalyticsQueryResponse(BaseModel):
    # SPEC-W43 Y-02: the bound SQL is deliberately NOT part of the response
    # (it would leak the injected tenant predicate and schema internals).
    columns: list[str]
    rows: list[list[Any]]
    truncated: bool


def _extract_sql(text: str) -> str:
    """Strip markdown fences / chatter around the LLM's SQL."""
    sql = text.strip()
    if sql.startswith("```"):
        lines = sql.splitlines()
        lines = lines[1:] if lines[0].startswith("```") else lines
        if lines and lines[-1].strip() == "```":
            lines = lines[:-1]
        sql = "\n".join(lines).strip()
    return sql


async def generate_sql(question: str, *, client: httpx.AsyncClient | None = None) -> str:
    """Ask the OpenAI-compatible LLM for the SQL translation."""
    payload = {
        "model": settings.llm_model,
        "messages": [
            {"role": "system", "content": GOLD_SCHEMA_PROMPT},
            {"role": "user", "content": question},
        ],
        "temperature": 0.0,
    }
    headers = {"Authorization": f"Bearer {settings.llm_api_key}"}
    own = client is None
    if own:
        client = httpx.AsyncClient(timeout=STATEMENT_TIMEOUT_SECONDS)
    try:
        resp = await client.post(
            f"{settings.llm_base_url.rstrip('/')}/chat/completions",
            json=payload,
            headers=headers,
        )
        resp.raise_for_status()
        data = resp.json()
        return _extract_sql(data["choices"][0]["message"]["content"])
    finally:
        if own:
            await client.aclose()


class TrinoExecutionError(RuntimeError):
    pass


async def execute_trino(
    sql: str, *, client: httpx.AsyncClient | None = None
) -> tuple[list[str], list[list[Any]]]:
    """Run the statement through Trino's HTTP API, following nextUri pages
    until the query finishes (or the 20s client timeout fires)."""
    headers = {
        "X-Trino-User": settings.trino_user,
        "X-Trino-Catalog": "iceberg",
        "X-Trino-Schema": "gold",
    }
    columns: list[str] = []
    rows: list[list[Any]] = []
    own = client is None
    if own:
        client = httpx.AsyncClient(timeout=STATEMENT_TIMEOUT_SECONDS)
    try:
        resp = await client.post(
            f"{settings.trino_url.rstrip('/')}/v1/statement", content=sql, headers=headers
        )
        resp.raise_for_status()
        payload = resp.json()
        while True:
            if "error" in payload:
                msg = payload["error"].get("message", "trino error")
                raise TrinoExecutionError(msg)
            if "columns" in payload and not columns:
                columns = [c["name"] for c in payload["columns"]]
            if "data" in payload:
                rows.extend(payload["data"])
            next_uri = payload.get("nextUri")
            if not next_uri:
                break
            resp = await client.get(next_uri, headers=headers)
            resp.raise_for_status()
            payload = resp.json()
        return columns, rows
    finally:
        if own:
            await client.aclose()


async def run_analytics_query(tenant_id: str, question: str) -> AnalyticsQueryResponse:
    """Full pipeline; failures raise a GENERIC 400 (SPEC-W43 Y-02: bound SQL
    and backend error text are logged server-side, never returned)."""
    try:
        llm_sql = await generate_sql(question)
    except Exception as exc:
        log.warning("analytics.llm_failed", error=str(exc))
        raise HTTPException(
            status_code=400,
            detail={"error": "analytics query translation failed"},
        ) from exc

    try:
        bound_sql = sqlguard.validate_and_bind(llm_sql, tenant_id)
    except sqlguard.SqlGuardError as exc:
        log.info("analytics.guard_rejected", reason=exc.reason, sql=llm_sql)
        raise HTTPException(
            status_code=400,
            detail={"error": "question could not be translated into an "
                             "allowed analytics query"},
        ) from exc

    try:
        columns, rows = await execute_trino(bound_sql)
    except TrinoExecutionError as exc:
        log.warning("analytics.trino_failed", error=str(exc), sql=bound_sql)
        raise HTTPException(
            status_code=400,
            detail={"error": "analytics query execution failed"},
        ) from exc
    except Exception as exc:
        log.warning("analytics.trino_unreachable", error=str(exc))
        raise HTTPException(
            status_code=400,
            detail={"error": "analytics backend unreachable"},
        ) from exc

    return AnalyticsQueryResponse(
        columns=columns,
        rows=rows,
        truncated=len(rows) >= sqlguard.MAX_LIMIT,
    )


@router.post("/v1/analytics/query", response_model=AnalyticsQueryResponse)
async def analytics_query(
    body: AnalyticsQueryRequest, request: Request
) -> AnalyticsQueryResponse:
    """Natural-language analytics over the lakehouse gold layer. The tenant
    filter is always injected server-side by sqlguard.

    SPEC-W43 C1: body.tenant is bound to the gateway-injected
    X-Tenant-Slugs header (exact match, else 403; no header -> 401 unless
    the OPENDESK_TRUST_DIRECT_TENANT=1 dev escape is on)."""
    from .dapr_client import DaprClient
    from .db import resolve_tenant_id

    value = bind_tenant_value(
        parse_tenant_slugs(request.headers.get("x-tenant-slugs")),
        body.tenant,
        trust_direct=settings.trust_direct_tenant,
    )
    dapr = DaprClient(settings.dapr_host, settings.dapr_http_port)
    try:
        tenant_id = await resolve_tenant_id(
            dapr, settings.identity_app_id, value, settings.identity_internal_token
        )
    finally:
        await dapr.aclose()
    return await run_analytics_query(tenant_id, body.question)
