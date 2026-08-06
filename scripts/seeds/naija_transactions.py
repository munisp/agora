#!/usr/bin/env python3
"""naija_transactions.py — labeled Nigerian transaction-behavior generator.

SPEC-W33 §2 item A1. Deterministic seeded generator (SEED env / ``--seed``,
default 42) producing **labeled** synthetic training data with realistic
Nigerian transaction patterns, closing the SPEC-W33 §0.7 gap ("synthetic seeds
have no transaction-behavior model and no fraud labels").

Dependency posture (contract F / I5): stdlib + mimesis only. Following the
``seed_agents.py`` convention, Nigerian realism comes from embedded en_NG-style
name lists drawn with a seeded stdlib ``random.Random`` — mimesis is permitted
but not required, and a single stdlib RNG stream is what makes byte-equal
determinism (gate GA1) trivially auditable. JSONL output (NOT parquet):
``pyarrow`` is not in requirements-seeds.txt and SPEC permits the JSONL
fallback; JSONL adds zero new deps.

Model summary (all documented regimes, drawn per event):

- **Personas** (weights): salary_worker .30, market_trader .35, student .20,
  agent .15. Per-persona base rate λ events/day (0.9 / 2.2 / 0.7 / 5.0) with
  exponential inter-arrivals (inhomogeneous Poisson: rate ×1.6 on the 25th–31st
  salary window). Salary workers receive one salary credit per month on a
  payday drawn uniformly in 25–31.
- **Hour-of-day curves:** transfers/USSD evening peak 17–21h (~49% of volume),
  POS midday peak 11–15h, agent/business channels 8–18h, airtime broad with an
  evening bump, salary credits batched 0–8h.
- **Amounts (NGN, log-normal per channel):** transfer median ₦8,500
  (σ=1.6 ⇒ p95 ≈ ₦120k), POS median ₦3,200, agent cash median ₦5,000,
  airtime/USSD ₦100–₦2,000 heavy-tailed, salary ₦50k–₦450k. Round-number
  bias: ~30% of amounts ≥₦1,000 snapped to ₦1,000 multiples, ~25% of the
  remainder ≥₦500 snapped to ₦500 multiples.
- **Geography:** the REAL 774-LGA ``data/nigeria_lgas.csv`` (same file the W17
  seeds use), metro-weighted (Lagos ×4, Kano ×2.5, FCT ×2, Rivers ×2), lat/lon
  from embedded state centroids + jitter, clamped to the Nigeria bbox.

Fraud injection (default rate 1.5% of events, ``--fraud-rate``) WITH LABELS —
six scenarios per SPEC: ``referral_ring`` (3–6 persons circular REFERRED +
reward bookings), ``sybil_cluster`` (same capture agent + same geo cell + burst
+ similar names), ``velocity_burst`` (>30 captures/hour by one agent),
``geo_impossibility`` (>120 km/h consecutive captures), ``ghost_booking``
(≥3 create→cancel ≤10 min by same staff/day), ``structuring`` (many ₦9xx,xxx
sub-₦1M-threshold transfers). Plus benign hard negatives labeled
``fraud: false, scenario: benign_*``: ``benign_family_referral`` (dense acyclic
family referrals), ``benign_market_day_burst`` (plausible market-day capture
burst), ``benign_round_transfer`` (a single large round-amount transfer).
Every scenario type is instantiated at least once per run (even at tiny
``--persons``) so label-completeness gates always have material to check.

PII discipline (I6 / W28): raw names and +234 phones are generated in memory
ONLY as hash inputs; outputs carry ``sha256:<W28-style digest>`` where the
digest is ``_lib.deterministic_id`` = SHA-256(SEED_SALT | value) — the W28
salted-SHA-256 scheme, deterministic per salt (unlike ``hash_pii``'s random
salt, which would break GA1 byte-equality). No BVN is ever generated.

Outputs (``<out>/naija_txn/{seed}/``):

- ``events.jsonl`` — one JSON object/line, sorted by (ts, event_id); every row
  carries ``fraud`` (bool) and ``scenario`` (str|null) label fields.
- ``persons.jsonl`` — opaque person_id, persona, phone/name HASHES, geography;
  label fields on injected persons (sybil nodes etc., consumed by W33-B3 GNN
  supervised head).
- ``graph_edges.jsonl`` — REFERRED / BOOKED / CAPTURED edges with fields
  matching ``graph_ml.extract.TenantGraph`` shapes (from_person_id,
  to_person_id, at, program / person_id, booking_id, offering_id, status,
  showed / agent_id, contact_id, captured_at, channel, lat, lon).
- ``labels.json`` — ``{seed, entries: [{entity_id, scenario, fraud,
  injected_at}]}`` ground truth; every labeled event/person/edge has an entry.
- ``manifest.json`` — seed, generation timestamp, row counts, per-scenario
  injection rates, and sha256 of every sibling output file (I3).

Determinism note (GA1): ``generated_at`` in the manifest is the SYNTHETIC
dataset epoch (``BASE_DATE``), not wall-clock time — the SPEC asks the manifest
to carry a generation timestamp AND the gate demands same-seed byte-equal
outputs; a deterministic timestamp is the only reading that satisfies both.

CLI: ``python scripts/seeds/naija_transactions.py --seed 42 --out /tmp/smoke``
(``--persons`` / ``--days`` / ``--fraud-rate`` / ``--dry-run``; seed also from
``SEED`` env, default 42). Non-zero exit on any failure (fail loud).
"""

from __future__ import annotations

import csv
import hashlib
import json
import math
import random
import sys
from datetime import UTC, date, datetime, timedelta
from pathlib import Path
from typing import Any, Iterator, Mapping, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _lib  # noqa: E402  (contract A lib — deterministic_id = W28 sha256+salt)

SEEDS_DIR = Path(__file__).resolve().parent
DATA_CSV = SEEDS_DIR / "data" / "nigeria_lgas.csv"
DEFAULT_OUT = SEEDS_DIR / "out"

DEFAULT_SEED = 42
DEFAULT_DAYS = 180
DEFAULT_PERSONS = 2_000
DEFAULT_FRAUD_RATE = 0.015

BASE_DATE = date(2026, 1, 1)  # synthetic dataset epoch (deterministic, GA1)

# Nigeria bounding box (documented ASSUMPTION; centroids jittered inside it).
NG_LAT_MIN, NG_LAT_MAX = 4.2, 13.9
NG_LON_MIN, NG_LON_MAX = 2.7, 14.7

OUTPUT_FILES = ("events.jsonl", "persons.jsonl", "graph_edges.jsonl", "labels.json")

# ---------------------------------------------------------------------------
# Embedded en_NG-style name lists (seed_agents.py convention — mimesis-only
# stack, Nigerian realism from embedded lists, drawn with the seeded RNG).
# ---------------------------------------------------------------------------
FIRST_NAMES = [
    # Yoruba
    "Adewale", "Adebayo", "Adekunle", "Olufemi", "Olumide", "Yetunde",
    "Funmilayo", "Temitope", "Oluwaseun", "Damilola",
    # Igbo
    "Chinedu", "Chukwuemeka", "Ngozi", "Adaeze", "Obinna", "Chiamaka",
    "Ikenna", "Uchechi", "Somtochi", "Ebubechukwu",
    # Hausa / northern
    "Aminu", "Ibrahim", "Fatima", "Aisha", "Musa", "Zainab", "Suleiman",
    "Halima", "Usman", "Khadija",
    # Cross-cutting
    "Blessing", "Chinonso", "Osas", "Efe", "Nneka", "Tunde",
]
LAST_NAMES = [
    "Okafor", "Adeyemi", "Abubakar", "Nwachukwu", "Ogunleye", "Eze",
    "Danjuma", "Olawale", "Obi", "Adeleke", "Igwe", "Yakubu", "Oladipo",
    "Chukwu", "Garba", "Balogun", "Okonkwo", "Akinola", "Mohammed",
    "Osei", "Nnaji", "Oyekan", "Sadiq", "Umeh", "Fashola", "Aliyu",
    "Ojo", "Nwosu", "Bello", "Ajayi",
]

# ---------------------------------------------------------------------------
# Geography: state centroids (approx. state capitals; jittered per entity and
# clamped to the Nigeria bbox) + metro weighting over the 774-LGA CSV.
# ---------------------------------------------------------------------------
STATE_CENTROIDS: dict[str, tuple[float, float]] = {
    "Abia": (5.45, 7.52), "Adamawa": (9.33, 12.40), "Akwa Ibom": (5.03, 7.93),
    "Anambra": (6.21, 7.07), "Bauchi": (10.31, 9.84), "Bayelsa": (4.93, 6.27),
    "Benue": (7.73, 8.54), "Borno": (11.83, 13.15), "Cross River": (4.95, 8.32),
    "Delta": (5.70, 5.93), "Ebonyi": (6.32, 8.10), "Edo": (6.34, 5.63),
    "Ekiti": (7.62, 5.22), "Enugu": (6.44, 7.50), "FCT": (9.06, 7.49),
    "Gombe": (10.29, 11.17), "Imo": (5.48, 7.03), "Jigawa": (11.76, 9.34),
    "Kaduna": (10.52, 7.44), "Kano": (12.00, 8.52), "Katsina": (12.99, 7.60),
    "Kebbi": (12.45, 4.20), "Kogi": (7.80, 6.74), "Kwara": (8.50, 4.55),
    "Lagos": (6.52, 3.38), "Nasarawa": (8.49, 8.52), "Niger": (9.62, 6.55),
    "Ogun": (7.15, 3.35), "Ondo": (7.25, 5.19), "Osun": (7.78, 4.54),
    "Oyo": (7.38, 3.90), "Plateau": (9.90, 8.89), "Rivers": (4.82, 7.01),
    "Sokoto": (13.06, 5.24), "Taraba": (8.89, 11.36), "Yobe": (11.75, 11.97),
    "Zamfara": (12.17, 6.66),
}
# Metro weighting (documented ASSUMPTION — no offline census; mirrors the
# zone-weight posture of seed_agents.py): Lagos / Kano / FCT / Port Harcourt.
METRO_STATE_WEIGHT = {"Lagos": 4.0, "Kano": 2.5, "FCT": 2.0, "Rivers": 2.0}

# ---------------------------------------------------------------------------
# Personas, channel mix, rates
# ---------------------------------------------------------------------------
PERSONAS: tuple[tuple[str, float], ...] = (
    ("salary_worker", 0.30),
    ("market_trader", 0.35),
    ("student", 0.20),
    ("agent", 0.15),
)
# Base event rate λ (events/day) per persona; ×SALARY_WINDOW_MULT on 25th–31st.
PERSONA_RATE = {
    "salary_worker": 0.9,
    "market_trader": 2.2,
    "student": 0.7,
    "agent": 5.0,
}
SALARY_WINDOW_MULT = 1.6

# Channel mix per persona (weights, not probabilities).
CHANNEL_MIX: dict[str, dict[str, float]] = {
    "salary_worker": {
        "transfer": 0.34, "pos": 0.24, "ussd": 0.14, "airtime": 0.15,
        "booking": 0.07, "agent_cashin": 0.03, "agent_cashout": 0.03,
    },
    "market_trader": {
        "transfer": 0.28, "pos": 0.14, "ussd": 0.15, "airtime": 0.15,
        "booking": 0.03, "agent_cashin": 0.20, "agent_cashout": 0.15,
    },
    "student": {
        "transfer": 0.20, "pos": 0.15, "ussd": 0.25, "airtime": 0.30,
        "booking": 0.08, "agent_cashin": 0.01, "agent_cashout": 0.01,
    },
    # Field/POS agents: many lead captures + float cash ops.
    "agent": {
        "capture": 0.45, "transfer": 0.18, "pos": 0.10, "ussd": 0.10,
        "airtime": 0.09, "agent_cashin": 0.05, "agent_cashout": 0.03,
    },
}

# Hour-of-day weight curves (24 entries, index == hour). Documented regimes:
# transfers/USSD evening peak 17–21h; POS midday; business 8–18h; salary batch.
HOUR_EVENING = [
    0.10, 0.05, 0.05, 0.05, 0.10, 0.20, 0.50, 1.00, 1.50, 2.00, 2.00, 2.00,
    2.00, 2.00, 2.00, 2.00, 2.50, 4.00, 5.00, 5.00, 4.50, 3.00, 1.50, 0.50,
]
HOUR_MIDDAY = [
    0.05, 0.03, 0.03, 0.03, 0.05, 0.10, 0.30, 0.80, 1.50, 2.50, 3.50, 4.50,
    5.00, 5.00, 4.50, 4.00, 3.00, 2.50, 2.00, 1.50, 1.00, 0.60, 0.30, 0.10,
]
HOUR_BUSINESS = [
    0.05, 0.02, 0.02, 0.02, 0.05, 0.10, 0.30, 1.00, 2.50, 4.00, 4.50, 4.50,
    4.00, 4.00, 4.00, 3.50, 3.00, 2.00, 1.00, 0.50, 0.30, 0.15, 0.10, 0.05,
]
HOUR_AIRTIME = [
    0.30, 0.15, 0.10, 0.10, 0.15, 0.30, 0.60, 1.00, 1.30, 1.50, 1.50, 1.50,
    1.50, 1.50, 1.50, 1.50, 1.60, 2.00, 2.50, 2.50, 2.20, 1.80, 1.20, 0.60,
]
HOUR_BATCH = [
    3.00, 3.00, 3.00, 2.50, 2.50, 2.00, 2.00, 1.50, 0.80, 0.30, 0.10, 0.05,
    0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05, 0.05,
]
CHANNEL_HOURS: dict[str, Sequence[float]] = {
    "transfer": HOUR_EVENING,
    "ussd": HOUR_EVENING,
    "pos": HOUR_MIDDAY,
    "airtime": HOUR_AIRTIME,
    "agent_cashin": HOUR_BUSINESS,
    "agent_cashout": HOUR_BUSINESS,
    "capture": HOUR_BUSINESS,
    "booking": HOUR_BUSINESS,
    "salary": HOUR_BATCH,
}

# ---------------------------------------------------------------------------
# Amount regimes (NGN, log-normal per channel — see module docstring).
# ---------------------------------------------------------------------------
AMOUNT_LOGNORMAL = {  # channel -> (median NGN, sigma)
    "transfer": (8_500.0, 1.6),     # p95 ≈ ₦120k by construction
    "pos": (3_200.0, 1.0),
    "agent_cashin": (5_000.0, 0.9),
    "agent_cashout": (5_000.0, 0.9),
    "airtime": (400.0, 0.9),        # clamped to ₦100–₦2,000 (heavy tail)
    "ussd": (500.0, 1.0),           # clamped to ₦100–₦2,000
    "salary": (150_000.0, 0.55),    # clamped to ₦50k–₦450k
    "booking": (12_000.0, 0.8),
    "capture": (0.0, 0.0),          # lead capture — no money movement
}
AMOUNT_CLAMP = {
    "airtime": (100.0, 2_000.0),
    "ussd": (100.0, 2_000.0),
    "salary": (50_000.0, 450_000.0),
    "transfer": (200.0, 5_000_000.0),
    "pos": (100.0, 2_000_000.0),
    "agent_cashin": (500.0, 2_000_000.0),
    "agent_cashout": (500.0, 2_000_000.0),
    "booking": (1_000.0, 500_000.0),
}
P_ROUND_1000 = 0.30  # round-number bias: multiples of ₦1,000 over-represented
P_ROUND_500 = 0.25   # ...and of the rest, multiples of ₦500

STRUCTURING_MIN, STRUCTURING_MAX = 900_000, 999_500  # sub-₦1M threshold band
MAX_TRAVEL_KMH = 120.0  # W30 D4 geo-impossibility threshold


# ---------------------------------------------------------------------------
# Small helpers
# ---------------------------------------------------------------------------
def _iso(dt: datetime) -> str:
    return dt.astimezone(UTC).isoformat().replace("+00:00", "Z")


def _clamp(x: float, lo: float, hi: float) -> float:
    return max(lo, min(hi, x))


def haversine_km(lat1: float, lon1: float, lat2: float, lon2: float) -> float:
    """Great-circle distance (km) — same math as W30 D4 geo-impossibility."""
    r = 6371.0
    p1, p2 = math.radians(lat1), math.radians(lat2)
    dp = math.radians(lat2 - lat1)
    dl = math.radians(lon2 - lon1)
    a = math.sin(dp / 2) ** 2 + math.cos(p1) * math.cos(p2) * math.sin(dl / 2) ** 2
    return 2 * r * math.asin(math.sqrt(a))


def _hash(value: str) -> str:
    """W28-style salted SHA-256 digest (deterministic per SEED_SALT)."""
    return "sha256:" + _lib.deterministic_id(value)


def load_lgas(path: Path = DATA_CSV) -> list[dict[str, Any]]:
    """Load the REAL 774-LGA reference set with metro weighting + centroids."""
    lgas: list[dict[str, Any]] = []
    with open(path, newline="", encoding="utf-8") as fh:
        for row in csv.DictReader(fh):
            state = row["state"].strip()
            lat, lon = STATE_CENTROIDS[state]
            lgas.append(
                {
                    "lga": row["lga_name"].strip(),
                    "state": state,
                    "zone": row["zone"].strip(),
                    "weight": METRO_STATE_WEIGHT.get(state, 1.0),
                    "lat": lat,
                    "lon": lon,
                }
            )
    if len(lgas) != 774:
        raise RuntimeError(f"expected 774 LGAs in {path}, got {len(lgas)}")
    return lgas


def _geo_point(rng: random.Random, lat: float, lon: float, jitter: float = 0.35) -> tuple[float, float]:
    """Jitter a centroid; clamp to the Nigeria bbox; round for stable bytes."""
    la = _clamp(lat + rng.uniform(-jitter, jitter), NG_LAT_MIN, NG_LAT_MAX)
    lo = _clamp(lon + rng.uniform(-jitter, jitter), NG_LON_MIN, NG_LON_MAX)
    return round(la, 6), round(lo, 6)


def sample_amount(rng: random.Random, channel: str) -> float:
    """Log-normal NGN amount per channel regime + round-number bias."""
    median, sigma = AMOUNT_LOGNORMAL[channel]
    if median == 0.0:
        return 0.0
    amount = rng.lognormvariate(math.log(median), sigma)
    lo, hi = AMOUNT_CLAMP.get(channel, (100.0, 5_000_000.0))
    amount = _clamp(amount, lo, hi)
    # Round-number bias: multiples of ₦1,000 (then ₦500) over-represented.
    if amount >= 1_000 and rng.random() < P_ROUND_1000:
        amount = round(amount / 1_000) * 1_000.0
    elif amount >= 500 and rng.random() < P_ROUND_500:
        amount = round(amount / 500) * 500.0
    return round(_clamp(amount, lo, hi), 2)


# ---------------------------------------------------------------------------
# Dataset builder — one RNG stream, fixed iteration order ⇒ byte-equal output
# ---------------------------------------------------------------------------
class DatasetBuilder:
    """Accumulates persons/events/edges/labels; single seeded RNG stream."""

    def __init__(self, seed: int, days: int) -> None:
        self.rng = random.Random(seed)
        self.seed = seed
        self.days = days
        self.lgas = load_lgas()
        self.persons: list[dict[str, Any]] = []
        self.events: list[dict[str, Any]] = []
        self.edges: list[dict[str, Any]] = []
        self.labels: list[dict[str, Any]] = []
        self.scenario_stats: dict[str, dict[str, int]] = {}
        self._event_seq = 0
        self._edge_seq = 0
        self._person_seq = 0
        self._entity_seq = 0

    # -- id factories (opaque, deterministic) --------------------------------
    def _next_event_id(self) -> str:
        self._event_seq += 1
        return f"evt-{self._event_seq:08d}"

    def _next_edge_id(self) -> str:
        self._edge_seq += 1
        return f"edg-{self._edge_seq:08d}"

    def new_person_id(self, kind: str) -> str:
        self._person_seq += 1
        digest = _lib.deterministic_id(
            f"naija-txn:{kind}:{self.seed}:{self._person_seq:06d}"
        )
        return f"per-{digest[:12]}"

    def _entity_id(self, kind: str) -> str:
        self._entity_seq += 1
        digest = _lib.deterministic_id(
            f"naija-txn:{kind}:{self.seed}:{self._entity_seq:06d}"
        )
        return f"{kind[:3]}-{digest[:12]}"

    # -- core emitters ---------------------------------------------------------
    def add_label(self, entity_id: str, scenario: str, fraud: bool, at: datetime) -> None:
        self.labels.append(
            {
                "entity_id": entity_id,
                "scenario": scenario,
                "fraud": bool(fraud),
                "injected_at": _iso(at),
            }
        )

    def _bump_scenario(self, scenario: str, events: int) -> None:
        stats = self.scenario_stats.setdefault(scenario, {"instances": 0, "events": 0})
        stats["instances"] += 1
        stats["events"] += events

    def make_person(
        self,
        persona: str,
        lga: Mapping[str, Any],
        *,
        last_name: str | None = None,
        home: tuple[float, float] | None = None,
        fraud: bool = False,
        scenario: str | None = None,
    ) -> dict[str, Any]:
        """A person row. Raw name/phone are hash inputs ONLY — never emitted."""
        rng = self.rng
        first = rng.choice(FIRST_NAMES)
        last = last_name or rng.choice(LAST_NAMES)
        phone = f"+234{rng.choice(('701', '703', '705', '706', '801', '803', '805', '806', '807', '809', '810', '811', '813', '814', '815', '816', '901', '903', '905'))}{rng.randrange(10_000_000, 99_999_999)}"
        lat, lon = home or _geo_point(rng, float(lga["lat"]), float(lga["lon"]))
        person = {
            "person_id": self.new_person_id("person"),
            "persona": persona,
            "name_hash": _hash(f"name:{first} {last}"),
            "phone_hash": _hash(f"phone:{phone}"),
            "lga": lga["lga"],
            "state": lga["state"],
            "zone": lga["zone"],
            "home_lat": lat,
            "home_lon": lon,
            "is_synthetic": True,
            "fraud": bool(fraud),
            "scenario": scenario,
        }
        self.persons.append(person)
        return person

    def add_event(
        self,
        person: Mapping[str, Any],
        ts: datetime,
        channel: str,
        amount: float,
        *,
        lat: float | None = None,
        lon: float | None = None,
        counterparty: str | None = None,
        reference_id: str | None = None,
        fraud: bool = False,
        scenario: str | None = None,
    ) -> dict[str, Any]:
        event = {
            "event_id": self._next_event_id(),
            "ts": _iso(ts),
            "event_type": channel,
            "person_id": person["person_id"],
            "amount_ngn": round(float(amount), 2),
            "lga": person["lga"],
            "state": person["state"],
            "lat": lat if lat is not None else person["home_lat"],
            "lon": lon if lon is not None else person["home_lon"],
            "counterparty": counterparty,
            "reference_id": reference_id,
            "fraud": bool(fraud),
            "scenario": scenario,
        }
        self.events.append(event)
        if scenario is not None:
            self.add_label(event["event_id"], scenario, fraud, ts)
        return event

    def add_edge(
        self,
        edge_type: str,
        at: datetime,
        *,
        fraud: bool = False,
        scenario: str | None = None,
        **fields: Any,
    ) -> dict[str, Any]:
        edge = {
            "edge_id": self._next_edge_id(),
            "edge_type": edge_type,
            "at": _iso(at),
            "fraud": bool(fraud),
            "scenario": scenario,
        }
        edge.update(fields)
        self.edges.append(edge)
        if scenario is not None:
            self.add_label(edge["edge_id"], scenario, fraud, at)
        return edge


# ---------------------------------------------------------------------------
# Benign population + transaction streams
# ---------------------------------------------------------------------------
def _pick_lga(rng: random.Random, lgas: Sequence[Mapping[str, Any]]) -> Mapping[str, Any]:
    return rng.choices(lgas, weights=[g["weight"] for g in lgas], k=1)[0]


def _pick_persona(rng: random.Random) -> str:
    return rng.choices([p for p, _ in PERSONAS], weights=[w for _, w in PERSONAS], k=1)[0]


def _pick_channel(rng: random.Random, persona: str) -> str:
    mix = CHANNEL_MIX[persona]
    return rng.choices(list(mix), weights=list(mix.values()), k=1)[0]


def _pick_hour(rng: random.Random, channel: str) -> int:
    return rng.choices(range(24), weights=CHANNEL_HOURS.get(channel, HOUR_BUSINESS), k=1)[0]


def _ts_on_day(rng: random.Random, day: date, channel: str, hour: int | None = None) -> datetime:
    h = _pick_hour(rng, channel) if hour is None else hour
    return datetime(day.year, day.month, day.day, h, rng.randrange(60), rng.randrange(60), tzinfo=UTC)


def _month_starts(days: int) -> Iterator[date]:
    """First-of-month dates overlapping the horizon (deterministic order)."""
    d = BASE_DATE.replace(day=1)
    end = BASE_DATE + timedelta(days=days)
    while d < end:
        yield d
        d = (d.replace(day=28) + timedelta(days=7)).replace(day=1)


def build_population(b: DatasetBuilder, n_persons: int) -> None:
    """Persons + counterparties + organic referrals (all benign)."""
    rng = b.rng
    for _ in range(n_persons):
        persona = _pick_persona(rng)
        b.make_person(persona, _pick_lga(rng, b.lgas))

    # Counterparty pools: POS agents, merchants, offerings (opaque ids only).
    b.merchants = [b._entity_id("merchant") for _ in range(max(8, n_persons // 12))]
    b.pos_agents = [b._entity_id("posagent") for _ in range(max(6, n_persons // 20))]
    b.offerings = [
        {"offering_id": b._entity_id("offering"), "price_ngn": round(rng.lognormvariate(math.log(15_000.0), 0.7), 2)}
        for _ in range(max(4, n_persons // 40))
    ]

    # Organic referrals (acyclic by construction: referrer index < referred).
    for i, person in enumerate(b.persons):
        if i > 0 and rng.random() < 0.22:
            referrer = b.persons[rng.randrange(i)]
            day = BASE_DATE + timedelta(days=rng.randrange(0, max(1, b.days)))
            at = _ts_on_day(rng, day, "booking")
            b.add_edge(
                "REFERRED", at,
                from_person_id=referrer["person_id"],
                to_person_id=person["person_id"],
                program="organic-referral",
            )


def build_streams(b: DatasetBuilder) -> None:
    """Per-person time-ordered event streams over the horizon (benign)."""
    rng = b.rng
    horizon_end = BASE_DATE + timedelta(days=b.days)
    for person in b.persons:
        persona = person["persona"]
        base_rate = PERSONA_RATE[persona]

        # Salary credits: one payday per month, day uniform in 25–31 (the
        # salary spike), hour from the batch curve, amount ₦50k–₦450k.
        if persona == "salary_worker":
            for month_start in _month_starts(b.days):
                last_day = (month_start.replace(day=28) + timedelta(days=7)).replace(day=1) - timedelta(days=1)
                payday = month_start.replace(day=25) + timedelta(days=rng.randrange(0, 7))
                payday = min(payday, last_day)
                if BASE_DATE <= payday < horizon_end:
                    ts = _ts_on_day(rng, payday, "salary")
                    b.add_event(person, ts, "salary", sample_amount(rng, "salary"))

        # Inhomogeneous Poisson stream: exponential inter-arrivals with rate
        # boosted ×SALARY_WINDOW_MULT inside the 25th–31st salary window.
        t = rng.expovariate(base_rate)
        while t < b.days:
            day = BASE_DATE + timedelta(days=int(t))
            rate = base_rate * (SALARY_WINDOW_MULT if 25 <= day.day <= 31 else 1.0)
            channel = _pick_channel(rng, persona)
            ts = _ts_on_day(rng, day, channel)
            if channel == "booking":
                offering = rng.choice(b.offerings)
                booking_id = b._entity_id("booking")
                amount = offering["price_ngn"]
                showed = rng.random() < 0.78
                b.add_event(
                    person, ts, "booking", amount,
                    counterparty=offering["offering_id"], reference_id=booking_id,
                )
                b.add_edge(
                    "BOOKED", ts,
                    person_id=person["person_id"], booking_id=booking_id,
                    offering_id=offering["offering_id"],
                    status="completed" if showed else "no_show", showed=showed,
                )
                # Benign cancellations: a share of bookings cancelled >1h later.
                if rng.random() < 0.12:
                    cancel_ts = ts + timedelta(hours=1 + rng.expovariate(1 / 6))
                    if cancel_ts.astimezone(UTC).date() < horizon_end:
                        b.add_event(
                            person, cancel_ts, "cancellation", amount,
                            counterparty=offering["offering_id"], reference_id=booking_id,
                        )
            elif channel == "capture":
                lat, lon = _geo_point(rng, person["home_lat"], person["home_lon"], jitter=0.05)
                contact_id = b._entity_id("contact")
                b.add_event(
                    person, ts, "capture", 0.0, lat=lat, lon=lon, reference_id=contact_id,
                )
                b.add_edge(
                    "CAPTURED", ts,
                    agent_id=person["person_id"], contact_id=contact_id,
                    captured_at=_iso(ts), channel="field-capture", lat=lat, lon=lon,
                )
            else:
                counterparty = None
                if channel == "pos":
                    counterparty = rng.choice(b.merchants)
                elif channel in ("agent_cashin", "agent_cashout"):
                    counterparty = rng.choice(b.pos_agents)
                b.add_event(person, ts, channel, sample_amount(rng, channel), counterparty=counterparty)
            t += rng.expovariate(rate)


# ---------------------------------------------------------------------------
# Fraud scenario injectors (all labeled) + benign hard negatives
# ---------------------------------------------------------------------------
def _rand_day(rng: random.Random, days: int, margin: int = 0) -> date:
    return BASE_DATE + timedelta(days=rng.randrange(0, max(1, days - margin)))


def _agent_person(b: DatasetBuilder) -> Mapping[str, Any]:
    agents = [p for p in b.persons if p["persona"] == "agent"]
    return b.rng.choice(agents or b.persons)


def _scenario_referral_ring(b: DatasetBuilder) -> int:
    """3–6 persons circular REFERRED + reward bookings (W30 F1)."""
    rng = b.rng
    n = rng.randrange(3, 7)
    members = rng.sample(b.persons, min(n, len(b.persons)))
    t0 = _ts_on_day(rng, _rand_day(rng, b.days), "booking")
    for member in members:
        b.add_label(member["person_id"], "referral_ring", True, t0)
    events = 0
    for i, member in enumerate(members):
        nxt = members[(i + 1) % len(members)]
        at = t0 + timedelta(minutes=rng.randrange(0, 60))
        b.add_edge(
            "REFERRED", at,
            from_person_id=member["person_id"], to_person_id=nxt["person_id"],
            program="reward-referral", fraud=True, scenario="referral_ring",
        )
        # Reward booking paid to the referrer minutes after the referral.
        ts = at + timedelta(minutes=5 + rng.randrange(25))
        offering = rng.choice(b.offerings)
        booking_id = b._entity_id("booking")
        b.add_event(
            member, ts, "booking", round(rng.uniform(500.0, 3_000.0), 2),
            counterparty=offering["offering_id"], reference_id=booking_id,
            fraud=True, scenario="referral_ring",
        )
        b.add_edge(
            "BOOKED", ts,
            person_id=member["person_id"], booking_id=booking_id,
            offering_id=offering["offering_id"], status="completed", showed=True,
            fraud=True, scenario="referral_ring",
        )
        events += 1
    return events


def _scenario_sybil_cluster(b: DatasetBuilder) -> int:
    """Same agent + same geo cell + burst + similar names (W30 F2)."""
    rng = b.rng
    k = rng.randrange(4, 9)
    agent = _agent_person(b)
    lga = _pick_lga(rng, b.lgas)
    cell_lat, cell_lon = _geo_point(rng, float(lga["lat"]), float(lga["lon"]), jitter=0.02)
    family_name = rng.choice(LAST_NAMES)
    t0 = _ts_on_day(rng, _rand_day(rng, b.days), "capture")
    events = 0
    for _ in range(k):
        person = b.make_person(
            _pick_persona(rng), lga, last_name=family_name,
            home=(cell_lat, cell_lon), fraud=True, scenario="sybil_cluster",
        )
        b.add_label(person["person_id"], "sybil_cluster", True, t0)
        ts = t0 + timedelta(minutes=rng.randrange(0, 120))
        lat, lon = _geo_point(rng, cell_lat, cell_lon, jitter=0.005)
        contact_id = b._entity_id("contact")
        b.add_event(
            agent, ts, "capture", 0.0, lat=lat, lon=lon, reference_id=contact_id,
            fraud=True, scenario="sybil_cluster",
        )
        b.add_edge(
            "CAPTURED", ts,
            agent_id=agent["person_id"], contact_id=contact_id,
            captured_at=_iso(ts), channel="field-capture", lat=lat, lon=lon,
            fraud=True, scenario="sybil_cluster",
        )
        channel = rng.choice(("airtime", "ussd"))
        b.add_event(
            person, ts + timedelta(minutes=10 + rng.randrange(80)), channel,
            sample_amount(rng, channel), fraud=True, scenario="sybil_cluster",
        )
        events += 2
    return events


def _scenario_velocity_burst(b: DatasetBuilder) -> int:
    """>30 captures within one hour by one agent (W30 F3 / D3)."""
    rng = b.rng
    agent = _agent_person(b)
    k = 31 + rng.randrange(15)  # 31–45 captures — above the >30/hr detector band
    day = _rand_day(rng, b.days)
    t0 = datetime(day.year, day.month, day.day, rng.randrange(8, 17), tzinfo=UTC)
    cell_lat, cell_lon = _geo_point(rng, agent["home_lat"], agent["home_lon"], jitter=0.05)
    for _ in range(k):
        ts = t0 + timedelta(seconds=rng.randrange(0, 3_500))
        lat, lon = _geo_point(rng, cell_lat, cell_lon, jitter=0.02)
        contact_id = b._entity_id("contact")
        b.add_event(
            agent, ts, "capture", 0.0, lat=lat, lon=lon, reference_id=contact_id,
            fraud=True, scenario="velocity_burst",
        )
        b.add_edge(
            "CAPTURED", ts,
            agent_id=agent["person_id"], contact_id=contact_id,
            captured_at=_iso(ts), channel="field-capture", lat=lat, lon=lon,
            fraud=True, scenario="velocity_burst",
        )
    return k


def _scenario_geo_impossibility(b: DatasetBuilder) -> int:
    """Consecutive captures implying >120 km/h travel (W30 D4)."""
    rng = b.rng
    agent = _agent_person(b)
    states = sorted(STATE_CENTROIDS)
    s1 = rng.choice(states)
    far = [s for s in states if haversine_km(*STATE_CENTROIDS[s1], *STATE_CENTROIDS[s]) > 250.0]
    s2 = rng.choice(far or states)
    t0 = _ts_on_day(rng, _rand_day(rng, b.days, margin=1), "capture")
    lat1, lon1 = _geo_point(rng, *STATE_CENTROIDS[s1], jitter=0.3)
    lat2, lon2 = _geo_point(rng, *STATE_CENTROIDS[s2], jitter=0.3)
    gap_min = rng.randrange(20, 46)
    dist = haversine_km(lat1, lon1, lat2, lon2)
    assert dist / (gap_min / 60.0) > MAX_TRAVEL_KMH  # construction invariant
    events = 0
    for i, (lat, lon) in enumerate(((lat1, lon1), (lat2, lon2))):
        ts = t0 + timedelta(minutes=gap_min * i)
        contact_id = b._entity_id("contact")
        b.add_event(
            agent, ts, "capture", 0.0, lat=lat, lon=lon, reference_id=contact_id,
            fraud=True, scenario="geo_impossibility",
        )
        b.add_edge(
            "CAPTURED", ts,
            agent_id=agent["person_id"], contact_id=contact_id,
            captured_at=_iso(ts), channel="field-capture", lat=lat, lon=lon,
            fraud=True, scenario="geo_impossibility",
        )
        events += 1
    return events


def _scenario_ghost_booking(b: DatasetBuilder) -> int:
    """≥3 create→cancel ≤10 min by the same staff person on one day (W30 F6/D6)."""
    rng = b.rng
    staff = rng.choice(b.persons)
    n = 3 + rng.randrange(3)  # 3–5 bookings
    day = _rand_day(rng, b.days)
    b.add_label(staff["person_id"], "ghost_booking", True, _ts_on_day(rng, day, "booking"))
    events = 0
    for _ in range(n):
        offering = rng.choice(b.offerings)
        booking_id = b._entity_id("booking")
        create_ts = _ts_on_day(rng, day, "booking")
        cancel_ts = create_ts + timedelta(minutes=3 + rng.randrange(7))  # ≤10 min
        b.add_event(
            staff, create_ts, "booking", offering["price_ngn"],
            counterparty=offering["offering_id"], reference_id=booking_id,
            fraud=True, scenario="ghost_booking",
        )
        b.add_edge(
            "BOOKED", create_ts,
            person_id=staff["person_id"], booking_id=booking_id,
            offering_id=offering["offering_id"], status="cancelled", showed=False,
            fraud=True, scenario="ghost_booking",
        )
        b.add_event(
            staff, cancel_ts, "cancellation", offering["price_ngn"],
            counterparty=offering["offering_id"], reference_id=booking_id,
            fraud=True, scenario="ghost_booking",
        )
        events += 2
    return events


def _scenario_structuring(b: DatasetBuilder) -> int:
    """Many ₦9xx,xxx sub-₦1M-threshold transfers by one person (structuring)."""
    rng = b.rng
    person = rng.choice(b.persons)
    n = 8 + rng.randrange(7)  # 8–14 transfers
    span = rng.randrange(2, 6)  # spread over 2–5 days
    base = _rand_day(rng, b.days, margin=span)
    t0 = _ts_on_day(rng, base, "transfer")
    b.add_label(person["person_id"], "structuring", True, t0)
    for i in range(n):
        day = base + timedelta(days=i % span)
        ts = _ts_on_day(rng, day, "transfer")
        amount = float(rng.randrange(STRUCTURING_MIN // 500, STRUCTURING_MAX // 500 + 1) * 500)
        b.add_event(
            person, ts, "transfer", amount,
            counterparty=rng.choice(b.merchants), fraud=True, scenario="structuring",
        )
    return n


def _scenario_benign_family_referral(b: DatasetBuilder) -> int:
    """Hard negative: dense ACYCLIC family referrals (same surname, weeks apart)."""
    rng = b.rng
    referrer = rng.choice(b.persons)
    lga = _pick_lga(rng, b.lgas)
    family_name = rng.choice(LAST_NAMES)
    n = rng.randrange(3, 7)
    base = _rand_day(rng, b.days, margin=45)
    events = 0
    for _ in range(n):
        person = b.make_person(_pick_persona(rng), lga, last_name=family_name)
        day = base + timedelta(days=5 + rng.randrange(40))
        at = _ts_on_day(rng, day, "booking")
        b.add_edge(
            "REFERRED", at,
            from_person_id=referrer["person_id"], to_person_id=person["person_id"],
            program="family-referral", fraud=False, scenario="benign_family_referral",
        )
        b.add_label(person["person_id"], "benign_family_referral", False, at)
        offering = rng.choice(b.offerings)
        b.add_event(
            person, at + timedelta(hours=1 + rng.randrange(20)), "booking",
            offering["price_ngn"], counterparty=offering["offering_id"],
            reference_id=b._entity_id("booking"),
            fraud=False, scenario="benign_family_referral",
        )
        events += 1
    return events


def _scenario_benign_market_day_burst(b: DatasetBuilder) -> int:
    """Hard negative: plausible market-day capture burst (<30/hr, one geo cell)."""
    rng = b.rng
    agent = _agent_person(b)
    k = 22 + rng.randrange(7)  # 22–28 — below the >30/hr fraud threshold
    day = _rand_day(rng, b.days)
    t0 = datetime(day.year, day.month, day.day, rng.randrange(8, 15), tzinfo=UTC)
    cell_lat, cell_lon = _geo_point(rng, agent["home_lat"], agent["home_lon"], jitter=0.03)
    for _ in range(k):
        ts = t0 + timedelta(minutes=rng.randrange(0, 90))
        lat, lon = _geo_point(rng, cell_lat, cell_lon, jitter=0.01)
        contact_id = b._entity_id("contact")
        b.add_event(
            agent, ts, "capture", 0.0, lat=lat, lon=lon, reference_id=contact_id,
            fraud=False, scenario="benign_market_day_burst",
        )
        b.add_edge(
            "CAPTURED", ts,
            agent_id=agent["person_id"], contact_id=contact_id,
            captured_at=_iso(ts), channel="field-capture", lat=lat, lon=lon,
            fraud=False, scenario="benign_market_day_burst",
        )
    return k


def _scenario_benign_round_transfer(b: DatasetBuilder) -> int:
    """Hard negative: one large round-amount transfer (structuring look-alike)."""
    rng = b.rng
    person = rng.choice(b.persons)
    ts = _ts_on_day(rng, _rand_day(rng, b.days), "transfer")
    amount = float(rng.choice((100_000, 250_000, 500_000, 750_000)))
    b.add_event(
        person, ts, "transfer", amount, counterparty=rng.choice(b.merchants),
        fraud=False, scenario="benign_round_transfer",
    )
    return 1


FRAUD_SCENARIOS: dict[str, Any] = {
    "referral_ring": _scenario_referral_ring,
    "sybil_cluster": _scenario_sybil_cluster,
    "velocity_burst": _scenario_velocity_burst,
    "geo_impossibility": _scenario_geo_impossibility,
    "ghost_booking": _scenario_ghost_booking,
    "structuring": _scenario_structuring,
}
BENIGN_SCENARIOS: dict[str, Any] = {
    "benign_family_referral": _scenario_benign_family_referral,
    "benign_market_day_burst": _scenario_benign_market_day_burst,
    "benign_round_transfer": _scenario_benign_round_transfer,
}


def inject_fraud(b: DatasetBuilder, fraud_rate: float) -> None:
    """Inject labeled fraud scenarios up to ~fraud_rate of the event volume.

    Every scenario (fraud AND benign hard negative) is instantiated at least
    once even when the budget is smaller than one full sweep (documented
    floor — label-completeness gates need every scenario present).
    """
    budget = max(1, int(round(len(b.events) * fraud_rate)))
    injected = 0

    def run(name: str, fn: Any) -> int:
        events = fn(b)
        b._bump_scenario(name, events)
        return events

    for name, fn in FRAUD_SCENARIOS.items():  # guaranteed first sweep
        injected += run(name, fn)
    for name, fn in BENIGN_SCENARIOS.items():
        run(name, fn)  # hard negatives are extra signal, not budgeted as fraud

    names = list(FRAUD_SCENARIOS)  # fixed order — deterministic round-robin
    i = 0
    while injected < budget:
        injected += run(names[i % len(names)], FRAUD_SCENARIOS[names[i % len(names)]])
        i += 1


# ---------------------------------------------------------------------------
# Output writers (JSONL — zero new deps; SPEC-permitted parquet fallback path)
# ---------------------------------------------------------------------------
def _write_jsonl(path: Path, rows: list[Mapping[str, Any]]) -> str:
    """Write canonical JSONL (sorted keys, compact separators) → sha256 hex."""
    digest = hashlib.sha256()
    with open(path, "w", encoding="utf-8", newline="\n") as fh:
        for row in rows:
            line = json.dumps(row, separators=(",", ":"), sort_keys=True) + "\n"
            digest.update(line.encode("utf-8"))
            fh.write(line)
    return digest.hexdigest()


def _write_json(path: Path, obj: Mapping[str, Any]) -> str:
    payload = json.dumps(obj, indent=2, sort_keys=True) + "\n"
    path.write_text(payload, encoding="utf-8")
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def write_outputs(b: DatasetBuilder, out_dir: Path, fraud_rate: float) -> dict[str, Any]:
    """Write events/persons/graph_edges/labels + manifest; return the manifest."""
    out_dir.mkdir(parents=True, exist_ok=True)
    # Canonical order: deterministic regardless of interleaved injection order.
    events = sorted(b.events, key=lambda e: (e["ts"], e["event_id"]))
    persons = sorted(b.persons, key=lambda p: p["person_id"])
    edges = sorted(b.edges, key=lambda e: (e["at"], e["edge_id"]))
    labels = sorted(b.labels, key=lambda l: (l["entity_id"], l["injected_at"]))

    sha: dict[str, str] = {}
    sha["events.jsonl"] = _write_jsonl(out_dir / "events.jsonl", events)
    sha["persons.jsonl"] = _write_jsonl(out_dir / "persons.jsonl", persons)
    sha["graph_edges.jsonl"] = _write_jsonl(out_dir / "graph_edges.jsonl", edges)
    labels_doc = {"seed": b.seed, "entries": labels}
    sha["labels.json"] = _write_json(out_dir / "labels.json", labels_doc)

    total_events = len(events)
    per_scenario = {
        name: {
            "instances": stats["instances"],
            "events": stats["events"],
            "rate": round(stats["events"] / total_events, 6) if total_events else 0.0,
        }
        for name, stats in sorted(b.scenario_stats.items())
    }
    fraud_events = sum(1 for e in events if e["fraud"])
    manifest = {
        "spec": "SPEC-W33 §2 A1",
        "generator": "scripts/seeds/naija_transactions.py",
        "seed": b.seed,
        # Deterministic synthetic epoch — GA1 (byte-equal) takes precedence
        # over a wall-clock generation timestamp (module docstring).
        "generated_at": datetime(BASE_DATE.year, BASE_DATE.month, BASE_DATE.day, tzinfo=UTC).isoformat().replace("+00:00", "Z"),
        "horizon_days": b.days,
        "fraud_rate_requested": fraud_rate,
        "counts": {
            "events": total_events,
            "events_fraud": fraud_events,
            "events_benign_hard_negative": sum(1 for e in events if not e["fraud"] and e["scenario"]),
            "persons": len(persons),
            "graph_edges": len(edges),
            "labels": len(labels),
        },
        "per_scenario": per_scenario,
        "sha256": sha,
    }
    _write_json(out_dir / "manifest.json", manifest)
    return manifest


# ---------------------------------------------------------------------------
# Public API + CLI
# ---------------------------------------------------------------------------
def generate_dataset(
    seed: int = DEFAULT_SEED,
    days: int = DEFAULT_DAYS,
    persons: int = DEFAULT_PERSONS,
    fraud_rate: float = DEFAULT_FRAUD_RATE,
    out_dir: Path | str = DEFAULT_OUT,
) -> dict[str, Any]:
    """Build + write the full labeled dataset; returns the manifest dict."""
    b = DatasetBuilder(seed, days)
    build_population(b, persons)
    build_streams(b)
    inject_fraud(b, fraud_rate)
    return write_outputs(b, Path(out_dir), fraud_rate)


def resolve_seed(cli_seed: int | None) -> int:
    """--seed beats SEED env; default 42."""
    if cli_seed is not None:
        return cli_seed
    raw = os_environ_seed()
    return raw if raw is not None else DEFAULT_SEED


def os_environ_seed() -> int | None:
    import os

    raw = os.environ.get("SEED")
    if raw is None:
        return None
    try:
        return int(raw)
    except ValueError:
        print(f"[naija_transactions] SEED={raw!r} is not an int; using default {DEFAULT_SEED}", file=sys.stderr)
        return None


def main(argv: list[str] | None = None) -> int:
    parser = _lib.seed_argparser(
        "Labeled Nigerian transaction-behavior generator (SPEC-W33 §2 A1, JSONL outputs)"
    )
    parser.add_argument("--seed", type=int, default=None, help="RNG seed (default: env SEED or 42)")
    parser.add_argument("--out", type=str, default=str(DEFAULT_OUT),
                        help="output ROOT; dataset lands in <out>/naija_txn/<seed>/")
    parser.add_argument("--days", type=int, default=DEFAULT_DAYS, help="horizon in days (default 180)")
    parser.add_argument("--persons", type=int, default=None,
                        help="population size (default: scaled(2000) via SEED_SCALE)")
    parser.add_argument("--fraud-rate", type=float, default=DEFAULT_FRAUD_RATE,
                        help="fraud injection rate, fraction of events (default 0.015)")
    args = parser.parse_args(argv)

    seed = resolve_seed(args.seed)
    days = args.days
    persons = args.persons if args.persons is not None else max(1, _lib.scaled(DEFAULT_PERSONS))
    if days <= 0 or persons <= 0 or not (0.0 <= args.fraud_rate <= 1.0):
        print("[naija_transactions] invalid --days/--persons/--fraud-rate", file=sys.stderr)
        return 2
    out_dir = Path(args.out) / "naija_txn" / str(seed)

    if args.dry_run:
        print(f"[naija_transactions] would generate seed={seed} days={days} persons={persons} "
              f"fraud_rate={args.fraud_rate} -> {out_dir}")
        print("[naija_transactions] dry-run: no writes")
        return 0
    try:
        manifest = generate_dataset(seed, days, persons, args.fraud_rate, out_dir)
    except Exception as exc:  # noqa: BLE001 - fail loud, non-zero exit
        print(f"[naija_transactions] FAILED: {exc}", file=sys.stderr)
        return 1
    counts = manifest["counts"]
    print(f"[naija_transactions] seed={seed} wrote {counts['events']} events, "
          f"{counts['persons']} persons, {counts['graph_edges']} edges, "
          f"{counts['labels']} labels -> {out_dir}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
