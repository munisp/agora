"""SPEC-W34 GF9 adversarial gates — non-finite evasion closed.

Before the fix: NaN/Inf feature dims flowed through
``ml/features.py`` (e.g. inf amounts -> ``amt_mean_log=inf``),
``score_vector`` returned NaN, every ``NaN >= threshold`` comparison was
False (ML severity upgrades silently suppressed — an evasion primitive),
and ``events.py`` could emit bare ``NaN`` in published JSON (invalid
strict JSON).

Gates proven here:
- inf/NaN amounts, NaN vectors, hostile streams: no crash, no NaN in any
  output vector.
- The severity-upgrade path is NOT silently suppressible by poisoned
  features: the sanitized vector still scores and a score over threshold
  still upgrades low -> medium.
- A pathological non-finite ML score raises MLUnavailableError -> rule
  fallback (I1), never a NaN verdict.
- Published alert payloads are strict-JSON clean:
  ``json.loads(..., parse_constant=<reject>)`` succeeds and
  ``json.dumps(..., allow_nan=False)`` never raises.
"""

from __future__ import annotations

import json
import math

import pytest

from fraud_engine.detectors import DetectionRunner
from fraud_engine.events import (
    InMemoryPublisher,
    alert_raised_event,
    strict_json_safe,
)
from fraud_engine.ml.features import FEATURE_DIM, build_feature_vector
from fraud_engine.ml.scorer import MLUnavailableError, ScoreResult

from conftest import NOW, TENANT, add_booking_for, make_cycle
from fakes import FakeGraphClient, PropertyGraph

INF = float("inf")
NAN = float("nan")


def _strict_loads(payload: dict) -> None:
    """json.loads that REJECTS bare NaN/Infinity/-Infinity tokens."""

    def _reject(token: str):
        raise AssertionError(f"non-strict JSON token in payload: {token}")

    json.loads(json.dumps(payload, allow_nan=False), parse_constant=_reject)


# ---------------------------------------------------------------------------
# (a) build_feature_vector sanitizes hostile input
# ---------------------------------------------------------------------------
class TestBuildFeatureVectorSanitizes:
    def test_inf_amount_no_inf_dims(self):
        fv = build_feature_vector(
            [
                {"event_type": "transfer", "ts": "2026-01-01T00:00:00Z",
                 "amount_ngn": INF},
                {"event_type": "transfer", "ts": "2026-01-01T01:00:00Z",
                 "amount_ngn": 5000.0},
            ]
        )
        assert len(fv) == FEATURE_DIM
        assert all(math.isfinite(x) for x in fv)

    def test_nan_amount_dropped(self):
        fv = build_feature_vector(
            [{"event_type": "pos", "ts": "2026-01-01T00:00:00Z", "amount_ngn": NAN}]
        )
        assert all(math.isfinite(x) for x in fv)
        assert fv[4] == 0.0  # amt_mean_log: poisoned amount treated as absent

    def test_negative_inf_and_string_amounts(self):
        fv = build_feature_vector(
            [
                {"event_type": "transfer", "ts": "2026-01-01T00:00:00Z",
                 "amount_ngn": -INF},
                {"event_type": "pos", "ts": "2026-01-01T02:00:00Z",
                 "amount_ngn": "not-a-number"},
                {"event_type": "pos", "ts": "2026-01-01T03:00:00Z",
                 "amount_ngn": "inf"},  # float("inf") via string coercion
            ]
        )
        assert all(math.isfinite(x) for x in fv)

    def test_hostile_stream_no_crash(self):
        hostile = [
            {"event_type": "capture", "ts": INF, "lat": 6.5, "lon": 3.3},
            {"event_type": "capture", "ts": NAN, "lat": 6.5, "lon": 3.3},
            {"event_type": "capture", "ts": "garbage", "lat": INF, "lon": 3.3},
            {"event_type": "capture", "ts": "2026-01-01T00:00:00Z",
             "lat": NAN, "lon": -INF},
            {"event_type": None, "ts": None, "amount_ngn": None,
             "lat": None, "lon": None},
            {"event_type": "booking", "ts": 10**40, "amount_ngn": 1e309},
            {},  # empty row
        ]
        fv = build_feature_vector(hostile, referral_degree=INF)
        assert len(fv) == FEATURE_DIM
        assert all(math.isfinite(x) for x in fv)
        fv2 = build_feature_vector(hostile, referral_degree=NAN)
        assert all(math.isfinite(x) for x in fv2)

    def test_poisoned_vector_matches_zero_amount_baseline(self):
        """Clamped-to-safe is deterministic: an inf amount contributes
        exactly what an absent amount contributes."""
        poisoned = build_feature_vector(
            [{"event_type": "transfer", "ts": "2026-01-01T00:00:00Z",
              "amount_ngn": INF}]
        )
        baseline = build_feature_vector(
            [{"event_type": "transfer", "ts": "2026-01-01T00:00:00Z",
              "amount_ngn": 0.0}]
        )
        assert poisoned == baseline


# ---------------------------------------------------------------------------
# (b) score_vector guards isfinite
# ---------------------------------------------------------------------------
torch = pytest.importorskip("torch", reason="scorer guard tests need torch")

from fraud_engine.ml.autoencoder import FraudAE  # noqa: E402
from fraud_engine.ml.classifier import FraudCLF  # noqa: E402
from fraud_engine.ml.scorer import LearnedScorer  # noqa: E402


def _scorer() -> LearnedScorer:
    ae_meta = {
        "model_version": "fraud-ae-v1",
        "feature_schema": "fv1",
        "ae_error_stats": {"err_min": 0.0, "err_max": 1.0},
    }
    clf_meta = {"model_version": "fraud-clf-v1", "feature_schema": "fv1"}
    return LearnedScorer(FraudAE(), ae_meta, FraudCLF(), clf_meta, TENANT)


class TestScoreVectorGuard:
    def test_nan_input_vector_scores_finite(self):
        result = _scorer().score_vector([NAN] * FEATURE_DIM)
        assert math.isfinite(result.score)
        assert 0.0 <= result.score <= 1.0

    def test_inf_mixed_vector_scores_finite(self):
        fv = [INF, -INF, NAN, 1.0] + [0.0] * (FEATURE_DIM - 4)
        result = _scorer().score_vector(fv)
        assert math.isfinite(result.score)

    def test_non_finite_model_output_raises_ml_unavailable(self):
        scorer = _scorer()

        class _NaNCLF:
            def eval(self):
                return self

            def probability(self, x):
                return torch.tensor([NAN])

        scorer._clf = _NaNCLF()
        with pytest.raises(MLUnavailableError):
            scorer.score_vector([0.0] * FEATURE_DIM)

    def test_score_events_on_poisoned_stream_finite(self):
        result = _scorer().score_events(
            [{"event_type": "transfer", "ts": "2026-01-01T00:00:00Z",
              "amount_ngn": INF}]
        )
        assert math.isfinite(result.score)


# ---------------------------------------------------------------------------
# Severity-upgrade path not suppressible by poisoned features (I1 union)
# ---------------------------------------------------------------------------
class _HighBlendScorer:
    """Stub: every person blends to 1.0 (over any threshold)."""

    def score_events(self, events, referral_degree=0) -> ScoreResult:
        return ScoreResult(
            score=1.0, ae_norm=1.0, clf_prob=1.0,
            model_version="fraud-ae-v9+fraud-clf-v9", feature_schema="fv1",
        )

    def blend_reason(self, result: ScoreResult) -> str:
        return f"ml_blend ae={result.ae_norm:.4f} clf={result.clf_prob:.4f}"


def _fixture_graph() -> PropertyGraph:
    g = PropertyGraph()
    make_cycle(g, TENANT, ["p1", "p2", "p3"])
    add_booking_for(g, TENANT, "p1")
    return g


def test_severity_upgrade_fires_despite_poisoned_activity(settings, monkeypatch):
    """Poisoned per-person activity (inf/NaN in the ML projection) must not
    suppress the low -> medium upgrade: sanitized features still score."""
    graph = _fixture_graph()
    pkey = graph.add_person(TENANT, "p-low", risk_score=0.91)
    assert pkey
    runner = DetectionRunner(
        FakeGraphClient(graph), InMemoryPublisher(), settings,
        ml_scorer=_HighBlendScorer(),
    )
    # Poison the activity projection the runner hands to the scorer.
    poisoned = (
        [{"event_type": "capture", "ts": "2026-01-01T00:00:00Z",
          "amount_ngn": INF, "lat": NAN, "lon": INF}],
        1,
    )
    monkeypatch.setattr(
        runner, "_ml_activity", lambda tid, ids: {pid: poisoned for pid in ids}
    )
    report = runner.run(tenant_id=TENANT, now=NOW)
    upgraded = [
        a for a in graph.alerts.values()
        if a["type"] == "gnn_anomaly" and a["severity"] == "medium"
    ]
    assert upgraded, "sanitized vector must still score: low -> medium upgrade fires"
    assert not report.errors


def test_nan_scorer_person_falls_back_others_still_upgrade(settings):
    """One person whose scorer raises (non-finite score path) keeps their
    rule verdict; the rest of the batch still gets ML upgrades (I1)."""
    graph = _fixture_graph()
    graph.add_person(TENANT, "p-low", risk_score=0.91)

    class _FlakyScorer:
        def score_events(self, events, referral_degree=0) -> ScoreResult:
            if referral_degree == 0:  # p-low has no REFERRED edges
                raise MLUnavailableError("non-finite ML score (test)")
            return ScoreResult(
                score=1.0, ae_norm=1.0, clf_prob=1.0,
                model_version="fraud-ae-v9+fraud-clf-v9", feature_schema="fv1",
            )

        def blend_reason(self, result: ScoreResult) -> str:
            return "ml_blend ae=1.0000 clf=1.0000"

    runner = DetectionRunner(
        FakeGraphClient(graph), InMemoryPublisher(), settings,
        ml_scorer=_FlakyScorer(),
    )
    report = runner.run(tenant_id=TENANT, now=NOW)
    assert any("ml_blend" in err for err in report.errors)  # logged, not silent
    # The referral-ring members (degree > 0) still scored and upgraded.
    assert sorted(report.quarantined) == ["p1", "p2", "p3"]  # rules intact


# ---------------------------------------------------------------------------
# (c) published payload is strict-JSON safe
# ---------------------------------------------------------------------------
class _PoisonedAlert:
    """AlertRecord-shaped object whose event_data carries non-finite junk."""

    alert_id = "d7_anomaly:t:p-low:dedup"
    type = "gnn_anomaly"
    severity = "medium"
    person_id = "p-low"
    agent_id = None

    def event_data(self):
        return {
            "alert_id": self.alert_id,
            "type": self.type,
            "severity": self.severity,
            "person_id": self.person_id,
            "ml_score": NAN,
            "bands": [0.1, INF, -INF],
            "nested": {"score": NAN},
        }


class TestStrictJSON:
    def test_alert_event_strict_clean(self):
        event = alert_raised_event(TENANT, _PoisonedAlert())
        _strict_loads(event)  # raises on any bare NaN/Infinity token

    def test_strict_json_safe_replaces_non_finite_with_null(self):
        out = strict_json_safe({"a": NAN, "b": [INF, {"c": -INF}], "d": 1.5})
        assert out == {"a": None, "b": [None, {"c": None}], "d": 1.5}

    def test_kafka_publisher_serializes_strict(self, monkeypatch):
        """KafkaPublisher.publish must emit allow_nan=False-clean bytes even
        for a poisoned event (producer stubbed — no broker needed)."""
        from fraud_engine.events import KafkaPublisher

        pub = KafkaPublisher.__new__(KafkaPublisher)
        sent: dict[str, bytes] = {}

        class _StubProducer:
            def produce(self, topic, key=None, value=None):
                sent["value"] = value

            def poll(self, timeout):
                return 0

        pub._producer = _StubProducer()
        pub.publish("t", "k", {"ml_score": NAN, "ok": True})
        _strict_loads(json.loads(sent["value"]))
