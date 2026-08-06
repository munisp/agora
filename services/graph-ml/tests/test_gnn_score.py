"""GNN inference + score-sweep integration tests (SPEC-W31 §1 + §3 gates G1/G3):
already-booked exclusion, provenance (recs graphsage-vN, propensity
heuristic-v1), per-tenant fallback ladder. Requires torch."""

from __future__ import annotations

import pytest

torch = pytest.importorskip("torch")
pytest.importorskip("torch_geometric")

pytestmark = pytest.mark.requires_torch

from datetime import datetime, timezone

from graph_ml import gnn as gnn_mod  # module ref: test_gnn.py reloads gnn
from graph_ml.config import Settings
from graph_ml.extract import StaticGraphClient
from graph_ml.gnn_train import train_tenant
from graph_ml.score import score_one_tenant
from tests.test_gnn_train import make_toy_graph, train_settings

NOW = datetime(2026, 8, 5, 12, 0, 0, tzinfo=timezone.utc)


class RecordingWriter:
    def __init__(self) -> None:
        self.score_posts: list[tuple[str, list[dict]]] = []
        self.rec_posts: list[tuple[str, list[dict]]] = []

    def post_scores(self, tenant_id, scores):
        self.score_posts.append((tenant_id, scores))
        return len(scores)

    def post_recommendations(self, tenant_id, recs):
        self.rec_posts.append((tenant_id, recs))
        return len(recs)

    def close(self):
        pass


def trained_backend(tmp_path, tenant_id="t1"):
    settings = train_settings(tmp_path)
    train_tenant(make_toy_graph(tenant_id), tenant_id, settings)
    return gnn_mod.GraphSAGEBackend(str(tmp_path)), settings


def test_inference_topk_excludes_already_booked(tmp_path):
    backend, _settings = trained_backend(tmp_path)
    graph = make_toy_graph()
    booked_by_person: dict[str, set[str]] = {}
    for b in graph.bookings:
        booked_by_person.setdefault(b.person_id, set()).add(b.offering_id)

    _scores, recs = backend.score_tenant(graph, now=NOW, top_k=3)
    assert recs
    per_person: dict[str, list] = {}
    for rec in recs:
        per_person.setdefault(rec.person_id, []).append(rec)
        assert rec.offering_id not in booked_by_person.get(rec.person_id, set())
    assert all(len(r) <= 3 for r in per_person.values())


def test_inference_provenance(tmp_path):
    backend, _settings = trained_backend(tmp_path)
    scores, recs = backend.score_tenant(make_toy_graph(), now=NOW, top_k=3)
    assert backend.model_version == "graphsage-v1"
    # G3: recommendations carry graphsage-vN + reason + scored_at
    assert all(r.model_version == "graphsage-v1" for r in recs)
    assert {r.reason for r in recs} == {
        f"graphsage link_prediction rank={i}" for i in (1, 2, 3)
    } & {r.reason for r in recs}
    assert all(r.reason.startswith("graphsage link_prediction rank=") for r in recs)
    assert all(r.scored_at == NOW.isoformat() for r in recs)
    assert all(0.0 <= r.score <= 1.0 for r in recs)
    assert all(r.rank == i for r in recs for i in [r.rank])
    # G3/R5: propensity scores stay heuristic-v1 this wave
    assert scores
    assert all(s.model_version == "heuristic-v1" for s in scores)
    assert len(scores) == 30


def test_inference_ranks_are_sequential(tmp_path):
    backend, _settings = trained_backend(tmp_path)
    _scores, recs = backend.score_tenant(make_toy_graph(), now=NOW, top_k=2)
    per_person: dict[str, list[int]] = {}
    for rec in recs:
        per_person.setdefault(rec.person_id, []).append(rec.rank)
    assert all(ranks == sorted(ranks) for ranks in per_person.values())


def test_no_model_raises_gnn_model_not_found(tmp_path):
    backend = gnn_mod.GraphSAGEBackend(str(tmp_path))
    with pytest.raises(gnn_mod.GNNModelNotFound):
        backend.score_tenant(make_toy_graph(), now=NOW, top_k=3)


def test_per_tenant_isolation_other_tenant_model_invisible(tmp_path):
    train_tenant(make_toy_graph("t1"), "t1", train_settings(tmp_path))
    backend = gnn_mod.GraphSAGEBackend(str(tmp_path))
    with pytest.raises(gnn_mod.GNNModelNotFound):  # t2 must not score with t1's model
        backend.score_tenant(make_toy_graph("t2"), now=NOW, top_k=3)


def test_score_one_tenant_gnn_no_model_falls_back(tmp_path):
    settings = train_settings(tmp_path, top_k=3, tenant_concurrency=1)
    client = StaticGraphClient({"t1": make_toy_graph()})
    result = score_one_tenant(settings, client, RecordingWriter(), "t1", "gnn")
    # G1: fallback, ok=True, heuristic-v1 — identical to W29 gate-5 behavior
    assert result.status == "ok"
    assert result.model_version == "heuristic-v1"
    assert result.persons_scored == 30
    assert result.recommendations > 0


def test_score_one_tenant_gnn_undersized_falls_back(tmp_path):
    settings = train_settings(tmp_path, top_k=3, tenant_concurrency=1)
    train_tenant(make_toy_graph("t1"), "t1", settings)
    tiny = make_toy_graph("t2")
    tiny.persons = tiny.persons[:5]  # trained model absent AND undersized
    client = StaticGraphClient({"t2": tiny})
    result = score_one_tenant(settings, client, RecordingWriter(), "t2", "gnn")
    assert result.status == "ok"
    assert result.model_version == "heuristic-v1"


def test_score_one_tenant_gnn_with_model_writes_provenance(tmp_path):
    settings = train_settings(tmp_path, top_k=3, tenant_concurrency=1)
    train_tenant(make_toy_graph(), "t1", settings)
    client = StaticGraphClient({"t1": make_toy_graph()})
    writer = RecordingWriter()
    result = score_one_tenant(settings, client, writer, "t1", "gnn")
    assert result.status == "ok"
    assert result.model_version == "graphsage-v1"

    (tid, score_payloads), = writer.score_posts
    assert tid == "t1"
    assert all(s["model_version"] == "heuristic-v1" for s in score_payloads)
    (tid, rec_payloads), = writer.rec_posts
    assert tid == "t1"
    assert rec_payloads
    assert all(r["model_version"] == "graphsage-v1" for r in rec_payloads)
    assert all(r["reason"].startswith("graphsage link_prediction rank=") for r in rec_payloads)
    assert all(r["scored_at"] for r in rec_payloads)
