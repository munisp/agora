"""Heuristic scorer math on the fixture graph (SPEC-W29 §3 WS-A tests)."""

from __future__ import annotations

import math

import pytest

from graph_ml import MODEL_VERSION_HEURISTIC
from graph_ml.features import build_features, tenant_median_interval
from graph_ml.heuristic import (
    W_RECENCY,
    W_REFERRAL,
    W_RESPONSE,
    W_TURNOUT_CONVERSION,
    W_TURNOUT_RESPONSE,
    TURNOUT_COLD_PRIOR,
    churn_score,
    clip01,
    convert_score,
    score_tenant,
    sigmoid,
    turnout_score,
)


def feats_by_id(graph, now):
    return {f.person_id: f for f in build_features(graph, now)}


def test_sigmoid_known_values():
    assert sigmoid(0.0) == pytest.approx(0.5)
    assert sigmoid(1.0) == pytest.approx(1.0 / (1.0 + math.e**-1))
    assert sigmoid(-10.0) < 0.001
    assert sigmoid(10.0) > 0.999


def test_clip01_bounds():
    assert clip01(1.7) == 1.0
    assert clip01(-0.2) == 0.0
    assert clip01(0.42) == pytest.approx(0.42)


def test_tenant_median_interval(tenant_graph, now):
    features = build_features(tenant_graph, now)
    # p1 interval mean 15d, p2 mean 5d -> median 10d
    assert tenant_median_interval(features) == pytest.approx(10.0)


def test_churn_exact_math(tenant_graph, now):
    f = feats_by_id(tenant_graph, now)["p1"]
    # recency 10d, tenant median interval 10d -> sigmoid(1.0)
    assert churn_score(f, 10.0) == pytest.approx(sigmoid(1.0))


def test_churn_increases_with_recency(tenant_graph, now):
    feats = feats_by_id(tenant_graph, now)
    median = 10.0
    assert churn_score(feats["p2"], median) < churn_score(feats["p1"], median)
    assert churn_score(feats["p1"], median) < churn_score(feats["p3"], median)
    # never booked -> maximal recency default -> highest churn band
    assert churn_score(feats["p4"], median) > churn_score(feats["p3"], median)


def test_churn_uses_tenant_median_interval(tenant_graph, now):
    f = feats_by_id(tenant_graph, now)["p1"]
    # Same recency, different tenant-typical interval -> different score.
    assert churn_score(f, 10.0) != pytest.approx(churn_score(f, 30.0))
    assert churn_score(f, 30.0) == pytest.approx(sigmoid(10.0 / 30.0))


def test_convert_exact_math(tenant_graph, now):
    f = feats_by_id(tenant_graph, now)["p1"]
    # recency 10d -> sigmoid((30-10)/30); response 0.5; referral_in 1 -> 1/3
    expected = (
        W_RECENCY * sigmoid((30.0 - 10.0) / 30.0)
        + W_RESPONSE * 0.5
        + W_REFERRAL * (1.0 / 3.0)
    )
    assert convert_score(f) == pytest.approx(expected)


def test_turnout_exact_math(tenant_graph, now):
    f = feats_by_id(tenant_graph, now)["p1"]
    # response 1/2, messaged->booked 1/2
    assert turnout_score(f) == pytest.approx(
        W_TURNOUT_RESPONSE * 0.5 + W_TURNOUT_CONVERSION * 0.5
    )
    f4 = feats_by_id(tenant_graph, now)["p4"]
    assert turnout_score(f4) == pytest.approx(W_TURNOUT_RESPONSE * 1.0)


def test_turnout_cold_prior_when_never_messaged(tenant_graph, now):
    f5 = feats_by_id(tenant_graph, now)["p5"]
    assert f5.message_count == 0
    assert turnout_score(f5) == pytest.approx(TURNOUT_COLD_PRIOR)


def test_cold_start_five_person_graph_scores(tenant_graph, now):
    """SPEC §4 gate 4: 5-person fixture -> scores produced, no crash, non-empty."""
    scores, recommendations = score_tenant(tenant_graph, now=now, top_k=5)
    assert len(scores) == 5
    assert recommendations  # non-empty even for never-booked persons
    for s in scores:
        assert 0.0 <= s.propensity_churn <= 1.0
        assert 0.0 <= s.propensity_convert <= 1.0
        assert 0.0 <= s.propensity_turnout <= 1.0
        assert s.tenant_id == "t1"


def test_scores_carry_provenance(tenant_graph, now):
    """SPEC §0.4: every score/recommendation carries model_version + scored_at."""
    scores, recommendations = score_tenant(tenant_graph, now=now, top_k=5)
    for s in scores:
        assert s.model_version == MODEL_VERSION_HEURISTIC
        assert s.scored_at == now.isoformat()
    for r in recommendations:
        assert r.model_version == MODEL_VERSION_HEURISTIC
        assert r.scored_at == now.isoformat()
