"""Sweep orchestration tests: tenant-loop isolation, per-tenant write-back,
backend resolution (SPEC-W29 §3 WS-A)."""

from __future__ import annotations

from graph_ml.config import Settings
from graph_ml.extract import StaticGraphClient, TenantGraph
from graph_ml.score import run_sweep, score_one_tenant


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


class FlakyGraphClient(StaticGraphClient):
    def fetch_tenant_graph(self, tenant_id):
        if tenant_id == "t_bad":
            raise RuntimeError("simulated FalkorDB failure")
        return super().fetch_tenant_graph(tenant_id)


def settings(**overrides) -> Settings:
    base = dict(top_k=5, tenant_concurrency=1, internal_token="tok")
    base.update(overrides)
    return Settings(**base)


def test_sweep_two_tenants_isolated_writes(tenant_graph, now):
    other = TenantGraph(tenant_id="t2", persons=list(tenant_graph.persons[:2]))
    client = StaticGraphClient({"t1": tenant_graph, "t2": other})
    writer = RecordingWriter()

    sweep = run_sweep(settings(), client, writer)

    assert sweep.ok
    assert sweep.backend == "heuristic"
    assert {t.tenant_id for t in sweep.tenants} == {"t1", "t2"}
    # write-back strictly per-tenant: no shared calls between tenants
    assert [t for t, _ in writer.score_posts] == ["t1", "t2"]
    t1_scores = dict(writer.score_posts)["t1"]
    assert len(t1_scores) == 5
    assert all(s["tenant_id"] == "t1" for s in t1_scores)
    t2_scores = dict(writer.score_posts)["t2"]
    assert all(s["tenant_id"] == "t2" for s in t2_scores)


def test_tenant_loop_isolation_one_failure_does_not_kill_sweep(tenant_graph):
    client = FlakyGraphClient({"t1": tenant_graph, "t_bad": TenantGraph("t_bad")})
    writer = RecordingWriter()

    sweep = run_sweep(settings(), client, writer)

    by_tenant = {t.tenant_id: t for t in sweep.tenants}
    assert by_tenant["t_bad"].status == "error"
    assert "simulated FalkorDB failure" in by_tenant["t_bad"].error
    assert by_tenant["t1"].status == "ok"
    assert by_tenant["t1"].persons_scored == 5
    # t1 write-back still happened despite t_bad exploding
    assert [t for t, _ in writer.score_posts] == ["t1"]
    assert not sweep.ok  # sweep reports the failure honestly


def test_single_tenant_run(tenant_graph):
    client = StaticGraphClient({"t1": tenant_graph, "t2": TenantGraph("t2")})
    writer = RecordingWriter()

    sweep = run_sweep(settings(), client, writer, tenant_id="t1")

    assert [t.tenant_id for t in sweep.tenants] == ["t1"]
    assert client.fetch_calls == ["t1"]  # t2 never touched


def test_tenant_discovery_failure_recorded():
    class Broken(StaticGraphClient):
        def list_tenants(self):
            raise RuntimeError("no graph db")

    sweep = run_sweep(settings(), Broken({}), RecordingWriter())
    assert sweep.tenants[0].status == "error"
    assert not sweep.ok


def test_gnn_backend_requested_degrades_to_heuristic(tenant_graph, monkeypatch, caplog):
    import logging

    monkeypatch.setenv("GRAPH_ML_BACKEND", "gnn")
    client = StaticGraphClient({"t1": tenant_graph})
    with caplog.at_level(logging.WARNING, logger="graph_ml.gnn"):
        sweep = run_sweep(settings(backend="gnn"), client, RecordingWriter())
    # SPEC §4 gate 5: degraded GNN -> heuristic fallback + warning, no failure
    assert sweep.backend == "heuristic"
    assert sweep.ok
    assert sweep.tenants[0].model_version == "heuristic-v1"
    assert any("falling back to heuristic" in r.message for r in caplog.records)


def test_score_one_tenant_dry_run_no_writer(tenant_graph):
    client = StaticGraphClient({"t1": tenant_graph})
    result = score_one_tenant(settings(), client, None, "t1", "heuristic")
    assert result.status == "ok"
    assert result.persons_scored == 5
    assert result.recommendations > 0
