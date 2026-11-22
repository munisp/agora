"""Cross-encoder rerank stage tests (SPEC-W113).

/v1/search and /v1/context rerank the RRF-fused candidate pool with a
cross-encoder. The stage is best-effort: error/timeout falls back to RRF
order and search never fails. The response gains `reranked` and per-hit
`rerank_score` (only when reranked); rrf_score is unchanged. The eval
harness metric functions are tested hermetically.

REDIRECTION PROOFS (SPEC-W113):
- R1: rerank stage removed from _hybrid -> test_rerank_order_applied FAILS
- R2: error fallback removed -> test_reranker_error_falls_back FAILS (500)
- R3: over-fusion removed (size=k) -> test_candidate_pool_over_fusion FAILS
- R4: MRR reciprocal removed -> test_mrr_at_k_first_relevant_rank FAILS
"""

from __future__ import annotations

import asyncio
import sys
import uuid

sys.path.insert(0, ".")

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app import main as main_module
from app.config import settings
from app.reranker import CrossEncoderReranker

pytestmark = pytest.mark.asyncio

TENANT_A = "11111111-1111-1111-1111-111111111111"


def _hit(chunk_id: str, content: str) -> dict:
    return {
        "_id": chunk_id,
        "_source": {
            "document_id": str(uuid.uuid4()),
            "title": "t",
            "content": content,
        },
    }


class FakeSearch:
    """BM25 leg ranks A > B > C; kNN leg ranks C > B > D — RRF order is
    deterministic and NOT relevance order, so a rerank flip is observable."""

    async def bm25_search(self, tenant_id, query, k):
        return [_hit("c-a", "alpha hours chunk"), _hit("c-b", "beta chunk"),
                _hit("c-c", "gamma refund chunk")]

    async def knn_search(self, tenant_id, vector, k):
        return [_hit("c-c", "gamma refund chunk"), _hit("c-b", "beta chunk"),
                _hit("c-d", "delta chunk")]


class FakeEmbedder:
    async def embed(self, texts):
        return [[0.0] * 384 for _ in texts]


class ScriptReranker:
    """Scores by scripted content keywords; records what it was given."""

    def __init__(self, outcome=None):
        self.calls = []
        self.outcome = outcome  # Exception instance -> raise; else score map

    async def rerank(self, query, hits):
        self.calls.append((query, [h["_id"] for h in hits]))
        if isinstance(self.outcome, Exception):
            raise self.outcome
        scores = {
            # gamma is truly relevant -> highest score flips it to the top
            "gamma": 10.0, "alpha": 1.0, "beta": 2.0, "delta": 0.5,
        }
        scored = sorted(
            hits,
            key=lambda h: -scores.get(str(h["_source"]["content"]).split()[0], 0.0),
        )
        out = []
        for h in scored:
            h = dict(h)
            h["_rerank_score"] = scores.get(
                str(h["_source"]["content"]).split()[0], 0.0
            )
            out.append(h)
        return out


class FakeConn:
    async def fetchrow(self, query, *args):
        return None  # suggestion INSERT is best-effort noise here


class FakeDB:
    from contextlib import asynccontextmanager

    @asynccontextmanager
    async def tenant_conn(self, tenant_id):
        yield FakeConn()


def _app(reranker) -> FastAPI:
    app = FastAPI()
    app.get("/v1/search")(main_module.search)
    app.get("/v1/context")(main_module.context)
    deps = type("D", (), {})()
    deps.db = FakeDB()
    deps.search = FakeSearch()
    deps.embedder = FakeEmbedder()
    deps.reranker = reranker
    deps.dapr = None
    app.state.deps = deps
    return app


def _get(client, path, **params):
    return client.get(
        path,
        params={"tenant": TENANT_A, "q": "when refund?", **params},
        headers={"X-Tenant-Slugs": TENANT_A},
    )


@pytest.fixture
def _rerank_cfg(monkeypatch):
    monkeypatch.setattr(settings, "rerank_enabled", True)
    monkeypatch.setattr(settings, "rerank_candidates", 3)
    monkeypatch.setattr(settings, "rerank_timeout_s", 1.0)
    yield


# ------------------------------------------------- pipeline behavior
def test_rerank_order_applied_and_annotated(_rerank_cfg):
    reranker = ScriptReranker()
    with TestClient(_app(reranker)) as c:
        r = _get(c, "/v1/search", k=2)
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["reranked"] is True
        ids = [h["chunk_id"] for h in body["results"]]
        # RRF order would put c-a/c-c mixed; rerank flips gamma (c-c) to #1
        assert ids[0] == "c-c"
        assert "rerank_score" in body["results"][0]
        assert "rrf_score" in body["results"][0]  # unchanged contract


def test_rerank_applies_to_context_endpoint(_rerank_cfg):
    reranker = ScriptReranker()
    with TestClient(_app(reranker)) as c:
        r = _get(c, "/v1/context", k=2)
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["snippets"][0] == "gamma refund chunk"


def test_candidate_pool_over_fusion(_rerank_cfg):
    """k=1 must still let the reranker re-score rerank_candidates hits."""
    reranker = ScriptReranker()
    with TestClient(_app(reranker)) as c:
        r = _get(c, "/v1/search", k=1)
        assert r.status_code == 200, r.text
    assert len(reranker.calls) == 1
    assert len(reranker.calls[0][1]) == 3  # the full candidate pool, not k=1
    assert r.json()["results"][0]["chunk_id"] == "c-c"  # best of the pool


def test_reranker_error_falls_back_to_rrf(_rerank_cfg):
    reranker = ScriptReranker(outcome=RuntimeError("model exploded"))
    with TestClient(_app(reranker)) as c:
        r = _get(c, "/v1/search", k=2)
        assert r.status_code == 200, r.text  # search NEVER fails on rerank
        body = r.json()
        assert body["reranked"] is False
        assert "rerank_score" not in body["results"][0]


def test_reranker_timeout_falls_back_to_rrf(_rerank_cfg, monkeypatch):
    monkeypatch.setattr(settings, "rerank_timeout_s", 0.05)

    class SlowReranker:
        async def rerank(self, query, hits):
            await asyncio.sleep(5)
            return hits

    with TestClient(_app(SlowReranker())) as c:
        r = _get(c, "/v1/search", k=2)
        assert r.status_code == 200, r.text
        assert r.json()["reranked"] is False


def test_disabled_config_skips_reranker(monkeypatch):
    monkeypatch.setattr(settings, "rerank_enabled", False)
    reranker = ScriptReranker()
    with TestClient(_app(reranker)) as c:
        r = _get(c, "/v1/search", k=2)
        assert r.status_code == 200, r.text
        assert r.json()["reranked"] is False
    assert reranker.calls == []  # never invoked


# ------------------------------------------------- reranker unit
async def test_crossencoder_rerank_stable_order_and_annotation():
    reranker = CrossEncoderReranker("unused", max_chars=2000)

    async def fake_score(query, contents):
        return [0.1, 0.9, 0.9]  # tie on the top score

    reranker.score = fake_score  # type: ignore[assignment]
    hits = [_hit("c-1", "one"), _hit("c-2", "two"), _hit("c-3", "three")]
    out = await reranker.rerank("q", hits)
    assert [h["_id"] for h in out] == ["c-2", "c-3", "c-1"]  # ties keep order
    assert out[0]["_rerank_score"] == 0.9
    assert hits[0].get("_rerank_score") is None  # input not mutated


async def test_crossencoder_rerank_empty():
    reranker = CrossEncoderReranker("unused")
    assert await reranker.rerank("q", []) == []


# ------------------------------------------------- eval harness metrics
def test_recall_at_k_counts_relevant_in_top_k():
    from eval.rerank_eval import recall_at_k

    assert recall_at_k(["a", "b", "c"], {"a", "c"}, 2) == 0.5
    assert recall_at_k(["a", "b", "c"], {"a", "c"}, 3) == 1.0
    assert recall_at_k(["a"], set(), 5) == 0.0
    assert recall_at_k([], {"a"}, 5) == 0.0


def test_mrr_at_k_first_relevant_rank():
    from eval.rerank_eval import mrr_at_k

    assert mrr_at_k(["a", "b"], {"a"}, 5) == 1.0
    assert mrr_at_k(["x", "a"], {"a"}, 5) == 0.5
    assert mrr_at_k(["x", "y", "a"], {"a"}, 2) == 0.0  # outside top-k
    assert mrr_at_k(["x"], {"a"}, 5) == 0.0


def test_evaluate_reports_uplift():
    from eval.rerank_eval import QUERIES, evaluate

    # before: relevant ids at rank 3; after: rank 1 — uplift must be positive
    before = {q: ["z1", "z2"] + sorted(rel) for q, rel in QUERIES}
    after = {q: sorted(rel) + ["z1", "z2"] for q, rel in QUERIES}
    report = evaluate(before, after, k=3)
    assert report["after"]["recall"] == 1.0
    assert report["after"]["mrr"] == 1.0
    # 8 single-relevant queries hit at rank 3 (in top-3); the 2 two-relevant
    # queries place only one of two ids in top-3 -> (8*1 + 2*0.5)/10 = 0.9
    assert report["before"]["recall"] == 0.9
    assert report["before"]["mrr"] < 1.0
    assert report["uplift"]["mrr"] > 0
    assert report["uplift"]["recall"] == 0.1


def test_harness_skips_honestly_without_models():
    """No HF cache in this sandbox — the harness must SKIP, never fake."""
    from eval.rerank_eval import main

    rc = main.__wrapped__ if hasattr(main, "__wrapped__") else None
    import subprocess

    proc = subprocess.run(
        [sys.executable, "eval/rerank_eval.py"],
        capture_output=True, text=True, timeout=60,
    )
    assert proc.returncode == 0
    assert proc.stdout.startswith("SKIP:")


def test_harness_require_flag_exits_1_on_skip():
    import subprocess

    proc = subprocess.run(
        [sys.executable, "eval/rerank_eval.py", "--require"],
        capture_output=True, text=True, timeout=60,
    )
    assert proc.returncode == 1
    assert proc.stdout.startswith("SKIP:")
