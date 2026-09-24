"""W46-F P12: batched internal write-back — one backend call per request.

The /v1/graph/internal/scores and /internal/recommendations endpoints used
to loop ``run_write`` per item (N×(check+write) round trips). They now
compile the whole batch into a single CompiledBatchWrite (one tenant
pre-check + one UNWIND statement per score-field group) while preserving
the per-item response semantics (written/skipped counts + ids, 422 on
cross-tenant endpoints, no stub nodes for unknown persons).
"""

from __future__ import annotations

import pytest
from fastapi.testclient import TestClient

from app.backend import InMemoryBackend
from app.config import Settings
from app.main import create_app
from app.store import SegmentStore
from conftest import StubLLM, build_graph

TOKEN = "internal-test-token"


@pytest.fixture()
def counting_client(tmp_path):
    settings = Settings(
        graph_backend="memory",
        segment_store_dir=str(tmp_path / "store"),
        jwt_public_key="",
        internal_token=TOKEN,
    )
    backend = InMemoryBackend(build_graph())
    app = create_app(
        settings,
        backend=backend,
        llm=StubLLM(),
        store=SegmentStore(str(tmp_path / "seg")),
    )
    calls: list[tuple[str, int]] = []

    async def _counting_execute_batch_write(batch, tenant_id):
        calls.append((type(batch.plan).__name__, len(batch.plan.items)))
        return await InMemoryBackend.execute_batch_write(backend, batch, tenant_id)

    backend.execute_batch_write = _counting_execute_batch_write  # type: ignore[method-assign]
    return TestClient(app), backend, calls


def _score_item(person_id="pa1", tenant="tenant-a", **scores):
    item = {
        "tenant_id": tenant,
        "person_id": person_id,
        "model_version": "heuristic-v1",
        "scored_at": "2026-08-05T00:00:00+00:00",
    }
    item.update(scores)
    return item


def test_scores_batch_is_one_backend_call(counting_client):
    client, backend, calls = counting_client
    resp = client.post(
        "/v1/graph/internal/scores",
        json={
            "tenant_id": "tenant-a",
            "scores": [
                _score_item("pa1", propensity_churn=0.8, risk_score=0.2),
                _score_item("pa2", propensity_convert=0.4),
                _score_item("ghost-1", risk_score=0.9),
            ],
        },
        headers={"X-Internal-Token": TOKEN},
    )
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["written"] == 2
    assert body["skipped_unknown"] == 1
    assert body["skipped_unknown_ids"] == ["ghost-1"]
    # ONE batched backend call carried all three items (mixed field sets are
    # grouped server-side; the router never loops per item).
    assert calls == [("ScoreBatchWritePlan", 3)]
    assert backend.graph.nodes["tenant-a:pa1"].props["propensity_churn"] == 0.8
    assert backend.graph.nodes["tenant-a:pa2"].props["propensity_convert"] == 0.4
    assert "tenant-a:ghost-1" not in backend.graph.nodes


def test_recommendations_batch_is_one_backend_call(counting_client):
    client, backend, calls = counting_client
    resp = client.post(
        "/v1/graph/internal/recommendations",
        json={
            "tenant_id": "tenant-a",
            "recommendations": [
                {
                    "tenant_id": "tenant-a",
                    "person_id": "pa1",
                    "offering_id": "o1",
                    "score": 0.9,
                    "rank": 1,
                },
                {
                    "tenant_id": "tenant-a",
                    "person_id": "nope",
                    "offering_id": "o1",
                    "score": 0.1,
                    "rank": 2,
                },
            ],
        },
        headers={"X-Internal-Token": TOKEN},
    )
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["written"] == 1
    assert body["skipped"] == [{"person_id": "nope", "offering_id": "o1"}]
    assert calls == [("RecommendationBatchWritePlan", 2)]
