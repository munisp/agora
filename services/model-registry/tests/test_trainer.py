"""C5/GC6: nightly tick — gate-pass → auto-promoted; sabotaged model
(Brier > 0.20) → stays staging + alert; AUC-PR regression gate; provenance."""

from __future__ import annotations

from datetime import datetime, timezone

from conftest import TENANT_A
from model_registry.trainer import SnapshotRef, TrainResult, run_nightly_tick


class FakePublisher:
    def __init__(self):
        self.messages = []

    def publish(self, topic, payload):
        self.messages.append((topic, payload))


class StubTrainer:
    """Fixture FamilyTrainer: returns a canned snapshot + train result."""

    def __init__(self, metrics, family="fraud-clf", dataset_hash="sha256:train"):
        self.snapshot = SnapshotRef(
            family=family, tenant_id=TENANT_A,
            uri=f"s3://lake/training/{family}/2025-06-01/",
            manifest_hash="sha256:manifest-1", seed=42)
        self.metrics = metrics
        self.dataset_hash = dataset_hash
        self.trained_with = []

    def latest_snapshot(self):
        return self.snapshot

    def train(self, snapshot, seed):
        self.trained_with.append((snapshot, seed))
        return TrainResult(
            artifact_uri=f"s3://lake/models/{snapshot.family}/v-new",
            metrics=dict(self.metrics), dataset_hash=self.dataset_hash)


class NoSnapshotTrainer(StubTrainer):
    def latest_snapshot(self):
        return None


def test_gate_pass_promotes_with_provenance(store):
    trainer = StubTrainer({"auc_pr": 0.93, "brier": 0.12,
                           "data_basis": "synthetic"})
    pub = FakePublisher()
    now = datetime(2025, 6, 2, 2, 0, tzinfo=timezone.utc)
    results = run_nightly_tick(store, {"fraud-clf": trainer}, now,
                               alerter=pub, git_sha="deadbeef")
    (res,) = results
    assert res.decision == "promoted"
    assert res.version == 1
    assert res.gate["brier_ok"] and res.gate["auc_ok"]
    assert pub.messages == []  # no alert on success

    prod = store.get_production("fraud-clf", TENANT_A)
    assert prod["version"] == 1
    # provenance chain (I2): manifest hash, seed, git sha, gate detail
    assert prod["dataset_hash"] == "sha256:train"
    assert prod["seed"] == 42
    assert prod["git_sha"] == "deadbeef"
    assert prod["metrics"]["gate"]["brier"] == 0.12
    assert prod["metrics"]["snapshot_uri"].endswith("2025-06-01/")
    # trainer was invoked with the snapshot seed
    assert trainer.trained_with[0][1] == 42


def test_sabotaged_model_stays_staging_and_alerts(store):
    trainer = StubTrainer({"auc_pr": 0.40, "brier": 0.55,
                           "data_basis": "synthetic-sabotaged"})
    pub = FakePublisher()
    results = run_nightly_tick(store, {"fraud-clf": trainer},
                               alerter=pub, git_sha="deadbeef")
    (res,) = results
    assert res.decision == "held"
    assert res.gate["brier_ok"] is False

    # version registered but still staging; nothing in production
    assert store.get_production("fraud-clf", TENANT_A) is None
    v1 = store.get_version("fraud-clf", TENANT_A, 1)
    assert v1["stage"] == "staging"

    assert len(pub.messages) == 1
    topic, payload = pub.messages[0]
    assert topic == "opendesk.ops.alerts"
    assert payload["type"] == "training_gate_failed"
    assert payload["family"] == "fraud-clf"
    assert payload["gate"]["brier"] == 0.55


def test_aucpr_regression_beyond_two_points_holds(store):
    # current production at auc_pr 0.90; candidate 0.87 → regression 0.03 > 0.02
    store.register_version(family="fraud-clf", tenant_id=TENANT_A,
                           artifact_uri="s3://x/v1", version=1,
                           metrics={"auc_pr": 0.90, "brier": 0.11})
    store.promote("fraud-clf", TENANT_A, 1)
    trainer = StubTrainer({"auc_pr": 0.87, "brier": 0.10})
    pub = FakePublisher()
    (res,) = run_nightly_tick(store, {"fraud-clf": trainer}, alerter=pub)
    assert res.decision == "held"
    assert res.gate["aucpr_regression"] == 0.02999999999999997 or         abs(res.gate["aucpr_regression"] - 0.03) < 1e-9
    # production untouched
    assert store.get_production("fraud-clf", TENANT_A)["version"] == 1
    assert pub.messages and pub.messages[0][1]["type"] == "training_gate_failed"


def test_within_tolerance_promotes(store):
    store.register_version(family="fraud-clf", tenant_id=TENANT_A,
                           artifact_uri="s3://x/v1", version=1,
                           metrics={"auc_pr": 0.90, "brier": 0.11})
    store.promote("fraud-clf", TENANT_A, 1)
    trainer = StubTrainer({"auc_pr": 0.89, "brier": 0.10})  # regression 0.01
    (res,) = run_nightly_tick(store, {"fraud-clf": trainer},
                              alerter=FakePublisher())
    assert res.decision == "promoted"
    assert store.get_production("fraud-clf", TENANT_A)["version"] == 2
    assert store.get_version("fraud-clf", TENANT_A, 1)["stage"] == "archived"


def test_no_snapshot_skips_honestly(store):
    (res,) = run_nightly_tick(store, {"fraud-clf": NoSnapshotTrainer({})},
                              alerter=FakePublisher())
    assert res.decision == "skipped"
    assert "no snapshot" in res.reason


def test_trainer_error_never_crashes_tick(store):
    class BoomTrainer:
        def latest_snapshot(self):
            raise RuntimeError("lakehouse down")

        def train(self, snapshot, seed):  # pragma: no cover
            raise AssertionError("unreachable")

    pub = FakePublisher()
    (res,) = run_nightly_tick(store, {"fraud-clf": BoomTrainer()}, alerter=pub)
    assert res.decision == "error"
    assert "lakehouse down" in res.reason
    assert pub.messages[0][1]["type"] == "training_tick_error"
