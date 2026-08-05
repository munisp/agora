"""Workforce auth seam (SPEC-W28 §4 WS-B): tenant from JWT `sub` claim.

- ``JWT_PUBLIC_KEY`` configured -> every /v1 request must carry
  ``Authorization: Bearer <jwt>``; the signature is verified and the `sub`
  claim becomes the tenant id injected into every graph query.
- ``JWT_PUBLIC_KEY`` unset -> dev mode (dev compose / tests): the
  ``X-Tenant-Id`` header supplies the tenant id, mirroring the
  analytics-pipeline sidecar conventions.

There is no unsigned path when a key is configured, and no endpoint is
exempt from tenant resolution except /healthz and /metrics.

JWT verification is dependency-light: HS256 via stdlib hmac; RS256/ES256 via
``cryptography`` (already a service dependency for the voice stack).
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import time
from typing import Any

from fastapi import Header, HTTPException, Request

from .config import Settings

_SUPPORTED_ALGS = ("HS256", "RS256", "ES256")


class AuthError(HTTPException):
    def __init__(self, detail: str):
        super().__init__(status_code=401, detail=detail)


def _b64url_decode(segment: str) -> bytes:
    pad = "=" * (-len(segment) % 4)
    try:
        return base64.urlsafe_b64decode(segment + pad)
    except Exception as exc:  # noqa: BLE001
        raise AuthError("malformed JWT") from exc


def _verify_rs256(signing_input: bytes, signature: bytes, key_pem: str) -> bool:
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import padding

    try:
        key = serialization.load_pem_public_key(key_pem.encode())
        key.verify(signature, signing_input, padding.PKCS1v15(), hashes.SHA256())
        return True
    except Exception:  # noqa: BLE001 — any verification failure
        return False


def _verify_es256(signing_input: bytes, signature: bytes, key_pem: str) -> bool:
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import ec

    try:
        key = serialization.load_pem_public_key(key_pem.encode())
        key.verify(signature, signing_input, ec.ECDSA(hashes.SHA256()))
        return True
    except Exception:  # noqa: BLE001
        return False


def decode_and_verify(token: str, key: str, algorithm: str) -> dict[str, Any]:
    """Verify signature + expiry, return claims. Raises AuthError."""
    algorithm = algorithm.upper()
    if algorithm not in _SUPPORTED_ALGS:
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
    elif algorithm == "RS256":
        ok = _verify_rs256(signing_input, signature, key)
    else:
        ok = _verify_es256(signing_input, signature, key)
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
) -> str:
    """Resolve the caller's tenant id. Raises AuthError (401)."""
    if authorization:
        scheme, _, token = authorization.partition(" ")
        if scheme.lower() != "bearer" or not token:
            raise AuthError("Authorization must be 'Bearer <token>'")
        if not settings.jwt_public_key:
            raise AuthError("Bearer auth not configured on this deployment")
        claims = decode_and_verify(token, settings.jwt_public_key, settings.jwt_algorithm)
        sub = claims.get("sub")
        if not sub or not isinstance(sub, str):
            raise AuthError("JWT missing sub claim")
        return sub
    # Dev-mode fallback (workforce seam): only when no public key is set.
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
    """FastAPI dependency: the tenant id for this request."""
    settings: Settings = request.app.state.settings
    return tenant_from_request(settings, authorization, x_tenant_id)
