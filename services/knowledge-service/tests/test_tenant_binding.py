"""SPEC-W43 C1 tenant binding — knowledge-service.

Unit matrix for db.bind_tenant_value/parse_tenant_slugs plus endpoint-level
proof on POST /v1/analytics/query that a missing gateway header is 401 and
a mismatched body tenant is 403 BEFORE any LLM/Trino/Dapr work happens.
"""

from __future__ import annotations

import sys

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

sys.path.insert(0, ".")

from app.analytics import router  # noqa: E402
from app.db import bind_tenant_value, parse_tenant_slugs  # noqa: E402
from fastapi import HTTPException  # noqa: E402

TENANT_A = "11111111-2222-3333-4444-555555555555"
TENANT_B = "99999999-8888-7777-6666-555555555555"


# ------------------------------------------------------------- unit matrix
def test_parse_header():
    assert parse_tenant_slugs(None) == []
    assert parse_tenant_slugs("") == []
    assert parse_tenant_slugs(f" {TENANT_A} , acme ,,{TENANT_B} ") == [
        TENANT_A, "acme", TENANT_B,
    ]


def test_single_slug_header_selects_tenant():
    assert bind_tenant_value([TENANT_A], None, trust_direct=False) == TENANT_A


def test_explicit_match_honored():
    assert bind_tenant_value([TENANT_A, TENANT_B], TENANT_B,
                             trust_direct=False) == TENANT_B


def test_explicit_mismatch_403():
    with pytest.raises(HTTPException) as ei:
        bind_tenant_value([TENANT_A], TENANT_B, trust_direct=False)
    assert ei.value.status_code == 403


def test_multi_entry_without_selector_400():
    with pytest.raises(HTTPException) as ei:
        bind_tenant_value([TENANT_A, TENANT_B], None, trust_direct=False)
    assert ei.value.status_code == 400


def test_no_header_401_by_default_even_with_explicit():
    with pytest.raises(HTTPException) as ei:
        bind_tenant_value([], TENANT_A, trust_direct=False)
    assert ei.value.status_code == 401


def test_dev_escape_restores_legacy_selection():
    assert bind_tenant_value([], TENANT_A, trust_direct=True) == TENANT_A
    with pytest.raises(HTTPException) as ei:
        bind_tenant_value([], None, trust_direct=True)
    assert ei.value.status_code == 401


# ------------------------------------------------- endpoint-level (router)
def _client() -> TestClient:
    app = FastAPI()
    app.include_router(router)
    return TestClient(app, raise_server_exceptions=False)


def test_analytics_query_without_gateway_header_is_401():
    with _client() as c:
        r = c.post("/v1/analytics/query",
                   json={"tenant": TENANT_A, "question": "bookings last week?"})
        assert r.status_code == 401, r.text


def test_analytics_query_body_tenant_mismatch_is_403():
    with _client() as c:
        r = c.post("/v1/analytics/query",
                   json={"tenant": TENANT_B, "question": "revenue?"},
                   headers={"X-Tenant-Slugs": TENANT_A})
        assert r.status_code == 403, r.text
