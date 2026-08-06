"""gnn_head tests (SPEC-W33 §3 B3): convergence smoke, GB1 determinism
(bit-identical final val loss + byte-equal meta.json across two identical
classifier runs), masked-metric correctness on a hand-built tiny graph,
val-split determinism, early-stopping behavior, torch-absent import safety,
and the W31 link-mode regression fixture (the default path is untouched).

Requires torch (skipped cleanly on heuristic-only deployments), except the
subprocess import-safety test which runs in BOTH deployments."""

from __future__ import annotations

import pytest

# Optional-stack pattern (NOT module-level importorskip): torch tests carry
# the requires_torch marker and skip cleanly on heuristic deployments; the
# torch-ABSENT import-safety test at the bottom runs in BOTH deployments
# (mirrors test_gnn_train_import.py).
try:
    import torch

    import torch_geometric  # noqa: F401

    _TORCH_STACK = True
except ImportError:
    torch = None  # type: ignore[assignment]
    _TORCH_STACK = False

requires_torch = pytest.mark.requires_torch

import hashlib
import json
import os
import subprocess
import sys
from pathlib import Path

import numpy as np

from graph_ml.config import Settings
from graph_ml.gnn import GNNInsufficientData
from graph_ml.gnn_data import build_graph_data
from graph_ml.gnn_head import (
    HeadConfig,
    _stratified_split,
    binary_metrics,
    masked_bce_loss,
    train_classifier,
)
from graph_ml.gnn_train import (
    SAGEConfig,
    load_latest,
    train_model,
    train_tenant,
    train_tenant_classifier,
)

from .test_gnn_train import NOW, make_toy_graph

NUM_PERSONS = 30
# p00..p05 positives (the referral-ring cluster), p06..p27 negatives,
# p28/p29 absent from the label map (masked out of supervision upstream).
TOY_LABELS = {f"p{i:02d}": (1 if i < 6 else 0) for i in range(28)}
DATASET_SHA = "0" * 64
GENERATED_AT = "2026-01-01T00:00:00Z"


def head_settings(tmp_path, **overrides) -> Settings:
    base = dict(
        model_dir=str(tmp_path),
        device="cpu",
        seed=42,
        gnn_epochs=80,
        gnn_hidden_dim=32,
        gnn_min_persons=20,
        gnn_min_edges=30,
        gnn_head_patience=20,
        gnn_head_val_fraction=0.2,
    )
    base.update(overrides)
    return Settings(**base)


def _toy_data():
    return build_graph_data(make_toy_graph(), NOW)


def _toy_config(**overrides):
    base = dict(hidden_dim=32, epochs=80, seed=42, device="cpu", patience=20)
    base.update(overrides)
    return HeadConfig(**base)


# ---------------------------------------------------------------------------
# Convergence smoke
# ---------------------------------------------------------------------------


@requires_torch
def test_classifier_converges_and_computes_val_metrics():
    result = train_classifier(_toy_data(), TOY_LABELS, _toy_config())
    assert result.train_losses[result.best_epoch] < result.train_losses[0]
    assert result.num_supervised == 28
    assert result.num_positives == 6
    assert result.val_indices  # deterministic val split produced nodes
    metrics = result.metrics
    for key in (
        "precision_pos",
        "recall_pos",
        "precision_neg",
        "recall_neg",
        "auc_pr",
        "brier",
        "threshold",
        "val_nodes",
    ):
        assert key in metrics
    assert metrics["val_nodes"] == len(result.val_indices) > 0
    assert metrics["support_pos"] > 0 and metrics["support_neg"] > 0
    assert 0.0 <= metrics["auc_pr"] <= 1.0
    assert 0.0 <= metrics["brier"] <= 1.0
    assert 0.0 <= result.final_val_loss


@requires_torch
def test_classifier_requires_positive_labels():
    with pytest.raises(GNNInsufficientData, match="no positive labels"):
        train_classifier(
            _toy_data(), {f"p{i:02d}": 0 for i in range(28)}, _toy_config()
        )


@requires_torch
def test_classifier_requires_supervised_nodes():
    with pytest.raises(GNNInsufficientData, match="no supervised"):
        train_classifier(_toy_data(), {}, _toy_config())


# ---------------------------------------------------------------------------
# GB1 determinism
# ---------------------------------------------------------------------------


@requires_torch
def test_gb1_two_identical_runs_bit_identical_val_loss():
    data = _toy_data()
    r1 = train_classifier(data, TOY_LABELS, _toy_config())
    r2 = train_classifier(data, TOY_LABELS, _toy_config())
    assert r1.final_val_loss == r2.final_val_loss  # bit-identical (GB1)
    assert r1.train_losses == r2.train_losses
    assert r1.val_losses == r2.val_losses
    assert r1.metrics == r2.metrics
    assert r1.val_indices == r2.val_indices


def _run_registry(root):
    settings = head_settings(root)
    return train_tenant_classifier(
        make_toy_graph(),
        TOY_LABELS,
        "t1",
        settings,
        dataset_sha256=DATASET_SHA,
        dataset_seed=42,
        dataset_generated_at=GENERATED_AT,
        masked_out=("p28", "p29"),
    )


@requires_torch
def test_gb1_two_registry_runs_byte_equal_meta(tmp_path):
    r1 = _run_registry(tmp_path / "run1")
    r2 = _run_registry(tmp_path / "run2")
    assert r1.final_val_loss == r2.final_val_loss
    meta1 = (tmp_path / "run1" / "t1" / "graphsage-v1" / "meta.json").read_bytes()
    meta2 = (tmp_path / "run2" / "t1" / "graphsage-v1" / "meta.json").read_bytes()
    assert meta1 == meta2  # byte-equal meta.json (GB1)
    pt1 = (tmp_path / "run1" / "t1" / "graphsage-v1" / "model.pt").read_bytes()
    pt2 = (tmp_path / "run2" / "t1" / "graphsage-v1" / "model.pt").read_bytes()
    assert hashlib.sha256(pt1).hexdigest() == hashlib.sha256(pt2).hexdigest()


@requires_torch
def test_classifier_artifacts_meta_and_load_latest_round_trip(tmp_path):
    result = _run_registry(tmp_path)
    assert result.model_version == "graphsage-v1"
    artifact_dir = os.path.join(str(tmp_path), "t1", "graphsage-v1")
    assert os.path.isfile(os.path.join(artifact_dir, "model.pt"))
    assert os.path.isfile(os.path.join(artifact_dir, "head.pt"))
    with open(os.path.join(artifact_dir, "meta.json"), encoding="utf-8") as fh:
        meta = json.load(fh)
    # W31 provenance keys preserved + classifier additions (I2/I3)
    assert meta["head"] == "classifier"
    assert meta["seed"] == 42
    assert meta["device"] == "cpu"
    assert meta["trained_at"] == GENERATED_AT  # deterministic, never wall-clock
    assert meta["dataset"] == {
        "sha256": DATASET_SHA,
        "seed": 42,
        "generated_at": GENERATED_AT,
    }
    assert meta["supervision"]["num_supervised"] == 28
    assert meta["supervision"]["num_positives"] == 6
    assert meta["supervision"]["num_masked_out"] == 2
    assert meta["val_metrics"] == result.metrics
    assert meta["final_val_loss"] == result.final_val_loss
    assert meta["epochs_run"] == result.epochs_run
    # GB5: registry meta round-trips through load_latest (encoder state_dict).
    state_dict, loaded_meta, feature_dim = load_latest(str(tmp_path), "t1")
    assert loaded_meta["head"] == "classifier"
    assert loaded_meta["model_version"] == "graphsage-v1"
    assert feature_dim == meta["feature_dim"]
    assert any(key.endswith("weight") for key in state_dict)


@requires_torch
def test_classifier_min_size_gate_raises(tmp_path):
    graph = make_toy_graph()
    graph.persons = graph.persons[:10]
    with pytest.raises(GNNInsufficientData, match="too small"):
        train_tenant_classifier(graph, TOY_LABELS, "t1", head_settings(tmp_path))
    assert not os.path.exists(os.path.join(str(tmp_path), "t1"))


# ---------------------------------------------------------------------------
# Masked-metric correctness on hand-built tiny inputs
# ---------------------------------------------------------------------------


def test_binary_metrics_exact_on_hand_built_case():
    y = np.array([1, 1, 0, 0], dtype=np.float64)
    scores = np.array([0.9, 0.4, 0.35, 0.8], dtype=np.float64)
    m = binary_metrics(y, scores, threshold=0.5)
    # preds [1,0,0,1] -> tp=1 fp=1 fn=1 tn=1
    assert m["precision_pos"] == pytest.approx(0.5)
    assert m["recall_pos"] == pytest.approx(0.5)
    assert m["precision_neg"] == pytest.approx(0.5)
    assert m["recall_neg"] == pytest.approx(0.5)
    # AP: hits at ranks 1 (P=1.0, dR=0.5) and 3 (P=2/3, dR=0.5) -> 5/6
    assert m["auc_pr"] == pytest.approx(5.0 / 6.0)
    assert m["brier"] == pytest.approx((0.01 + 0.36 + 0.1225 + 0.64) / 4.0)
    assert m["support_pos"] == 2 and m["support_neg"] == 2


def test_binary_metrics_auc_pr_none_without_positives():
    m = binary_metrics(np.zeros(4), np.array([0.1, 0.2, 0.3, 0.4]))
    assert m["auc_pr"] is None


@requires_torch
def test_masked_bce_excludes_masked_positions():
    logits = torch.tensor([2.0, -1.0, 50.0, -50.0])
    targets = torch.tensor([1.0, 0.0, 1.0, 0.0])
    mask = torch.tensor([True, True, False, False])
    loss = masked_bce_loss(logits, targets, mask)
    expected = torch.nn.functional.binary_cross_entropy_with_logits(
        logits[:2], targets[:2]
    )
    assert float(loss) == pytest.approx(float(expected))
    # the huge masked-out logits (positions 2,3) contribute NOTHING
    assert float(loss) < 1.0
    with pytest.raises(ValueError, match="empty supervision mask"):
        masked_bce_loss(logits, targets, torch.zeros(4, dtype=torch.bool))


# ---------------------------------------------------------------------------
# Val-split determinism
# ---------------------------------------------------------------------------


@requires_torch
def test_val_split_deterministic_and_stratified():
    labels_vec = np.array([1] * 6 + [0] * 22, dtype=np.int64)
    gen1 = torch.Generator().manual_seed(42)
    gen2 = torch.Generator().manual_seed(42)
    tr1, va1 = _stratified_split(labels_vec, 0.2, gen1)
    tr2, va2 = _stratified_split(labels_vec, 0.2, gen2)
    assert (tr1, va1) == (tr2, va2)  # deterministic
    assert set(tr1).isdisjoint(va1)
    assert sorted(tr1 + va1) == list(range(28))
    # stratified: both classes represented in val
    val_classes = {labels_vec[i] for i in va1}
    assert val_classes == {0, 1}
    gen3 = torch.Generator().manual_seed(7)
    tr3, va3 = _stratified_split(labels_vec, 0.2, gen3)
    assert (tr1, va1) != (tr3, va3)  # seed-sensitive


# ---------------------------------------------------------------------------
# Early stopping
# ---------------------------------------------------------------------------


@requires_torch
def test_early_stopping_restores_best_and_stops_before_max_epochs():
    data = _toy_data()
    # Label noise w.r.t. graph structure -> val loss plateaus quickly.
    noise = {f"p{i:02d}": int((i * 2654435761) % 97 < 20) for i in range(28)}
    result = train_classifier(
        data, noise, _toy_config(epochs=200, patience=5)
    )
    assert result.stopped_early is True
    assert result.epochs_run < 200
    # stopped exactly `patience` non-improving epochs after the best epoch
    assert result.epochs_run == result.best_epoch + 1 + 5
    # restored best state: reported final val loss is the best val loss
    assert result.final_val_loss == min(result.val_losses)
    assert result.val_losses[result.best_epoch] == result.final_val_loss


@requires_torch
def test_full_run_when_val_keeps_improving():
    cfg = _toy_config(epochs=8, patience=20)
    result = train_classifier(_toy_data(), TOY_LABELS, cfg)
    assert result.epochs_run == 8  # patience never triggered
    assert result.stopped_early is False


# ---------------------------------------------------------------------------
# W31 link-mode regression (default path untouched, byte-for-byte)
# ---------------------------------------------------------------------------


@requires_torch
def test_link_mode_loss_fixture_unchanged_after_w33_edits():
    """Recorded on the pristine W31 tree (torch 2.8.0, CPU) BEFORE the W33-B3
    additive edits; the link path must reproduce it exactly."""
    data = _toy_data()
    config = SAGEConfig(hidden_dim=32, epochs=60, seed=42, device="cpu")
    _model, losses = train_model(data, config)
    assert len(losses) == 60
    assert losses[0] == 16.063486099243164
    assert losses[-1] == 0.7209099531173706


@requires_torch
def test_link_mode_meta_gains_explicit_head_link_only(tmp_path):
    result = train_tenant(make_toy_graph(), "t1", head_settings(tmp_path, gnn_epochs=60))
    assert result.model_version == "graphsage-v1"
    with open(
        os.path.join(str(tmp_path), "t1", "graphsage-v1", "meta.json"),
        encoding="utf-8",
    ) as fh:
        meta = json.load(fh)
    assert meta["head"] == "link"  # additive provenance key
    # every W31 meta key still present with identical values
    assert meta["tenant_id"] == "t1"
    assert meta["model_version"] == "graphsage-v1"
    assert meta["hidden_dim"] == 32
    assert meta["epochs"] == 60
    assert meta["seed"] == 42
    assert meta["device"] == "cpu"
    assert meta["node_counts"] == {"persons": NUM_PERSONS, "offerings": 8}
    assert meta["edge_counts"] == {"booked": 56, "person_person": 30}
    assert meta["final_loss"] == result.final_loss
    assert meta["trained_at"] == result.trained_at
    assert "val_metrics" not in meta  # link mode stays metric-free (W31)


# ---------------------------------------------------------------------------
# torch-absent import safety (runs in BOTH deployments — NOT requires_torch;
# mirrors test_gnn_train_import.py)
# ---------------------------------------------------------------------------

SERVICE_ROOT = Path(__file__).resolve().parents[1]

IMPORT_SAFETY_SCRIPT = """
import builtins
import sys

real_import = builtins.__import__

def blocked(name, *args, **kwargs):
    if name == "torch" or name.startswith("torch_geometric"):
        raise ImportError(f"No module named {name!r} (simulated)")
    return real_import(name, *args, **kwargs)

builtins.__import__ = blocked
for mod in [m for m in sys.modules if m == "torch" or m.startswith("torch_geometric")]:
    del sys.modules[mod]

from types import SimpleNamespace

import graph_ml.gnn_head as gnn_head  # must import without torch
import graph_ml.gnn_labels  # noqa: F401 - loader is torch-free by design
import graph_ml.gnn_train  # noqa: F401 - train module still import-safe
from graph_ml.gnn import GNNBackendUnavailable

assert gnn_head._TORCH_AVAILABLE is False
assert gnn_head.NodeClassifierHead is None

try:
    gnn_head.train_classifier(
        SimpleNamespace(person_ids=(), feature_dim=0), {}, gnn_head.HeadConfig()
    )
except GNNBackendUnavailable:
    pass
else:
    raise SystemExit("train_classifier did not raise GNNBackendUnavailable")

try:
    gnn_head.masked_bce_loss(None, None, None)
except GNNBackendUnavailable:
    pass
else:
    raise SystemExit("masked_bce_loss did not raise GNNBackendUnavailable")

# binary_metrics is pure numpy: works WITHOUT torch (metrics on CPU always)
import numpy as np

m = gnn_head.binary_metrics(np.array([1, 0]), np.array([0.9, 0.1]))
assert m["precision_pos"] == 1.0 and m["recall_pos"] == 1.0

print("HEAD-IMPORT-SAFE-OK")
"""


def test_gnn_head_import_safe_and_raises_without_torch():
    proc = subprocess.run(
        [sys.executable, "-c", IMPORT_SAFETY_SCRIPT],
        capture_output=True,
        text=True,
        cwd=str(SERVICE_ROOT),
        timeout=120,
    )
    assert proc.returncode == 0, f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
    assert "HEAD-IMPORT-SAFE-OK" in proc.stdout
