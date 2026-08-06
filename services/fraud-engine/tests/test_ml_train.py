"""GB1/GB2 — training determinism + metric floor (SPEC-W33 §3 B1 gates).

Dataset: A1 generator ``scripts/seeds/naija_transactions.py`` with
``--persons 800 --days 120 --seed 42`` (chosen params: 1049 persons / ~491
fraud-labeled / 205k events — large enough that the 90/10 val split carries
~48 positives, small enough to generate + train twice in seconds).

GB1: two identical ``train_models`` invocations (same seed, same dataset)
produce BYTE-EQUAL meta.json for both families and bit-identical final val
losses. train.py pins single-threaded torch and
``torch.use_deterministic_algorithms(True)`` (accepted on CPU for these ops)
and meta.json carries no wall-clock fields by design.

GB2: metric floor on the deterministic held-out val split — AUC-PR >= 0.80
and Brier <= 0.15. I3 HONESTY: these metrics are computed against the
SYNTHETIC A1 dataset only; they say nothing about real-world fraud
performance. The floors reflect synthetic separability by design.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import pytest

SERVICE_ROOT = Path(__file__).resolve().parents[1]
REPO_ROOT = Path(__file__).resolve().parents[3]
SEED_SCRIPT = REPO_ROOT / "scripts" / "seeds" / "naija_transactions.py"

DATASET_PERSONS = 800
DATASET_DAYS = 120
DATASET_SEED = 42

torch = pytest.importorskip("torch", reason="GB1/GB2 need the torch overlay")

from fraud_engine.ml.train import train_models  # noqa: E402

GB2_AUC_PR_FLOOR = 0.80
GB2_BRIER_CEILING = 0.15


@pytest.fixture(scope="module")
def dataset_dir(tmp_path_factory) -> Path:
    out = tmp_path_factory.mktemp("a1-data")
    subprocess.run(
        [
            sys.executable,
            str(SEED_SCRIPT),
            "--seed",
            str(DATASET_SEED),
            "--out",
            str(out),
            "--persons",
            str(DATASET_PERSONS),
            "--days",
            str(DATASET_DAYS),
        ],
        check=True,
        capture_output=True,
    )
    return out / "naija_txn" / str(DATASET_SEED)


@pytest.fixture(scope="module")
def two_runs(dataset_dir, tmp_path_factory):
    """Two identical training runs into separate registries (GB1 pair)."""
    base = tmp_path_factory.mktemp("registries")
    summaries = []
    for name in ("run-a", "run-b"):
        out = base / name / "_global"
        summaries.append(train_models(str(dataset_dir), str(out), seed=DATASET_SEED))
    return base, summaries


def _meta_bytes(base: Path, run: str, version: str) -> bytes:
    return (base / run / "_global" / version / "meta.json").read_bytes()


def test_gb1_meta_json_byte_equal(two_runs):
    base, _ = two_runs
    for version in ("fraud-ae-v1", "fraud-clf-v1"):
        a = _meta_bytes(base, "run-a", version)
        b = _meta_bytes(base, "run-b", version)
        assert a == b, f"{version} meta.json differs between identical runs"


def test_gb1_final_val_loss_bit_identical(two_runs):
    base, _ = two_runs
    for version in ("fraud-ae-v1", "fraud-clf-v1"):
        ma = json.loads(_meta_bytes(base, "run-a", version))
        mb = json.loads(_meta_bytes(base, "run-b", version))
        assert ma["val_metrics"]["final_val_loss"] == mb["val_metrics"]["final_val_loss"]
        assert ma["val_metrics"]["final_train_loss"] == mb["val_metrics"]["final_train_loss"]


def test_gb2_metric_floor_on_synthetic_holdout(two_runs):
    """I3: SYNTHETIC-DATA metrics only (A1 seed 42, persons=800, days=120)."""
    base, _ = two_runs
    meta = json.loads(_meta_bytes(base, "run-a", "fraud-clf-v1"))
    vm = meta["val_metrics"]
    assert vm["val_positives"] > 0, "val split must contain positives"
    assert vm["auc_pr"] >= GB2_AUC_PR_FLOOR, f"AUC-PR {vm['auc_pr']} < {GB2_AUC_PR_FLOOR}"
    assert vm["brier"] <= GB2_BRIER_CEILING, f"Brier {vm['brier']} > {GB2_BRIER_CEILING}"
    assert vm["auc_roc"] >= 0.80  # supporting signal, not the gate


def test_meta_json_contents(two_runs):
    base, _ = two_runs
    for version in ("fraud-ae-v1", "fraud-clf-v1"):
        meta = json.loads(_meta_bytes(base, "run-a", version))
        assert meta["seed"] == DATASET_SEED
        assert meta["git_sha"]  # present ("unknown" without .git)
        assert len(meta["dataset_manifest_sha256"]) == 64
        assert meta["feature_schema"] == "fv1"
        assert meta["model_version"] == version
        assert meta["training_args"]["lr"] == 1e-3
        assert meta["training_args"]["val_fraction"] == 0.10
        # GB1 honesty: no wall-clock provenance fields.
        assert "trained_at" not in meta
        assert "created_at" not in meta
    ae = json.loads(_meta_bytes(base, "run-a", "fraud-ae-v1"))
    assert ae["ae_error_stats"]["err_max"] > ae["ae_error_stats"]["err_min"] >= 0.0
    assert ae["label_counts"]["fraud"] > 0
    assert ae["label_counts"]["benign_hard_negatives"] > 0  # benign_* joined


def test_version_increment(two_runs, dataset_dir, tmp_path_factory):
    base, _ = two_runs
    out = base / "run-a" / "_global"
    summary = train_models(str(dataset_dir), str(out), seed=DATASET_SEED)
    assert summary["ae_version"] == "fraud-ae-v2"
    assert summary["clf_version"] == "fraud-clf-v2"
    assert (out / "fraud-ae-v2" / "model.pt").is_file()
    assert (out / "fraud-ae-v2" / "meta.json").is_file()
