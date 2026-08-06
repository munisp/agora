"""Environment-driven settings (stdlib dataclass, mirroring
services/graph-ml/graph_ml/config.py conventions). SPEC-W33 §3 B2.

The learned scorer is OFF by default: ``CREDIT_ML_REGISTRY_DIR`` unset
(or holding no ``credit-ml-v{N}`` artifact for the tenant) means the
service answers with the ported Go rule score only
(``model_version: heuristic-v1``) — invariant I1 honest degradation.
"""

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
    # HTTP server.
    port: int = field(default_factory=lambda: _int("PORT", 7017))
    host: str = field(default_factory=lambda: _str("HOST", "0.0.0.0"))

    # Learned-scorer registry root (invariant I4: resolution is
    #   {registry}/{tenant_id}/credit-ml-v{N} then {registry}/global/…).
    # Empty (default) = ML OFF, rules-only.
    ml_registry_dir: str = field(default_factory=lambda: _str("CREDIT_ML_REGISTRY_DIR", ""))

    # Auth seam (mirrors services/graph-service/app/auth.py): when
    # JWT_PUBLIC_KEY is set, /v1 requires a Bearer JWT and `sub` is the
    # tenant; unset = dev mode, the X-Tenant-Id header supplies the tenant.
    jwt_public_key: str = field(default_factory=lambda: _str("JWT_PUBLIC_KEY", ""))
    jwt_algorithm: str = field(default_factory=lambda: _str("JWT_ALGORITHM", "HS256"))

    # Blend weights (SPEC-W33 §3 B2: 0.6*ml + 0.4*rule, clamp [300,900]).
    blend_ml_weight: float = 0.6
    score_min: int = 300
    score_max: int = 900


def load_settings() -> Settings:
    return Settings()
