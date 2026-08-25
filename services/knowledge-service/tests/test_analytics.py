"""SPEC-W43 Y-02: the analytics query endpoint NEVER returns bound SQL or
backend error text to clients — generic 400s, detail logged server-side.
"""

from __future__ import annotations

import sys

import pytest
from fastapi import HTTPException

sys.path.insert(0, ".")

from app import analytics  # noqa: E402

pytestmark = pytest.mark.asyncio

TENANT = "11111111-2222-3333-4444-555555555555"
SENTINEL = "supersecret-backend-detail-9f3a"


async def test_llm_failure_is_generic_400(monkeypatch):
    async def _boom(question, *, client=None):
        raise RuntimeError(SENTINEL)

    monkeypatch.setattr(analytics, "generate_sql", _boom)
    with pytest.raises(HTTPException) as ei:
        await analytics.run_analytics_query(TENANT, "revenue last week?")
    assert ei.value.status_code == 400
    assert SENTINEL not in str(ei.value.detail)


async def test_guard_rejection_does_not_echo_sql(monkeypatch):
    async def _evil_sql(question, *, client=None):
        return "DROP TABLE gold.revenue_daily"

    monkeypatch.setattr(analytics, "generate_sql", _evil_sql)
    with pytest.raises(HTTPException) as ei:
        await analytics.run_analytics_query(TENANT, "drop everything")
    assert ei.value.status_code == 400
    detail = str(ei.value.detail)
    assert "DROP TABLE" not in detail  # offending LLM SQL not echoed
    assert "guardrails" not in detail  # guard internals not echoed


async def test_trino_error_text_not_returned(monkeypatch):
    async def _ok_sql(question, *, client=None):
        return "SELECT day FROM gold.revenue_daily"

    async def _boom(sql, *, client=None):
        raise analytics.TrinoExecutionError(SENTINEL)

    monkeypatch.setattr(analytics, "generate_sql", _ok_sql)
    monkeypatch.setattr(analytics, "execute_trino", _boom)
    with pytest.raises(HTTPException) as ei:
        await analytics.run_analytics_query(TENANT, "revenue?")
    assert ei.value.status_code == 400
    detail = str(ei.value.detail)
    assert SENTINEL not in detail
    assert TENANT not in detail  # the bound predicate is not echoed either


async def test_success_response_has_no_bound_sql(monkeypatch):
    async def _ok_sql(question, *, client=None):
        return "SELECT day FROM gold.revenue_daily"

    async def _ok_exec(sql, *, client=None):
        return ["day"], [["2026-08-01"]]

    monkeypatch.setattr(analytics, "generate_sql", _ok_sql)
    monkeypatch.setattr(analytics, "execute_trino", _ok_exec)
    resp = await analytics.run_analytics_query(TENANT, "revenue?")
    payload = resp.model_dump()
    assert "sql" not in payload
    assert payload["columns"] == ["day"]
    assert payload["rows"] == [["2026-08-01"]]
    assert payload["truncated"] is False
