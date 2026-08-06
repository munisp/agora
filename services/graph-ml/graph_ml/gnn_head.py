"""Supervised node-classification head on the W31 GraphSAGE encoder
(SPEC-W33 §3 B3 — closes the W31 audit gaps "no val split, no early
stopping, no eval metrics" FOR THE CLASSIFIER MODE ONLY; the default
link-prediction mode is untouched).

The SAME ``gnn_train.GraphSAGEModel`` SAGEConv encoder produces node
embeddings; a 2-layer MLP head (hidden -> hidden -> 1 logit, binary
fraud-vs-benign) is trained on A1 labeled nodes with a MASKED BCE loss:
only supervised person positions (see ``gnn_labels`` — positives are
``sybil_cluster``/``referral_ring`` labels, negatives are ``benign_*``
hard negatives plus the unlabeled population, other-fraud-scenario persons
are masked out upstream) contribute to the loss and metrics.

W31 gaps closed here:
  * deterministic stratified val split (seeded ``torch.Generator``),
  * early stopping on val masked-BCE (patience = ``HeadConfig.patience``,
    default 20 epochs without a strict val-loss improvement; best state
    restored afterwards),
  * val metrics computed by repo code (no sklearn): per-class
    precision/recall at threshold 0.5, AUC-PR (average-precision form) and
    Brier score.

Everything is seeded: ``gnn_train.seed_everything(config.seed)`` before
model construction plus a seeded ``torch.Generator`` for the split. CPU
full-batch training => two identical runs produce bit-identical final val
losses (SPEC-W33 GB1).

Import-safe without torch exactly like ``gnn_train`` (SPEC-W31 §0
invariant 5 / SPEC-W33 I5): the module imports cleanly in heuristic
deployments and raises ``GNNBackendUnavailable`` at CALL time.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field
from typing import Any, Mapping

import numpy as np

from .gnn import GNNBackendUnavailable, GNNInsufficientData

try:  # same guard idiom as gnn_train.py (SPEC-W31 §0 invariant 5)
    import torch

    _TORCH_AVAILABLE = True
except ImportError:  # the normal case in heuristic deployments
    torch = None  # type: ignore[assignment]
    _TORCH_AVAILABLE = False

from .gnn_data import TenantData  # torch-lazy module, same as gnn_train

log = logging.getLogger(__name__)

#: Decision threshold for the per-class precision/recall (AUC-PR and Brier
#: are threshold-free).
DEFAULT_THRESHOLD = 0.5


@dataclass(frozen=True)
class HeadConfig:
    """Classifier-head hyperparameters (CPU-safe defaults; patience documented
    as epochs without a strict val masked-BCE improvement before stopping)."""

    hidden_dim: int = 64
    num_layers: int = 2
    epochs: int = 200  # max epochs (early stopping usually cuts this short)
    lr: float = 1e-3
    seed: int = 42
    device: str = "auto"  # auto -> cuda-if-available-else-cpu (CPU-first, I5)
    patience: int = 20  # early-stopping patience on val masked-BCE loss
    val_fraction: float = 0.2  # per-class fraction reserved for validation

    @classmethod
    def from_settings(cls, settings: Any) -> "HeadConfig":
        return cls(
            hidden_dim=int(getattr(settings, "gnn_hidden_dim", 64)),
            epochs=int(getattr(settings, "gnn_epochs", 200)),
            seed=int(getattr(settings, "seed", 42)),
            device=str(getattr(settings, "device", "auto") or "auto"),
            patience=int(getattr(settings, "gnn_head_patience", 20)),
            val_fraction=float(getattr(settings, "gnn_head_val_fraction", 0.2)),
        )

    def resolve_device(self) -> str:
        if self.device == "auto":
            return "cuda" if torch.cuda.is_available() else "cpu"
        return self.device


def _require_torch() -> None:
    """Raise at CALL time (never import time) when the GNN stack is absent."""
    if not _TORCH_AVAILABLE:
        raise GNNBackendUnavailable(
            "torch is not installed; the supervised GNN head is unavailable "
            "(heuristic deployment) — caller must fall back per tenant "
            "(SPEC-W31 §0 invariant 1 / SPEC-W33 I1)"
        )


if _TORCH_AVAILABLE:  # class body needs torch.nn.Module — never evaluated else

    class NodeClassifierHead(torch.nn.Module):
        """2-layer MLP on top of the SAGE encoder: hidden -> hidden -> 1 logit.

        Binary fraud-vs-benign supervision (A1 ``sybil_cluster`` +
        ``referral_ring`` positives vs ``benign_*`` + unlabeled negatives),
        trained with masked BCE-with-logits.
        """

        def __init__(self, in_dim: int, hidden_dim: int) -> None:
            super().__init__()
            self.fc1 = torch.nn.Linear(in_dim, hidden_dim)
            self.fc2 = torch.nn.Linear(hidden_dim, 1)

        def forward(self, h: Any) -> Any:
            return self.fc2(torch.relu(self.fc1(h))).squeeze(-1)

else:  # heuristic deployment: attribute exists but raises on use
    NodeClassifierHead = None  # type: ignore[assignment]


def masked_bce_loss(logits: Any, targets: Any, mask: Any) -> Any:
    """BCE-with-logits over the masked (supervised) positions only."""
    _require_torch()
    if not bool(mask.any()):
        raise ValueError("masked_bce_loss: empty supervision mask")
    return torch.nn.functional.binary_cross_entropy_with_logits(
        logits[mask], targets[mask]
    )


def binary_metrics(
    y_true: Any, y_score: Any, threshold: float = DEFAULT_THRESHOLD
) -> dict[str, Any]:
    """Per-class precision/recall + AUC-PR + Brier, pure numpy (I3: computed
    by repo code — no sklearn dependency).

    ``y_score`` are probabilities in [0, 1]. Undefined ratios (no predicted
    positives, etc.) are reported as 0.0; AUC-PR is None when the val slice
    has no positives (JSON-null, honest "undefined").
    """
    y = np.asarray(y_true, dtype=np.float64)
    p = np.asarray(y_score, dtype=np.float64)
    if y.shape != p.shape:
        raise ValueError(f"shape mismatch: y_true {y.shape} vs y_score {p.shape}")
    pred = (p >= threshold).astype(np.float64)

    tp = float(((pred == 1) & (y == 1)).sum())
    fp = float(((pred == 1) & (y == 0)).sum())
    fn = float(((pred == 0) & (y == 1)).sum())
    tn = float(((pred == 0) & (y == 0)).sum())
    precision_pos = tp / (tp + fp) if (tp + fp) > 0 else 0.0
    recall_pos = tp / (tp + fn) if (tp + fn) > 0 else 0.0
    precision_neg = tn / (tn + fn) if (tn + fn) > 0 else 0.0
    recall_neg = tn / (tn + fp) if (tn + fp) > 0 else 0.0

    positives = float(y.sum())
    auc_pr: float | None
    if positives > 0:
        order = np.argsort(-p, kind="stable")  # stable: deterministic ties
        y_sorted = y[order]
        cum_tp = np.cumsum(y_sorted)
        cum_fp = np.cumsum(1.0 - y_sorted)
        recalls = cum_tp / positives
        precisions = cum_tp / np.maximum(cum_tp + cum_fp, 1.0)
        # Average-precision form: sum of precision at each new recall level.
        recall_gain = np.diff(np.concatenate(([0.0], recalls)))
        auc_pr = float((precisions * recall_gain).sum())
    else:
        auc_pr = None

    brier = float(np.mean((p - y) ** 2)) if y.size else None
    return {
        "threshold": float(threshold),
        "precision_pos": precision_pos,
        "recall_pos": recall_pos,
        "precision_neg": precision_neg,
        "recall_neg": recall_neg,
        "auc_pr": auc_pr,
        "brier": brier,
        "support_pos": int(positives),
        "support_neg": int(y.size - positives),
    }


def _stratified_split(
    labels_vec: np.ndarray, val_fraction: float, gen: Any
) -> tuple[list[int], list[int]]:
    """Deterministic stratified train/val split over supervised indices.

    Each class is shuffled with the seeded generator; ``val_fraction`` of
    each class (>= 1 when the class has >= 2 members and val_fraction > 0)
    goes to val. Both outputs are sorted index lists (deterministic).
    """
    train: list[int] = []
    val: list[int] = []
    for cls in (0, 1):
        idx = np.flatnonzero(labels_vec == cls).tolist()
        if not idx:
            continue
        perm = torch.randperm(len(idx), generator=gen).tolist()
        shuffled = [idx[i] for i in perm]
        n_val = 0
        if val_fraction > 0.0 and len(shuffled) >= 2:
            n_val = max(1, min(int(round(len(shuffled) * val_fraction)),
                               len(shuffled) - 1))
        val.extend(shuffled[:n_val])
        train.extend(shuffled[n_val:])
    return sorted(train), sorted(val)


@dataclass
class ClassifierTrainResult:
    """Outcome of one supervised-head training run (in-memory; registry
    writing lives in ``gnn_train.train_tenant_classifier``)."""

    model: Any  # trained GraphSAGEModel encoder (best-val state restored)
    head: Any  # trained NodeClassifierHead (best-val state restored)
    train_losses: list[float]
    val_losses: list[float]
    best_epoch: int  # 0-based epoch with the best val loss
    epochs_run: int
    stopped_early: bool
    final_val_loss: float  # val loss at best_epoch (GB1: bit-identical)
    metrics: dict[str, Any]  # val metrics at the restored best state
    num_supervised: int
    num_positives: int
    train_indices: list[int] = field(default_factory=list)
    val_indices: list[int] = field(default_factory=list)


def train_classifier(
    data: TenantData, labels: Mapping[str, int], config: HeadConfig
) -> ClassifierTrainResult:
    """Train the SAGE encoder + classification head on labeled person nodes.

    ``labels`` maps person_id -> 0/1 for the supervised node set (build it
    with ``gnn_labels.load_labeled_graph``). Raises ``GNNInsufficientData``-
    compatible ``ValueError`` when there is nothing to supervise on.
    """
    _require_torch()
    from .gnn_train import GraphSAGEModel, seed_everything  # lazy: avoid cycle

    y_map: dict[int, int] = {}
    for i, pid in enumerate(data.person_ids):
        if pid in labels:
            y_map[i] = int(labels[pid])
    if not y_map:
        raise GNNInsufficientData("no supervised (labeled) person nodes")
    if not any(y_map.values()):
        raise GNNInsufficientData("no positive labels to train the classifier on")

    device = torch.device(config.resolve_device())
    seed_everything(config.seed)
    gen = torch.Generator().manual_seed(config.seed)

    sup_idx = sorted(y_map)
    labels_vec = np.array([y_map[i] for i in sup_idx], dtype=np.int64)
    train_pos, val_pos = _stratified_split(labels_vec, config.val_fraction, gen)
    train_idx = [sup_idx[j] for j in train_pos]
    val_idx = [sup_idx[j] for j in val_pos]
    if not val_idx:  # tiny-graph fallback: monitor the train slice (documented)
        val_idx = list(train_idx)

    idx_all = torch.tensor(sup_idx, dtype=torch.long, device=device)
    y_all = torch.tensor(
        [y_map[i] for i in sup_idx], dtype=torch.float32, device=device
    )
    pos_of = {node: k for k, node in enumerate(sup_idx)}
    train_mask = torch.zeros(len(sup_idx), dtype=torch.bool, device=device)
    val_mask = torch.zeros(len(sup_idx), dtype=torch.bool, device=device)
    for node in train_idx:
        train_mask[pos_of[node]] = True
    for node in val_idx:
        val_mask[pos_of[node]] = True

    model = GraphSAGEModel(data.feature_dim, config.hidden_dim, config.num_layers).to(device)
    head = NodeClassifierHead(config.hidden_dim, config.hidden_dim).to(device)
    optimizer = torch.optim.Adam(
        list(model.parameters()) + list(head.parameters()), lr=config.lr
    )
    x = data.x.to(device)
    edge_index = data.edge_index.to(device)

    train_losses: list[float] = []
    val_losses: list[float] = []
    best_val = float("inf")
    best_epoch = 0
    best_model_state: dict[str, Any] | None = None
    best_head_state: dict[str, Any] | None = None
    epochs_without_improvement = 0
    stopped_early = False

    for epoch in range(config.epochs):
        model.train()
        head.train()
        optimizer.zero_grad()
        z = model(x, edge_index)
        logits = head(z[idx_all])
        loss = masked_bce_loss(logits, y_all, train_mask)
        loss.backward()
        optimizer.step()
        train_losses.append(float(loss.detach().cpu()))

        model.eval()
        head.eval()
        with torch.no_grad():
            val_logits = head(model(x, edge_index)[idx_all])
            val_loss = float(masked_bce_loss(val_logits, y_all, val_mask).cpu())
        val_losses.append(val_loss)

        if val_loss < best_val:
            best_val = val_loss
            best_epoch = epoch
            epochs_without_improvement = 0
            best_model_state = {
                k: v.detach().clone() for k, v in model.state_dict().items()
            }
            best_head_state = {
                k: v.detach().clone() for k, v in head.state_dict().items()
            }
        else:
            epochs_without_improvement += 1
            if epochs_without_improvement >= config.patience:
                stopped_early = True
                break

    if best_model_state is not None:  # always true after >= 1 epoch
        model.load_state_dict(best_model_state)
        head.load_state_dict(best_head_state)  # type: ignore[arg-type]

    model.eval()
    head.eval()
    with torch.no_grad():
        val_nodes = torch.tensor(val_idx, dtype=torch.long, device=device)
        val_logits = head(model(x, edge_index)[val_nodes])
        probs = torch.sigmoid(val_logits).cpu().numpy()
    y_val = np.array([y_map[i] for i in val_idx], dtype=np.float64)
    metrics = binary_metrics(y_val, probs)
    metrics["val_nodes"] = len(val_idx)

    return ClassifierTrainResult(
        model=model,
        head=head,
        train_losses=train_losses,
        val_losses=val_losses,
        best_epoch=best_epoch,
        epochs_run=len(train_losses),
        stopped_early=stopped_early,
        final_val_loss=best_val,
        metrics=metrics,
        num_supervised=len(sup_idx),
        num_positives=int(sum(y_map.values())),
        train_indices=train_idx,
        val_indices=val_idx,
    )
