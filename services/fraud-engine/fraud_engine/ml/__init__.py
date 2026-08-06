"""fraud_engine.ml — W33-B learned fraud scorer package (SPEC-W33 §3 B1).

Real PyTorch training loops (autoencoder anomaly model + supervised
classifier) trained on the A1 labeled synthetic dataset
(``scripts/seeds/naija_transactions.py``), with versioned filesystem
artifacts and CPU-first inference.

TORCH IS OPTIONAL (I5 / W31 §0 invariant 5): this package imports cleanly
without torch. Torch-dependent entry points raise :class:`MLBackendUnavailable`
at CALL time; :func:`fraud_engine.ml.scorer.LearnedScorer.load` returns None
instead, so the service always degrades to the pure rule path (I1).
"""

from __future__ import annotations

import json
import os
import random
from typing import Any

try:  # same guard idiom as graph_ml/gnn_train.py (SPEC-W31 §0 invariant 5)
    import torch

    _TORCH_AVAILABLE = True
except ImportError:  # the normal case in heuristic deployments
    torch = None  # type: ignore[assignment]
    _TORCH_AVAILABLE = False

MODEL_VERSION_AE_PREFIX = "fraud-ae-v"
MODEL_VERSION_CLF_PREFIX = "fraud-clf-v"
GLOBAL_TENANT_DIR = "_global"

DEFAULT_SEED = 42


class MLBackendUnavailable(RuntimeError):
    """Raised at CALL time when torch is absent (heuristic deployment)."""


def _require_torch() -> None:
    if not _TORCH_AVAILABLE:
        raise MLBackendUnavailable(
            "torch is not installed; fraud-engine learned-scorer training/"
            "inference unavailable (heuristic deployment) — caller must fall "
            "back to the D1-D8 rule path (SPEC-W33 §0 I1/I5)"
        )


def seed_everything(seed: int) -> None:
    """Seed python/torch for reproducible CPU training (GB1)."""
    random.seed(seed)
    _require_torch()
    torch.manual_seed(seed)
    if torch.cuda.is_available():  # pragma: no cover - CI is CPU
        torch.cuda.manual_seed_all(seed)


def resolve_device(device: str = "auto") -> str:
    """CPU-first: ``auto`` -> cuda only if present, else cpu (I5)."""
    if device == "auto":
        _require_torch()
        return "cuda" if torch.cuda.is_available() else "cpu"
    return device


def next_model_version(model_dir: str, prefix: str) -> str:
    """Next free ``{prefix}{N}`` (N = 1 + max existing) inside ``model_dir``."""
    existing: list[int] = []
    if os.path.isdir(model_dir):
        for entry in os.listdir(model_dir):
            if entry.startswith(prefix) and entry[len(prefix):].isdigit():
                if os.path.isdir(os.path.join(model_dir, entry)):
                    existing.append(int(entry[len(prefix):]))
    return f"{prefix}{(max(existing) + 1) if existing else 1}"


def load_latest(model_dir: str, prefix: str) -> tuple[dict[str, Any], dict[str, Any]] | None:
    """Latest ``(state_dict, meta)`` for a model family dir, or None.

    ``model_dir`` is the tenant (or ``_global``) registry directory containing
    ``{prefix}{N}/model.pt + meta.json``. None means no usable versioned
    artifact — the caller falls back to pure rules (I1). Loads with
    ``map_location="cpu"`` always (I5).
    """
    _require_torch()
    if not os.path.isdir(model_dir):
        return None
    versions = sorted(
        (
            entry
            for entry in os.listdir(model_dir)
            if entry.startswith(prefix)
            and entry[len(prefix):].isdigit()
            and os.path.isdir(os.path.join(model_dir, entry))
        ),
        key=lambda e: int(e[len(prefix):]),
    )
    for version in reversed(versions):
        artifact_dir = os.path.join(model_dir, version)
        model_path = os.path.join(artifact_dir, "model.pt")
        meta_path = os.path.join(artifact_dir, "meta.json")
        if not (os.path.isfile(model_path) and os.path.isfile(meta_path)):
            continue
        with open(meta_path, encoding="utf-8") as fh:
            meta = json.load(fh)
        state_dict = torch.load(model_path, map_location="cpu", weights_only=True)
        return state_dict, meta
    return None
