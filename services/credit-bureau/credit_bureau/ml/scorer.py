"""LearnedScorer (SPEC-W33 §3 B2): loads a CreditMLP artifact from the
filesystem registry and scores fv1 vectors on CPU (I5).

Absent weights -> ``None`` (I1 honest degradation): ``LearnedScorer.load``
returns None when torch is unavailable, the registry dir is unset/empty,
or no ``credit-ml-v{N}`` artifact exists for the tenant — the API then
answers with the ported Go rule score (``heuristic-v1``).

Tenant scoping (I4): resolution order is
``{registry}/{tenant_id}/credit-ml-v{N}`` (highest N), then
``{registry}/global/credit-ml-v{N}``. There is no cross-tenant read.

W33-C (SPEC-W33 §4 C1 consumer clause, ADDITIVE): resolution order in
:meth:`LearnedScorer.load` is now (a) the model-registry service record
(``credit_bureau.ml.registry_client``, env ``MODEL_REGISTRY_URL`` — read
inside ``load``, no signature change) translated to a local artifact scope
dir, then (b) EXACTLY the bootstrap local-dir scan above. Any registry
failure mode yields None and the bootstrap scan runs unchanged (I1). When
resolution came from the registry, ``model_version`` is the registry
record's ``version`` (I2 provenance honesty).
"""

from __future__ import annotations

import json
import logging
import os
import re
from typing import Any

import numpy as np

from .. import MODEL_VERSION_ML_PREFIX
from . import registry_client
from .features import FEATURE_SCHEMA, FEATURE_DIM, build_feature_vector
from .model import TORCH_AVAILABLE, CreditMLP

if TORCH_AVAILABLE:
    import torch

log = logging.getLogger(__name__)


def _latest_version_dir(base: str) -> tuple[str, int] | None:
    if not os.path.isdir(base):
        return None
    pattern = re.compile(rf"^{re.escape(MODEL_VERSION_ML_PREFIX)}(\d+)$")
    best: tuple[str, int] | None = None
    for entry in os.listdir(base):
        match = pattern.match(entry)
        if match and os.path.isfile(os.path.join(base, entry, "model.pt")):
            n = int(match.group(1))
            if best is None or n > best[1]:
                best = (os.path.join(base, entry), n)
    return best


class LearnedScorer:
    """CPU inference wrapper over one CreditMLP artifact."""

    def __init__(self, model: Any, model_version: str, meta: dict[str, Any]) -> None:
        self._model = model
        self.model_version = model_version
        self.meta = meta

    @classmethod
    def _load_artifact_dir(
        cls, artifact_dir: str, version_override: str | None = None
    ) -> "LearnedScorer | None":
        """Load one versioned artifact dir (model.pt + meta.json); None (I1).

        ``version_override`` is set only when the dir came from the
        model-registry service (W33-C): the scorer then stamps the registry
        record's ``version`` instead of the dir-derived version (I2).
        """
        try:
            with open(os.path.join(artifact_dir, "meta.json"), encoding="utf-8") as f:
                meta = json.load(f)
            blob = torch.load(os.path.join(artifact_dir, "model.pt"), map_location="cpu", weights_only=True)
            if blob.get("feature_schema") != FEATURE_SCHEMA:
                log.warning("artifact feature schema %r != %r — ignoring", blob.get("feature_schema"), FEATURE_SCHEMA)
                return None
            torch.set_num_threads(1)
            model = CreditMLP(input_dim=int(blob["input_dim"]), hidden_dim=int(blob["hidden_dim"]))
            model.load_state_dict(blob["state_dict"])
            model.eval()
        except Exception as exc:  # noqa: BLE001 — any corrupt artifact falls back to rules
            log.warning("credit-ml artifact %s unreadable (%s) — rules fallback", artifact_dir, exc)
            return None
        version = version_override or str(meta.get("model_version") or os.path.basename(artifact_dir))
        return cls(model, version, meta)

    @classmethod
    def _load_scope(
        cls, scope_dir: str, version_override: str | None = None
    ) -> "LearnedScorer | None":
        """Highest-N ``credit-ml-v{N}`` inside one scope dir; None (I1)."""
        latest = _latest_version_dir(scope_dir)
        if latest is None:
            return None
        return cls._load_artifact_dir(latest[0], version_override=version_override)

    @classmethod
    def load(cls, registry_dir: str, tenant_id: str = "global") -> "LearnedScorer | None":
        """Resolve + load the tenant's artifact; None when absent (I1).

        Resolution order (W33-C, ADDITIVE): (a) the model-registry service
        record (env ``MODEL_REGISTRY_URL`` read inside, via
        :mod:`credit_bureau.ml.registry_client`) translated to a local
        artifact scope dir; (b) on None — service unset/empty/404/timeout/
        error/unsupported scheme — EXACTLY the W33-B bootstrap scan
        (``{registry}/{tenant_id}``, then ``{registry}/global``).
        """
        if not TORCH_AVAILABLE:
            log.warning("torch unavailable — learned credit scorer disabled (rules fallback)")
            return None
        # (a) model-registry service (W33-C). Never raises; any failure mode
        # returns None and falls through to the bootstrap scan unchanged.
        resolved = registry_client.resolve_artifact_dir(tenant_id)
        if resolved is not None:
            scope_dir, record = resolved
            scorer = cls._load_scope(scope_dir, version_override=record.version)
            if scorer is not None:
                log.info(
                    "resolved %s for tenant %s via model-registry (version=%s)",
                    registry_client.FAMILY,
                    tenant_id,
                    record.version,
                )
                return scorer
            log.warning(
                "registry artifact dir %s unusable; bootstrap fallback", scope_dir
            )
        # (b) bootstrap local-dir scan (W33-B behavior, unchanged).
        if not registry_dir:
            return None
        scorer = cls._load_scope(os.path.join(registry_dir, tenant_id))
        if scorer is None:
            scorer = cls._load_scope(os.path.join(registry_dir, "global"))
        return scorer

    def score(self, signals: dict[str, Any] | None) -> tuple[float, float]:
        """(bureau score in [300,900], P(default-in-12m)) for one payload."""
        vec = build_feature_vector(signals)
        with torch.no_grad():
            x = torch.from_numpy(np.asarray(vec, dtype=np.float32)).unsqueeze(0)
            score, logit = self._model(x)
            prob = torch.sigmoid(logit)
        return float(score[0]), float(prob[0])

    @property
    def feature_dim(self) -> int:
        return FEATURE_DIM
