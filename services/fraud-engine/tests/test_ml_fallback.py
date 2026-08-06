"""GB3 + I1 — fallback integrity and rule union (SPEC-W33 §3 B1 gates).

- GB3: with the registry dir EMPTY vs UNSET, the detection pipeline output
  for a fixed fixture is byte-equal (canonical report minus uuid, alert
  records, quarantine writes, published event payloads).
- UNION (I1): with an ML blend forced to 1.0, every rule AUTO_QUARANTINE
  still quarantines, severities only move UP (low -> medium), and ML never
  produces the high band (auto-quarantine stays rule-only).
- I1: a failing scorer degrades to unchanged rule findings, exit 0, logged
  into report.errors.
- torch-absent: package imports, LearnedScorer.load -> None, service boots,
  and the rule path works with torch simulated-absent (import-hook
  subprocess, mirroring graph-ml test_gnn_train_import.py). This test runs
  in BOTH deployments (not marked requires_torch).
"""

from __future__ import annotations

import dataclasses
import json
import subprocess
import sys
from pathlib import Path

import pytest

from fraud_engine.config import Settings
from fraud_engine.detectors import DetectionRunner
from fraud_engine.events import InMemoryPublisher
from fraud_engine.ml.scorer import ScoreResult

from conftest import NOW, TENANT, add_booking_for, make_cycle
from fakes import FakeGraphClient, PropertyGraph

SERVICE_ROOT = Path(__file__).resolve().parents[1]
TESTS_DIR = Path(__file__).resolve().parent


def _fixture_graph() -> PropertyGraph:
    g = PropertyGraph()
    make_cycle(g, TENANT, ["p1", "p2", "p3"])
    add_booking_for(g, TENANT, "p1")  # reward-bearing -> ring is HIGH
    return g


def _canonical_report(report) -> dict:
    d = report.as_dict()
    d.pop("run_id")  # uuid4 per run — everything else is deterministic
    return d


def _canonical_events(publisher: InMemoryPublisher) -> list:
    out = []
    for topic, key, event in publisher.published:
        e = dict(event)
        e.pop("time", None)  # wall-clock inside the CloudEvent envelope
        out.append((topic, key, e))
    return out


def test_gb3_empty_registry_byte_equal_to_unset(tmp_path, settings):
    empty_registry = tmp_path / "empty-registry"
    empty_registry.mkdir()

    graph_a, pub_a = _fixture_graph(), InMemoryPublisher()
    report_a = DetectionRunner(FakeGraphClient(graph_a), pub_a, settings).run(
        tenant_id=TENANT, now=NOW
    )

    graph_b, pub_b = _fixture_graph(), InMemoryPublisher()
    settings_ml = dataclasses.replace(settings, ml_registry_dir=str(empty_registry))
    report_b = DetectionRunner(FakeGraphClient(graph_b), pub_b, settings_ml).run(
        tenant_id=TENANT, now=NOW
    )

    assert json.dumps(_canonical_report(report_a), sort_keys=True) == json.dumps(
        _canonical_report(report_b), sort_keys=True
    )
    assert graph_a.alerts == graph_b.alerts
    assert _canonical_events(pub_a) == _canonical_events(pub_b)
    # Rule behaviour intact in both: the ring quarantined.
    assert sorted(report_a.quarantined) == ["p1", "p2", "p3"]
    assert sorted(report_b.quarantined) == ["p1", "p2", "p3"]


class _MaxBlendScorer:
    """Stub LearnedScorer: every person blends to 1.0 (union edge case)."""

    def score_events(self, events, referral_degree=0) -> ScoreResult:
        return ScoreResult(
            score=1.0,
            ae_norm=1.0,
            clf_prob=1.0,
            model_version="fraud-ae-v9+fraud-clf-v9",
            feature_schema="fv1",
        )

    def blend_reason(self, result: ScoreResult) -> str:
        return f"ml_blend ae={result.ae_norm:.4f} clf={result.clf_prob:.4f}"


def test_union_rules_never_weakened(settings):
    graph = _fixture_graph()
    # D7 low finding: risk_score above alert threshold, below medium.
    pkey = graph.add_person(TENANT, "p-low", risk_score=0.91)
    assert pkey
    pub = InMemoryPublisher()
    runner = DetectionRunner(
        FakeGraphClient(graph), pub, settings, ml_scorer=_MaxBlendScorer()
    )
    report = runner.run(tenant_id=TENANT, now=NOW)

    # Rule AUTO_QUARANTINE stays AUTO_QUARANTINE.
    assert sorted(report.quarantined) == ["p1", "p2", "p3"]
    for aid, alert in graph.alerts.items():
        evidence = json.loads(alert["evidence"])
        # Every scored person carries the ml_blend reason + provenance (GB5).
        assert evidence["ml_blend"] == "ml_blend ae=1.0000 clf=1.0000"
        assert evidence["ml_model_version"] == "fraud-ae-v9+fraud-clf-v9"
        assert evidence["ml_feature_schema"] == "fv1"
        assert evidence["ml_score"] == 1.0
        # UNION: severity only moved up or stayed; ML never reaches high.
        assert alert["severity"] in ("medium", "high")
        if alert["type"] == "gnn_anomaly":
            # low (rule) + blend 1.0 -> medium, NOT high: the high band and
            # auto-quarantine stay rule-only in W33-B.
            assert alert["severity"] == "medium"
            assert "raised low -> medium" in evidence["severity_rule"]
    # p-low was gnn_anomaly only: never quarantined by ML.
    for _, p in graph.persons(TENANT):
        if p.get("person_id") == "p-low":
            assert p.get("quarantine") is not True


def test_ml_failure_degrades_to_rules(settings):
    class _BoomScorer:
        def score_events(self, events, referral_degree=0):
            raise RuntimeError("weights exploded")

        def blend_reason(self, result):
            return "unreachable"

    graph = _fixture_graph()
    pub = InMemoryPublisher()
    runner = DetectionRunner(FakeGraphClient(graph), pub, settings, ml_scorer=_BoomScorer())
    report = runner.run(tenant_id=TENANT, now=NOW)  # exit 0, no raise
    assert sorted(report.quarantined) == ["p1", "p2", "p3"]  # rules intact
    assert any("ml_blend" in err for err in report.errors)
    for alert in graph.alerts.values():
        evidence = json.loads(alert["evidence"])
        assert "ml_blend" not in evidence  # findings untouched
        assert alert["severity"] == "high"  # unchanged rule severities


def test_unset_registry_never_queries_ml(settings):
    graph = _fixture_graph()
    client = FakeGraphClient(graph)
    DetectionRunner(client, InMemoryPublisher(), settings).run(tenant_id=TENANT, now=NOW)
    markers = [cypher.splitlines()[1].strip() for cypher, _ in client.calls]
    assert not any("ml_person_activity" in m for m in markers)


# ---------------------------------------------------------------------------
# torch-absent (I5): simulated via import hook in a subprocess. Runs in both
# deployments — no requires_torch marker.
# ---------------------------------------------------------------------------
SCRIPT = """
import builtins
import dataclasses
import sys

real_import = builtins.__import__

def blocked(name, *args, **kwargs):
    if name == "torch" or name.startswith("torch."):
        raise ImportError(f"No module named {name!r} (simulated)")
    return real_import(name, *args, **kwargs)

builtins.__import__ = blocked
for mod in [m for m in sys.modules if m == "torch" or m.startswith("torch.")]:
    del sys.modules[mod]

sys.path.insert(0, "@@SERVICE_ROOT@@")
sys.path.insert(0, "@@TESTS_DIR@@")

import fraud_engine.ml as ml
assert ml._TORCH_AVAILABLE is False

from fraud_engine.ml import MLBackendUnavailable
from fraud_engine.ml.scorer import LearnedScorer
assert LearnedScorer.load("/tmp/definitely-not-there", "t1") is None
assert LearnedScorer.load("/tmp", "t1") is None  # no artifacts -> None, not raise

from fraud_engine.ml.train import train_models
try:
    train_models("/tmp/no-dataset", "/tmp/no-out")
except MLBackendUnavailable:
    pass
else:
    raise SystemExit("train_models did not raise MLBackendUnavailable")

import fraud_engine.main  # noqa: F401 - heuristic service boot must not crash

# Rule path works end-to-end with ml_registry_dir SET but scorer None.
from fraud_engine.config import Settings
from fraud_engine.detectors import DetectionRunner
from fraud_engine.events import InMemoryPublisher
from conftest import NOW, TENANT, make_cycle, add_booking_for
from fakes import FakeGraphClient, PropertyGraph

g = PropertyGraph()
make_cycle(g, TENANT, ["p1", "p2", "p3"])
add_booking_for(g, TENANT, "p1")
settings = dataclasses.replace(Settings(), ml_registry_dir="/tmp")
report = DetectionRunner(FakeGraphClient(g), InMemoryPublisher(), settings).run(
    tenant_id=TENANT, now=NOW
)
assert sorted(report.quarantined) == ["p1", "p2", "p3"], report.quarantined
print("TORCH-ABSENT-OK")
"""


def test_torch_absent_import_and_rule_path():
    script = SCRIPT.replace("@@SERVICE_ROOT@@", str(SERVICE_ROOT)).replace(
        "@@TESTS_DIR@@", str(TESTS_DIR)
    )
    proc = subprocess.run(
        [sys.executable, "-c", script],
        capture_output=True,
        text=True,
        cwd=str(TESTS_DIR),
    )
    assert proc.returncode == 0, proc.stderr
    assert "TORCH-ABSENT-OK" in proc.stdout
