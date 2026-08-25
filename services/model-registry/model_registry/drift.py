"""Drift monitoring (SPEC-W33 §4 C2).

Two pure-math primitives, no deps beyond stdlib (I5):

* ``psi`` — Population Stability Index between a reference histogram and a
  serving-time histogram over the SAME bin edges.
* ``ks_statistic`` — population (empirical-CDF) two-sample Kolmogorov–Smirnov
  statistic.

The scheduled sweep (``DriftJob``), every ``DRIFT_INTERVAL_MINUTES`` (default
15) via APScheduler in main.py, compares per (family, tenant) production model:

  (a) incoming feature distributions (feature_observations, last interval)
      vs the training-snapshot REFERENCE MANIFEST;
  (b) score distributions (score_observations, last interval) vs the trailing
      7-day baseline.

PSI > ``DRIFT_PSI_THRESHOLD`` (default 0.25) → alert on Kafka topic
``opendesk.ops.alerts`` + Prometheus gauge ``opendesk_model_drift_psi{family,tenant}``.

Reference manifest schema (``opendesk/training-manifest/v1``) — written by the
lakehouse ``training_snapshot.py`` job (Slice A, sibling owner); this service
codes against THIS documented schema and ships fixtures:

.. code-block:: json

    {
      "schema": "opendesk/training-manifest/v1",
      "family": "fraud_features",
      "snapshot_date": "2025-06-01",
      "manifest_hash": "sha256:<hex>",
      "seed": 42,
      "features": {
        "<feature_name>": {
          "histogram": {"edges": [e0, e1, ..., eN], "counts": [c0, ..., cN-1]}
        }
      },
      "score_baseline": {"histogram": {"edges": [...], "counts": [...]}}
    }

Histogram semantics: ``edges`` has N+1 ascending bin edges, ``counts`` has N
bin populations. Serving values are binned on the same edges; values below
``edges[0]`` fall in the first bin, values above ``edges[-1]`` in the last.
"""

from __future__ import annotations

import bisect
import json
import logging
import math
import os
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any, Protocol

log = logging.getLogger(__name__)

EPS = 1e-6  # PSI zero-cell smoothing floor


# ---------------------------------------------------------------------------
# Pure math
# ---------------------------------------------------------------------------

def histogram_counts(samples: list[float], edges: list[float]) -> list[int]:
    """Bin ``samples`` on ``edges`` (len(edges)-1 bins; out-of-range clamps)."""
    counts = [0] * (len(edges) - 1)
    for v in samples:
        i = bisect.bisect_right(edges, v) - 1
        i = min(max(i, 0), len(counts) - 1)
        counts[i] += 1
    return counts


def _proportions(counts: list[float]) -> list[float]:
    total = sum(counts)
    if total <= 0:
        return [0.0] * len(counts)
    return [c / total for c in counts]


def psi(expected_counts: list[float], actual_counts: list[float]) -> float:
    """Population Stability Index: sum((a-e) * ln(a/e)) over bins, eps-floored.

    Conventional bands: <0.1 stable, 0.1–0.25 moderate, >0.25 alert.
    """
    if len(expected_counts) != len(actual_counts):
        raise ValueError("psi: bin counts must align")
    e = _proportions(expected_counts)
    a = _proportions(actual_counts)
    total = 0.0
    for ei, ai in zip(e, a):
        ei = max(ei, EPS)
        ai = max(ai, EPS)
        total += (ai - ei) * math.log(ai / ei)
    return total


def ks_statistic(sample_a: list[float], sample_b: list[float]) -> float:
    """Population two-sample KS: sup_x |F_a(x) - F_b(x)| over empirical CDFs."""
    if not sample_a or not sample_b:
        raise ValueError("ks_statistic: both samples must be non-empty")
    a = sorted(sample_a)
    b = sorted(sample_b)
    na, nb = len(a), len(b)
    d = 0.0
    for x in sorted(set(a + b)):
        fa = bisect.bisect_right(a, x) / na
        fb = bisect.bisect_right(b, x) / nb
        d = max(d, abs(fa - fb))
    return d


# ---------------------------------------------------------------------------
# Reference manifest provider
# ---------------------------------------------------------------------------

class ManifestProvider(Protocol):
    def get(self, family: str) -> dict[str, Any] | None:
        """Latest reference manifest for a family, or None (skip honestly)."""
        ...


class DirectoryManifestProvider:
    """Reads ``<dir>/<family>.json`` manifests; missing → None (I1)."""

    def __init__(self, directory: str) -> None:
        self.directory = directory

    def get(self, family: str) -> dict[str, Any] | None:
        path = os.path.join(self.directory, f"{family}.json")
        try:
            with open(path, "r", encoding="utf-8") as fh:
                return json.load(fh)
        except FileNotFoundError:
            return None
        except Exception:  # noqa: BLE001 — corrupt manifest: log-and-continue
            log.exception("manifest unreadable: %s", path)
            return None


# ---------------------------------------------------------------------------
# Drift job
# ---------------------------------------------------------------------------

@dataclass
class DriftFinding:
    family: str
    tenant_id: str
    subject: str           # feature name or "score"
    kind: str              # "feature" | "score"
    psi: float
    ks: float | None
    threshold: float
    alerted: bool


@dataclass
class DriftRun:
    checked: int = 0
    findings: list[DriftFinding] = field(default_factory=list)
    skipped: list[str] = field(default_factory=list)  # honest skip reasons


class DriftJob:
    """One 15-minute sweep. Never raises (I1: Kafka down → log-and-continue,
    no data → honest skip); safe to run from APScheduler.
    """

    def __init__(
        self,
        store,
        manifests: ManifestProvider,
        publisher,
        *,
        threshold: float = 0.25,
        gauge=None,
        window_minutes: int = 15,
        baseline_days: int = 7,
        min_samples: int = 10,
        alerts_topic: str = "opendesk.ops.alerts",
    ) -> None:
        self.store = store
        self.manifests = manifests
        self.publisher = publisher
        self.threshold = threshold
        self.gauge = gauge
        self.window = timedelta(minutes=window_minutes)
        self.baseline = timedelta(days=baseline_days)
        self.min_samples = min_samples
        self.alerts_topic = alerts_topic

    def _set_gauge(self, family: str, tenant: str, value: float) -> None:
        if self.gauge is not None:
            self.gauge.labels(family=family, tenant=tenant).set(value)

    def _alert(self, finding: DriftFinding) -> None:
        # I1: publisher.publish never raises.
        self.publisher.publish(self.alerts_topic, {
            "type": "model_drift",
            "family": finding.family,
            "tenant_id": finding.tenant_id,
            "subject": finding.subject,
            "kind": finding.kind,
            "psi": round(finding.psi, 6),
            "ks": None if finding.ks is None else round(finding.ks, 6),
            "threshold": finding.threshold,
            "observed_at": datetime.now(timezone.utc).isoformat(),
        })

    def run_once(self, now: datetime | None = None) -> DriftRun:
        now = now or datetime.now(timezone.utc)
        run = DriftRun()
        try:
            productions = self.store.list_productions()
        except Exception:  # noqa: BLE001 — DB down: log-and-continue (I1)
            log.exception("drift sweep: cannot list productions; skipping tick")
            run.skipped.append("store.list_productions failed")
            return run

        for row in productions:
            family, tenant = row["family"], str(row["tenant_id"])
            manifest = self.manifests.get(family)
            if manifest is None:
                run.skipped.append(f"{family}/{tenant}: no reference manifest")
                continue
            max_psi = 0.0
            # (a) feature distributions vs training-snapshot reference
            ref_features = (manifest.get("features") or {})
            try:
                window = self.store.feature_window(
                    family, tenant, now - self.window, now)
            except Exception:  # noqa: BLE001
                log.exception("drift sweep: feature window failed for %s/%s",
                              family, tenant)
                continue
            for fname, hist in ref_features.items():
                ref = (hist or {}).get("histogram") or {}
                edges, counts = ref.get("edges"), ref.get("counts")
                samples = window.get(fname) or []
                if not edges or not counts or len(samples) < self.min_samples:
                    continue
                value = psi(counts, histogram_counts(samples, edges))
                max_psi = max(max_psi, value)
                run.checked += 1
                if value > self.threshold:
                    finding = DriftFinding(
                        family=family, tenant_id=tenant, subject=fname,
                        kind="feature", psi=value, ks=None,
                        threshold=self.threshold, alerted=True)
                    run.findings.append(finding)
                    self._alert(finding)
            # (b) score distribution vs trailing 7-day baseline
            try:
                current = self.store.score_window(
                    family, tenant, now - self.window, now)
                baseline = self.store.score_window(
                    family, tenant, now - self.baseline, now - self.window)
            except Exception:  # noqa: BLE001
                log.exception("drift sweep: score window failed for %s/%s",
                              family, tenant)
                current, baseline = [], []
            if len(current) >= self.min_samples and len(baseline) >= self.min_samples:
                ks = ks_statistic(current, baseline)
                lo = min(min(current), min(baseline))
                hi = max(max(current), max(baseline))
                edges = [lo + (hi - lo) * i / 10 for i in range(11)]
                value = psi(histogram_counts(baseline, edges),
                            histogram_counts(current, edges))
                max_psi = max(max_psi, value)
                run.checked += 1
                if value > self.threshold:
                    finding = DriftFinding(
                        family=family, tenant_id=tenant, subject="score",
                        kind="score", psi=value, ks=ks,
                        threshold=self.threshold, alerted=True)
                    run.findings.append(finding)
                    self._alert(finding)
            self._set_gauge(family, tenant, max_psi)
        return run
