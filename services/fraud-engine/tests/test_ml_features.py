"""fv1 feature builder unit tests (SPEC-W33 §3 B1).

Pure-function tests: frozen dim order, known values (independently
recomputed), determinism under input shuffling, empty-stream behaviour, and
I6 (outputs are floats only — built from hashed-id streams).
"""

from __future__ import annotations

import math

import pytest

from fraud_engine.ml.features import (
    CHANNEL_AMOUNT_STATS,
    FEATURE_DIM,
    FEATURE_NAMES,
    FEATURE_SCHEMA,
    build_feature_vector,
    haversine_km,
)

# Reference geometry: Lagos <-> Abuja.
LAGOS = (6.5244, 3.3792)
ABUJA = (9.0765, 7.3986)


def _ev(ts, etype, amount=None, lat=None, lon=None, ref=None, cp=None):
    return {
        "ts": ts,
        "event_type": etype,
        "amount_ngn": amount,
        "lat": lat,
        "lon": lon,
        "reference_id": ref,
        "counterparty": cp,
    }


def test_schema_stamp_and_frozen_order():
    assert FEATURE_SCHEMA == "fv1"
    assert FEATURE_DIM == 16
    assert FEATURE_NAMES == (
        "ev_1h_max",
        "ev_24h_max",
        "ev_7d_max",
        "ev_total_log",
        "amt_mean_log",
        "amt_std_log",
        "amt_z_max",
        "amt_z_mean",
        "round_rate",
        "night_rate",
        "geo_last_km_log",
        "geo_max_kmh_log",
        "device_count",
        "referral_degree",
        "structuring_rate",
        "cancel_rate",
    )


def test_haversine_known_pair():
    # Independent reference: Lagos-Abuja great circle ~ 526 km.
    km = haversine_km(*LAGOS, *ABUJA)
    assert km == pytest.approx(525.9, abs=2.0)
    assert haversine_km(*LAGOS, *LAGOS) == pytest.approx(0.0, abs=1e-9)


def test_known_vector_values():
    events = [
        # 3 transfers inside one rolling hour (10:00/10:30/10:55).
        _ev("2026-03-01T10:00:00Z", "transfer", 1000.0, *LAGOS, ref="con-a"),
        _ev("2026-03-01T10:30:00Z", "transfer", 2000.0, ref="con-b"),
        _ev("2026-03-01T10:55:00Z", "pos", 500.0),
        # night event 2h later in Abuja -> geo speed vs the 10:00 Lagos fix.
        _ev("2026-03-01T03:00:00Z", "transfer", 950_000.0, *ABUJA, cp="per-x"),
        # booking + cancellation pair.
        _ev("2026-03-02T12:00:00Z", "booking", None, ref="bk-1"),
        _ev("2026-03-02T12:05:00Z", "cancellation", None, ref="bk-1"),
    ]
    fv = build_feature_vector(events, referral_degree=3)
    assert len(fv) == FEATURE_DIM

    # Velocity: the 10:00-10:55 cluster gives a 1h max of 3.
    assert fv[0] == 3.0
    # 24h window: day-1 has 4 events; day-2 has 2 -> max 4.
    assert fv[1] == 4.0
    assert fv[2] == 6.0
    assert fv[3] == pytest.approx(math.log1p(6))

    # Amount stats over [1000, 2000, 500, 950000].
    amounts = [1000.0, 2000.0, 500.0, 950_000.0]
    mean = sum(amounts) / 4
    var = sum((a - mean) ** 2 for a in amounts) / 4
    assert fv[4] == pytest.approx(math.log1p(mean))
    assert fv[5] == pytest.approx(math.log1p(math.sqrt(var)))

    # Robust z vs frozen per-channel stats (independent recompute).
    zs = [
        (1000.0 - CHANNEL_AMOUNT_STATS["transfer"][0]) / CHANNEL_AMOUNT_STATS["transfer"][1],
        (2000.0 - CHANNEL_AMOUNT_STATS["transfer"][0]) / CHANNEL_AMOUNT_STATS["transfer"][1],
        (500.0 - CHANNEL_AMOUNT_STATS["pos"][0]) / CHANNEL_AMOUNT_STATS["pos"][1],
        (950_000.0 - CHANNEL_AMOUNT_STATS["transfer"][0]) / CHANNEL_AMOUNT_STATS["transfer"][1],
    ]
    assert fv[6] == pytest.approx(max(zs))
    assert fv[7] == pytest.approx(sum(zs) / len(zs))

    # All 4 amounts are >= 500 and multiples of 500.
    assert fv[8] == pytest.approx(1.0)
    # One of 6 events at 03:00 UTC.
    assert fv[9] == pytest.approx(1 / 6)

    # Geo: last two geo fixes (by time) are Lagos 10:00 -> Abuja next day.
    assert fv[10] == pytest.approx(math.log1p(haversine_km(*LAGOS, *ABUJA)))
    # Max speed is Abuja(03:00) -> Lagos(10:00): ~536 km in 7h.
    km = haversine_km(*LAGOS, *ABUJA)
    assert fv[11] == pytest.approx(math.log1p(km / 7.0))

    # device_count proxy: distinct entities {con-a, con-b, per-x, bk-1} = 4.
    assert fv[12] == pytest.approx(math.log1p(4))
    assert fv[13] == pytest.approx(math.log1p(3))
    # 1 of 3 transfers in the 900k-1M structuring band.
    assert fv[14] == pytest.approx(1 / 3)
    # 1 of 2 booking-like events is a cancellation.
    assert fv[15] == pytest.approx(0.5)


def test_determinism_under_shuffle():
    events = [
        _ev("2026-01-05T09:00:00Z", "transfer", 12345.0, *LAGOS, ref="r1"),
        _ev("2026-01-05T09:30:00Z", "pos", 800.0),
        _ev("2026-01-06T21:00:00Z", "ussd", 250.0, *ABUJA, cp="c9"),
        _ev("2026-01-07T01:15:00Z", "airtime", 100.0),
    ]
    a = build_feature_vector(events, referral_degree=2)
    b = build_feature_vector(list(reversed(events)), referral_degree=2)
    assert a == b  # exact equality: the builder sorts internally


def test_empty_and_degenerate_streams():
    assert build_feature_vector([], 0) == [0.0] * FEATURE_DIM
    # events without ts/amounts/geo still produce a bounded vector
    fv = build_feature_vector([_ev(None, "capture"), _ev("not-a-date", "pos", "bad")])
    assert len(fv) == FEATURE_DIM
    assert all(isinstance(x, float) for x in fv)
    assert fv[3] == pytest.approx(math.log1p(2))  # ev_total_log still counts rows


def test_i6_outputs_are_floats_only():
    # Inputs use hashed ids (W28 sha256 style); the vector must contain no
    # identifiers at all — only 16 floats.
    events = [
        _ev(
            "2026-02-01T08:00:00Z",
            "transfer",
            5000.0,
            ref="sha256:deadbeef" * 2,
            cp="per-0123456789ab",
        )
    ]
    fv = build_feature_vector(events, referral_degree=1)
    assert all(type(x) is float for x in fv)
