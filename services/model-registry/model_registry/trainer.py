"""Continuous training scheduler (SPEC-W33 §4 C5).

Nightly APScheduler cron (``TRAIN_CRON_HOUR``, default 02:00). Per family:

  pull latest training snapshot → run the family trainer → evaluate on
  holdout → CALIBRATION GATE → auto-promote staging→production, else stay
  staging + alert.

CALIBRATION GATE: ``brier ≤ 0.20`` AND ``AUC-PR regression vs current
production ≤ 2 points (0.02)`` (first promotion of a family has no production
baseline, so only the Brier leg applies).

Family trainers live in SIBLING services (fraud-api, credit-bureau, graph-ml)
— this service only orchestrates through the pluggable ``FamilyTrainer``
protocol; tests ship stub/fixture implementations. The tick is a plain
callable (``run_nightly_tick(store, trainers, now)``) so a gate can invoke it
manually without waiting for cron.

Provenance (I2): the new model_version row records the snapshot manifest hash
(dataset_hash), the snapshot seed (seed) and the service build's git sha
(git_sha), plus the gate inputs inside metrics jsonb.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Mapping, Protocol

log = logging.getLogger(__name__)

# Gate defaults mirror config.Settings; kept here so run_nightly_tick is a
# self-contained plain callable.
BRIER_MAX = 0.20
AUCPR_TOLERANCE = 0.02


@dataclass(frozen=True)
class SnapshotRef:
    """Pointer to the latest versioned training snapshot (Slice A output)."""
    family: str
    tenant_id: str
    uri: str
    manifest_hash: str          # → model_version.dataset_hash (I2)
    seed: int                   # → model_version.seed (I2)


@dataclass(frozen=True)
class TrainResult:
    artifact_uri: str
    # Must carry holdout "brier" and "auc_pr"; producers label their data
    # basis (synthetic vs real) inside metrics per I3.
    metrics: dict[str, float]
    dataset_hash: str = ""      # may extend/override the manifest hash


class FamilyTrainer(Protocol):
    """Pluggable per-family trainer hook (implemented by sibling services)."""

    def latest_snapshot(self) -> SnapshotRef | None:
        """Latest snapshot for this family, or None → skip honestly."""
        ...

    def train(self, snapshot: SnapshotRef, seed: int) -> TrainResult:
        """Train + evaluate on holdout; must not fabricate metrics (I3)."""
        ...


@dataclass
class TickResult:
    family: str
    tenant_id: str | None = None
    version: int | None = None
    decision: str = "skipped"   # promoted | held | skipped | error
    reason: str = ""
    brier: float | None = None
    auc_pr: float | None = None
    gate: dict[str, Any] = field(default_factory=dict)


class _NoopAlerter:
    def publish(self, topic: str, payload: dict) -> None:  # pragma: no cover
        log.warning("training alert (no alerter): %s %s", topic, payload)


def gate_decision(result: TrainResult, production: dict | None, *,
                  brier_max: float = BRIER_MAX,
                  aucpr_tolerance: float = AUCPR_TOLERANCE) -> tuple[bool, dict]:
    """Calibration gate (C5). Returns (passes, gate_detail)."""
    brier = float(result.metrics["brier"])
    auc_pr = float(result.metrics["auc_pr"])
    prod_auc = None
    if production is not None:
        prod_auc = (production.get("metrics") or {}).get("auc_pr")
        prod_auc = None if prod_auc is None else float(prod_auc)
    brier_ok = brier <= brier_max
    if prod_auc is None:
        regression = None
        auc_ok = True
    else:
        regression = prod_auc - auc_pr
        auc_ok = regression <= aucpr_tolerance
    detail = {
        "brier": brier, "brier_max": brier_max, "brier_ok": brier_ok,
        "auc_pr": auc_pr, "production_auc_pr": prod_auc,
        "aucpr_regression": regression, "aucpr_tolerance": aucpr_tolerance,
        "auc_ok": auc_ok,
    }
    return (brier_ok and auc_ok), detail


def run_nightly_tick(store, trainers: Mapping[str, FamilyTrainer],
                     now: datetime | None = None, *,
                     alerter=None, git_sha: str | None = None,
                     brier_max: float = BRIER_MAX,
                     aucpr_tolerance: float = AUCPR_TOLERANCE,
                     alerts_topic: str = "ops.alerts") -> list[TickResult]:
    """One nightly pass over all families. NEVER raises (I1): per-family
    errors are caught and reported as decision='error'.
    """
    now = now or datetime.now(timezone.utc)
    alerter = alerter or _NoopAlerter()
    results: list[TickResult] = []
    for family, trainer in trainers.items():
        res = TickResult(family=family)
        try:
            snapshot = trainer.latest_snapshot()
            if snapshot is None:
                res.reason = "no snapshot available"
                results.append(res)
                continue
            res.tenant_id = str(snapshot.tenant_id)
            trained = trainer.train(snapshot, snapshot.seed)
            res.brier = float(trained.metrics["brier"])
            res.auc_pr = float(trained.metrics["auc_pr"])
            production = store.get_production(family, str(snapshot.tenant_id))
            passed, detail = gate_decision(
                trained, production,
                brier_max=brier_max, aucpr_tolerance=aucpr_tolerance)
            res.gate = detail
            metrics = dict(trained.metrics)
            metrics["gate"] = detail
            metrics["trained_at"] = now.isoformat()
            metrics["snapshot_uri"] = snapshot.uri
            version = store.register_version(
                family=family,
                tenant_id=str(snapshot.tenant_id),
                artifact_uri=trained.artifact_uri,
                metrics=metrics,
                seed=snapshot.seed,
                dataset_hash=trained.dataset_hash or snapshot.manifest_hash,
                git_sha=git_sha,
            )
            res.version = version["version"]
            if passed:
                store.promote(family, str(snapshot.tenant_id), version["version"])
                res.decision = "promoted"
                res.reason = "calibration gate passed"
            else:
                res.decision = "held"
                res.reason = "calibration gate failed"
                alerter.publish(alerts_topic, {
                    "type": "training_gate_failed",
                    "family": family,
                    "tenant_id": str(snapshot.tenant_id),
                    "version": version["version"],
                    "gate": detail,
                    "observed_at": now.isoformat(),
                })
        except Exception as exc:  # noqa: BLE001 — I1: never crash the tick
            log.exception("nightly tick failed for family %s", family)
            res.decision = "error"
            res.reason = f"{type(exc).__name__}: {exc}"
            try:
                alerter.publish(alerts_topic, {
                    "type": "training_tick_error",
                    "family": family,
                    "error": res.reason,
                    "observed_at": now.isoformat(),
                })
            except Exception:  # noqa: BLE001
                log.exception("alerter failed while reporting tick error")
        results.append(res)
    return results
