"""I1: service boots with an EMPTY database and answers honestly —
404 / empty payloads, never 500s."""

from __future__ import annotations

import uuid

from conftest import TENANT_A
from model_registry.trainer import run_nightly_tick


def test_healthz_ok_on_empty_db(client):
    r = client.get("/healthz")
    assert r.status_code == 200
    assert r.json()["status"] == "ok"


def test_production_404_on_empty_db(client):
    r = client.get(f"/v1/registry/fraud-clf/{TENANT_A}/production")
    assert r.status_code == 404


def test_versions_empty_list(client):
    r = client.get(f"/v1/registry/fraud-clf/{TENANT_A}/versions")
    assert r.status_code == 200
    assert r.json()["versions"] == []


def test_assignment_champion_null_version_on_empty_db(client):
    r = client.get("/v1/registry/experiments/assignment", params={
        "family": "fraud-clf", "tenant_id": TENANT_A, "person_id": "p-1"})
    assert r.status_code == 200
    body = r.json()
    assert body["arm"] == "champion"          # fail-closed
    assert body["version"] is None            # honest: nothing to serve


def test_report_404_on_empty_db(client):
    r = client.get(f"/v1/registry/experiments/{uuid.uuid4()}/report")
    assert r.status_code == 404


def test_rollback_404_on_empty_db(client):
    r = client.post("/v1/registry/rollback", json={
        "family": "fraud-clf", "tenant_id": TENANT_A})
    assert r.status_code == 404


def test_nightly_tick_noop_on_empty_db(store):
    assert run_nightly_tick(store, {}) == []


def test_metrics_endpoint(client):
    r = client.get("/metrics")
    assert r.status_code == 200
    assert "opendesk_model_drift_psi" in r.text or r.text == ""  # gauge may be unset
