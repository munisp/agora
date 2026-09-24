"""credit-bureau HTTP API (SPEC-W33 §3 B2).

``POST /v1/credit/score`` computes the ported Go rule score ALWAYS and,
when a learned artifact resolves for the tenant (CREDIT_ML_REGISTRY_DIR),
blends ``0.6*ml + 0.4*rule`` clamped to [300,900]. Rule reasons are never
dropped (the Go rule emits none — see credit_bureau/rules.py docstring);
provenance rides in ``model_version`` (``credit-ml-v{N}`` |
``heuristic-v1``) and ``ml_contribution`` (I2). Without an artifact the
response is the pure rule output, byte-stable for a fixed payload (I1).

Tenant scoping (I4) mirrors services/graph-service/app/auth.py: with
``JWT_PUBLIC_KEY`` set, /v1 requires a Bearer JWT whose `sub` is the
tenant; unset = dev mode, the ``X-Tenant-Id`` header supplies the tenant.
/healthz is exempt.
"""

from __future__ import annotations

import asyncio
import base64
import hashlib
import hmac
import json
import logging
import os
import time
from typing import Any

from fastapi import Depends, FastAPI, Header, HTTPException, Request
from pydantic import BaseModel, Field

from . import FEATURE_SCHEMA, MODEL_VERSION_HEURISTIC
from .config import Settings, load_settings
from .ml.scorer import LearnedScorer
from .rules import BUREAU_MAX, BUREAU_MIN, ScoreSignals, naive_score, naive_to_bureau, score as rule_score

log = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Auth seam (graph-service convention, dependency-light: HS256 via stdlib;
# RS256/ES256 via lazy `cryptography` import).
# ---------------------------------------------------------------------------


class AuthError(HTTPException):
    def __init__(self, detail: str):
        super().__init__(status_code=401, detail=detail)


def _b64url_decode(segment: str) -> bytes:
    pad = "=" * (-len(segment) % 4)
    try:
        return base64.urlsafe_b64decode(segment + pad)
    except Exception as exc:  # noqa: BLE001
        raise AuthError("malformed JWT") from exc


def _load_pem_public_key(key_pem: str) -> Any:
    """Parse a PEM public key once (W46-F P7). Returns the key object or
    None on parse failure (fail-closed: requests then 401 exactly as the
    previous per-request parse failure did)."""
    try:
        from cryptography.hazmat.primitives import serialization
    except ImportError:
        return None  # per-request path raises the precise AuthError
    try:
        return serialization.load_pem_public_key(key_pem.encode())
    except Exception:  # noqa: BLE001
        log.error("JWT public key PEM failed to parse at startup; "
                  "asymmetric verification will fail closed")
        return None


def _verify_rs256_es256(
    algorithm: str,
    signing_input: bytes,
    signature: bytes,
    key_pem: str,
    parsed_key: Any = None,
) -> bool:
    try:
        from cryptography.hazmat.primitives import hashes, serialization
        from cryptography.hazmat.primitives.asymmetric import ec, padding
    except ImportError as exc:
        raise AuthError(f"JWT_ALGORITHM {algorithm} requires the cryptography package") from exc
    try:
        # W46-F P7: the PEM is parsed ONCE at startup (parsed_key) instead of
        # per request; fall back to the legacy per-request parse only when no
        # parsed key was supplied (direct decode_and_verify callers/tests).
        key = parsed_key if parsed_key is not None else serialization.load_pem_public_key(key_pem.encode())
        if algorithm == "RS256":
            key.verify(signature, signing_input, padding.PKCS1v15(), hashes.SHA256())
        else:
            key.verify(signature, signing_input, ec.ECDSA(hashes.SHA256()))
        return True
    except Exception:  # noqa: BLE001 — any verification failure
        return False


def decode_and_verify(
    token: str, key: str, algorithm: str, parsed_key: Any = None
) -> dict[str, Any]:
    algorithm = algorithm.upper()
    if algorithm not in ("HS256", "RS256", "ES256"):
        raise AuthError(f"unsupported JWT_ALGORITHM {algorithm!r}")
    parts = token.split(".")
    if len(parts) != 3:
        raise AuthError("malformed JWT")
    try:
        header = json.loads(_b64url_decode(parts[0]))
        claims = json.loads(_b64url_decode(parts[1]))
    except (ValueError, UnicodeDecodeError) as exc:
        raise AuthError("malformed JWT") from exc
    if header.get("alg") != algorithm:
        raise AuthError("JWT alg mismatch")
    signing_input = f"{parts[0]}.{parts[1]}".encode()
    signature = _b64url_decode(parts[2])
    if algorithm == "HS256":
        expected = hmac.new(key.encode(), signing_input, hashlib.sha256).digest()
        ok = hmac.compare_digest(expected, signature)
    else:
        ok = _verify_rs256_es256(algorithm, signing_input, signature, key, parsed_key)
    if not ok:
        raise AuthError("JWT signature verification failed")
    exp = claims.get("exp")
    if exp is not None and float(exp) <= time.time():
        raise AuthError("JWT expired")
    return claims


def tenant_from_request(
    settings: Settings,
    authorization: str | None,
    x_tenant_id: str | None,
    parsed_key: Any = None,
) -> str:
    if authorization:
        scheme, _, token = authorization.partition(" ")
        if scheme.lower() != "bearer" or not token:
            raise AuthError("Authorization must be 'Bearer <token>'")
        if not settings.jwt_public_key:
            raise AuthError("Bearer auth not configured on this deployment")
        claims = decode_and_verify(
            token, settings.jwt_public_key, settings.jwt_algorithm, parsed_key
        )
        sub = claims.get("sub")
        if not sub or not isinstance(sub, str):
            raise AuthError("JWT missing sub claim")
        return sub
    if not settings.jwt_public_key:
        if x_tenant_id and x_tenant_id.strip():
            return x_tenant_id.strip()
        raise AuthError("X-Tenant-Id header required (dev auth mode)")
    raise AuthError("Authorization Bearer token required")


async def current_tenant(
    request: Request,
    authorization: str | None = Header(default=None),
    x_tenant_id: str | None = Header(default=None),
) -> str:
    settings: Settings = request.app.state.settings
    return tenant_from_request(
        settings,
        authorization,
        x_tenant_id,
        getattr(request.app.state, "jwt_verification_key", None),
    )


# ---------------------------------------------------------------------------
# Schemas
# ---------------------------------------------------------------------------


class RuleSignalsIn(BaseModel):
    """The Go ScoreSignals mirror (lending.go:398-402)."""

    tenure_days: int = 0
    completed_bookings: int = 0
    repaid_loans: int = 0


class ScoreRequest(BaseModel):
    signals: RuleSignalsIn = Field(default_factory=RuleSignalsIn)
    # Optional fv1 raw signals for the learned head (I6: derived/hashed-
    # domain values only — band indices, rates, clamped amounts).
    features: dict[str, Any] | None = None


class ScoreResponse(BaseModel):
    score: int  # blended, clamped [300,900]
    reasons: list[str]  # rule reasons — never dropped (Go emits none)
    model_version: str  # credit-ml-v{N} | heuristic-v1 (I2)
    ml_contribution: float  # blended score minus rule bureau score
    feature_schema: str  # "fv1"
    rule_score: int  # ported Go naive score, 0..100
    rule_score_bureau: int  # affine 300..900 mapping of the naive score
    default_probability: float | None = None  # learned head only
    tenant_id: str


# ---------------------------------------------------------------------------
# App
# ---------------------------------------------------------------------------


def _registry_tenant_dirs(registry_dir: str) -> list[str]:
    """Tenant scopes present in the filesystem bootstrap registry (direct
    subdirectories, e.g. per-tenant dirs + the `global` fallback scope)."""
    try:
        return sorted(
            entry
            for entry in os.listdir(registry_dir)
            if os.path.isdir(os.path.join(registry_dir, entry))
        )
    except OSError:
        return []


def create_app(settings: Settings | None = None) -> FastAPI:
    settings = settings or load_settings()
    app = FastAPI(title="credit-bureau", version="1.0.0")
    app.state.settings = settings
    app.state.scorer_cache = {}
    # W46-F P7: parse the JWT verification PEM ONCE at startup instead of
    # per request (RS256/ES256 only; HS256 needs no parse). None when the
    # parse fails → fail-closed 401s, same as the legacy per-request path.
    app.state.jwt_verification_key = (
        _load_pem_public_key(settings.jwt_public_key)
        if settings.jwt_public_key
        and settings.jwt_algorithm.upper() in ("RS256", "ES256")
        else None
    )

    async def scorer_for(tenant_id: str) -> LearnedScorer | None:
        cache = app.state.scorer_cache
        if tenant_id not in cache:
            # W46-F P7: artifact resolution (urllib GET to model-registry +
            # torch.load + load_state_dict) is BLOCKING — run it off the
            # event loop so a cold-miss never stalls concurrent requests.
            cache[tenant_id] = await asyncio.to_thread(
                LearnedScorer.load, settings.ml_registry_dir, tenant_id
            )
            if cache[tenant_id] is None:
                log.info("no credit-ml artifact for tenant %s — rules-only (heuristic-v1)", tenant_id)
        return cache[tenant_id]

    @app.on_event("startup")
    async def _warm_scorers() -> None:
        """W46-F P7: best-effort scorer warm at boot for the tenant scopes
        the filesystem bootstrap registry lists (per-tenant dirs + global).
        Failures are logged and skipped — LearnedScorer.load already
        degrades to None (I1), and the first request re-tries lazily."""
        if not settings.ml_registry_dir:
            return
        for tenant_id in await asyncio.to_thread(
            _registry_tenant_dirs, settings.ml_registry_dir
        ):
            try:
                scorer = await scorer_for(tenant_id)
                if scorer is not None:
                    log.info("warmed credit-ml scorer for tenant scope %s (%s)",
                             tenant_id, scorer.model_version)
            except Exception as exc:  # noqa: BLE001 — best-effort warm
                log.warning("scorer warm failed for tenant scope %s: %s",
                            tenant_id, exc)

    @app.get("/healthz")
    async def healthz() -> dict[str, Any]:
        return {
            "ok": True,
            "service": "credit-bureau",
            "ml_registry": "on" if settings.ml_registry_dir else "off",
            "feature_schema": FEATURE_SCHEMA,
        }

    @app.post("/v1/credit/score", response_model=ScoreResponse)
    async def score_endpoint(payload: ScoreRequest, tenant_id: str = Depends(current_tenant)) -> ScoreResponse:
        sig = ScoreSignals(
            tenure_days=payload.signals.tenure_days,
            completed_bookings=payload.signals.completed_bookings,
            repaid_loans=payload.signals.repaid_loans,
        )
        naive, reasons = rule_score(sig)
        rule_bureau = naive_to_bureau(naive)

        scorer = await scorer_for(tenant_id)
        if scorer is None:
            # I1 honest degradation: pure rule output, byte-stable.
            return ScoreResponse(
                score=rule_bureau,
                reasons=reasons,
                model_version=MODEL_VERSION_HEURISTIC,
                ml_contribution=0.0,
                feature_schema=FEATURE_SCHEMA,
                rule_score=naive,
                rule_score_bureau=rule_bureau,
                default_probability=None,
                tenant_id=tenant_id,
            )

        # W46-F P7: torch inference is CPU-bound — off the event loop.
        ml_score, p_default = await asyncio.to_thread(scorer.score, payload.features)
        blended = settings.blend_ml_weight * ml_score + (1.0 - settings.blend_ml_weight) * rule_bureau
        blended_int = int(round(blended))
        blended_int = max(BUREAU_MIN, min(BUREAU_MAX, blended_int))
        return ScoreResponse(
            score=blended_int,
            reasons=reasons,
            model_version=scorer.model_version,
            ml_contribution=float(blended_int - rule_bureau),
            feature_schema=FEATURE_SCHEMA,
            rule_score=naive,
            rule_score_bureau=rule_bureau,
            default_probability=p_default,
            tenant_id=tenant_id,
        )

    return app


app = create_app()
