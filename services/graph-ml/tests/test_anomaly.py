"""Anomaly head / risk_score tests (SPEC-W30 §1/§2, verification gate WARN #2).

Calibration contract: fraud-engine D7 consumes Person.risk_score >= 0.9, so
only genuine structural outliers may cross 0.9 on a normal fixture.
"""

from __future__ import annotations

import math

import pytest

from graph_ml.anomaly import MIN_PERSONS_FOR_RISK, risk_scores
from graph_ml.extract import PersonRec, ReferralRec, TenantGraph
from graph_ml.features import build_features
from graph_ml.heuristic import score_tenant


def test_zero_variance_tenant_all_zero(now):
    persons = [PersonRec(person_id=f"p{i}") for i in range(6)]
    graph = TenantGraph(tenant_id="t_flat", persons=persons)
    scores = risk_scores(build_features(graph, now))
    assert len(scores) == 6
    assert all(v == 0.0 for v in scores.values())


def test_cold_start_small_tenant_all_zero(now):
    persons = [PersonRec(person_id=f"p{i}") for i in range(MIN_PERSONS_FOR_RISK - 1)]
    # give them wildly different structure: still 0.0 below the minimum size
    referrals = [ReferralRec("p0", "p1"), ReferralRec("p0", "p2"), ReferralRec("p0", "p3")]
    graph = TenantGraph(tenant_id="t_small", persons=persons, referrals=referrals)
    scores = risk_scores(build_features(graph, now))
    assert all(v == 0.0 for v in scores.values())


def outlier_graph(now) -> TenantGraph:
    persons = [PersonRec(person_id=f"p{i}") for i in range(6)]
    # 50x structural outlier: p5 referral out-degree 50, spread over p0..p4
    # (10 each) so no *target* reads as anomalous on referral in-degree.
    referrals = [ReferralRec("p5", f"p{i % 5}") for i in range(50)]
    return TenantGraph(tenant_id="t_out", persons=persons, referrals=referrals)


def test_clear_outlier_exceeds_threshold_others_low(now):
    scores = risk_scores(build_features(outlier_graph(now), now))
    assert scores["p5"] > 0.9
    for pid, value in scores.items():
        if pid != "p5":
            assert value < 0.5, f"{pid} should not read as anomalous: {value}"


def test_normal_fixture_nobody_exceeds_threshold(tenant_graph, now):
    """Calibration guard: the standard 5-person fixture stays well under 0.9."""
    scores = risk_scores(build_features(tenant_graph, now))
    assert max(scores.values()) < 0.9


def test_no_nan_or_inf(now):
    graphs = [
        TenantGraph(tenant_id="t_empty"),
        TenantGraph(tenant_id="t_flat", persons=[PersonRec(person_id=f"p{i}") for i in range(8)]),
        outlier_graph(now),
    ]
    for graph in graphs:
        for value in risk_scores(build_features(graph, now)).values():
            assert math.isfinite(value)
            assert 0.0 <= value <= 1.0


def test_deterministic(now):
    graph = outlier_graph(now)
    first = risk_scores(build_features(graph, now))
    second = risk_scores(build_features(graph, now))
    assert first == second


def test_score_payload_includes_risk_score_and_model_version(tenant_graph, now):
    scores, _ = score_tenant(tenant_graph, now=now, top_k=5)
    assert scores
    for s in scores:
        payload = s.as_payload()
        assert "risk_score" in payload
        assert 0.0 <= payload["risk_score"] <= 1.0
        assert payload["model_version"] == "heuristic-v1"


def test_outlier_risk_flows_into_writeback_payload(now):
    scores, _ = score_tenant(outlier_graph(now), now=now, top_k=5)
    by_person = {s.person_id: s for s in scores}
    assert by_person["p5"].risk_score > 0.9
    assert by_person["p5"].as_payload()["risk_score"] > 0.9
    assert all(s.risk_score < 0.5 for pid, s in by_person.items() if pid != "p5")


def test_risk_score_monotone_in_outlier_magnitude(now):
    def graph_with(n_referrals: int) -> TenantGraph:
        persons = [PersonRec(person_id=f"p{i}") for i in range(6)]
        refs = [ReferralRec("p5", "p0") for _ in range(n_referrals)]
        return TenantGraph(tenant_id="t", persons=persons, referrals=refs)

    low = risk_scores(build_features(graph_with(2), now))["p5"]
    high = risk_scores(build_features(graph_with(50), now))["p5"]
    assert high > low
