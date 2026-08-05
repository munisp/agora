"""HTTP API tests: /healthz, POST /v1/score/run, GET /v1/score/status
(SPEC-W29 §3 WS-A main.py)."""

from __future__ import annotations

from fastapi.testclient import TestClient

from graph_ml.config import Settings
from graph_ml.extract import StaticGraphClient, TenantGraph
from graph_ml.main import create_app

from .test_score_sweep import RecordingWriter


def make_client(tenant_graph, **overrides) -> TestClient:
    settings = Settings(
        top_k=5,
        tenant_concurrency=1,
        internal_token="tok",
        score_interval_minutes=60,
        **overrides,
    )
    app = create_app(
        settings,
        graph_client=StaticGraphClient({"t1": tenant_graph}),
        writer=RecordingWriter(),
        enable_scheduler=False,  # tests never start the APScheduler loop
    )
    return TestClient(app)


def test_healthz(tenant_graph):
    client = make_client(tenant_graph)
    resp = client.get("/healthz")
    assert resp.status_code == 200
    body = resp.json()
    assert body["status"] == "ok"
    assert body["service"] == "graph-ml"
    assert body["backend"] == "heuristic"
    assert body["gnn_available"] is False


def test_score_run_full_sweep(tenant_graph):
    client = make_client(tenant_graph)
    resp = client.post("/v1/score/run", json={})
    assert resp.status_code == 200
    body = resp.json()
    assert body["ok"] is True
    assert body["backend"] == "heuristic"
    assert body["run_id"] == 1
    assert body["tenants"][0]["tenant_id"] == "t1"
    assert body["tenants"][0]["persons_scored"] == 5
    assert body["tenants"][0]["model_version"] == "heuristic-v1"


def test_score_run_single_tenant(tenant_graph):
    client = make_client(tenant_graph)
    resp = client.post("/v1/score/run", json={"tenant_id": "t1"})
    assert resp.status_code == 200
    assert [t["tenant_id"] for t in resp.json()["tenants"]] == ["t1"]


def test_score_status_reports_last_sweep(tenant_graph):
    client = make_client(tenant_graph)
    before = client.get("/v1/score/status").json()
    assert before["run_count"] == 0
    assert before["last_sweep"] is None
    assert before["top_k"] == 5

    client.post("/v1/score/run", json={"tenant_id": "t1"})
    after = client.get("/v1/score/status").json()
    assert after["run_count"] == 1
    assert after["running"] is False
    assert after["last_sweep"]["ok"] is True
    assert after["last_sweep"]["tenants"][0]["tenant_id"] == "t1"
    assert after["backend"] == "heuristic"


def test_healthz_reports_gnn_fallback_backend(tenant_graph):
    client = make_client(tenant_graph, backend="gnn")
    body = client.get("/healthz").json()
    assert body["backend"] == "heuristic"  # degraded, still serving
    assert body["gnn_available"] is False
