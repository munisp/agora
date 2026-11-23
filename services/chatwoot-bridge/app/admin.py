"""Bridge admin routes (SPEC-W118 §4): per-tenant mapping lifecycle.

X-Internal-Token gated with the K19 idiom (503 fail-closed when
CHATWOOT_BRIDGE_INTERNAL_TOKEN is unset, 401 on a missing/wrong token).
/internal/* — never routed by APISIX (the standing rule); reachable only
inside the compose network.
"""

from __future__ import annotations

import hmac
import secrets
import uuid
from typing import Any

from fastapi import APIRouter, HTTPException, Request, status
from pydantic import BaseModel

from .client import ChatwootError
from .logging import get_logger

log = get_logger(__name__)

router = APIRouter()

WEBHOOK_SUBSCRIPTIONS = ["message_type"]


def _require_internal_token(request: Request) -> None:
    """K19-pattern guard: fail closed when the server token is unset."""
    token = getattr(request.app.state.cfg, "chatwoot_bridge_internal_token", "") or ""
    if not token:
        raise HTTPException(
            status.HTTP_503_SERVICE_UNAVAILABLE,
            "internal routes disabled: CHATWOOT_BRIDGE_INTERNAL_TOKEN unset",
        )
    presented = request.headers.get("X-Internal-Token", "")
    if not presented or not hmac.compare_digest(presented, token):
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "invalid internal token")


class RegisterRequest(BaseModel):
    tenant_id: uuid.UUID
    account_id: int
    inbox_id: int
    api_access_token: str
    hmac_secret: str | None = None
    auto_register_webhook: bool = True


def _mask(token: str) -> str:
    return f"…{token[-4:]}" if len(token) >= 4 else "…"


def _public_mapping(row: dict[str, Any], base_url: str) -> dict[str, Any]:
    return {
        "tenant_id": str(row["tenant_id"]),
        "account_id": row["account_id"],
        "inbox_id": row["inbox_id"],
        "api_access_token": _mask(row["api_access_token"]),
        "webhook_url": (
            f"{base_url}/v1/chatwoot/webhook/{row['tenant_id']}"
            f"/{row['webhook_token']}"
        ),
        "webhook_id": row.get("webhook_id"),
        "hmac_configured": bool(row.get("hmac_secret")),
        "enabled": row["enabled"],
    }


@router.post("/internal/tenants/register")
async def register_tenant(body: RegisterRequest, request: Request) -> dict[str, Any]:
    """Register (or re-register) a tenant's Chatwoot mapping.

    The credential is validated LIVE (the inbox must be listable and
    present) BEFORE anything is stored — a typo'd token must fail loudly
    here, not silently break sync later. The per-tenant webhook token is
    stable across re-registrations.
    """
    _require_internal_token(request)
    st = request.app.state
    tenant_id = str(body.tenant_id)

    try:
        ok = await st.chatwoot.validate_inbox(
            body.api_access_token, body.account_id, body.inbox_id
        )
    except ChatwootError as exc:
        raise HTTPException(
            status.HTTP_400_BAD_REQUEST,
            f"chatwoot credential validation failed: {exc}",
        ) from None
    if not ok:
        raise HTTPException(
            status.HTTP_400_BAD_REQUEST,
            f"inbox {body.inbox_id} not visible to the supplied credential",
        )

    existing = await st.store.get_tenant(tenant_id)
    webhook_token = (
        existing["webhook_token"] if existing else secrets.token_urlsafe(24)
    )
    webhook_url = (
        f"{st.cfg.bridge_public_base_url}/v1/chatwoot/webhook"
        f"/{tenant_id}/{webhook_token}"
    )

    webhook_id = existing.get("webhook_id") if existing else None
    registered = False
    if body.auto_register_webhook:
        try:
            hooks = await st.chatwoot.list_webhooks(
                body.api_access_token, body.account_id
            )
            match = next((h for h in hooks if h.get("url") == webhook_url), None)
            if match is not None:
                webhook_id = int(match.get("id", webhook_id or 0)) or webhook_id
            else:
                webhook_id = await st.chatwoot.create_webhook(
                    body.api_access_token,
                    body.account_id,
                    url=webhook_url,
                    subscriptions=WEBHOOK_SUBSCRIPTIONS,
                )
            registered = True
        except ChatwootError as exc:
            # Webhook auto-registration is best-effort: the mapping is still
            # stored (operator can register the URL by hand from the
            # response). Logged loudly.
            log.error("chatwoot webhook auto-registration failed",
                      error=str(exc), tenant_id=tenant_id)

    row = await st.store.upsert_tenant(
        tenant_id,
        account_id=body.account_id,
        inbox_id=body.inbox_id,
        api_access_token=body.api_access_token,
        webhook_token=webhook_token,
        webhook_id=webhook_id,
        hmac_secret=body.hmac_secret,
    )
    log.info("chatwoot tenant registered", tenant_id=tenant_id,
             webhook_registered=registered)
    out = _public_mapping(row, st.cfg.bridge_public_base_url)
    out["webhook_registered"] = registered
    return out


@router.get("/internal/tenants/{tenant_id}")
async def get_tenant(tenant_id: uuid.UUID, request: Request) -> dict[str, Any]:
    _require_internal_token(request)
    st = request.app.state
    row = await st.store.get_tenant(str(tenant_id))
    if row is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "tenant not registered")
    return _public_mapping(row, st.cfg.bridge_public_base_url)


@router.delete("/internal/tenants/{tenant_id}")
async def delete_tenant(tenant_id: uuid.UUID, request: Request) -> dict[str, Any]:
    _require_internal_token(request)
    st = request.app.state
    row = await st.store.get_tenant(str(tenant_id))
    if row is None:
        raise HTTPException(status.HTTP_404_NOT_FOUND, "tenant not registered")
    await st.store.delete_tenant(str(tenant_id))
    log.info("chatwoot tenant deregistered", tenant_id=str(tenant_id))
    return {"status": "deleted", "tenant_id": str(tenant_id)}
