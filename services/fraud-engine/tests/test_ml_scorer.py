"""GB4/GB5 — CPU inference latency + provenance (SPEC-W33 §3 B1 gates).

Trains one small registry (A1 generator params: ``--persons 400 --days 90
--seed 42``; reduced epochs — these tests exercise the scorer, not the GB2
metric floor, which lives in test_ml_train.py) and checks:

- GB4: 100 scorings through the learned path on CPU, p95 < 50 ms/event.
  CPU is forced with CUDA_VISIBLE_DEVICES="" and every torch.load uses
  map_location="cpu" (load_latest hardcodes it).
- GB5: ML-scored outputs carry non-null model_version ("fraud-ae-vN +
  fraud-clf-vN") + feature_schema ("fv1"); meta.json round-trips through
  ``load_latest``.
- Blend semantics: score = 0.5*ae_norm + 0.5*clf_prob, ae_norm min-max
  clamped to [0,1] vs meta.json training-error stats.
- I1: absent/incomplete weights -> LearnedScorer.load returns None.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import time
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[3]
SEED_SCRIPT = REPO_ROOT / "scripts" / "seeds" / "naija_transactions.py"

DATASET_PERSONS = 400
DATASET_DAYS = 90
DATASET_SEED = 42

torch = pytest.importorskip("torch", reason="scorer tests need the torch overlay")

from fraud_engine.ml import (  # noqa: E402
    MODEL_VERSION_AE_PREFIX,
    MODEL_VERSION_CLF_PREFIX,
    load_latest,
)
from fraud_engine.ml.autoencoder import FraudAE  # noqa: E402
from fraud_engine.ml.classifier import FraudCLF  # noqa: E402
from fraud_engine.ml.scorer import LearnedScorer, ScoreResult  # noqa: E402
from fraud_engine.ml.train import load_dataset, train_models  # noqa: E402


@pytest.fixture(scope="module")
def registry(tmp_path_factory) -> dict[str, Path]:
    data_out = tmp_path_factory.mktemp("a1-data-small")
    subprocess.run(
        [
            sys.executable,
            str(SEED_SCRIPT),
            "--seed",
            str(DATASET_SEED),
            "--out",
            str(data_out),
            "--persons",
            str(DATASET_PERSONS),
            "--days",
            str(DATASET_DAYS),
        ],
        check=True,
        capture_output=True,
    )
    dataset_dir = data_out / "naija_txn" / str(DATASET_SEED)
    registry = tmp_path_factory.mktemp("registry")
    train_models(
        str(dataset_dir),
        str(registry / "_global"),
        seed=DATASET_SEED,
        ae_epochs=60,
        clf_epochs=80,
    )
    return {"registry": registry, "dataset_dir": dataset_dir}


@pytest.fixture(scope="module")
def scorer(registry) -> LearnedScorer:
    sc = LearnedScorer.load(str(registry["registry"]), "tenant-alpha")
    assert sc is not None
    return sc


def test_gb5_provenance_fields(scorer):
    result = scorer.score_vector([0.0] * 16)
    assert result.model_version == "fraud-ae-v1+fraud-clf-v1"
    assert result.feature_schema == "fv1"
    assert scorer.blend_reason(result).startswith("ml_blend ae=")
    assert " clf=" in scorer.blend_reason(result)


def test_gb5_meta_roundtrip_through_load_latest(registry):
    scope = str(registry["registry"] / "_global")
    for prefix, model_cls in (
        (MODEL_VERSION_AE_PREFIX, FraudAE),
        (MODEL_VERSION_CLF_PREFIX, FraudCLF),
    ):
        loaded = load_latest(scope, prefix)
        assert loaded is not None
        state_dict, meta = loaded
        assert meta["model_version"].startswith(prefix)
        assert meta["feature_schema"] == "fv1"
        assert isinstance(meta["val_metrics"], dict)
        model = model_cls()
        model.load_state_dict(state_dict)  # weights round-trip


def test_blend_formula_and_clamp(scorer):
    zero = scorer.score_vector([0.0] * 16)
    assert zero.score == pytest.approx(0.5 * zero.ae_norm + 0.5 * zero.clf_prob)
    assert 0.0 <= zero.ae_norm <= 1.0
    assert 0.0 <= zero.clf_prob <= 1.0
    # Extreme vector: ae_norm must clamp into [0, 1], never exceed it.
    extreme = scorer.score_vector([500.0, 5000.0, 50000.0] + [100.0] * 13)
    assert 0.0 <= extreme.ae_norm <= 1.0
    assert 0.0 <= extreme.score <= 1.0


def test_gb4_cpu_inference_p95(registry, scorer, monkeypatch):
    """100 scorings, CPU-forced, p95 < 50 ms/event (SYNTHETIC vectors)."""
    monkeypatch.setenv("CUDA_VISIBLE_DEVICES", "")
    assert not torch.cuda.is_available()
    data = load_dataset(str(registry["dataset_dir"]))
    vectors = data["vectors"][:100]
    assert len(vectors) == 100
    latencies_ms = []
    for fv in vectors:
        t0 = time.perf_counter()
        result = scorer.score_vector(fv)
        latencies_ms.append((time.perf_counter() - t0) * 1000.0)
        assert isinstance(result, ScoreResult)
    latencies_ms.sort()
    p95 = latencies_ms[94]
    assert p95 < 50.0, f"p95 {p95:.3f} ms/event >= 50 ms budget"


def test_tenant_dir_preferred_over_global(tmp_path, scorer):
    registry = tmp_path / "reg"
    (registry / "_global").mkdir(parents=True)
    (registry / "tenant-alpha").mkdir(parents=True)
    # Distinct training seeds -> distinct ae error stats to tell dirs apart.
    # Reuse the already-trained artifacts by copying is non-trivial; instead
    # write minimal metas + state dicts via a fresh tiny train per scope.
    import torch as _t

    def _write(scope_dir: Path, seed: int, err_min: float) -> None:
        ae = FraudAE()
        clf = FraudCLF()
        g = _t.Generator().manual_seed(seed)
        with _t.no_grad():
            for p in ae.parameters():
                p.copy_(_t.randn(p.shape, generator=g) * 0.1)
        for version, model, extra in (
            ("fraud-ae-v1", ae, {"ae_error_stats": {"err_min": err_min, "err_max": 9.0}}),
            ("fraud-clf-v1", clf, {}),
        ):
            d = scope_dir / version
            d.mkdir()
            _t.save(model.state_dict(), d / "model.pt")
            (d / "meta.json").write_text(
                json.dumps({"model_version": version, "feature_schema": "fv1", **extra})
            )

    _write(registry / "_global", seed=1, err_min=1.0)
    _write(registry / "tenant-alpha", seed=2, err_min=7.0)
    sc = LearnedScorer.load(str(registry), "tenant-alpha")
    assert sc is not None
    assert sc._err_min == 7.0  # tenant dir wins over _global
    sc_other = LearnedScorer.load(str(registry), "tenant-beta")
    assert sc_other is not None
    assert sc_other._err_min == 1.0  # falls back to _global


def test_absent_weights_return_none(tmp_path):
    assert LearnedScorer.load(None, "t1") is None
    assert LearnedScorer.load(str(tmp_path / "does-not-exist"), "t1") is None
    empty = tmp_path / "empty"
    empty.mkdir()
    assert LearnedScorer.load(str(empty), "t1") is None
    # Incomplete pair (AE only) must NOT engage.
    partial = tmp_path / "partial"
    scope = partial / "_global"
    (scope / "fraud-ae-v1").mkdir(parents=True)
    torch.save(FraudAE().state_dict(), scope / "fraud-ae-v1" / "model.pt")
    (scope / "fraud-ae-v1" / "meta.json").write_text(
        json.dumps({"model_version": "fraud-ae-v1", "feature_schema": "fv1"})
    )
    assert LearnedScorer.load(str(partial), "t1") is None
