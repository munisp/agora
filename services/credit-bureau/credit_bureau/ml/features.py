"""fv1 feature vector builder (SPEC-W33 §3 B2) — FROZEN schema.

Feature groups (spec): utilization, dpd history, on-time rate, income
band, tenure, exposure/limits, LGA group, product mix. The frozen order
below is versioned ``fv1`` and stamped into every score payload and every
training meta.json; any change requires a new schema id (fv2) — never
edit in place (I2/I3 provenance).

PII discipline (I6): every input is a DERIVED or HASHED-domain signal
(band indices, rates, clamped amounts, zone index). No names, phones,
BVN/NIN, or raw identifiers ever enter a vector.

Frozen dimension order (12 dims):
  0  utilization            current exposure / total limit, clamp [0,1]
  1  dpd_max_12m            max days-past-due in 12m, /90 clamp [0,1]
  2  dpd_count_12m          # of >0dpd episodes in 12m, /12 clamp [0,1]
  3  on_time_rate           share of obligations paid on time, [0,1]
  4  income_band            0..4 band index /4, [0,1]
  5  tenure_months          months since first known activity, /60 clamp
  6  exposure_ngn           log1p(exposure)/log1p(10_000_000) clamp
  7  limit_ngn              log1p(limit)/log1p(10_000_000) clamp
  8  lga_metro              1 when the LGA/state is a metro hub
                            (Lagos/Kano/FCT/Rivers), else 0
  9  lga_zone_index         geo-political zone index 0..5, /5
                            (alphabetical, frozen: North Central,
                            North East, North West, South East,
                            South South, South West)
  10 product_mix_count      distinct credit products used, /6 clamp
  11 product_mix_secured    share of products that are secured, [0,1]

Neutral defaults (documented): a missing signal in an API payload maps
to the neutral midpoint listed in ``DEFAULTS`` — the vector is always
buildable, never an error (the Go sidecar only carries the 3 rule
signals, so the learned path must degrade gracefully, I1).
"""

from __future__ import annotations

import math
from typing import Any, Mapping, Sequence

import numpy as np

FEATURE_SCHEMA = "fv1"

FEATURE_NAMES: tuple[str, ...] = (
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

FEATURE_DIM = len(FEATURE_NAMES)

ZONES: tuple[str, ...] = (
    "North Central",
    "North East",
    "North West",
    "South East",
    "South South",
    "South West",
)

METRO_STATES: frozenset[str] = frozenset({"Lagos", "Kano", "FCT", "Rivers"})

# Neutral midpoints per feature (documented above).
DEFAULTS: dict[str, float] = {
    "utilization": 0.5,
    "dpd_max_12m": 0.0,
    "dpd_count_12m": 0.0,
    "on_time_rate": 0.75,
    "income_band": 0.5,  # band 2 of 0..4, normalized
    "tenure_months": 0.1,  # 6 months of 60
    "exposure_ngn": 0.35,
    "limit_ngn": 0.45,
    "lga_metro": 0.0,
    "lga_zone_index": 0.4,  # South East-ish midpoint
    "product_mix_count": 1.0 / 6.0,
    "product_mix_secured": 0.0,
}

_LOG_SCALE = math.log1p(10_000_000.0)


def _clamp01(x: float) -> float:
    return 0.0 if x < 0.0 else (1.0 if x > 1.0 else x)


def normalize_feature(name: str, raw: Any) -> float:
    """Normalize one raw signal to [0,1] per the fv1 definitions."""
    if name == "utilization":
        return _clamp01(float(raw))
    if name == "dpd_max_12m":
        return _clamp01(float(raw) / 90.0)
    if name == "dpd_count_12m":
        return _clamp01(float(raw) / 12.0)
    if name == "on_time_rate":
        return _clamp01(float(raw))
    if name == "income_band":
        return _clamp01(float(raw) / 4.0)
    if name == "tenure_months":
        return _clamp01(float(raw) / 60.0)
    if name == "exposure_ngn":
        return _clamp01(math.log1p(max(0.0, float(raw))) / _LOG_SCALE)
    if name == "limit_ngn":
        return _clamp01(math.log1p(max(0.0, float(raw))) / _LOG_SCALE)
    if name == "lga_metro":
        return 1.0 if raw else 0.0
    if name == "lga_zone_index":
        # Accept a zone NAME (canonical, from seeds) or a raw index.
        if isinstance(raw, str):
            idx = ZONES.index(raw) if raw in ZONES else 0
        else:
            idx = int(raw)
        return _clamp01(idx / 5.0)
    if name == "product_mix_count":
        return _clamp01(float(raw) / 6.0)
    if name == "product_mix_secured":
        return _clamp01(float(raw))
    raise KeyError(f"unknown fv1 feature {name!r}")


def build_feature_vector(signals: Mapping[str, Any] | None) -> np.ndarray:
    """Build the frozen fv1 vector; missing signals take DEFAULTS.

    Accepts raw (unnormalized) signal values keyed by feature name;
    already-normalized floats in [0,1] pass through the clamp unchanged
    for the ratio features.
    """
    raw = dict(signals or {})
    vec = np.empty(FEATURE_DIM, dtype=np.float32)
    for i, name in enumerate(FEATURE_NAMES):
        if name in raw and raw[name] is not None:
            vec[i] = normalize_feature(name, raw[name])
        else:
            vec[i] = DEFAULTS[name]
    return vec


def feature_schema_payload() -> dict[str, Any]:
    """Provenance payload for meta.json / API responses (I2)."""
    return {
        "feature_schema": FEATURE_SCHEMA,
        "feature_dim": FEATURE_DIM,
        "feature_names": list(FEATURE_NAMES),
    }


# ---------------------------------------------------------------------------
# A1 dataset derivation (training only) — SPEC-W33 §3 B2 "train.py on A1
# lending-outcome synthetic". A1 (scripts/seeds/naija_transactions.py)
# carries NO lending repayment events (documented gap); the derivations
# below are HONEST, DOCUMENTED proxies from the behavioral stream, stated
# as synthetic (I3). They are used ONLY to build bootstrap training rows.
# ---------------------------------------------------------------------------

# Income bands (NGN/month, documented): 0 = no observed salary,
# 1 < ₦100k, 2 ₦100k–200k, 3 ₦200k–350k, 4 > ₦350k.
def income_band_for(monthly_income_ngn: float) -> int:
    if monthly_income_ngn <= 0:
        return 0
    if monthly_income_ngn < 100_000:
        return 1
    if monthly_income_ngn < 200_000:
        return 2
    if monthly_income_ngn < 350_000:
        return 3
    return 4


_FINANCIAL_CHANNELS = ("transfer", "pos", "ussd", "airtime", "agent_cashin", "agent_cashout")


def derive_signals_from_events(
    person: Mapping[str, Any],
    events: Sequence[Mapping[str, Any]],
    horizon_days: int,
) -> dict[str, Any]:
    """Derive fv1 raw signals for one A1 person from their event stream.

    Proxy documentation (synthetic, I3):
      * income: mean monthly salary credits (salary_worker persona);
        other personas get a persona-level estimate from their observed
        agent/transfer inflow.
      * limit: 3x monthly income estimate (documented constant).
      * exposure: mean monthly outflow (pos + transfer + cash-out).
      * on_time_rate: booking showed-rate proxy (A1 bookings carry
        showed/no_show outcomes on graph edges; cancellations count
        against it here).
      * dpd proxies: A1 has no repayment events; each no-show booking
        counts as one dpd episode with 15 documented days each
        (dpd_max = min(90, 15*episodes)).
      * product_mix: distinct financial channels used; secured share 0
        (A1 has no secured lending products — documented dead-in-boot-
        strap feature, kept so fv1 stays frozen).
    """
    salary_sum = 0.0
    salary_months: set[str] = set()
    inflow_sum = 0.0
    outflow_sum = 0.0
    bookings = 0
    showed = 0
    channels: set[str] = set()
    first_ts = None
    last_ts = None
    for ev in events:
        et = ev.get("event_type")
        amt = float(ev.get("amount_ngn") or 0.0)
        ts = str(ev.get("ts") or "")
        if ts:
            first_ts = ts if first_ts is None or ts < first_ts else first_ts
            last_ts = ts if last_ts is None or ts > last_ts else last_ts
        if et == "salary":
            salary_sum += amt
            salary_months.add(ts[:7])
        elif et in ("agent_cashin",):
            inflow_sum += amt
        elif et in ("pos", "transfer", "agent_cashout"):
            outflow_sum += amt
        if et == "booking":
            bookings += 1
            showed += 1  # adjusted below via cancellation events
        elif et == "cancellation":
            showed -= 1
        if et in _FINANCIAL_CHANNELS:
            channels.add(et)

    months = max(1.0, horizon_days / 30.0)
    if salary_months:
        monthly_income = salary_sum / max(1, len(salary_months))
    else:
        # Non-salary personas: estimate income from cash-in inflow.
        monthly_income = inflow_sum / months
    band = income_band_for(monthly_income)
    limit_ngn = 3.0 * monthly_income
    exposure_ngn = outflow_sum / months
    utilization = (exposure_ngn / limit_ngn) if limit_ngn > 0 else 0.0
    on_time_rate = (showed / bookings) if bookings > 0 else DEFAULTS["on_time_rate"]
    no_shows = max(0, bookings - showed)
    # Tenure: span between the first and last observed event (months);
    # A1 persons exist for the whole horizon, floored at 1 month.
    if first_ts and last_ts:
        try:
            from datetime import datetime

            fmt = "%Y-%m-%dT%H:%M:%SZ"
            span_days = max(
                1.0,
                (datetime.strptime(last_ts, fmt) - datetime.strptime(first_ts, fmt)).total_seconds() / 86400.0,
            )
        except ValueError:
            span_days = float(horizon_days)
    else:
        span_days = float(horizon_days)
    tenure_months = max(1.0, span_days / 30.0)

    return {
        "utilization": utilization,
        "dpd_max_12m": min(90, 15 * no_shows),
        "dpd_count_12m": no_shows,
        "on_time_rate": on_time_rate,
        "income_band": band,
        "tenure_months": tenure_months,
        "exposure_ngn": exposure_ngn,
        "limit_ngn": limit_ngn,
        "lga_metro": str(person.get("state", "")) in METRO_STATES,
        "lga_zone_index": str(person.get("zone", "")),
        "product_mix_count": len(channels),
        "product_mix_secured": 0.0,
    }
