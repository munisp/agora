"""W46-F P13: ML scorer + activity cache discipline.

- activity cache keyed by (tenant, frozenset(person_ids)) — a second run
  against a DIFFERENT person set must not reuse the first set's rows
  (correctness bug in the old tenant-only key);
- TTL expiry re-queries / re-loads (version-keyed invalidation window);
- caches are bounded (oldest-first eviction at maxsize).
"""

from __future__ import annotations

import time

from fraud_engine.config import Settings
from fraud_engine.detectors import DetectionRunner
from fraud_engine.detectors.base import _BoundedTTLCache
from fraud_engine.events import InMemoryPublisher

from conftest import TENANT
from fakes import FakeGraphClient, PropertyGraph


def _runner(settings: Settings | None = None) -> DetectionRunner:
    return DetectionRunner(
        FakeGraphClient(PropertyGraph()),
        InMemoryPublisher(),
        settings or Settings(),
    )


class _CountingScorer:
    model_version = "fraud-ml-v1"

    def __init__(self):
        self.loads = 0


def test_bounded_ttl_cache_expiry_and_eviction():
    cache = _BoundedTTLCache(ttl_s=60.0, maxsize=2)
    cache.set("a", 1)
    assert cache.get("a") == (True, 1)
    # expiry: entries past their TTL are treated as missing (and dropped)
    key, (exp, _) = next(iter(cache._entries.items()))
    cache._entries[key] = (time.monotonic() - 0.01, 1)
    assert cache.get("a") == (False, None)
    assert len(cache) == 0
    # bound: oldest evicted at maxsize
    cache.set("a", 1)
    cache.set("b", 2)
    cache.set("c", 3)
    assert len(cache) == 2
    assert cache.get("a") == (False, None)
    assert cache.get("b") == (True, 2)
    assert cache.get("c") == (True, 3)


def test_activity_cache_keyed_by_person_set(monkeypatch):
    runner = _runner()
    calls: list[list[str]] = []

    def fake_query(tenant_id, params):
        calls.append(list(params["person_ids"]))
        return [
            {"person_id": pid, "events": [], "referral_degree": 0}
            for pid in params["person_ids"]
        ]

    monkeypatch.setattr(runner, "_run_ml_query", fake_query)
    first = runner._ml_activity(TENANT, ["p1", "p2"])
    second = runner._ml_activity(TENANT, ["p3"])  # different person set
    again = runner._ml_activity(TENANT, ["p2", "p1"])  # same set, other order
    # the different set triggered a NEW query (old code reused tenant-only)
    assert calls == [["p1", "p2"], ["p3"]]
    assert set(first) == {"p1", "p2"}
    assert set(second) == {"p3"}
    # same set (order-insensitive) is a cache hit — no third query
    assert set(again) == {"p1", "p2"}
    assert len(runner._ml_activity_cache) == 2


def test_activity_cache_ttl_expiry_requeries(monkeypatch):
    runner = _runner()
    calls = 0

    def fake_query(tenant_id, params):
        nonlocal calls
        calls += 1
        return []

    monkeypatch.setattr(runner, "_run_ml_query", fake_query)
    runner._ml_activity(TENANT, ["p1"])
    runner._ml_activity(TENANT, ["p1"])
    assert calls == 1
    # force expiry
    key = (TENANT, frozenset({"p1"}))
    _, value = runner._ml_activity_cache._entries[key]
    runner._ml_activity_cache._entries[key] = (time.monotonic() - 0.01, value)
    runner._ml_activity(TENANT, ["p1"])
    assert calls == 2


def test_scorer_cache_ttl_expiry_reloads(tmp_path, monkeypatch):
    settings = Settings(ml_registry_dir=str(tmp_path))
    runner = _runner(settings)
    scorer = _CountingScorer()
    loads = 0

    class _FakeLearnedScorer:
        @staticmethod
        def load(registry_dir, tenant_id):
            nonlocal loads
            loads += 1
            return scorer

    import fraud_engine.ml.scorer as scorer_mod

    monkeypatch.setattr(scorer_mod, "LearnedScorer", _FakeLearnedScorer)
    assert runner._ml_scorer_for(TENANT) is scorer
    assert runner._ml_scorer_for(TENANT) is scorer
    assert loads == 1
    # version-keyed entry: (scorer, model_version) cached under the tenant
    hit, entry = runner._ml_scorer_cache.get(TENANT)
    assert hit and entry == (scorer, "fraud-ml-v1")
    # TTL expiry re-resolves the artifact (picks up a promoted version)
    runner._ml_scorer_cache._entries[TENANT] = (
        time.monotonic() - 0.01,
        entry,
    )
    assert runner._ml_scorer_for(TENANT) is scorer
    assert loads == 2


def test_caches_bounded_via_runner():
    runner = _runner()
    assert runner._ml_scorer_cache._maxsize == runner._ml_activity_cache._maxsize
    assert runner._ml_scorer_cache._maxsize > 0
    assert runner._ml_scorer_cache._ttl_s == 300.0
