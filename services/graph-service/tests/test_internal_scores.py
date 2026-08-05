"""SPEC-W29 §3 WS-B: internal score/recommendation write-back API.

Covers: X-Internal-Token auth (missing/wrong/correct), JWT never accepted on
internal routes, per-item tenant validation, cross-tenant write-back
rejection (4xx), MERGE-overwrite keeps the latest score, recommendation
endpoint verification + skip of missing nodes, and the
``scores_written_total{tenant}`` metric.
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import json

import pytest
from fastapi.testclient import TestClient

from app.backend import InMemoryBackend
from app.config import Settings
from app.main import create_app
from app.store import SegmentStore
from conftest import HDR_A, StubLLM, build_graph

TOKEN = "internal-test-token"


def _hs256_token(claims: dict, secret: str) -> str:
    def enc(obj) -> str:
        raw = json.dumps(obj, separators=(",", ":")).encode()
        return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()

    header = enc({"alg": "HS256", "typ": "JWT"})
    payload = enc(claims)
    signing = f"{header}.{payload}"
    sig = hmac.new(secret.encode(), signing.encode(), hashlib.sha256).digest()
    return f"{signing}.{base64.urlsafe_b64encode(sig).rstrip(b'=').decode()}"


@pytest.fixture()
def internal_client(tmp_path):
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
    return TestClient(app), backend


def _score_item(person_id="pa1", tenant="tenant-a", **scores):
    item = {
        "tenant_id": tenant,
        "person_id": person_id,
        "model_version": "heuristic-v1",
        "scored_at": "2026-08-05T00:00:00+00:00",
    }
    item.update(scores)
    return item


# ---------------------------------------------------------------- auth
def test_scores_missing_token_401(internal_client):
    client, _ = internal_client
    resp = client.post(
        "/v1/graph/internal/scores",
        json={"tenant_id": "tenant-a", "scores": [_score_item()]},
    )
    assert resp.status_code == 401


def test_scores_wrong_token_401(internal_client):
    client, _ = internal_client
    resp = client.post(
        "/v1/graph/internal/scores",
        json={"tenant_id": "tenant-a", "scores": [_score_item()]},
        headers={"X-Internal-Token": "wrong-token"},
    )
    assert resp.status_code == 401


def test_scores_jwt_never_accepted(tmp_path):
    """Even a VALID JWT (sub=tenant-a) must not authenticate internal routes."""
    secret = "jwt-secret"
    settings = Settings(
        graph_backend="memory",
        segment_store_dir=str(tmp_path / "store"),
        jwt_public_key=secret,
        jwt_algorithm="HS256",
        internal_token=TOKEN,
    )
    app = create_app(
        settings,
        backend=InMemoryBackend(build_graph()),
        llm=StubLLM(),
        store=SegmentStore(str(tmp_path / "seg")),
    )
    client = TestClient(app)
    jwt = _hs256_token({"sub": "tenant-a"}, secret)
    resp = client.post(
        "/v1/graph/internal/scores",
        json={"tenant_id": "tenant-a", "scores": [_score_item()]},
        headers={"Authorization": f"Bearer {jwt}"},
    )
    assert resp.status_code == 401


def test_scores_unconfigured_token_fails_closed(tmp_path):
    settings = Settings(
        graph_backend="memory",
        segment_store_dir=str(tmp_path / "store"),
        jwt_public_key="",
        internal_token="",  # not configured
    )
    app = create_app(
        settings,
        backend=InMemoryBackend(build_graph()),
        llm=StubLLM(),
        store=SegmentStore(str(tmp_path / "seg")),
    )
    resp = TestClient(app).post(
        "/v1/graph/internal/scores",
        json={"tenant_id": "tenant-a", "scores": [_score_item()]},
        headers={"X-Internal-Token": TOKEN},
    )
    assert resp.status_code == 401


# ---------------------------------------------------------------- scores
def test_scores_happy_path_writes_props(internal_client):
    client, backend = internal_client
    resp = client.post(
        "/v1/graph/internal/scores",
        json={
            "tenant_id": "tenant-a",
            "scores": [
                _score_item("pa1", propensity_churn=0.82, risk_score=0.1),
                _score_item("pa2", propensity_convert=0.4),
            ],
        },
        headers={"X-Internal-Token": TOKEN},
    )
    assert resp.status_code == 200, resp.text
    assert resp.json()["written"] == 2
    pa1 = backend.graph.nodes["tenant-a:pa1"]
    assert pa1.props["propensity_churn"] == 0.82
    assert pa1.props["risk_score"] == 0.1
    assert pa1.props["model_version"] == "heuristic-v1"
    assert pa1.props["scored_at"] == "2026-08-05T00:00:00+00:00"
    assert backend.graph.nodes["tenant-a:pa2"].props["propensity_convert"] == 0.4


def test_scores_merge_overwrite_keeps_latest(internal_client):
    client, backend = internal_client
    body = {"tenant_id": "tenant-a", "scores": [_score_item("pa1", propensity_churn=0.5)]}
    hdr = {"X-Internal-Token": TOKEN}
    assert client.post("/v1/graph/internal/scores", json=body, headers=hdr).status_code == 200
    newer = _score_item("pa1", propensity_churn=0.9)
    newer["model_version"] = "graphsage-v1"
    newer["scored_at"] = "2026-08-06T00:00:00+00:00"
    assert (
        client.post(
            "/v1/graph/internal/scores",
            json={"tenant_id": "tenant-a", "scores": [newer]},
            headers=hdr,
        ).status_code
        == 200
    )
    pa1 = backend.graph.nodes["tenant-a:pa1"]
    assert pa1.props["propensity_churn"] == 0.9
    assert pa1.props["model_version"] == "graphsage-v1"
    assert pa1.props["scored_at"] == "2026-08-06T00:00:00+00:00"


def test_scores_item_tenant_mismatch_rejected(internal_client):
    """Item tenant != envelope tenant -> 422, nothing written."""
    client, backend = internal_client
    resp = client.post(
        "/v1/graph/internal/scores",
        json={"tenant_id": "tenant-b", "scores": [_score_item("pa1", tenant="tenant-a", propensity_churn=0.9)]},
        headers={"X-Internal-Token": TOKEN},
    )
    assert resp.status_code == 422
    assert "propensity_churn" not in backend.graph.nodes["tenant-a:pa1"].props


def test_scores_cross_tenant_node_rejected(internal_client):
    """Score for tenant A's person submitted UNDER tenant B -> 4xx, and no
    stub person is created in tenant B."""
    client, backend = internal_client
    resp = client.post(
        "/v1/graph/internal/scores",
        json={
            "tenant_id": "tenant-b",
            "scores": [_score_item("pa1", tenant="tenant-b", propensity_churn=0.9)],
        },
        headers={"X-Internal-Token": TOKEN},
    )
    assert 400 <= resp.status_code < 500
    assert "propensity_churn" not in backend.graph.nodes["tenant-a:pa1"].props
    assert "tenant-b:pa1" not in backend.graph.nodes


def test_scores_unknown_person_skipped_not_created(internal_client):
    """WARN #4: scoring a person the graph doesn't know must NOT create a
    bare stub Person; the item is skipped and counted in the response."""
    client, backend = internal_client
    before = set(backend.graph.nodes)
    resp = client.post(
        "/v1/graph/internal/scores",
        json={
            "tenant_id": "tenant-a",
            "scores": [
                _score_item("ghost-1", propensity_churn=0.9),
                _score_item("pa1", propensity_churn=0.5),
            ],
        },
        headers={"X-Internal-Token": TOKEN},
    )
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["written"] == 1
    assert body["skipped_unknown"] == 1
    assert body["skipped_unknown_ids"] == ["ghost-1"]
    assert set(backend.graph.nodes) == before  # no stub created
    assert backend.graph.nodes["tenant-a:pa1"].props["propensity_churn"] == 0.5


def test_scores_all_unknown_writes_nothing(internal_client):
    client, backend = internal_client
    before = set(backend.graph.nodes)
    resp = client.post(
        "/v1/graph/internal/scores",
        json={
            "tenant_id": "tenant-a",
            "scores": [_score_item("ghost-1", risk_score=0.3), _score_item("ghost-2", risk_score=0.4)],
        },
        headers={"X-Internal-Token": TOKEN},
    )
    assert resp.status_code == 200
    assert resp.json()["written"] == 0
    assert resp.json()["skipped_unknown"] == 2
    assert set(backend.graph.nodes) == before


def test_scores_visible_on_person_360_projection(internal_client):
    """BLOCKER-2a: person_by_id projects the W29/W30 score fields."""
    client, _ = internal_client
    client.post(
        "/v1/graph/internal/scores",
        json={
            "tenant_id": "tenant-a",
            "scores": [
                _score_item("pa1", propensity_churn=0.82, propensity_convert=0.4, risk_score=0.1)
            ],
        },
        headers={"X-Internal-Token": TOKEN},
    )
    resp = client.get("/v1/graph/persons/pa1", headers=HDR_A)
    assert resp.status_code == 200, resp.text
    person = resp.json()["person"]
    assert person["propensity_churn"] == 0.82
    assert person["propensity_convert"] == 0.4
    assert person["risk_score"] == 0.1
    assert person["model_version"] == "heuristic-v1"
    assert person["scored_at"] == "2026-08-05T00:00:00+00:00"
    assert person["propensity_turnout"] is None  # unscored -> null
    assert person["risk_flags"] == []
    # Unscored person: all score keys present and null.
    other = client.get("/v1/graph/persons/pa2", headers=HDR_A).json()["person"]
    assert other["propensity_churn"] is None
    assert other["risk_score"] is None


def test_scores_requires_at_least_one_score_field(internal_client):
    client, _ = internal_client
    resp = client.post(
        "/v1/graph/internal/scores",
        json={"tenant_id": "tenant-a", "scores": [_score_item("pa1")]},
        headers={"X-Internal-Token": TOKEN},
    )
    assert resp.status_code == 422


def test_scores_metric_emitted(internal_client):
    client, _ = internal_client
    import re

    def _value() -> float:
        match = re.search(
            r'scores_written_total\{tenant="tenant-a"\} (\d+\.0)',
            client.get("/metrics").text,
        )
        return float(match.group(1)) if match else 0.0

    before = _value()
    client.post(
        "/v1/graph/internal/scores",
        json={"tenant_id": "tenant-a", "scores": [_score_item("pa1", propensity_churn=0.7)]},
        headers={"X-Internal-Token": TOKEN},
    )
    assert _value() == before + 1.0


# ---------------------------------------------------------- recommendations
def _rec_item(person_id="pa1", offering_id="o1", tenant="tenant-a", rank=1, score=0.9):
    return {
        "tenant_id": tenant,
        "person_id": person_id,
        "offering_id": offering_id,
        "score": score,
        "rank": rank,
        "reason": "booked_cleaning_2x",
        "model_version": "heuristic-v1",
        "scored_at": "2026-08-05T00:00:00+00:00",
    }


def test_recommendations_write_and_merge_overwrite(internal_client):
    client, backend = internal_client
    hdr = {"X-Internal-Token": TOKEN}
    resp = client.post(
        "/v1/graph/internal/recommendations",
        json={"tenant_id": "tenant-a", "recommendations": [_rec_item()]},
        headers=hdr,
    )
    assert resp.status_code == 200, resp.text
    assert resp.json()["written"] == 1
    edges = backend.graph.edges_from("tenant-a:pa1", "RECOMMENDED_FOR")
    assert len(edges) == 1 and edges[0].props["score"] == 0.9
    # Re-score overwrites in place (MERGE on person+offering keeps latest).
    resp = client.post(
        "/v1/graph/internal/recommendations",
        json={"tenant_id": "tenant-a", "recommendations": [_rec_item(score=0.4, rank=2)]},
        headers=hdr,
    )
    assert resp.status_code == 200
    edges = backend.graph.edges_from("tenant-a:pa1", "RECOMMENDED_FOR")
    assert len(edges) == 1
    assert edges[0].props["score"] == 0.4 and edges[0].props["rank"] == 2


def test_recommendations_cross_tenant_offering_rejected(internal_client):
    """Offering 'o1' exists only in tenant A; writing it under tenant B must
    be a 4xx (endpoint tenant verification before MERGE)."""
    client, backend = internal_client
    resp = client.post(
        "/v1/graph/internal/recommendations",
        json={
            "tenant_id": "tenant-b",
            "recommendations": [_rec_item(person_id="pb1", tenant="tenant-b")],
        },
        headers={"X-Internal-Token": TOKEN},
    )
    assert 400 <= resp.status_code < 500
    assert backend.graph.edges_from("tenant-b:pb1", "RECOMMENDED_FOR") == []


def test_recommendations_missing_nodes_skipped(internal_client):
    client, _ = internal_client
    resp = client.post(
        "/v1/graph/internal/recommendations",
        json={
            "tenant_id": "tenant-a",
            "recommendations": [
                _rec_item(person_id="nope"),
                _rec_item(person_id="pa1", offering_id="nope2"),
                _rec_item(),
            ],
        },
        headers={"X-Internal-Token": TOKEN},
    )
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["written"] == 1
    assert len(body["skipped"]) == 2


def test_recommendations_item_tenant_mismatch_rejected(internal_client):
    client, _ = internal_client
    resp = client.post(
        "/v1/graph/internal/recommendations",
        json={
            "tenant_id": "tenant-a",
            "recommendations": [_rec_item(tenant="tenant-b")],
        },
        headers={"X-Internal-Token": TOKEN},
    )
    assert resp.status_code == 422


def test_internal_routes_ignore_tenant_header(internal_client):
    """X-Tenant-Id (dev JWT fallback) must not authenticate internal routes."""
    client, _ = internal_client
    resp = client.post(
        "/v1/graph/internal/scores",
        json={"tenant_id": "tenant-a", "scores": [_score_item(propensity_churn=0.5)]},
        headers=HDR_A,
    )
    assert resp.status_code == 401
