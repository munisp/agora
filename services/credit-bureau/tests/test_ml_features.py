"""fv1 feature-builder tests (SPEC-W33 §3 B2): frozen order, documented
neutral defaults, normalization bands, clamps, and the A1 derivation."""

from __future__ import annotations

import numpy as np
import pytest

from credit_bureau.ml import features
from credit_bureau.ml.features import (
    DEFAULTS,
    FEATURE_DIM,
    FEATURE_NAMES,
    ZONES,
    build_feature_vector,
    derive_signals_from_events,
    income_band_for,
    normalize_feature,
)


def test_frozen_order_and_dim() -> None:
    # fv1 is FROZEN — this test fails loudly on any reorder (needs fv2).
    assert FEATURE_NAMES == (
        "utilization",
        "dpd_max_12m",
        "dpd_count_12m",
        "on_time_rate",
        "income_band",
        "tenure_months",
        "exposure_ngn",
        "limit_ngn",
        "lga_metro",
        "lga_zone_index",
        "product_mix_count",
        "product_mix_secured",
    )
    assert FEATURE_DIM == 12
    assert features.FEATURE_SCHEMA == "fv1"


def test_empty_payload_yields_documented_defaults() -> None:
    vec = build_feature_vector(None)
    for i, name in enumerate(FEATURE_NAMES):
        assert vec[i] == pytest.approx(DEFAULTS[name]), name
    assert vec.dtype == np.float32


def test_determinism_same_input_same_vector() -> None:
    sig = {"utilization": 0.3, "income_band": 2, "lga_zone_index": "South West"}
    a = build_feature_vector(sig)
    b = build_feature_vector(dict(sig))
    np.testing.assert_array_equal(a, b)


def test_normalization_bands_and_clamps() -> None:
    assert normalize_feature("dpd_max_12m", 90) == 1.0
    assert normalize_feature("dpd_max_12m", 45) == 0.5
    assert normalize_feature("dpd_max_12m", 900) == 1.0  # clamp
    assert normalize_feature("dpd_count_12m", 6) == 0.5
    assert normalize_feature("income_band", 4) == 1.0
    assert normalize_feature("income_band", 9) == 1.0  # clamp
    assert normalize_feature("tenure_months", 60) == 1.0
    assert normalize_feature("utilization", 1.7) == 1.0  # clamp
    assert normalize_feature("utilization", -0.2) == 0.0
    # log-scaled money features are monotonic and bounded.
    assert 0.0 < normalize_feature("exposure_ngn", 50_000) < normalize_feature("exposure_ngn", 500_000) < 1.0
    assert normalize_feature("exposure_ngn", 10**12) == 1.0
    # zone: canonical name or raw index, alphabetical frozen order.
    assert normalize_feature("lga_zone_index", "North Central") == 0.0
    assert normalize_feature("lga_zone_index", "South West") == 1.0
    assert normalize_feature("lga_zone_index", 5) == 1.0
    assert ZONES == tuple(sorted(ZONES))


def test_income_bands_documented_thresholds() -> None:
    assert income_band_for(0) == 0
    assert income_band_for(99_999) == 1
    assert income_band_for(100_000) == 2
    assert income_band_for(200_000) == 3
    assert income_band_for(350_000) == 4


def test_derive_signals_from_events_proxy() -> None:
    person = {"person_id": "p1", "state": "Lagos", "zone": "South West", "persona": "salary_worker"}
    events = [
        {"event_type": "salary", "amount_ngn": 150_000, "ts": "2026-01-25T09:00:00Z"},
        {"event_type": "salary", "amount_ngn": 150_000, "ts": "2026-02-25T09:00:00Z"},
        {"event_type": "pos", "amount_ngn": 40_000, "ts": "2026-02-10T12:00:00Z"},
        {"event_type": "transfer", "amount_ngn": 20_000, "ts": "2026-02-11T12:00:00Z"},
        {"event_type": "booking", "amount_ngn": 5_000, "ts": "2026-02-12T12:00:00Z"},
        {"event_type": "cancellation", "amount_ngn": 5_000, "ts": "2026-02-12T14:00:00Z"},
    ]
    sig = derive_signals_from_events(person, events, horizon_days=60)
    assert sig["income_band"] == 2  # 150k monthly salary
    assert sig["limit_ngn"] == pytest.approx(3 * 150_000)
    assert sig["exposure_ngn"] == pytest.approx(60_000 / 2.0)  # outflow over 2 months
    assert sig["on_time_rate"] == pytest.approx(0.0)  # 1 booking, cancelled
    assert sig["dpd_count_12m"] == 1  # the cancellation proxy
    assert sig["lga_metro"] is True
    assert sig["lga_zone_index"] == "South West"
    assert sig["product_mix_count"] == 2  # pos + transfer
    assert sig["product_mix_secured"] == 0.0  # A1 has no secured products
    vec = build_feature_vector(sig)
    assert vec.shape == (FEATURE_DIM,)
    assert np.all(vec >= 0.0) and np.all(vec <= 1.0)
