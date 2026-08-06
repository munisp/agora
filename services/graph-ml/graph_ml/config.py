"""Environment-driven settings (stdlib dataclass, mirroring
services/graph-service/app/config.py conventions). SPEC-W29 §3 WS-A."""

from __future__ import annotations

import os
from dataclasses import dataclass, field


def _int(name: str, default: int) -> int:
    raw = os.getenv(name)
    if raw is None or raw == "":
        return default
    return int(raw)


def _str(name: str, default: str) -> str:
    raw = os.getenv(name)
    return default if raw is None or raw == "" else raw


@dataclass(frozen=True)
class Settings:
    # HTTP server (optional on-demand rescore API, SPEC-W29 §1).
    port: int = field(default_factory=lambda: _int("PORT", 7016))
    host: str = field(default_factory=lambda: _str("HOST", "0.0.0.0"))

    # FalkorDB (Redis protocol). Read-only for this service — the single
    # write path is the graph-service internal API (SPEC-W29 §4 gate 3).
    falkordb_host: str = field(default_factory=lambda: _str("FALKORDB_HOST", "localhost"))
    falkordb_port: int = field(default_factory=lambda: _int("FALKORDB_PORT", 6379))
    falkordb_db: str = field(default_factory=lambda: _str("FALKORDB_DB", "graph"))
    falkordb_username: str = field(default_factory=lambda: _str("FALKORDB_USERNAME", ""))
    falkordb_password: str = field(default_factory=lambda: _str("FALKORDB_PASSWORD", ""))

    # graph-service internal write-back API.
    graph_service_url: str = field(
        default_factory=lambda: _str("GRAPH_SERVICE_URL", "http://localhost:7014")
    )
    internal_token: str = field(default_factory=lambda: _str("INTERNAL_TOKEN", ""))

    # Backend: "heuristic" (numpy only, default) or "gnn" (requires
    # torch/torch-geometric; falls back to heuristic with a warning when the
    # imports are unavailable — SPEC-W29 §4 gate 5).
    backend: str = field(default_factory=lambda: _str("GRAPH_ML_BACKEND", "heuristic"))
    model_dir: str = field(default_factory=lambda: _str("GRAPH_ML_MODEL_DIR", "./models"))

    # GNN training/inference knobs (SPEC-W31 §1; safe CPU-first defaults).
    device: str = field(default_factory=lambda: _str("GRAPH_ML_DEVICE", "auto"))
    seed: int = field(default_factory=lambda: _int("GRAPH_ML_SEED", 42))
    gnn_epochs: int = field(default_factory=lambda: _int("GRAPH_ML_EPOCHS", 200))
    gnn_hidden_dim: int = field(default_factory=lambda: _int("GRAPH_ML_HIDDEN_DIM", 64))
    gnn_min_persons: int = field(
        default_factory=lambda: _int("GRAPH_ML_GNN_MIN_PERSONS", 20)
    )
    gnn_min_edges: int = field(default_factory=lambda: _int("GRAPH_ML_GNN_MIN_EDGES", 30))
    # Supervised head opt-in (SPEC-W33 §3 B3): "link" (default, exact W31
    # link-prediction behavior) or "classifier" (A1-labeled node
    # classification; graph_ml/gnn_head.py).
    gnn_head: str = field(default_factory=lambda: _str("GNN_HEAD", "link"))
    gnn_head_patience: int = field(
        default_factory=lambda: _int("GNN_HEAD_PATIENCE", 20)
    )
    gnn_head_val_fraction: float = field(
        default_factory=lambda: float(os.getenv("GNN_HEAD_VAL_FRACTION") or 0.2)
    )
    # Nightly retrain scheduler interval; 0 = off (default).
    train_interval_minutes: int = field(
        default_factory=lambda: _int("GRAPH_ML_TRAIN_INTERVAL_MINUTES", 0)
    )

    # Scoring knobs.
    top_k: int = field(default_factory=lambda: _int("GRAPH_ML_TOP_K", 5))
    score_interval_minutes: int = field(
        default_factory=lambda: _int("SCORE_INTERVAL_MINUTES", 60)
    )
    tenant_concurrency: int = field(default_factory=lambda: _int("TENANT_CONCURRENCY", 4))
    writeback_chunk_size: int = 500  # SPEC-W29 §3 WS-A: chunked (500), fixed.

    http_timeout_s: float = 30.0


def load_settings() -> Settings:
    return Settings()
