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

import asyncio
import base64
import hashlib
import hmac
import json
import logging
import time
import urllib.parse
import urllib.request
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


def decode_and_verify(
    token: str,
    key: str,
    algorithm: str,
    issuer: str | None = None,
    audience: str | None = None,
) -> dict[str, Any]:
    """Verify signature + REQUIRED exp (+ iss/aud when configured), return
    claims. Raises AuthError (401)."""
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

    # SPEC-W43 Y-07: exp is REQUIRED — a token without an expiry never
    # authenticates.
    exp = claims.get("exp")
    if exp is None:
        raise AuthError("JWT missing exp claim")
    try:
        expired = float(exp) <= time.time()
    except (TypeError, ValueError):
        raise AuthError("JWT malformed exp claim") from None
    if expired:
        raise AuthError("JWT expired")
    # iss/aud validated when GRAPH_JWT_ISSUER / GRAPH_JWT_AUDIENCE are set.
    if issuer is not None and claims.get("iss") != issuer:
        raise AuthError("JWT issuer mismatch")
    if audience is not None:
        aud = claims.get("aud")
        ok = aud == audience or (isinstance(aud, list) and audience in aud)
        if not ok:
            raise AuthError("JWT audience mismatch")
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
        claims = decode_and_verify(
            token,
            settings.jwt_public_key,
            settings.jwt_algorithm,
            issuer=settings.jwt_issuer or None,
            audience=settings.jwt_audience or None,
        )
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


_log = logging.getLogger(__name__)
# slug -> (expires_at, tenant uuid): the business id rarely changes; ten minutes keeps a lookup off most requests.
_TENANT_ID_CACHE: dict[str, tuple[float, str]] = {}
_TENANT_ID_TTL_S = 600.0


def _lookup_tenant_id(identity_url: str, slug: str, authorization: str, slugs_header: str) -> str | None:
    """The business's tenant uuid from identity-service (GET /v1/tenants/{slug}), asked with the caller's own token,
    so a user can only resolve a business they belong to."""
    req = urllib.request.Request(
        f"{identity_url.rstrip('/')}/v1/tenants/{urllib.parse.quote(slug, safe='')}",
        headers={"Authorization": authorization, "X-Tenant-Slugs": slugs_header, "Accept": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=5) as resp:  # noqa: S310 - internal service URL from config
        data = json.loads(resp.read().decode() or "{}")
    tid = data.get("id") or data.get("tenant_id")
    return str(tid) if tid else None


async def tenant_id_from_slugs(settings: Settings, authorization: str | None, x_tenant_slugs: str | None) -> str | None:
    """The business behind this request, from the gateway-injected X-Tenant-Slugs (the JWT tenant_slugs claim; first
    slug wins, as in the other services). graph-sync writes every node under the business's tenant uuid (the
    CloudEvent tenantid), so reads must use the same id - the JWT sub is the USER, and resolving to it filed segments
    under the user and hid every synced person and fraud alert from the business."""
    if not authorization or not x_tenant_slugs or not settings.identity_url:
        return None
    slugs = [s.strip().rsplit("/", 1)[-1] for s in x_tenant_slugs.split(",") if s.strip()]
    if not slugs:
        return None
    slug = slugs[0]
    hit = _TENANT_ID_CACHE.get(slug)
    if hit and hit[0] > time.time():
        return hit[1]
    try:
        tid = await asyncio.to_thread(_lookup_tenant_id, settings.identity_url, slug, authorization, x_tenant_slugs)
    except Exception as exc:  # noqa: BLE001 - fall back to the old resolution, never fail the request on this
        _log.warning("tenant lookup for slug %s failed: %s", slug, exc)
        return None
    if tid:
        _TENANT_ID_CACHE[slug] = (time.time() + _TENANT_ID_TTL_S, tid)
    return tid


async def current_tenant(
    request: Request,
    authorization: str | None = Header(default=None),
    x_tenant_id: str | None = Header(default=None),
    x_tenant_slugs: str | None = Header(default=None),
) -> str:
    """FastAPI dependency: the tenant id for this request."""
    settings: Settings = request.app.state.settings
    fallback = tenant_from_request(settings, authorization, x_tenant_id)  # also verifies the token
    return (await tenant_id_from_slugs(settings, authorization, x_tenant_slugs)) or fallback


async def resolve_admin_principal(
    request: Request,
    authorization: str | None = Header(default=None),
    x_tenant_id: str | None = Header(default=None),
    x_internal_token: str | None = Header(default=None),
    x_user_roles: str | None = Header(default=None),
    x_tenant_slugs: str | None = Header(default=None),
) -> tuple[str, str]:
    """SPEC-W45 OOS-12 principal for alert-resolution routes.

    Returns ``(tenant_id, resolved_by)``:

    - ``X-Internal-Token`` present → service-to-service path (constant-time
      compare, fail-closed — the same semantics as
      ``app.routers.require_internal_token``). The tenant comes from the
      ``X-Tenant-Id`` header (a service caller holds no JWT in prod) and
      the resolver is recorded as ``service:internal``.
    - Otherwise → human path: tenant via the standard workforce seam (JWT
      sub / dev header), then the caller must carry the ``admin`` realm
      role on ``X-User-Roles`` (injected by the trusted gateway from the
      JWT realm roles — the same posture as booking-service). Resolving an
      alert suppresses a fraud signal, hence 403 for any non-admin role.
    """
    settings: Settings = request.app.state.settings
    if x_internal_token:
        expected = (settings.internal_token or "").strip()
        if not expected or not hmac.compare_digest(x_internal_token, expected):
            raise HTTPException(status_code=401, detail="invalid X-Internal-Token")
        if not x_tenant_id or not x_tenant_id.strip():
            raise HTTPException(
                status_code=400,
                detail="X-Tenant-Id header required for service callers",
            )
        return x_tenant_id.strip(), "service:internal"
    user_id = tenant_from_request(settings, authorization, x_tenant_id)  # the person resolving (JWT sub)
    tenant_id = (await tenant_id_from_slugs(settings, authorization, x_tenant_slugs)) or user_id
    roles = {r.strip().lower() for r in (x_user_roles or "").split(",") if r.strip()}
    if "admin" not in roles:
        raise HTTPException(
            status_code=403, detail="admin realm role required to resolve alerts"
        )
    return tenant_id, user_id
