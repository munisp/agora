"""A/B testing (SPEC-W33 §4 C3).

Deterministic bucketing — no state, no RNG:

    sha256(f"{tenant}|{person}|{experiment}") % 100 < pct  →  challenger

Same (tenant, person, experiment) triple always lands on the same arm, across
processes and restarts (GC4). Scoring services resolve the assignment per
request and FAIL-CLOSED to champion on any error or missing experiment row.

Promotion of a winning challenger is MANUAL via /v1/registry/promote — there
is deliberately no auto-promotion path here (human gate, SPEC-W33 §4 C3).
"""

from __future__ import annotations

import hashlib
from typing import Any

CHAMPION = "champion"
CHALLENGER = "challenger"


def bucket(tenant_id: str, person_id: str, experiment_id: str) -> int:
    """Deterministic bucket in [0, 100)."""
    key = f"{tenant_id}|{person_id}|{experiment_id}".encode("utf-8")
    return int.from_bytes(hashlib.sha256(key).digest(), "big") % 100


def assign_arm(experiment: dict[str, Any] | None, *,
               tenant_id: str, person_id: str, now=None) -> str:
    """Resolve the arm for one request. FAIL-CLOSED to champion.

    Returns CHAMPION when the experiment row is missing/stopped/expired/not
    yet started, or on any unexpected error — callers always get a valid arm.
    """
    try:
        if not experiment:
            return CHAMPION
        if experiment.get("status") != "active":
            return CHAMPION
        if now is not None:
            starts = experiment.get("starts_at")
            ends = experiment.get("ends_at")
            if starts is not None and now < starts:
                return CHAMPION
            if ends is not None and now >= ends:
                return CHAMPION
        pct = int(experiment["pct"])
        if bucket(tenant_id, person_id, str(experiment["id"])) < pct:
            return CHALLENGER
        return CHAMPION
    except Exception:  # noqa: BLE001 — fail-closed on ANY error
        return CHAMPION


def arm_version(experiment: dict[str, Any] | None, arm: str) -> int | None:
    """Version to serve for an arm; None when no experiment (caller falls back
    to the current production version — I1)."""
    if not experiment:
        return None
    return (experiment["challenger_version"] if arm == CHALLENGER
            else experiment["champion_version"])
