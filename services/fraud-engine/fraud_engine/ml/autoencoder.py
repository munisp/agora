"""FraudAE — MLP autoencoder anomaly model (SPEC-W33 §3 B1).

Architecture (frozen): 16 -> 8 -> 3 -> 8 -> 16, ReLU activations, MSE
reconstruction loss. Trained on BENIGN person feature vectors only; the
anomaly score is the per-sample reconstruction error, min-max normalised at
inference time against the training-error stats stored in meta.json.

Training: Adam lr 1e-3, <= 200 epochs, deterministic 90/10 train/val split
(seeded stdlib shuffle over sorted input), early stopping patience 20 on val
loss, seeded ``torch.Generator``. Full-batch (dataset is small; full-batch
plus a seeded generator is what makes GB1 bit-identical val loss cheap to
reason about). CPU-first (I5).

Import-safe without torch: module imports fine; every public entry point
raises :class:`MLBackendUnavailable` at CALL time.
"""

from __future__ import annotations

import random
from typing import Any, Sequence

from . import _TORCH_AVAILABLE, _require_torch, seed_everything
from .features import FEATURE_DIM

if _TORCH_AVAILABLE:
    import torch
else:  # heuristic deployment: attribute exists but raises on use
    torch = None  # type: ignore[assignment]

AE_HIDDEN_DIM = 8
AE_LATENT_DIM = 3
AE_MAX_EPOCHS = 200
AE_LR = 1e-3
AE_PATIENCE = 20
AE_VAL_FRACTION = 0.10


if _TORCH_AVAILABLE:  # class body needs torch.nn.Module — never evaluated else

    class FraudAE(torch.nn.Module):
        """16 -> 8 -> 3 -> 8 -> 16 MLP autoencoder (frozen fv1 architecture)."""

        def __init__(self, in_dim: int = FEATURE_DIM) -> None:
            super().__init__()
            self.encoder = torch.nn.Sequential(
                torch.nn.Linear(in_dim, AE_HIDDEN_DIM),
                torch.nn.ReLU(),
                torch.nn.Linear(AE_HIDDEN_DIM, AE_LATENT_DIM),
            )
            self.decoder = torch.nn.Sequential(
                torch.nn.Linear(AE_LATENT_DIM, AE_HIDDEN_DIM),
                torch.nn.ReLU(),
                torch.nn.Linear(AE_HIDDEN_DIM, in_dim),
            )

        def forward(self, x: Any) -> Any:
            return self.decoder(self.encoder(x))

        def reconstruction_error(self, x: Any) -> Any:
            """Per-sample MSE reconstruction error (the anomaly score)."""
            recon = self.forward(x)
            return torch.mean((recon - x) ** 2, dim=1)

else:
    FraudAE = None  # type: ignore[assignment]


def deterministic_split(
    items: Sequence[Any], val_fraction: float, seed: int
) -> tuple[list[Any], list[Any]]:
    """Deterministic train/val split: seeded stdlib shuffle over the input
    order. Same (items, val_fraction, seed) -> same split, always (GB1)."""
    idx = list(range(len(items)))
    random.Random(seed).shuffle(idx)
    n_val = max(1, int(round(len(items) * val_fraction))) if len(items) > 1 else 0
    val_idx = sorted(idx[:n_val])
    train_idx = sorted(idx[n_val:])
    return [items[i] for i in train_idx], [items[i] for i in val_idx]


def train_autoencoder(
    vectors: Sequence[Sequence[float]],
    seed: int,
    epochs: int = AE_MAX_EPOCHS,
    lr: float = AE_LR,
    patience: int = AE_PATIENCE,
    device: str = "cpu",
) -> tuple[Any, dict[str, Any]]:
    """Train FraudAE on benign fv1 vectors. Returns (model, history).

    history: final_train_loss, final_val_loss, epochs_run, stopped_early,
    err_min/err_max (training-set reconstruction-error stats used by the
    scorer for min-max normalisation).
    """
    _require_torch()
    if len(vectors) < 10:
        raise ValueError(f"need >= 10 benign vectors to train FraudAE, got {len(vectors)}")
    seed_everything(seed)
    gen = torch.Generator().manual_seed(seed)

    train_vecs, val_vecs = deterministic_split(list(vectors), AE_VAL_FRACTION, seed)
    x_train = torch.tensor(train_vecs, dtype=torch.float32, device=device)
    x_val = (
        torch.tensor(val_vecs, dtype=torch.float32, device=device)
        if val_vecs
        else torch.empty((0, len(vectors[0])), dtype=torch.float32, device=device)
    )

    model = FraudAE(len(vectors[0])).to(device)
    # Seeded init (SPEC: seeded torch.Generator): kaiming-normal weights drawn
    # from ``gen``, zero biases. Deterministic for a fixed seed (GB1).
    with torch.no_grad():
        for module in model.modules():
            if isinstance(module, torch.nn.Linear):
                std = (2.0 / module.in_features) ** 0.5
                module.weight.copy_(
                    torch.randn(module.weight.shape, generator=gen, device=device) * std
                )
                module.bias.zero_()
    optimizer = torch.optim.Adam(model.parameters(), lr=lr)
    loss_fn = torch.nn.MSELoss()

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
        loss = loss_fn(model(x_train), x_train)
        loss.backward()
        optimizer.step()
        final_train_loss = float(loss.detach().cpu())

        model.eval()
        with torch.no_grad():
            val_loss = (
                float(loss_fn(model(x_val), x_val).detach().cpu()) if val_vecs else final_train_loss
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

    # Training-error stats for scorer-side min-max normalisation.
    model.eval()
    with torch.no_grad():
        errs = model.reconstruction_error(x_train).detach().cpu().tolist()
    history = {
        "final_train_loss": final_train_loss,
        "final_val_loss": final_val_loss,
        "epochs_run": epochs_run,
        "stopped_early": epochs_run < epochs,
        "err_min": float(min(errs)),
        "err_max": float(max(errs)),
        "train_samples": len(train_vecs),
        "val_samples": len(val_vecs),
    }
    return model, history
