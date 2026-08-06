"""FraudCLF — supervised MLP classifier (SPEC-W33 §3 B1).

Architecture (frozen): 16 -> 32 -> 16 -> 1, ReLU, BCE-with-logits. Trained
on A1 ``labels.json`` ground truth joined to fv1 person vectors (fraud
scenarios = positives; ``benign_*`` hard negatives + unlabeled background
persons = negatives — see train.py docstring).

Metrics (AUC-PR, AUC-ROC, Brier) are implemented here in PURE PYTHON over
ranked score lists — no sklearn/numpy dependency (SPEC: "implement metrics in
numpy/torch — do NOT add sklearn dependency"; pure stdlib is a strict subset
of that constraint and keeps the heuristic deployment numpy-free).

Import-safe without torch: module imports fine; training raises
:class:`MLBackendUnavailable` at CALL time. The metric helpers are
torch-free and work in every deployment.
"""

from __future__ import annotations

from typing import Any, Sequence

from . import _TORCH_AVAILABLE, _require_torch, seed_everything
from .autoencoder import deterministic_split
from .features import FEATURE_DIM

if _TORCH_AVAILABLE:
    import torch
else:
    torch = None  # type: ignore[assignment]

CLF_HIDDEN1 = 32
CLF_HIDDEN2 = 16
CLF_MAX_EPOCHS = 200
CLF_LR = 1e-3
CLF_PATIENCE = 20
CLF_VAL_FRACTION = 0.10


if _TORCH_AVAILABLE:  # class body needs torch.nn.Module — never evaluated else

    class FraudCLF(torch.nn.Module):
        """16 -> 32 -> 16 -> 1 MLP binary classifier (frozen fv1 architecture)."""

        def __init__(self, in_dim: int = FEATURE_DIM) -> None:
            super().__init__()
            self.net = torch.nn.Sequential(
                torch.nn.Linear(in_dim, CLF_HIDDEN1),
                torch.nn.ReLU(),
                torch.nn.Linear(CLF_HIDDEN1, CLF_HIDDEN2),
                torch.nn.ReLU(),
                torch.nn.Linear(CLF_HIDDEN2, 1),
            )

        def forward(self, x: Any) -> Any:
            """Logit per sample, shape (N,)."""
            return self.net(x).squeeze(-1)

        def probability(self, x: Any) -> Any:
            return torch.sigmoid(self.forward(x))

else:
    FraudCLF = None  # type: ignore[assignment]


# ---------------------------------------------------------------------------
# Metrics — pure python, deterministic.
# ---------------------------------------------------------------------------
def auc_roc(labels: Sequence[int], scores: Sequence[float]) -> float:
    """Area under ROC via the Mann-Whitney U statistic (ties count 0.5)."""
    pos = [(s,) for y, s in zip(labels, scores) if y == 1]
    neg = [(s,) for y, s in zip(labels, scores) if y == 0]
    if not pos or not neg:
        return 0.0
    wins = 0.0
    for (ps,) in pos:
        for (ns,) in neg:
            if ps > ns:
                wins += 1.0
            elif ps == ns:
                wins += 0.5
    return wins / (len(pos) * len(neg))


def auc_pr(labels: Sequence[int], scores: Sequence[float]) -> float:
    """Area under the precision-recall curve (step interpolation, sklearn-
    compatible: sums (recall_n - recall_{n-1}) * precision_n over thresholds
    sorted by descending score)."""
    pairs = sorted(zip(scores, labels), key=lambda p: -p[0])
    total_pos = sum(1 for _, y in pairs if y == 1)
    if total_pos == 0:
        return 0.0
    tp = 0
    fp = 0
    prev_recall = 0.0
    area = 0.0
    i = 0
    n = len(pairs)
    while i < n:
        j = i
        while j < n and pairs[j][0] == pairs[i][0]:
            if pairs[j][1] == 1:
                tp += 1
            else:
                fp += 1
            j += 1
        recall = tp / total_pos
        precision = tp / (tp + fp)
        area += (recall - prev_recall) * precision
        prev_recall = recall
        i = j
    return area


def brier_score(labels: Sequence[int], probs: Sequence[float]) -> float:
    """Mean squared error of probabilistic predictions (lower is better)."""
    if not labels:
        return 0.0
    return sum((p - y) ** 2 for y, p in zip(labels, probs)) / len(labels)


def train_classifier(
    vectors: Sequence[Sequence[float]],
    labels: Sequence[int],
    seed: int,
    epochs: int = CLF_MAX_EPOCHS,
    lr: float = CLF_LR,
    patience: int = CLF_PATIENCE,
    device: str = "cpu",
) -> tuple[Any, dict[str, Any]]:
    """Train FraudCLF on labeled fv1 vectors. Returns (model, history).

    history: final_train_loss, final_val_loss, epochs_run, stopped_early,
    val_auc_pr, val_auc_roc, val_brier (on the deterministic val split),
    val_positives/val_samples.
    """
    _require_torch()
    if len(vectors) != len(labels):
        raise ValueError("vectors and labels must have equal length")
    if len(vectors) < 10:
        raise ValueError(f"need >= 10 labeled vectors to train FraudCLF, got {len(vectors)}")
    seed_everything(seed)
    gen = torch.Generator().manual_seed(seed)

    pairs = list(zip(vectors, labels))
    train_pairs, val_pairs = deterministic_split(pairs, CLF_VAL_FRACTION, seed)
    x_train = torch.tensor([p[0] for p in train_pairs], dtype=torch.float32, device=device)
    y_train = torch.tensor([p[1] for p in train_pairs], dtype=torch.float32, device=device)
    x_val = (
        torch.tensor([p[0] for p in val_pairs], dtype=torch.float32, device=device)
        if val_pairs
        else torch.empty((0, len(vectors[0])), dtype=torch.float32, device=device)
    )
    y_val = [int(p[1]) for p in val_pairs]

    model = FraudCLF(len(vectors[0])).to(device)
    # Seeded init (SPEC: seeded torch.Generator): kaiming-normal from ``gen``.
    with torch.no_grad():
        for module in model.modules():
            if isinstance(module, torch.nn.Linear):
                std = (2.0 / module.in_features) ** 0.5
                module.weight.copy_(
                    torch.randn(module.weight.shape, generator=gen, device=device) * std
                )
                module.bias.zero_()
    optimizer = torch.optim.Adam(model.parameters(), lr=lr)
    loss_fn = torch.nn.BCEWithLogitsLoss()

    best_val = float("inf")
    best_state: dict[str, Any] | None = None
    epochs_without_improve = 0
    final_train_loss = 0.0
    final_val_loss = 0.0
    epochs_run = 0
    for epoch in range(1, epochs + 1):
        epochs_run = epoch
        model.train()
        optimizer.zero_grad()
        loss = loss_fn(model(x_train), y_train)
        loss.backward()
        optimizer.step()
        final_train_loss = float(loss.detach().cpu())

        model.eval()
        with torch.no_grad():
            val_loss = (
                float(loss_fn(model(x_val), torch.tensor(y_val, dtype=torch.float32, device=device)).detach().cpu())
                if val_pairs
                else final_train_loss
            )
        final_val_loss = val_loss
        if val_loss < best_val - 1e-12:
            best_val = val_loss
            best_state = {k: v.detach().cpu().clone() for k, v in model.state_dict().items()}
            epochs_without_improve = 0
        else:
            epochs_without_improve += 1
            if epochs_without_improve >= patience:
                break

    if best_state is not None:
        model.load_state_dict(best_state)

    model.eval()
    with torch.no_grad():
        val_probs = (
            [float(p) for p in model.probability(x_val).detach().cpu().tolist()]
            if val_pairs
            else []
        )
    history = {
        "final_train_loss": final_train_loss,
        "final_val_loss": final_val_loss,
        "epochs_run": epochs_run,
        "stopped_early": epochs_run < epochs,
        "val_auc_pr": auc_pr(y_val, val_probs),
        "val_auc_roc": auc_roc(y_val, val_probs),
        "val_brier": brier_score(y_val, val_probs),
        "val_positives": sum(y_val),
        "train_samples": len(train_pairs),
        "val_samples": len(val_pairs),
    }
    return model, history
