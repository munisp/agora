"""SPEC-W45 K23 (OOS-08): X-Internal-Token gate on the mutating registry
routes (register/promote/rollback) and every experiments route.

Matrix: unset server token -> 503 fail-closed; missing/wrong token -> 401;
correct token -> works. Read-only lookups stay reachable without a token
(contract scope: only the routes above are gated).
"""

from __future__ import annotations

import dataclasses

import pytest
from fastapi.testclient import TestClient

from conftest import TENANT_A, TEST_INTERNAL_TOKEN
from model_registry.main import create_app


@pytest.fixture()
def raw_client(settings, store):
    """TestClient WITHOUT the default token header (auth matrix)."""
    app = create_app(settings=settings, store=store, enable_scheduler=False)
    with TestClient(app) as c:
        yield c


def _register_payload():
    return {"family": "fraud-clf", "tenant_id": TENANT_A,
            "artifact_uri": "s3://lake/models/fraud-clf/v1"}


GATED_POSTS = [
    "/v1/registry/register",
    "/v1/registry/promote",
    "/v1/registry/rollback",
    "/v1/registry/experiments",
]

GATED_GETS = [
    "/v1/registry/experiments/assignment?family=fraud-clf"
    f"&tenant_id={TENANT_A}&person_id=p1",
]


def test_missing_token_401_on_gated_posts(raw_client):
    for path in GATED_POSTS:
        resp = raw_client.post(path, json=_register_payload())
        assert resp.status_code == 401, (path, resp.text)


def test_wrong_token_401(raw_client):
    resp = raw_client.post("/v1/registry/register", json=_register_payload(),
                           headers={"X-Internal-Token": "wrong"})
    assert resp.status_code == 401
    resp = raw_client.get(GATED_GETS[0],
                          headers={"X-Internal-Token": "wrong"})
    assert resp.status_code == 401


def test_missing_token_401_on_gated_gets(raw_client):
    for path in GATED_GETS:
        resp = raw_client.get(path)
        assert resp.status_code == 401, (path, resp.text)


def test_correct_token_passes(raw_client):
    resp = raw_client.post("/v1/registry/register", json=_register_payload(),
                           headers={"X-Internal-Token": TEST_INTERNAL_TOKEN})
    assert resp.status_code == 201, resp.text
    resp = raw_client.get(GATED_GETS[0],
                          headers={"X-Internal-Token": TEST_INTERNAL_TOKEN})
    assert resp.status_code == 200, resp.text


def test_unset_server_token_503_fail_closed(settings, store):
    insecure = dataclasses.replace(settings, internal_token=None)
    app = create_app(settings=insecure, store=store, enable_scheduler=False)
    with TestClient(app) as c:
        resp = c.post("/v1/registry/register", json=_register_payload(),
                      headers={"X-Internal-Token": "anything"})
        assert resp.status_code == 503, resp.text


def test_readonly_routes_ungated(raw_client):
    # Contract scope: production/versions GETs and health/metrics stay open.
    resp = raw_client.get("/healthz")
    assert resp.status_code == 200
    resp = raw_client.get(f"/v1/registry/fraud-clf/{TENANT_A}/versions")
    assert resp.status_code == 200, resp.text
