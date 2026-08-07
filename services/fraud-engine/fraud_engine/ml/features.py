"""fv1 — deterministic per-person feature vector builder (SPEC-W33 §3 B1).

16 dims, frozen order, computed ONLY from hashed-id event streams
(A1 ``events.jsonl`` row shape: ``person_id``/``ts``/``event_type``/
``amount_ngn``/``lat``/``lon``/``reference_id``/``counterparty``) plus the
person's REFERRED degree from ``graph_edges.jsonl``. No raw PII is ever read:
callers pass rows whose identifiers are already W28-style hashes (I6).

``FEATURE_SCHEMA = "fv1"`` is stamped in every meta.json and every ML-scored
output; any change to dim count/order/meaning requires a schema bump.

SPEC-W34 GF9 (clamp-to-safe, documented): hostile non-finite inputs
(NaN/±inf amounts, coordinates, epochs, referral degrees) never propagate
into the vector. Each poisoned contribution is clamped to the safe default
0.0 (amount absent / point skipped / degree 0) and a final per-dim
``math.isfinite`` sweep guarantees fv1 is always finite — so a poisoned
stream cannot turn the ML score into NaN and silently suppress the
``score >= threshold`` severity-upgrade path (NaN comparisons are False).

Frozen dim order (index: name — meaning):

 0  ev_1h_max         max events in any rolling 60-min window
 1  ev_24h_max        max events in any rolling 24-hour window
 2  ev_7d_max         max events in any rolling 7-day window
 3  ev_total_log      log1p(total events)
 4  amt_mean_log      log1p(mean amount over monetary events, NGN)
 5  amt_std_log       log1p(population std of monetary amounts, NGN)
 6  amt_z_max         max robust z of amount vs per-channel stats
 7  amt_z_mean        mean robust z of amount vs per-channel stats
 8  round_rate        share of monetary amounts >= N500 that are multiples of 500
 9  night_rate        share of events with hour in [00:00, 06:00) UTC
10  geo_last_km_log   log1p(haversine km between the last two geo-tagged events)
11  geo_max_kmh_log   log1p(max implied speed km/h between consecutive
                      geo-tagged events, dt > 0)
12  device_count      DOCUMENTED DEVIATION: the A1 event schema carries no
                      device_id field (SPEC-W33 §3 names "device-count"), so
                      fv1 defines it as log1p(distinct reference_id +
                      counterparty entities the person touches) — the entity
                      footprint that a device dimension would proxy.
13  referral_degree   log1p(in + out REFERRED edge degree)
14  structuring_rate  share of transfer events with 900k <= amount < 1M NGN
                      (sub-threshold structuring band)
15  cancel_rate       share of booking+cancellation events that are
                      cancellations

Per-channel amount stats (median, MAD) are FROZEN constants derived from the
A1 seed-42 reference generation (they match the documented regimes in
``scripts/seeds/naija_transactions.py``: transfer median ~N8.5k, POS ~N3.2k,
agent cash ~N5k). Channels absent from the table fall back to the global
monetary median/MAD so unseen channels still produce bounded z-stats.
"""

from __future__ import annotations

import math
from datetime import UTC, datetime
from typing import Any, Iterable, Mapping, Sequence

FEATURE_SCHEMA = "fv1"
FEATURE_DIM = 16

FEATURE_NAMES: tuple[str, ...] = (
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

# Frozen per-channel (median_ngn, mad_ngn) — see module docstring.
CHANNEL_AMOUNT_STATS: dict[str, tuple[float, float]] = {
    "transfer": (8802.48, 7197.52),
    "pos": (3062.05, 1937.95),
    "airtime": (400.06, 213.19),
    "ussd": (496.82, 299.17),
    "agent_cashin": (5000.0, 2881.81),
    "agent_cashout": (5000.0, 2856.56),
    "booking": (14450.74, 5089.49),
    "cancellation": (18570.52, 6089.90),
    "salary": (148397.23, 51581.68),
}
# Global fallback for channels not in the table (documented: keeps z bounded).
GLOBAL_AMOUNT_STATS: tuple[float, float] = (5000.0, 4200.0)

STRUCTURING_LOW_NGN = 900_000.0
STRUCTURING_HIGH_NGN = 1_000_000.0
ROUND_UNIT_NGN = 500.0
ROUND_MIN_NGN = 500.0
NIGHT_HOURS = frozenset(range(0, 6))  # [00:00, 06:00) UTC

_EARTH_RADIUS_KM = 6371.0088


def _parse_ts(value: Any) -> datetime | None:
    """ISO-8601 (``...Z`` ok) or epoch seconds -> aware UTC datetime."""
    if value is None:
        return None
    if isinstance(value, datetime):
        return value if value.tzinfo else value.replace(tzinfo=UTC)
    if isinstance(value, (int, float)):
        # SPEC-W34 GF9: non-finite / out-of-range epochs are hostile input —
        # drop the timestamp instead of crashing (OverflowError) or
        # propagating NaN/Inf into the vector.
        if isinstance(value, float) and not math.isfinite(value):
            return None
        try:
            return datetime.fromtimestamp(value, tz=UTC)
        except (OverflowError, OSError, ValueError):
            return None
    s = str(value).strip()
    if not s:
        return None
    try:
        dt = datetime.fromisoformat(s.replace("Z", "+00:00"))
    except ValueError:
        return None
    return dt if dt.tzinfo else dt.replace(tzinfo=UTC)


def haversine_km(lat1: float, lon1: float, lat2: float, lon2: float) -> float:
    """Great-circle distance in km (mean Earth radius 6371.0088 km)."""
    p1, p2 = math.radians(lat1), math.radians(lat2)
    dphi = math.radians(lat2 - lat1)
    dlmb = math.radians(lon2 - lon1)
    a = math.sin(dphi / 2.0) ** 2 + math.cos(p1) * math.cos(p2) * math.sin(dlmb / 2.0) ** 2
    return 2.0 * _EARTH_RADIUS_KM * math.asin(min(1.0, math.sqrt(a)))


def _rolling_max(timestamps_s: Sequence[float], window_s: float) -> int:
    """Max number of points in any window of ``window_s`` seconds (two-pointer,
    O(n log n); deterministic on sorted input)."""
    if not timestamps_s:
        return 0
    ts = sorted(timestamps_s)
    best = 1
    lo = 0
    for hi in range(len(ts)):
        while ts[hi] - ts[lo] > window_s:
            lo += 1
        best = max(best, hi - lo + 1)
    return best


def build_feature_vector(
    events: Iterable[Mapping[str, Any]],
    referral_degree: int = 0,
) -> list[float]:
    """Build the frozen 16-dim fv1 vector for one person.

    ``events``: iterable of A1-shaped event dicts for ONE person (any order;
    the builder sorts internally). ``referral_degree``: in+out REFERRED edge
    degree from the graph/edges stream. Pure function — same input, same
    vector, always (GB1 relies on this).
    """
    times: list[float] = []
    hours: list[int] = []
    amounts: list[float] = []
    amount_channels: list[str] = []
    transfers: list[float] = []
    geo: list[tuple[float, float, float]] = []  # (ts_s, lat, lon)
    entities: set[str] = set()
    n_booking_like = 0
    n_cancellation = 0
    n_events = 0

    for ev in events:
        n_events += 1
        dt = _parse_ts(ev.get("ts"))
        if dt is not None:
            times.append(dt.timestamp())
            hours.append(dt.hour)
        etype = str(ev.get("event_type") or "")
        amt = ev.get("amount_ngn")
        try:
            amt_f = float(amt) if amt is not None else 0.0
        except (TypeError, ValueError):
            amt_f = 0.0
        if not math.isfinite(amt_f):
            # SPEC-W34 GF9 clamp-to-safe: a non-finite amount (inf/-inf/NaN)
            # is poisoned input; the documented safe default treats the
            # event's amount as absent (0.0) so it cannot turn amt_* dims
            # into inf/NaN and silently suppress the ML path downstream.
            amt_f = 0.0
        if amt_f > 0.0:
            amounts.append(amt_f)
            amount_channels.append(etype)
            if etype == "transfer":
                transfers.append(amt_f)
        if etype in ("booking", "cancellation"):
            n_booking_like += 1
            if etype == "cancellation":
                n_cancellation += 1
        lat, lon = ev.get("lat"), ev.get("lon")
        if lat is not None and lon is not None and dt is not None:
            try:
                lat_f, lon_f = float(lat), float(lon)
                # SPEC-W34 GF9: non-finite coordinates are poisoned input —
                # skip the geo point so haversine never sees NaN/Inf.
                if math.isfinite(lat_f) and math.isfinite(lon_f):
                    geo.append((dt.timestamp(), lat_f, lon_f))
            except (TypeError, ValueError):
                pass
        ref = ev.get("reference_id")
        if ref:
            entities.add(str(ref))
        cp = ev.get("counterparty")
        if cp:
            entities.add(str(cp))

    # Velocity windows (dims 0-2) + total (dim 3).
    ev_1h = _rolling_max(times, 3600.0)
    ev_24h = _rolling_max(times, 24 * 3600.0)
    ev_7d = _rolling_max(times, 7 * 24 * 3600.0)
    ev_total_log = math.log1p(n_events)

    # Amount stats (dims 4-7).
    if amounts:
        mean_amt = sum(amounts) / len(amounts)
        var = sum((a - mean_amt) ** 2 for a in amounts) / len(amounts)
        amt_mean_log = math.log1p(mean_amt)
        amt_std_log = math.log1p(math.sqrt(var))
        zs = []
        for a, ch in zip(amounts, amount_channels):
            med, mad = CHANNEL_AMOUNT_STATS.get(ch, GLOBAL_AMOUNT_STATS)
            zs.append((a - med) / mad if mad > 0 else 0.0)
        amt_z_max = max(zs)
        amt_z_mean = sum(zs) / len(zs)
    else:
        amt_mean_log = amt_std_log = amt_z_max = amt_z_mean = 0.0

    # Round-number + night-hour rates (dims 8-9).
    eligible = [a for a in amounts if a >= ROUND_MIN_NGN]
    round_rate = (
        sum(1 for a in eligible if a % ROUND_UNIT_NGN == 0.0) / len(eligible) if eligible else 0.0
    )
    night_rate = (sum(1 for h in hours if h in NIGHT_HOURS) / len(hours)) if hours else 0.0

    # Geo dims (10-11).
    geo.sort()
    geo_last_km_log = 0.0
    geo_max_kmh_log = 0.0
    if len(geo) >= 2:
        (t1, la1, lo1), (t2, la2, lo2) = geo[-2], geo[-1]
        geo_last_km_log = math.log1p(haversine_km(la1, lo1, la2, lo2))
        max_kmh = 0.0
        for (ta, laa, loa), (tb, lab, lob) in zip(geo, geo[1:]):
            dt_h = (tb - ta) / 3600.0
            if dt_h <= 0:
                continue
            kmh = haversine_km(laa, loa, lab, lob) / dt_h
            if kmh > max_kmh:
                max_kmh = kmh
        geo_max_kmh_log = math.log1p(max_kmh)

    # Entity footprint proxy for device-count (dim 12) + referral degree (13).
    device_count = math.log1p(len(entities))
    try:
        referral_n = max(0, int(referral_degree))
    except (TypeError, ValueError, OverflowError):
        # SPEC-W34 GF9: poisoned degree (NaN/inf/None-ish) -> safe default 0.
        referral_n = 0
    referral_log = math.log1p(referral_n)

    # Structuring + cancellation rates (dims 14-15).
    structuring_rate = (
        sum(1 for a in transfers if STRUCTURING_LOW_NGN <= a < STRUCTURING_HIGH_NGN)
        / len(transfers)
        if transfers
        else 0.0
    )
    cancel_rate = (n_cancellation / n_booking_like) if n_booking_like else 0.0

    fv = [
        float(ev_1h),
        float(ev_24h),
        float(ev_7d),
        ev_total_log,
        amt_mean_log,
        amt_std_log,
        amt_z_max,
        amt_z_mean,
        round_rate,
        night_rate,
        geo_last_km_log,
        geo_max_kmh_log,
        device_count,
        referral_log,
        structuring_rate,
        cancel_rate,
    ]
    assert len(fv) == FEATURE_DIM, f"fv1 must be {FEATURE_DIM} dims, got {len(fv)}"
    # SPEC-W34 GF9 final clamp (belt-and-braces): fv1 NEVER emits a
    # non-finite dim. Any residual NaN/Inf from hostile input becomes the
    # documented safe default 0.0 so ``score_vector`` always sees finite
    # features and a poisoned stream cannot silently suppress the ML path.
    return [x if math.isfinite(x) else 0.0 for x in fv]
