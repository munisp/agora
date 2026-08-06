"""OPTIONAL GraphSAGE backend (SPEC-W29 §3 WS-A; inference landed in SPEC-W31 §1).

torch/torch-geometric are imported behind a guard: when either is missing
this module still imports cleanly, ``GNN_AVAILABLE`` is False, and
:func:`resolve_backend` falls back to the heuristic backend with a logged
warning (SPEC-W29 §4 gate 5 — degraded GNN exits 0 in heuristic mode).

Model artifacts live under ``GRAPH_ML_MODEL_DIR`` in tenant-scoped versioned
dirs ``{model_dir}/{tenant_id}/graphsage-v{N}/`` (SPEC-W31 §0 invariant 3 —
a tenant's model never scores another tenant). The heuristic test-suite never
touches torch; the torch-dependent helpers (``gnn_data``, ``gnn_train``) are
imported lazily inside methods only.
"""

from __future__ import annotations

import logging
import os
import re
from datetime import datetime, timezone
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


class GNNModelNotFound(RuntimeError):
    """No trained model artifact for this tenant -> heuristic fallback."""


class GNNInsufficientData(RuntimeError):
    """Tenant graph below the min-size gate -> heuristic fallback."""


class GraphSAGEBackend:
    """GraphSAGE link predictor over per-tenant versioned artifacts.

    SPEC-W31 §1: the GNN replaces ONLY the recommendation half — propensity
    scores are delegated to ``heuristic.score_tenant`` unchanged (propensity
    stays ``heuristic-v1`` until the R5 calibration gate clears).
    """

    def __init__(
        self, model_dir: str, min_persons: int = 20, min_edges: int = 30
    ) -> None:
        if not GNN_AVAILABLE:
            raise GNNBackendUnavailable(
                f"torch/torch-geometric unavailable: {GNN_IMPORT_ERROR}"
            )
        self.model_dir = model_dir
        self.min_persons = min_persons
        self.min_edges = min_edges
        # Set per-tenant by score_tenant (SPEC-W31 tenant-scoped versioning).
        self.model_version = next_model_version(model_dir)

    def score_tenant(self, graph: Any, now: Any = None, top_k: int = 5):  # pragma: no cover
        """Score one tenant with its OWN trained model.

        Raises ``GNNModelNotFound`` (no artifact) or ``GNNInsufficientData``
        (undersized graph) — ``score.py`` maps both to the per-tenant
        heuristic fallback exactly like the W29 gate-5 path.
        """
        from . import heuristic
        from .gnn_data import build_graph_data, graph_stats
        from .gnn_train import GraphSAGEModel, load_latest

        now = now or datetime.now(timezone.utc)
        loaded = load_latest(self.model_dir, graph.tenant_id)
        if loaded is None:
            raise GNNModelNotFound(
                f"no trained GraphSAGE model for tenant {graph.tenant_id} "
                f"under {self.model_dir}; heuristic fallback applies"
            )
        num_persons, num_edges = graph_stats(graph)
        if num_persons < self.min_persons or num_edges < self.min_edges:
            raise GNNInsufficientData(
                f"tenant {graph.tenant_id}: graph too small for GNN inference "
                f"(persons={num_persons}<{self.min_persons} or "
                f"edges={num_edges}<{self.min_edges}); heuristic fallback applies"
            )

        state_dict, meta, feature_dim = loaded
        model = GraphSAGEModel(
            feature_dim,
            hidden_dim=int(meta["hidden_dim"]),
            num_layers=int(meta.get("num_layers", 2)),
        )
        model.load_state_dict(state_dict)
        model.eval()

        data = build_graph_data(graph, now)
        if data.feature_dim != feature_dim:
            raise GNNModelNotFound(
                f"tenant {graph.tenant_id}: artifact feature_dim={feature_dim} "
                f"!= live graph feature_dim={data.feature_dim}; retrain required"
            )
        with torch.no_grad():
            z = model(data.x, data.edge_index)
        person_z = z[: data.num_persons]
        offering_z = z[data.num_persons :]
        # Dot-product score for every person x offering pair, squashed to [0,1].
        logits = person_z @ offering_z.t()
        sim = torch.sigmoid(logits)

        # Already-booked exclusion, same source as heuristic.CooccurrenceModel.
        booked_by_person: dict[str, set[str]] = {}
        for b in graph.bookings:
            booked_by_person.setdefault(b.person_id, set()).add(b.offering_id)

        model_version = str(meta["model_version"])
        scored_at = now.isoformat()
        recommendations = []
        for i, person_id in enumerate(data.person_ids):
            already = booked_by_person.get(person_id, set())
            ranked = sorted(
                (
                    (offering_id, float(sim[i, j]))
                    for j, offering_id in enumerate(data.offering_ids)
                    if offering_id not in already
                ),
                key=lambda item: (-item[1], item[0]),
            )[: max(0, top_k)]
            for rank, (offering_id, score) in enumerate(ranked, start=1):
                recommendations.append(
                    heuristic.RecommendationRecord(
                        tenant_id=graph.tenant_id,
                        person_id=person_id,
                        offering_id=offering_id,
                        score=score,
                        rank=rank,
                        reason=f"graphsage link_prediction rank={rank}",
                        model_version=model_version,
                        scored_at=scored_at,
                    )
                )

        # Propensity half stays heuristic-v1 this wave (SPEC-W31 §0.4).
        scores, _unused_recs = heuristic.score_tenant(graph, now=now, top_k=top_k)
        self.model_version = model_version
        return scores, recommendations


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
