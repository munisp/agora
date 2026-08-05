"""Feature extraction math on the fixture graph (SPEC-W29 §3 WS-A features)."""

from __future__ import annotations

from datetime import datetime, timezone

import pytest

from graph_ml.features import (
    DEFAULT_INTERVAL_DAYS,
    DEFAULT_RECENCY_DAYS,
    build_features,
    parse_time,
)


def feats_by_id(graph, now):
    return {f.person_id: f for f in build_features(graph, now)}


def test_parse_time_variants(now):
    assert parse_time(now.isoformat()) == now
    assert parse_time(now.isoformat().replace("+00:00", "Z")) == now
    assert parse_time(now) == now
    assert parse_time(now.timestamp()) == now
    assert parse_time(None) is None
    assert parse_time("not-a-date") is None


def test_recency_and_counts(tenant_graph, now):
    f = feats_by_id(tenant_graph, now)["p1"]
    assert f.recency_days == pytest.approx(10.0)
    assert f.booking_count == 3
    assert f.booking_interval_mean == pytest.approx(15.0)
    assert f.booking_interval_std == pytest.approx(0.0)
    assert f.monetary_total_cents == 13000
    assert f.distinct_offerings == 2


def test_single_booking_interval_default(tenant_graph, now):
    f3 = feats_by_id(tenant_graph, now)["p3"]
    assert f3.booking_count == 1
    assert f3.booking_interval_mean == pytest.approx(DEFAULT_INTERVAL_DAYS)
    assert f3.recency_days == pytest.approx(20.0)


def test_cold_start_person_defaults(tenant_graph, now):
    f5 = feats_by_id(tenant_graph, now)["p5"]
    assert f5.has_booked is False
    assert f5.recency_days == pytest.approx(DEFAULT_RECENCY_DAYS)
    assert f5.booking_count == 0
    assert f5.message_response_rate == 0.0
    assert f5.days_since_capture == pytest.approx(DEFAULT_RECENCY_DAYS)


def test_referral_degrees(tenant_graph, now):
    feats = feats_by_id(tenant_graph, now)
    assert feats["p1"].referral_out_degree == 1
    assert feats["p1"].referral_in_degree == 1
    assert feats["p2"].referral_out_degree == 1
    assert feats["p2"].referral_in_degree == 0
    assert feats["p5"].referral_in_degree == 1


def test_message_response_and_conversion_rates(tenant_graph, now):
    feats = feats_by_id(tenant_graph, now)
    f1 = feats["p1"]
    assert f1.message_count == 2
    assert f1.message_response_rate == pytest.approx(0.5)  # responded / 2
    # booking at -10d follows message at -30d, not the one at -8d -> 1/2
    assert f1.message_to_booking_rate == pytest.approx(0.5)
    f4 = feats["p4"]
    assert f4.message_response_rate == pytest.approx(1.0)  # responded + replied
    assert f4.message_to_booking_rate == pytest.approx(0.0)  # no bookings at all


def test_consent_and_capture_features(tenant_graph, now):
    f4 = feats_by_id(tenant_graph, now)["p4"]
    assert f4.consent_purpose_count == 2
    assert f4.days_since_capture == pytest.approx(60.0)


def test_feature_vector_shape(tenant_graph, now):
    import numpy as np

    f = feats_by_id(tenant_graph, now)["p1"]
    vec = f.vector()
    assert isinstance(vec, np.ndarray)
    assert vec.shape == (11,)
    assert vec.dtype == np.float64
