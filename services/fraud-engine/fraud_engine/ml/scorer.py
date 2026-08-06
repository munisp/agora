"""scorer.py — LearnedScorer: blended AE + CLF inference (SPEC-W33 §3 B1).

Loads the latest ``fraud-ae-vN`` + ``fraud-clf-vN`` artifacts for a tenant
from the filesystem registry (tenant dir first, ``_global`` fallback dir
second — I4 single-tenant bootstrap), and scores fv1 person vectors:

    score = 0.5 * ae_norm + 0.5 * clf_prob

``ae_norm`` is the AE reconstruction error min-max normalised against the
training-error stats stored in fraud-ae meta.json (err_min/err_max), clamped
to [0, 1]. ``model_version`` is stamped ``fraud-ae-vN+fraud-clf-vN`` and
``feature_schema`` is always ``fv1`` (I2/I5 provenance).

ABSENT WEIGHTS -> None (never an exception): torch missing, registry dir
missing/empty, or an incomplete family pair all degrade to the pure D1-D8
rule path (I1 honest degradation). All torch loads use map_location="cpu".

W33-C (SPEC-W33 §4 C1 consumer clause, ADDITIVE): resolution order in
:meth:`LearnedScorer.load` is now (a) the model-registry service record
(``fraud_engine.ml.registry_client``, env ``MODEL_REGISTRY_URL`` — read
inside ``load``, no signature change) translated to a local artifact scope
dir, then (b) EXACTLY the W31/W33-B bootstrap local-dir scan above. Any
registry failure mode (unset/404/timeout/error/unsupported scheme) yields
None and the bootstrap scan runs unchanged. When resolution came from the
registry, ``model_version`` is the registry record's ``version`` (I2
provenance honesty); otherwise it stays the dir-derived
``fraud-ae-vN+fraud-clf-vN``.
"""

from __future__ import annotations

import logging
import os
from dataclasses import dataclass
from typing import Any, Iterable, Mapping

from . import (
    GLOBAL_TENANT_DIR,
    MODEL_VERSION_AE_PREFIX,
    MODEL_VERSION_CLF_PREFIX,
    _TORCH_AVAILABLE,
    load_latest,
    registry_client,
)
from .features import FEATURE_SCHEMA, build_feature_vector

if _TORCH_AVAILABLE:
    import torch
else:
    torch = None  # type: ignore[assignment]

log = logging.getLogger("fraud_engine.ml.scorer")


@dataclass(frozen=True)
class ScoreResult:
    """One blended ML score. ``model_version``/``feature_schema`` are always
    non-null on ML-scored outputs (GB5)."""

    score: float  # 0.5*ae_norm + 0.5*clf_prob, in [0, 1]
    ae_norm: float
    clf_prob: float
    model_version: str  # "fraud-ae-vN+fraud-clf-vN"
    feature_schema: str  # "fv1"


def _minmax_norm(value: float, lo: float, hi: float) -> float:
    if hi <= lo:
        return 0.0
    return max(0.0, min(1.0, (value - lo) / (hi - lo)))


class LearnedScorer:
    """Blended fraud-ae + fraud-clf scorer for one tenant (CPU inference)."""

    def __init__(
        self,
        ae_model: Any,
        ae_meta: Mapping[str, Any],
        clf_model: Any,
        clf_meta: Mapping[str, Any],
        tenant_id: str,
        registry_version: str | None = None,
    ) -> None:
        self._ae = ae_model
        self._clf = clf_model
        self.tenant_id = tenant_id
        self.ae_version = str(ae_meta.get("model_version") or "fraud-ae-v?")
        self.clf_version = str(clf_meta.get("model_version") or "fraud-clf-v?")
        err_stats = ae_meta.get("ae_error_stats") or {}
        self._err_min = float(err_stats.get("err_min") or 0.0)
        self._err_max = float(err_stats.get("err_max") or 0.0)
        self.feature_schema = str(ae_meta.get("feature_schema") or FEATURE_SCHEMA)
        self.model_version = f"{self.ae_version}+{self.clf_version}"
        if registry_version:  # W33-C: registry-resolved provenance (I2)
            self.model_version = registry_version
            self.registry_version = registry_version
        else:
            self.registry_version = None
        self._ae.eval()
        self._clf.eval()

    # -- loading -----------------------------------------------------------
    @classmethod
    def _load_scope(
        cls,
        scope_dir: str,
        tenant_id: str,
        registry_version: str | None = None,
    ) -> "LearnedScorer | None":
        """AE+CLF pair from one scope dir (W33-B layout), or None (I1).

        ``registry_version`` is set only when ``scope_dir`` came from the
        model-registry service (W33-C): the scorer then stamps the registry
        record's ``version`` instead of the dir-derived pair version (I2).
        """
        from .autoencoder import FraudAE
        from .classifier import FraudCLF

        ae_loaded = load_latest(scope_dir, MODEL_VERSION_AE_PREFIX)
        clf_loaded = load_latest(scope_dir, MODEL_VERSION_CLF_PREFIX)
        if ae_loaded is None or clf_loaded is None:
            return None
        ae_state, ae_meta = ae_loaded
        clf_state, clf_meta = clf_loaded
        try:
            ae_model = FraudAE()
            ae_model.load_state_dict(ae_state)
            clf_model = FraudCLF()
            clf_model.load_state_dict(clf_state)
        except Exception as exc:  # noqa: BLE001 - corrupt artifact -> fallback
            log.warning("unusable artifacts in %s: %s (rule fallback)", scope_dir, exc)
            return None
        return cls(
            ae_model, ae_meta, clf_model, clf_meta, tenant_id,
            registry_version=registry_version,
        )

    @classmethod
    def load(cls, registry_dir: str | None, tenant_id: str) -> "LearnedScorer | None":
        """Latest AE+CLF pair for ``tenant_id`` (registry, then local scan).

        Resolution order (W33-C, ADDITIVE): (a) the model-registry service
        record (env ``MODEL_REGISTRY_URL`` read inside, via
        :mod:`fraud_engine.ml.registry_client`) translated to a local
        artifact scope dir; (b) on None — service unset/empty/404/timeout/
        error/unsupported scheme — EXACTLY the W31/W33-B bootstrap scan
        (tenant dir, then ``_global``). Returns None when torch is absent,
        no registry/local artifact resolves, or no complete family pair
        exists — the caller stays on pure rules (I1).
        """
        if not _TORCH_AVAILABLE:
            log.info("torch absent: learned scorer disabled (rule fallback, I1)")
            return None
        # (a) model-registry service (W33-C). Never raises; any failure mode
        # returns None and falls through to the bootstrap scan unchanged.
        resolved = registry_client.resolve_artifact_dir(tenant_id)
        if resolved is not None:
            scope_dir, record = resolved
            scorer = cls._load_scope(scope_dir, tenant_id, registry_version=record.version)
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
        # (b) bootstrap local-dir scan (W31/W33-B behavior, unchanged).
        if not registry_dir or not os.path.isdir(registry_dir):
            return None
        for scope in (tenant_id, GLOBAL_TENANT_DIR):
            scorer = cls._load_scope(os.path.join(registry_dir, scope), tenant_id)
            if scorer is not None:
                return scorer
        return None

    # -- scoring -----------------------------------------------------------
    def score_vector(self, fv: Iterable[float]) -> ScoreResult:
        """Blend one fv1 vector. CPU, single sample, torch.no_grad."""
        x = torch.tensor([list(fv)], dtype=torch.float32, device="cpu")
        with torch.no_grad():
            ae_err = float(self._ae.reconstruction_error(x).detach().cpu()[0])
            clf_prob = float(self._clf.probability(x).detach().cpu()[0])
        ae_norm = _minmax_norm(ae_err, self._err_min, self._err_max)
        score = 0.5 * ae_norm + 0.5 * clf_prob
        return ScoreResult(
            score=score,
            ae_norm=ae_norm,
            clf_prob=clf_prob,
            model_version=self.model_version,
            feature_schema=self.feature_schema,
        )

    def score_events(
        self, events: Iterable[Mapping[str, Any]], referral_degree: int = 0
    ) -> ScoreResult:
        """Build fv1 from an A1-shaped event stream and blend it."""
        return self.score_vector(build_feature_vector(events, referral_degree))

    def blend_reason(self, result: ScoreResult) -> str:
        """The ``ml_blend`` reason string stamped into alert evidence."""
        return f"ml_blend ae={result.ae_norm:.4f} clf={result.clf_prob:.4f}"
