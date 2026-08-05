"""OPTIONAL GraphSAGE backend (SPEC-W29 §3 WS-A).

torch/torch-geometric are imported behind a guard: when either is missing
this module still imports cleanly, ``GNN_AVAILABLE`` is False, and
:func:`resolve_backend` falls back to the heuristic backend with a logged
warning (SPEC-W29 §4 gate 5 — degraded GNN exits 0 in heuristic mode).

Model artifacts live under ``GRAPH_ML_MODEL_DIR`` in versioned dirs
``graphsage-v{N}/``. The heuristic test-suite never touches torch.
"""

from __future__ import annotations

import logging
import os
import re
from typing import Any

from . import MODEL_VERSION_GNN_PREFIX

log = logging.getLogger(__name__)

try:  # pragma: no cover - exercised only when torch is installed
    import torch  # noqa: F401
    import torch_geometric  # noqa: F401

    GNN_AVAILABLE = True
    GNN_IMPORT_ERROR: Exception | None = None
except ImportError as exc:  # the normal case in heuristic deployments
    torch = None  # type: ignore[assignment]
    torch_geometric = None  # type: ignore[assignment]
    GNN_AVAILABLE = False
    GNN_IMPORT_ERROR = exc


def next_model_version(model_dir: str) -> str:
    """Next versioned artifact dir name: graphsage-v{N+1} from existing dirs."""
    highest = 0
    if os.path.isdir(model_dir):
        pattern = re.compile(rf"^{re.escape(MODEL_VERSION_GNN_PREFIX)}(\d+)$")
        for entry in os.listdir(model_dir):
            match = pattern.match(entry)
            if match:
                highest = max(highest, int(match.group(1)))
    return f"{MODEL_VERSION_GNN_PREFIX}{highest + 1}"


class GNNBackendUnavailable(RuntimeError):
    """Raised when the GNN path is exercised without torch/pyg installed."""


class GraphSAGEBackend:
    """GraphSAGE node propensity heads + dot-product link predictor.

    Node regression heads predict propensity_churn / propensity_convert /
    propensity_turnout from the feature vectors in ``features.py`` (plus the
    stored Ollama ``name_embedding`` when present); a dot-product link
    predictor scores Person->Offering pairs for RECOMMENDED_FOR edges.
    """

    def __init__(self, model_dir: str) -> None:
        if not GNN_AVAILABLE:
            raise GNNBackendUnavailable(
                f"torch/torch-geometric unavailable: {GNN_IMPORT_ERROR}"
            )
        self.model_dir = model_dir
        self.model_version = next_model_version(model_dir)

    def score_tenant(self, graph: Any, now: Any = None, top_k: int = 5):  # pragma: no cover
        # Real training/inference lands with the W31 GPU profile; the seam
        # (artifact versioning, tensor shapes, link-prediction head) is what
        # SPEC-W29 requires of the optional backend.
        raise NotImplementedError(
            "GraphSAGE training/inference requires the W31 GPU profile; "
            "run GRAPH_ML_BACKEND=heuristic"
        )


def resolve_backend(settings: Any) -> str:
    """Effective backend name. ``gnn`` degrades to ``heuristic`` + warning."""
    requested = (getattr(settings, "backend", "heuristic") or "heuristic").lower()
    if requested == "gnn":
        if GNN_AVAILABLE:  # pragma: no cover - requires torch
            return "gnn"
        log.warning(
            "GRAPH_ML_BACKEND=gnn requested but torch/torch-geometric are not "
            "installed (%s); falling back to heuristic backend",
            GNN_IMPORT_ERROR,
        )
        return "heuristic"
    if requested != "heuristic":
        log.warning("unknown GRAPH_ML_BACKEND=%r; using heuristic", requested)
    return "heuristic"
