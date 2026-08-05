"""Health/metrics + segment-store persistence tests."""

from __future__ import annotations

from app.backend import InMemoryBackend
from app.config import Settings
from app.main import create_app
from app.store import SegmentStore
from fastapi.testclient import TestClient

from conftest import HDR_A, MARKETING_SEGMENT, StubLLM, build_graph


class DownBackend(InMemoryBackend):
    async def ping(self) -> bool:
        return False


def test_healthz_ok(client):
    resp = client.get("/healthz")
    assert resp.status_code == 200
    body = resp.json()
    assert body["status"] == "ok"
    assert body["graph"] is True


def test_healthz_degraded_when_graph_down(tmp_path):
    settings = Settings(graph_backend="memory", segment_store_dir=str(tmp_path / "s"))
    app = create_app(
        settings,
        backend=DownBackend(build_graph()),
        llm=StubLLM(),
        store=SegmentStore(str(tmp_path / "seg")),
    )
    resp = TestClient(app).get("/healthz")
    assert resp.status_code == 503
    assert resp.json()["status"] == "degraded"


def test_metrics_endpoint_exposes_prometheus(client):
    client.get("/v1/graph/segments", headers=HDR_A)
    resp = client.get("/metrics")
    assert resp.status_code == 200
    assert "graph_http_requests_total" in resp.text


def test_segment_store_survives_restart(tmp_path):
    store_dir = str(tmp_path / "store")
    settings = Settings(graph_backend="memory", segment_store_dir=store_dir)
    app1 = create_app(
        settings,
        backend=InMemoryBackend(build_graph()),
        llm=StubLLM(),
        store=SegmentStore(store_dir),
    )
    client1 = TestClient(app1)
    seg = client1.post("/v1/graph/segments", json=MARKETING_SEGMENT, headers=HDR_A).json()

    # "Restart": new store instance over the same directory.
    settings2 = Settings(graph_backend="memory", segment_store_dir=str(tmp_path / "other"))
    app2 = create_app(
        settings2,
        backend=InMemoryBackend(build_graph()),
        llm=StubLLM(),
        store=SegmentStore(store_dir),
    )
    client2 = TestClient(app2)
    resp = client2.get(f"/v1/graph/segments/{seg['id']}/count", headers=HDR_A)
    assert resp.status_code == 200
    assert resp.json()["count"] == 3
