"""SPEC-W66 WS1-A: RunRequest.lookback_days — per-run D9 lookback override.

Proves: the override is threaded to a per-run settings copy, is honored by
D9 ONLY (another detector's window is byte-identical), the app-level
Settings is never mutated, default behavior (absent field) is unchanged,
and non-positive values are rejected (422).
"""

from __future__ import annotations

import dataclasses
from datetime import UTC, datetime, timedelta

import pytest
from fastapi.testclient import TestClient

from fraud_engine.config import Settings
from fraud_engine.detectors.base import parse_ts
from fraud_engine.main import create_app

from conftest import TENANT

TEST_INTERNAL_TOKEN = "test-fraud-internal-token"

SHA_A = "a" * 64


@pytest.fixture()
def api_settings():
    return dataclasses.replace(
        Settings(), sweep_enabled=False, kafka_enabled=False,
        internal_token=TEST_INTERNAL_TOKEN)


@pytest.fixture()
def api(client, publisher, api_settings):
    app = create_app(client=client, publisher=publisher, settings=api_settings)
    with TestClient(app, headers={"X-Internal-Token": TEST_INTERNAL_TOKEN}) as tc:
        yield tc


def _seed_old_duplicates(graph, tenant, *, days_old=60):
    """Two same-sha256 Document stubs OUTSIDE the default 30d D9 window."""
    created = (datetime.now(UTC) - timedelta(days=days_old)).isoformat()
    for i in range(2):
        graph.add_document(
            tenant,
            f"old-dup-{i}",
            sha256=SHA_A,
            phash=None,
            kind="receipt",
            status="extracted",
            created_at=created,
        )


def _detector_since(fake_client, marker):
    for cypher, params in fake_client.calls:
        if f"// detector:{marker}" in cypher:
            return parse_ts(params["since"])
    raise AssertionError(f"no query recorded for {marker}")


def test_lookback_override_reaches_d9(api, client, graph, api_settings):
    _seed_old_duplicates(graph, TENANT)
    # Default window (30d): the 60d-old duplicates are invisible.
    resp = api.post(
        "/v1/detect/run", json={"tenant_id": TENANT, "detector": "d9_document_abuse"}
    )
    assert resp.status_code == 200
    assert resp.json()["findings"] == 0
    # Override reaches D9: the same stubs trip S1.
    resp = api.post(
        "/v1/detect/run",
        json={"tenant_id": TENANT, "detector": "d9_document_abuse", "lookback_days": 90},
    )
    assert resp.status_code == 200
    body = resp.json()
    assert body["findings"] == 1
    assert body["alerts_created"] == 1


def test_lookback_override_window_param(api, client, graph):
    _seed_old_duplicates(graph, TENANT)
    before = datetime.now(UTC)
    resp = api.post(
        "/v1/detect/run",
        json={"tenant_id": TENANT, "detector": "d9_document_abuse", "lookback_days": 90},
    )
    assert resp.status_code == 200
    after = datetime.now(UTC)
    since = _detector_since(client, "d9_documents")
    # since ≈ now - 90d (bounded by the test's own runtime).
    assert before - timedelta(days=90, seconds=30) <= since <= after - timedelta(
        days=89, hours=23
    )


def test_lookback_override_is_d9_scoped(api, client, graph, api_settings):
    """A full sweep with lookback_days moves ONLY D9's window — D8's
    civic_report_lookback_days (7) is byte-identical."""
    _seed_old_duplicates(graph, TENANT)
    before = datetime.now(UTC)
    resp = api.post("/v1/detect/run", json={"tenant_id": TENANT, "lookback_days": 90})
    assert resp.status_code == 200
    assert resp.json()["findings"] == 1  # D9 tripped via the override
    d9_since = _detector_since(client, "d9_documents")
    assert d9_since <= before - timedelta(days=89)
    d8_since = _detector_since(client, "d8_report_velocity")
    # D8 default window = 7 days, utterly unaffected by the D9 override.
    assert before - timedelta(days=7, seconds=30) <= d8_since <= datetime.now(UTC) - timedelta(
        days=6, hours=23
    )
    # Per-run copy: the app-level Settings was never mutated.
    assert api_settings.d9_lookback_days == 30


def test_lookback_absent_default_unchanged(api, client, graph):
    """No lookback_days => default 30d behavior, byte-identical."""
    _seed_old_duplicates(graph, TENANT)
    before = datetime.now(UTC)
    resp = api.post(
        "/v1/detect/run", json={"tenant_id": TENANT, "detector": "d9_document_abuse"}
    )
    assert resp.status_code == 200
    assert resp.json()["findings"] == 0
    since = _detector_since(client, "d9_documents")
    assert before - timedelta(days=30, seconds=30) <= since <= datetime.now(UTC) - timedelta(
        days=29, hours=23
    )


def test_lookback_validation(api):
    assert api.post("/v1/detect/run", json={"lookback_days": 0}).status_code == 422
    assert api.post("/v1/detect/run", json={"lookback_days": -3}).status_code == 422
    assert api.post("/v1/detect/run", json={"lookback_days": 1.5}).status_code == 422
    assert api.post("/v1/detect/run", json={"lookback_days": "abc"}).status_code == 422
    # Legacy bodies still accepted unchanged.
    assert api.post("/v1/detect/run", json={}).status_code == 200
    assert api.post("/v1/detect/run", json={"detector": "d9_document_abuse"}).status_code == 200
