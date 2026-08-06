"""HTTP API tests: /healthz, POST /v1/score/run, GET /v1/score/status
(SPEC-W29 §3 WS-A main.py); W31: POST /v1/score/train + healthz GNN fields."""

from __future__ import annotations

import os
from types import SimpleNamespace

from fastapi.testclient import TestClient

from graph_ml import gnn_train
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


def make_gnn_client(monkeypatch, tenant_graph, tenants=None, **overrides):
    """Client in gnn mode: backend resolution forced, train_tenant stubbed.

    torch is never installed in this suite — the gnn_train import is the
    only seam, and it is monkeypatched per test."""
    import graph_ml.main as main_mod

    monkeypatch.setattr(main_mod, "resolve_backend", lambda settings: "gnn")
    settings = Settings(
        top_k=5,
        tenant_concurrency=1,
        internal_token="tok",
        score_interval_minutes=60,
        backend="gnn",
        **overrides,
    )
    app = create_app(
        settings,
        graph_client=StaticGraphClient(tenants or {"t1": tenant_graph}),
        writer=RecordingWriter(),
        enable_scheduler=False,
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
    assert after["last_sweep"]["tenants"]["tenant_id"] == "t1"
    assert after["backend"] == "heuristic"


def test_healthz_reports_gnn_fallback_backend(tenant_graph):
    client = make_client(tenant_graph, backend="gnn")
    body = client.get("/healthz").json()
    assert body["backend"] == "heuristic"  # degraded, still serving
    assert body["gnn_available"] is False


# --- W31: POST /v1/score/train + healthz GNN model stats -------------------


def test_train_409_in_heuristic_mode(tenant_graph):
    """Honest degradation (G6): the heuristic base image refuses training."""
    client = make_client(tenant_graph)
    resp = client.post("/v1/score/train", json={})
    assert resp.status_code == 409
    assert resp.json() == {"error": "gnn backend not enabled"}


def test_train_gnn_mode_trains_tenants(tenant_graph, monkeypatch, tmp_path):
    result = SimpleNamespace(model_version="graphsage-v1", final_loss=0.42)
    monkeypatch.setattr(gnn_train, "train_tenant", lambda g, tid, s: result)
    client = make_gnn_client(monkeypatch, tenant_graph, model_dir=str(tmp_path))
    resp = client.post("/v1/score/train", json={"tenant_id": "t1"})
    assert resp.status_code == 200
    body = resp.json()
    assert body["ok"] is True
    assert body["run_id"] == 1
    assert body["trained"] == [
        {"tenant_id": "t1", "model_version": "graphsage-v1", "final_loss": 0.42}
    ]
    assert body["skipped"] == []


def test_train_insufficient_data_lands_in_skipped(tenant_graph, monkeypatch, tmp_path):
    def raise_insufficient(graph, tid, settings):
        raise gnn_train.GNNInsufficientData(f"tenant {tid}: 5 persons < 20")

    monkeypatch.setattr(gnn_train, "train_tenant", raise_insufficient)
    client = make_gnn_client(monkeypatch, tenant_graph, model_dir=str(tmp_path))
    resp = client.post("/v1/score/train", json={})
    assert resp.status_code == 200
    body = resp.json()
    assert body["ok"] is True
    assert body["trained"] == []
    assert body["skipped"][0]["tenant_id"] == "t1"
    assert "insufficient data" in body["skipped"][0]["reason"]


def test_train_error_skipped_never_fails_run(tenant_graph, monkeypatch, tmp_path):
    """One tenant trains, one raises mid-training: skipped, ok stays True."""
    other = TenantGraph(tenant_id="t2", persons=list(tenant_graph.persons[:2]))

    def flaky_train(graph, tid, settings):
        if tid == "t2":
            raise RuntimeError("simulated cuda oom")
        return SimpleNamespace(model_version="graphsage-v3", final_loss=0.1)

    monkeypatch.setattr(gnn_train, "train_tenant", flaky_train)
    client = make_gnn_client(
        monkeypatch,
        tenant_graph,
        tenants={"t1": tenant_graph, "t2": other},
        model_dir=str(tmp_path),
    )
    resp = client.post("/v1/score/train", json={})
    assert resp.status_code == 200
    body = resp.json()
    assert body["ok"] is True
    assert [t["tenant_id"] for t in body["trained"]] == ["t1"]
    assert body["skipped"] == [
        {"tenant_id": "t2", "reason": "train error: RuntimeError: simulated cuda oom"}
    ]


def test_train_unknown_tenant_is_skipped_not_error(tenant_graph, monkeypatch, tmp_path):
    seen = []
    monkeypatch.setattr(
        gnn_train,
        "train_tenant",
        lambda g, tid, s: seen.append(tid)
        or SimpleNamespace(model_version="graphsage-v1", final_loss=0.0),
    )
    client = make_gnn_client(monkeypatch, tenant_graph, model_dir=str(tmp_path))
    resp = client.post("/v1/score/train", json={"tenant_id": "ghost"})
    assert resp.status_code == 200
    body = resp.json()
    assert body["ok"] is True
    assert body["trained"] == []
    assert body["skipped"][0]["tenant_id"] == "ghost"
    assert body["skipped"][0]["reason"].startswith("no data")
    assert seen == []  # train_tenant never called for an unfetchable graph


def test_healthz_gnn_model_fields(tenant_graph, tmp_path):
    """healthz counts tenant subdirs with a graphsage-v* dir (W31 §2)."""
    (tmp_path / "t1" / "graphsage-v2").mkdir(parents=True)
    (tmp_path / "t2").mkdir()  # tenant dir without a trained model: not counted
    client = make_client(tenant_graph, model_dir=str(tmp_path))
    resp = client.get("/healthz")
    assert resp.status_code == 200
    body = resp.json()
    assert body["gnn_models_dir"] == str(tmp_path)
    assert body["gnn_tenants_with_models"] == 1


def test_healthz_gnn_fields_omitted_on_fs_error(tenant_graph, monkeypatch, tmp_path):
    """Best-effort: a filesystem error omits the fields, never a 500."""
    monkeypatch.setattr(
        os.path, "isdir", lambda p: (_ for _ in ()).throw(OSError("disk gone"))
    )
    client = make_client(tenant_graph, model_dir=str(tmp_path))
    resp = client.get("/healthz")
    assert resp.status_code == 200
    body = resp.json()
    assert body["status"] == "ok"
    assert "gnn_models_dir" not in body
    assert "gnn_tenants_with_models" not in body
