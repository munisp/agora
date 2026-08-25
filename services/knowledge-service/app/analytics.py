"""Text-to-SQL analytics (SPEC-W3 §3, innovation 8).

POST /v1/analytics/query: natural-language question -> LLM (OpenAI-compatible,
default qwen3:8b via Ollama) generates SELECT-only SQL -> validated -> executed
against Trino (PostgreSQL connector, read-only role) -> rows returned. The LLM
never sees tenant data: the schema prompt lists tables, the generated SQL is
validated, and a mandatory ``tenant_id = '<slug>'`` predicate is injected.

SPEC-W44 V2-D1 / S1-F7-04: the endpoint is now gated on an authenticated
tenant-scoped caller (X-User-Id + tenant binding via resolve_tenant_slug) —
previously anonymous and tenant-spoofable. The LLM/Trino execution path is
unchanged.
"""

from __future__ import annotations

import logging
import re
from typing import Any

import httpx
from fastapi import APIRouter, HTTPException, Request
from pydantic import BaseModel, Field

from .config import settings
from .tenancy import resolve_tenant_slug

logger = logging.getLogger(__name__)

router = APIRouter(tags=["analytics"])

# Schema the LLM is allowed to query (Trino catalog `postgres`). Kept in sync
# with the per-service schemas; columns the NL questions reference.
SCHEMA_PROMPT = """You translate questions into one read-only Trino SQL SELECT.
Schema (Trino catalog `postgres`):
- postgres.public.bookings(id, tenant_id, contact_id, service_id, site_id, status,
  scheduled_start, scheduled_end, price_minor, currency, created_at)
- postgres.public.contacts(id, tenant_id, phone, email, display_name, created_at)
- postgres.public.services(id, tenant_id, name, duration_minutes, price_minor, currency)
- postgres.public.sites(id, tenant_id, slug, name, industry, plan, created_at)
- postgres.public.tenants(id, slug, name, plan, industry, created_at)
- postgres.public.orders(id, tenant_id, contact_id, status, total_minor, currency, created_at)
- postgres.public.payments(id, tenant_id, order_id, amount_minor, currency, status, created_at)
Rules:
- Output ONLY the SQL, no explanation, no markdown fences.
- Single SELECT statement. Never INSERT/UPDATE/DELETE/DROP/ALTER/GRANT.
- ALWAYS filter tenant-scoped tables with tenant_id = '<TENANT>'.
- Use ILIKE for case-insensitive text match. LIMIT 100 unless asked otherwise.
"""

FORBIDDEN = re.compile(
    r"\b(insert|update|delete|drop|alter|grant|revoke|truncate|create|merge|call|execute|copy)\b",
    re.IGNORECASE,
)


class AnalyticsQuery(BaseModel):
    """Natural-language analytics request."""

    question: str = Field(min_length=3, max_length=2000)
    tenant: str | None = None  # slug; defaults to X-Tenant-Slugs binding
    limit: int = Field(default=100, ge=1, le=1000)


class AnalyticsResult(BaseModel):
    question: str
    tenant: str
    sql: str
    columns: list[str]
    rows: list[list[Any]]
    row_count: int


async def _generate_sql(question: str, tenant: str) -> str:
    prompt = SCHEMA_PROMPT.replace("<TENANT>", tenant)
    try:
        async with httpx.AsyncClient(timeout=60) as client:
            resp = await client.post(
                f"{settings.llm_base_url.rstrip('/')}/chat/completions",
                headers={"Authorization": f"Bearer {settings.llm_api_key}"},
                json={
                    "model": settings.llm_model,
                    "messages": [
                        {"role": "system", "content": prompt},
                        {"role": "user", "content": question},
                    ],
                    "temperature": 0,
                },
            )
            resp.raise_for_status()
            return resp.json()["choices"][0]["message"]["content"]
    except Exception as exc:  # noqa: BLE001 - surfaced as 502
        logger.error("analytics llm failed: %s", exc)
        raise HTTPException(status_code=502, detail="llm backend error") from exc


def _clean_and_validate_sql(raw: str, tenant: str, limit: int) -> str:
    sql = raw.strip()
    # Strip markdown fences / stray semicolons the model may emit.
    sql = re.sub(r"^```(?:sql)?\s*|\s*```$", "", sql).strip().rstrip(";")
    if not re.match(r"^\s*select\b", sql, re.IGNORECASE):
        raise HTTPException(status_code=422, detail="generated SQL is not a SELECT")
    if FORBIDDEN.search(sql):
        raise HTTPException(status_code=422, detail="generated SQL contains a forbidden statement")
    # Mandatory tenant predicate (S1-F7-04: never let the LLM drop it).
    if "tenant_id" not in sql:
        raise HTTPException(status_code=422, detail="generated SQL is missing the tenant filter")
    if f"tenant_id = '{tenant}'" not in sql and f'tenant_id = "{tenant}"' not in sql:
        raise HTTPException(status_code=422, detail="tenant filter mismatch")
    if "limit" not in sql.lower():
        sql = f"{sql} LIMIT {limit}"
    return sql


async def _run_trino(sql: str) -> tuple[list[str], list[list[Any]]]:
    """Execute via Trino's HTTP API, following nextUri pages to completion."""
    columns: list[str] = []
    rows: list[list[Any]] = []
    try:
        async with httpx.AsyncClient(
            timeout=120,
            headers={"X-Trino-User": settings.trino_user},
        ) as client:
            resp = await client.post(f"{settings.trino_url.rstrip('/')}/v1/statement", content=sql)
            if resp.status_code >= 400:
                raise HTTPException(status_code=502, detail=f"trino error: {resp.text[:300]}")
            payload = resp.json()
            while True:
                if "error" in payload:
                    raise HTTPException(
                        status_code=422,
                        detail=f"query failed: {payload['error'].get('message', 'unknown')[:300]}",
                    )
                if not columns and payload.get("columns"):
                    columns = [c["name"] for c in payload["columns"]]
                rows.extend(payload.get("data", []))
                nxt = payload.get("nextUri")
                if not nxt:
                    break
                resp = await client.get(nxt)
                resp.raise_for_status()
                payload = resp.json()
    except HTTPException:
        raise
    except Exception as exc:  # noqa: BLE001 - surfaced as 502
        logger.error("trino call failed: %s", exc)
        raise HTTPException(status_code=502, detail="trino backend error") from exc
    return columns, rows


@router.post("/analytics/query", response_model=AnalyticsResult)
async def analytics_query(body: AnalyticsQuery, request: Request) -> AnalyticsResult:
    # SPEC-W44 V2-D1 / S1-F7-04: was anonymous + tenant-spoofable. Now the
    # caller is resolved and tenant-bound exactly like /search (X-User-Id +
    # X-Tenant-Slugs K1 binding), so generated SQL can only ever carry the
    # caller's own tenant filter.
    tenant = resolve_tenant_slug(request, body.tenant)
    raw_sql = await _generate_sql(body.question, tenant)
    sql = _clean_and_validate_sql(raw_sql, tenant, body.limit)
    columns, rows = await _run_trino(sql)
    return AnalyticsResult(
        question=body.question,
        tenant=tenant,
        sql=sql,
        columns=columns,
        rows=rows,
        row_count=len(rows),
    )
