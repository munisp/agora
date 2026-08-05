"""Deterministic heuristic baseline scorers (SPEC-W29 §3 WS-A).

numpy is the ONLY dependency — cold start works with zero ML stack. Every
score is per-tenant, in [0, 1], and carries model_version + scored_at
(SPEC-W29 §0.4 provenance).
"""

from __future__ import annotations

import math
import re
from collections import defaultdict
from dataclasses import dataclass
from datetime import datetime, timezone

import numpy as np

from . import MODEL_VERSION_HEURISTIC
from .anomaly import risk_scores
from .extract import TenantGraph
from .features import PersonFeatures, build_features, tenant_median_interval

# convert = W_RECENCY*recency_term + W_RESPONSE*response_rate + W_REFERRAL*referral_term
W_RECENCY, W_RESPONSE, W_REFERRAL = 0.45, 0.35, 0.20
RECENCY_SCALE_DAYS = 30.0
REFERRAL_SATURATION = 3.0
# turnout = W_TURNOUT_RESPONSE*response_rate + W_TURNOUT_CONVERSION*messaged->booked rate
W_TURNOUT_RESPONSE, W_TURNOUT_CONVERSION = 0.5, 0.5
TURNOUT_COLD_PRIOR = 0.5  # never messaged -> neutral prior, not 0


def sigmoid(x: float) -> float:
    return float(1.0 / (1.0 + math.exp(-x)))


def clip01(x: float) -> float:
    return float(min(1.0, max(0.0, x)))


def slug(name: str) -> str:
    text = re.sub(r"[^a-z0-9]+", "_", (name or "").lower()).strip("_")
    return text or "service"


@dataclass(frozen=True)
class ScoreRecord:
    tenant_id: str
    person_id: str
    propensity_churn: float
    propensity_convert: float
    propensity_turnout: float
    risk_score: float  # SPEC-W30 §1/§2 anomaly head; fraud-engine D7 reads >= 0.9
    model_version: str
    scored_at: str

    def as_payload(self) -> dict:
        return {
            "tenant_id": self.tenant_id,
            "person_id": self.person_id,
            "propensity_churn": round(self.propensity_churn, 6),
            "propensity_convert": round(self.propensity_convert, 6),
            "propensity_turnout": round(self.propensity_turnout, 6),
            "risk_score": round(self.risk_score, 6),
            "model_version": self.model_version,
            "scored_at": self.scored_at,
        }


@dataclass(frozen=True)
class RecommendationRecord:
    tenant_id: str
    person_id: str
    offering_id: str
    score: float
    rank: int
    reason: str
    model_version: str
    scored_at: str

    def as_payload(self) -> dict:
        return {
            "tenant_id": self.tenant_id,
            "person_id": self.person_id,
            "offering_id": self.offering_id,
            "score": round(self.score, 6),
            "rank": self.rank,
            "reason": self.reason,
            "model_version": self.model_version,
            "scored_at": self.scored_at,
        }


# ---------------------------------------------------------------------------
# Propensity scorers
# ---------------------------------------------------------------------------


def churn_score(f: PersonFeatures, tenant_median_interval_days: float) -> float:
    """sigmoid(days_since_last_booking / tenant_median_interval) — SPEC §3."""
    denom = max(float(tenant_median_interval_days), 1.0)
    return clip01(sigmoid(f.recency_days / denom))


def convert_score(f: PersonFeatures) -> float:
    """f(recency, response_rate, referral_in_degree) — SPEC §3."""
    recency_term = sigmoid((RECENCY_SCALE_DAYS - f.recency_days) / RECENCY_SCALE_DAYS)
    referral_term = min(f.referral_in_degree, REFERRAL_SATURATION) / REFERRAL_SATURATION
    return clip01(
        W_RECENCY * recency_term
        + W_RESPONSE * f.message_response_rate
        + W_REFERRAL * referral_term
    )


def turnout_score(f: PersonFeatures) -> float:
    """f(response_rate, past MESSAGED->BOOKED conversion) — SPEC §3.

    Campaign tenants: likelihood of showing up/voting when contacted. A person
    never messaged gets the neutral cold-start prior, not zero.
    """
    if f.message_count == 0:
        return TURNOUT_COLD_PRIOR
    return clip01(
        W_TURNOUT_RESPONSE * f.message_response_rate
        + W_TURNOUT_CONVERSION * f.message_to_booking_rate
    )


# ---------------------------------------------------------------------------
# Offering co-occurrence recommendations
# ---------------------------------------------------------------------------


class CooccurrenceModel:
    """Booked-A -> booked-B conditional co-occurrence over one tenant.

    cooc[A][B] = # persons who booked both; score(B | person booked A) =
    max_A cooc[A][B] / bookers(A). Deterministic ties break on offering_id.
    """

    def __init__(self, graph: TenantGraph) -> None:
        person_offerings: dict[str, dict[str, int]] = defaultdict(lambda: defaultdict(int))
        for b in graph.bookings:
            person_offerings[b.person_id][b.offering_id] += 1
        self.person_offerings = {p: dict(o) for p, o in person_offerings.items()}
        self.offering_names = {o.offering_id: o.name for o in graph.offerings}
        for booked in person_offerings.values():
            for oid in booked:
                self.offering_names.setdefault(oid, oid)

        self.bookers: dict[str, int] = defaultdict(int)
        self.cooc: dict[str, dict[str, int]] = defaultdict(lambda: defaultdict(int))
        for booked in person_offerings.values():
            offering_ids = sorted(booked)
            for oid in offering_ids:
                self.bookers[oid] += 1
            for i, a in enumerate(offering_ids):
                for b in offering_ids:
                    if a != b:
                        self.cooc[a][b] += 1
        self.max_bookers = max(self.bookers.values(), default=0)

    def conditional(self, from_offering: str, to_offering: str) -> float:
        denom = self.bookers.get(from_offering, 0)
        if denom == 0:
            return 0.0
        return self.cooc.get(from_offering, {}).get(to_offering, 0) / denom

    def popularity(self, offering_id: str) -> float:
        if self.max_bookers == 0:
            return 0.0
        return self.bookers.get(offering_id, 0) / self.max_bookers

    def recommend(
        self, person_id: str, top_k: int
    ) -> list[tuple[str, float, str]]:
        """Top-K (offering_id, score, reason) minus already-booked."""
        booked = self.person_offerings.get(person_id, {})
        already = set(booked)
        candidates: dict[str, tuple[float, str, int]] = {}  # oid -> (score, via, n)
        for a, n in booked.items():
            for b, co in self.cooc.get(a, {}).items():
                if b in already:
                    continue  # never recommend what the person already booked
                score = co / self.bookers[a]
                prev = candidates.get(b)
                if prev is None or score > prev[0] or (score == prev[0] and a < prev[1]):
                    candidates[b] = (score, a, n)

        ranked: list[tuple[str, float, str]] = []
        for oid, (score, via, n) in candidates.items():
            reason = f"booked_{slug(self.offering_names.get(via, via))}_{n}x"
            ranked.append((oid, clip01(score), reason))

        if not ranked:
            # Cold start: tenant-popular offerings the person has not booked.
            for oid in sorted(self.bookers, key=lambda o: (-self.bookers[o], o)):
                if oid in already:
                    continue
                ranked.append((oid, clip01(self.popularity(oid)), "clients_like_them_booked"))

        ranked.sort(key=lambda item: (-item[1], item[0]))
        return ranked[: max(0, top_k)]


# ---------------------------------------------------------------------------
# Tenant scoring entry point
# ---------------------------------------------------------------------------


def score_tenant(
    graph: TenantGraph,
    now: datetime | None = None,
    top_k: int = 5,
    model_version: str = MODEL_VERSION_HEURISTIC,
) -> tuple[list[ScoreRecord], list[RecommendationRecord]]:
    """Score every person of one tenant; returns (scores, recommendations)."""
    now = now or datetime.now(timezone.utc)
    scored_at = now.isoformat()
    features = build_features(graph, now)
    median_interval = tenant_median_interval(features)
    risk_by_person = risk_scores(features)  # SPEC-W30 anomaly head

    scores = [
        ScoreRecord(
            tenant_id=graph.tenant_id,
            person_id=f.person_id,
            propensity_churn=churn_score(f, median_interval),
            propensity_convert=convert_score(f),
            propensity_turnout=turnout_score(f),
            risk_score=risk_by_person.get(f.person_id, 0.0),
            model_version=model_version,
            scored_at=scored_at,
        )
        for f in features
    ]

    model = CooccurrenceModel(graph)
    recommendations: list[RecommendationRecord] = []
    for f in features:
        for rank, (offering_id, score, reason) in enumerate(
            model.recommend(f.person_id, top_k), start=1
        ):
            recommendations.append(
                RecommendationRecord(
                    tenant_id=graph.tenant_id,
                    person_id=f.person_id,
                    offering_id=offering_id,
                    score=score,
                    rank=rank,
                    reason=reason,
                    model_version=model_version,
                    scored_at=scored_at,
                )
            )
    return scores, recommendations
