"""Training tests (SPEC-W33 §3 B2, gates GB6/GB1 adapted):

  * DETERMINISM: two identical ``train`` invocations (same seed, same
    synthetic A1 dataset, separate out-dirs) produce BYTE-EQUAL meta.json
    and BIT-IDENTICAL val_loss.
  * METRIC SMOKE: on the documented synthetic lending-outcome labels the
    model must beat trivial floors (I3 — stated as synthetic).
  * ARTIFACT: model.pt + meta.json schema, registry versioning, scorer
    round-trip.

Requires torch (requirements-ml.txt overlay) — skipped in rules-only
deployments, matching the graph-ml requires_torch convention.
"""

from __future__ import annotations

import json
import random
from pathlib import Path

import pytest

torch = pytest.importorskip("torch", reason="torch overlay not installed (rules-only deployment)", exc_type=ImportError)

from credit_bureau.ml import train as train_mod  # noqa: E402
from credit_bureau.ml.scorer import LearnedScorer  # noqa: E402


def _write_synthetic_a1(root: Path, n_persons: int = 240, horizon_days: int = 120, seed: int = 7) -> Path:
    """Minimal honest A1-shaped dataset (persons/events/manifest jsonl)."""
    rng = random.Random(seed)
    ds = root / "naija_txn" / "7"
    ds.mkdir(parents=True)
    zones = ["North Central", "North East", "North West", "South East", "South South", "South West"]
    states = ["Lagos", "Kano", "FCT", "Rivers", "Borno", "Oyo"]
    with open(ds / "persons.jsonl", "w", encoding="utf-8") as pf, open(
        ds / "events.jsonl", "w", encoding="utf-8"
    ) as ef:
        for i in range(n_persons):
            pid = f"per-{i:06d}"
            persona = "salary_worker" if i % 3 else "market_trader"
            pf.write(json.dumps({
                "person_id": pid,
                "persona": persona,
                "state": rng.choice(states),
                "zone": rng.choice(zones),
                "fraud": False,
                "scenario": None,
            }) + "\n")
            months = horizon_days // 30
            if persona == "salary_worker":
                salary = rng.choice([60_000, 150_000, 260_000, 400_000])
                for m in range(months):
                    ef.write(json.dumps({
                        "event_type": "salary", "person_id": pid,
                        "amount_ngn": salary, "ts": f"2026-{m + 1:02d}-25T09:00:00Z",
                    }) + "\n")
            else:
                for m in range(months):
                    ef.write(json.dumps({
                        "event_type": "agent_cashin", "person_id": pid,
                        "amount_ngn": rng.choice([30_000, 80_000]),
                        "ts": f"2026-{m + 1:02d}-15T09:00:00Z",
                    }) + "\n")
            # Outflow + bookings; some persons cancel a lot (risky proxy).
            cancels = rng.randrange(0, 5)
            outflow = rng.randrange(5_000, 220_000)
            for m in range(months):
                ef.write(json.dumps({
                    "event_type": "pos", "person_id": pid,
                    "amount_ngn": outflow,
                    "ts": f"2026-{m + 1:02d}-10T12:00:00Z",
                }) + "\n")
                ef.write(json.dumps({
                    "event_type": "booking", "person_id": pid,
                    "amount_ngn": 5_000, "ts": f"2026-{m + 1:02d}-12T12:00:00Z",
                }) + "\n")
                if m < cancels:
                    ef.write(json.dumps({
                        "event_type": "cancellation", "person_id": pid,
                        "amount_ngn": 5_000, "ts": f"2026-{m + 1:02d}-12T14:00:00Z",
                    }) + "\n")
    with open(ds / "manifest.json", "w", encoding="utf-8") as mf:
        json.dump({"seed": seed, "days": horizon_days, "counts": {"persons": n_persons}}, mf)
    return ds


@pytest.fixture(scope="module")
def dataset(tmp_path_factory: pytest.TempPathFactory) -> Path:
    return _write_synthetic_a1(tmp_path_factory.mktemp("a1"))


def test_train_determinism_byte_equal_meta(dataset: Path, tmp_path: Path) -> None:
    out_a = tmp_path / "reg_a"
    out_b = tmp_path / "reg_b"
    meta_a = train_mod.train(str(dataset), str(out_a), seed=42, epochs=60)
    meta_b = train_mod.train(str(dataset), str(out_b), seed=42, epochs=60)
    bytes_a = (out_a / "credit-ml-v1" / "meta.json").read_bytes()
    bytes_b = (out_b / "credit-ml-v1" / "meta.json").read_bytes()
    assert bytes_a == bytes_b, "meta.json must be byte-equal across identical runs (GB1-style)"
    assert meta_a["metrics"]["val_loss"] == meta_b["metrics"]["val_loss"]
    assert repr(meta_a["metrics"]["val_loss"]) == repr(meta_b["metrics"]["val_loss"])


def test_train_metric_smoke_synthetic(dataset: Path, tmp_path: Path) -> None:
    """Floors are lenient on purpose (smoke, not a quality gate); labels
    are SYNTHETIC (I3) — actual numbers are reported in the wave report."""
    meta = train_mod.train(str(dataset), str(tmp_path / "reg"), seed=42, epochs=300)
    m = meta["metrics"]
    assert meta["label_provenance"] == "synthetic"
    assert m["val_mae_score"] < 120.0, m
    assert m["val_auc_pr"] > m["val_default_rate"] * 1.5, m  # beats prevalence
    assert 0.0 <= m["val_brier"] <= 0.25, m
    assert m["val_rows"] > 0 and m["train_rows"] > m["val_rows"]


def test_artifact_schema_and_versioning(dataset: Path, tmp_path: Path) -> None:
    reg = tmp_path / "reg"
    meta1 = train_mod.train(str(dataset), str(reg), seed=42, epochs=20)
    meta2 = train_mod.train(str(dataset), str(reg), seed=42, epochs=20)
    assert meta1["model_version"] == "credit-ml-v1"
    assert meta2["model_version"] == "credit-ml-v2"  # monotonic versioning
    artifact = reg / "credit-ml-v1"
    assert (artifact / "model.pt").is_file()
    meta = json.loads((artifact / "meta.json").read_text())
    for key in ("seed", "git_sha", "dataset", "feature_schema", "synthetic_outcome_model", "metrics"):
        assert key in meta, key
    assert meta["seed"] == 42
    assert meta["dataset"]["sha256"]
    assert meta["feature_schema"]["feature_schema"] == "fv1"
    assert meta["synthetic_outcome_model"]["label_provenance"] == "synthetic"


def test_scorer_roundtrip_and_absent_weights(dataset: Path, tmp_path: Path) -> None:
    reg = tmp_path / "reg"
    # Absent weights -> None (I1).
    assert LearnedScorer.load(str(reg), "tenant-x") is None
    meta = train_mod.train(str(dataset), str(reg), seed=42, epochs=20)
    # train writes at registry ROOT; the scorer resolves per tenant/global,
    # so move the artifact under the "global" tenant dir.
    tenant_dir = reg / "global"
    tenant_dir.mkdir()
    import shutil

    shutil.move(str(reg / "credit-ml-v1"), str(tenant_dir / "credit-ml-v1"))
    scorer = LearnedScorer.load(str(reg), "tenant-x")
    assert scorer is not None
    assert scorer.model_version == meta["model_version"]
    score, prob = scorer.score({"utilization": 0.9, "income_band": 1})
    assert 300.0 <= score <= 900.0
    assert 0.0 <= prob <= 1.0
    # Deterministic inference.
    assert scorer.score({"utilization": 0.9, "income_band": 1}) == (score, prob)
