"""Per-tenant GraphSAGE training + artifact registry (SPEC-W31 §1 WS-A).

This module is IMPORT-SAFE WITHOUT torch: ``main.py`` imports it at module
top even in the heuristic base image (SPEC-W31 §0 invariant 5), so
torch/torch-geometric sit behind a guarded import exactly like ``gnn.py``.
When the stack is absent, ``_TORCH_AVAILABLE`` is False and every public
entry point raises :class:`gnn.GNNBackendUnavailable` at CALL time — the
caller (train sweep / score seam) maps that to the per-tenant heuristic
fallback, never a 500.

Objective (SPEC-W31 §1):
  * unsupervised GraphSAGE link loss over Person->Person (REFERRED) edges
    with negative sampling, plus
  * a supervised dot-product link-prediction head on Person->Offering BOOKED
    edges with negative sampling (the recommendation signal).

Artifacts are tenant-scoped and versioned:
``{model_dir}/{tenant_id}/graphsage-v{N}/model.pt`` + ``meta.json``.
"""

from __future__ import annotations

import json
import logging
import os
import random
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any

import numpy as np

from . import MODEL_VERSION_GNN_PREFIX
from .gnn import GNNBackendUnavailable, GNNInsufficientData, next_model_version

try:  # same guard idiom as gnn.py (SPEC-W31 §0 invariant 5)
    import torch
    from torch_geometric.nn import SAGEConv

    _TORCH_AVAILABLE = True
except ImportError:  # the normal case in heuristic deployments
    torch = None  # type: ignore[assignment]
    SAGEConv = None  # type: ignore[assignment]
    _TORCH_AVAILABLE = False
from .gnn_data import TenantData, build_graph_data, graph_stats

log = logging.getLogger(__name__)


@dataclass(frozen=True)
class SAGEConfig:
    """Training hyperparameters (SPEC-W31 §1; defaults safe for CPU)."""

    hidden_dim: int = 64
    num_layers: int = 2
    epochs: int = 200
    lr: float = 1e-3
    seed: int = 42
    device: str = "auto"  # auto -> cuda-if-available-else-cpu
    min_persons: int = 20
    min_edges: int = 30

    @classmethod
    def from_settings(cls, settings: Any) -> "SAGEConfig":
        return cls(
            hidden_dim=int(getattr(settings, "gnn_hidden_dim", 64)),
            epochs=int(getattr(settings, "gnn_epochs", 200)),
            seed=int(getattr(settings, "seed", 42)),
            device=str(getattr(settings, "device", "auto") or "auto"),
            min_persons=int(getattr(settings, "gnn_min_persons", 20)),
            min_edges=int(getattr(settings, "gnn_min_edges", 30)),
        )

    def resolve_device(self) -> str:
        if self.device == "auto":
            return "cuda" if torch.cuda.is_available() else "cpu"
        return self.device


@dataclass(frozen=True)
class TrainResult:
    model_version: str
    model_dir: str  # versioned artifact dir {model_dir}/{tenant_id}/graphsage-vN
    final_loss: float
    epochs: int
    node_counts: dict[str, int]
    edge_counts: dict[str, int]
    device: str
    trained_at: str


def seed_everything(seed: int) -> None:
    """Seed torch/numpy/python for reproducible CPU training (SPEC-W31 G4)."""
    random.seed(seed)
    np.random.seed(seed)
    torch.manual_seed(seed)
    if torch.cuda.is_available():  # pragma: no cover - CI is CPU
        torch.cuda.manual_seed_all(seed)


def _require_torch() -> None:
    """Raise at CALL time (never import time) when the GNN stack is absent."""
    if not _TORCH_AVAILABLE:
        raise GNNBackendUnavailable(
            "torch/torch-geometric are not installed; GraphSAGE training/"
            "inference unavailable (heuristic deployment) — caller must fall "
            "back per tenant (SPEC-W31 §0 invariant 1)"
        )


if _TORCH_AVAILABLE:  # class body needs torch.nn.Module — never evaluated else

    class GraphSAGEModel(torch.nn.Module):
        """SAGEConv encoder stack; dot-product decoder for link scoring."""

        def __init__(self, in_dim: int, hidden_dim: int = 64, num_layers: int = 2) -> None:
            super().__init__()
            if num_layers < 1:
                raise ValueError("num_layers must be >= 1")
            self.convs = torch.nn.ModuleList()
            dims = [in_dim] + [hidden_dim] * num_layers
            for i in range(num_layers):
                self.convs.append(SAGEConv(dims[i], dims[i + 1]))
            self.out_dim = hidden_dim

        def encode(self, x: Any, edge_index: Any) -> Any:
            h = x
            for conv in self.convs[:-1]:
                h = torch.relu(conv(h, edge_index))
            return self.convs[-1](h, edge_index)

        def forward(self, x: Any, edge_index: Any) -> Any:
            return self.encode(x, edge_index)

else:  # heuristic deployment: attribute exists but raises on use
    GraphSAGEModel = None  # type: ignore[assignment]


def _dot_scores(z: Any, pairs: Any) -> Any:
    return (z[pairs[0]] * z[pairs[1]]).sum(dim=-1)


def _sample_booked_negatives(
    data: TenantData, count: int, gen: torch.Generator
) -> Any:
    """Random (person, offering) pairs that are NOT booked."""
    positives = {
        (int(p), int(o)) for p, o in data.booked_edge_index.t().tolist()
    }
    pairs: list[tuple[int, int]] = []
    while len(pairs) < count:
        p = int(torch.randint(data.num_persons, (1,), generator=gen))
        o = data.num_persons + int(torch.randint(data.num_offerings, (1,), generator=gen))
        if (p, o) not in positives:
            positives.add((p, o))
            pairs.append((p, o))
    return torch.tensor(pairs, dtype=torch.long).t()


def _sample_person_negatives(data: TenantData, count: int, gen: torch.Generator) -> Any:
    """Random person-person pairs not linked by a REFERRED edge."""
    positives = {
        (int(a), int(b)) for a, b in data.person_edge_index.t().tolist()
    } | {(int(b), int(a)) for a, b in data.person_edge_index.t().tolist()}
    pairs: list[tuple[int, int]] = []
    attempts = 0
    while len(pairs) < count and attempts < count * 20:
        attempts += 1
        a = int(torch.randint(data.num_persons, (1,), generator=gen))
        b = int(torch.randint(data.num_persons, (1,), generator=gen))
        if a != b and (a, b) not in positives:
            positives.add((a, b))
            pairs.append((a, b))
    return torch.tensor(pairs, dtype=torch.long).t() if pairs else None


def _link_bce(z: Any, pos_pairs: Any, neg_pairs: Any) -> Any:
    pos_score = _dot_scores(z, pos_pairs)
    neg_score = _dot_scores(z, neg_pairs)
    scores = torch.cat([pos_score, neg_score])
    labels = torch.cat(
        [torch.ones_like(pos_score), torch.zeros_like(neg_score)]
    )
    return torch.nn.functional.binary_cross_entropy_with_logits(scores, labels)


def train_model(
    data: TenantData, config: SAGEConfig
) -> tuple[GraphSAGEModel, list[float]]:
    """Train on one tenant's tensors; returns (model, per-epoch losses)."""
    _require_torch()
    device = torch.device(config.resolve_device())
    seed_everything(config.seed)
    gen = torch.Generator().manual_seed(config.seed)

    model = GraphSAGEModel(data.feature_dim, config.hidden_dim, config.num_layers).to(device)
    optimizer = torch.optim.Adam(model.parameters(), lr=config.lr)
    x = data.x.to(device)
    edge_index = data.edge_index.to(device)
    booked_pos = data.booked_edge_index.to(device)
    person_pos = data.person_edge_index.to(device)

    model.train()
    losses: list[float] = []
    for _epoch in range(config.epochs):
        optimizer.zero_grad()
        z = model(x, edge_index)
        terms = []
        if booked_pos.shape[1] > 0:
            booked_neg = _sample_booked_negatives(data, booked_pos.shape[1], gen).to(device)
            terms.append(_link_bce(z, booked_pos, booked_neg))
        if person_pos.shape[1] > 0:
            person_neg = _sample_person_negatives(data, person_pos.shape[1], gen)
            if person_neg is not None:
                terms.append(_link_bce(z, person_pos, person_neg.to(device)))
        if not terms:  # pragma: no cover - min_edges gate makes this unreachable
            raise GNNInsufficientData("no positive edges to train on")
        loss = torch.stack(terms).mean()
        loss.backward()
        optimizer.step()
        losses.append(float(loss.detach().cpu()))
    return model, losses


def train_tenant(graph: Any, tenant_id: str, settings: Any) -> TrainResult:
    """Train (or refuse) one tenant's model; writes versioned artifacts.

    Raises ``GNNInsufficientData`` when the tenant graph is below the
    min-size gate — the caller maps this to the heuristic fallback, it is
    NOT an error (SPEC-W31 §0 invariant 1).
    """
    _require_torch()
    config = SAGEConfig.from_settings(settings)
    num_persons, num_edges = graph_stats(graph)
    if num_persons < config.min_persons or num_edges < config.min_edges:
        raise GNNInsufficientData(
            f"tenant {tenant_id}: graph too small for GNN training "
            f"(persons={num_persons}<{config.min_persons} or "
            f"edges={num_edges}<{config.min_edges}); heuristic fallback applies"
        )

    data = build_graph_data(graph)
    model, losses = train_model(data, config)
    final_loss = losses[-1]
    device = config.resolve_device()

    tenant_dir = os.path.join(settings.model_dir, tenant_id)
    version = next_model_version(tenant_dir)
    artifact_dir = os.path.join(tenant_dir, version)
    os.makedirs(artifact_dir, exist_ok=True)
    torch.save(model.state_dict(), os.path.join(artifact_dir, "model.pt"))

    trained_at = datetime.now(timezone.utc).isoformat()
    node_counts = {"persons": data.num_persons, "offerings": data.num_offerings}
    edge_counts = {
        "booked": int(data.booked_edge_index.shape[1]),
        "person_person": int(data.person_edge_index.shape[1]),
    }
    meta = {
        "tenant_id": tenant_id,
        "model_version": version,
        "trained_at": trained_at,
        "feature_dim": data.feature_dim,
        "hidden_dim": config.hidden_dim,
        "num_layers": config.num_layers,
        "epochs": config.epochs,
        "final_loss": final_loss,
        "node_counts": node_counts,
        "edge_counts": edge_counts,
        "device": device,
        "seed": config.seed,
    }
    with open(os.path.join(artifact_dir, "meta.json"), "w", encoding="utf-8") as fh:
        json.dump(meta, fh, indent=2, sort_keys=True)
    log.info(
        "trained %s for tenant %s (final_loss=%.6f)",
        version,
        tenant_id,
        final_loss,
        extra={"tenant_id": tenant_id, "model_version": version},
    )
    return TrainResult(
        model_version=version,
        model_dir=artifact_dir,
        final_loss=final_loss,
        epochs=config.epochs,
        node_counts=node_counts,
        edge_counts=edge_counts,
        device=device,
        trained_at=trained_at,
    )


def load_latest(
    model_dir: str, tenant_id: str
) -> tuple[dict[str, Any], dict[str, Any], int] | None:
    """Latest (state_dict, meta, feature_dim) for a tenant, or None.

    None means no versioned artifact dir exists — the caller falls back to
    heuristic (SPEC-W31 §0 invariant 1). Never reads another tenant's dir.
    """
    _require_torch()
    tenant_dir = os.path.join(model_dir, tenant_id)
    if not os.path.isdir(tenant_dir):
        return None
    versions = sorted(
        (
            entry
            for entry in os.listdir(tenant_dir)
            if entry.startswith(MODEL_VERSION_GNN_PREFIX)
            and os.path.isdir(os.path.join(tenant_dir, entry))
        ),
        key=lambda e: int(e[len(MODEL_VERSION_GNN_PREFIX) :])
        if e[len(MODEL_VERSION_GNN_PREFIX) :].isdigit()
        else -1,
    )
    if not versions:
        return None
    artifact_dir = os.path.join(tenant_dir, versions[-1])
    model_path = os.path.join(artifact_dir, "model.pt")
    meta_path = os.path.join(artifact_dir, "meta.json")
    if not (os.path.isfile(model_path) and os.path.isfile(meta_path)):
        return None
    with open(meta_path, encoding="utf-8") as fh:
        meta = json.load(fh)
    state_dict = torch.load(model_path, map_location="cpu", weights_only=True)
    return state_dict, meta, int(meta["feature_dim"])
