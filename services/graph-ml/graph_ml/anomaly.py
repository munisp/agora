"""Heuristic anomaly head -> ``risk_score`` (SPEC-W30 §1/§2).

Per-tenant structural outlier detection over the feature vectors already
computed in ``features.py``. Method: for each structural feature, a robust
z-score across the tenant's persons using median/MAD (MAD scaled by 1.4826
to approximate sigma), aggregate the mean of |z| over the structural set,
and squash with ``min(mean_z / 6, 1)``.

Calibration contract (fraud-engine D7 consumes ``Person.risk_score >= 0.9``):
a person only crosses 0.9 when their mean robust |z| exceeds 5.4 — i.e.
genuine structural outliers (e.g. 50x referral degree). Normal fixture
populations stay well below (max ~0.25 on the test fixture).

Cold start (never crash, never NaN): tenants with fewer than
``MIN_PERSONS_FOR_RISK`` scored persons, or zero variance in a feature,
produce risk_score 0.0 for everyone.
"""

from __future__ import annotations

import numpy as np

from .features import PersonFeatures

# Structural features (attr on PersonFeatures, denominator floor). Floors keep
# unit-level wobble (one extra booking, a 0.25 response-rate delta, a week of
# interval drift) from ever reading as anomalous when MAD collapses to 0.
STRUCTURAL_FEATURES: tuple[tuple[str, float], ...] = (
    ("referral_out_degree", 1.0),
    ("referral_in_degree", 1.0),
    ("booking_count", 1.0),
    ("booking_interval_mean", 7.0),
    ("message_response_rate", 0.25),
)

MIN_PERSONS_FOR_RISK = 5
MAD_TO_SIGMA = 1.4826  # consistency constant: MAD * 1.4826 ~= std for normals
Z_SQUASH = 6.0  # risk = min(mean|z| / Z_SQUASH, 1); >0.9 needs mean|z| > 5.4


def risk_scores(features: list[PersonFeatures]) -> dict[str, float]:
    """Map person_id -> risk_score in [0, 1]. Deterministic, NaN-free."""
    person_ids = [f.person_id for f in features]
    if len(features) < MIN_PERSONS_FOR_RISK:
        return {pid: 0.0 for pid in person_ids}

    z_sum = np.zeros(len(features), dtype=np.float64)
    for attr, floor in STRUCTURAL_FEATURES:
        values = np.array([float(getattr(f, attr)) for f in features], dtype=np.float64)
        median = float(np.median(values))
        mad = float(np.median(np.abs(values - median)))
        denom = max(MAD_TO_SIGMA * mad, floor)
        z_sum += np.abs((values - median) / denom)

    mean_z = z_sum / len(STRUCTURAL_FEATURES)
    risk = np.clip(mean_z / Z_SQUASH, 0.0, 1.0)
    risk = np.nan_to_num(risk, nan=0.0, posinf=1.0, neginf=0.0)
    return {pid: float(r) for pid, r in zip(person_ids, risk)}
