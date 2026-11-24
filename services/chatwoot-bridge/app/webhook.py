"""Inbound sync: Chatwoot webhook -> conversation-service agent turns
(SPEC-W118 §4).

Route: POST /v1/chatwoot/webhook/{tenant_id}/{token}

- Tenant scope comes from the PATH (RLS-safe: the mapping lookup runs with
  app.tenant_id set), the per-tenant token is constant-time compared, and
  an unknown tenant/token is a flat 404 (no oracle).
- When the mapping carries an hmac_secret, the X-Chatwoot-Signature header
  MUST verify (HMAC-SHA256 over the raw body) — 401 otherwise.
- PROCESS only human-agent replies: event "message_created" +
  message_type "outgoing" + not private + text content + source_id NOT in
  the sent registry (the echo of bridge-posted agent turns dies here) +
  the Chatwoot conversation reverse-maps to a link. Everything else is a
  200 "ignored_*" — never an error, so Chatwoot does not retry-loop.
  ("incoming" on an API channel can only be bridge-created — never
  appended.)
- The append uses Idempotency-Key
  "chatwoot-<account_id>-<cw_conversation_id>-<message_id>": Chatwoot
  delivery retries and duplicate webhooks are no-ops.
- conversation-service failure -> 503 so Chatwoot retries; permanent
  skips -> 200.
"""

from __future__ import annotations

import hashlib
import hmac
import json
import uuid
from typing import Any

from fastapi import APIRouter, HTTPException, Request, status

from .conversations import ConversationNotFound, ConversationServiceError
from .logging import get_logger

log = get_logger(__name__)

router = APIRouter()


@router.post("/v1/chatwoot/webhook/{tenant_id}/{token}")
async def chatwoot_webhook(
    tenant_id: uuid.UUID, token: str, request: Request
) -> dict[str, Any]:
    st = request.app.state
    mapping = await st.store.get_tenant(str(tenant_id))
    if mapping is None or not hmac.compare_digest(
        token, mapping["webhook_token"]
    ):
        raise HTTPException(status.HTTP_404_NOT_FOUND, "unknown webhook")

    body = await request.body()
    secret = mapping.get("hmac_secret")
    if secret:
        presented = request.headers.get("X-Chatwoot-Signature", "")
        expected = hmac.new(
            secret.encode(), body, hashlib.sha256
        ).hexdigest()
        if not presented or not hmac.compare_digest(presented, expected):
            raise HTTPException(
                status.HTTP_401_UNAUTHORIZED, "invalid webhook signature"
            )

    try:
        payload = json.loads(body) if body else {}
    except json.JSONDecodeError:
        raise HTTPException(
            status.HTTP_400_BAD_REQUEST, "invalid JSON body"
        ) from None

    try:
        outcome = await process_webhook_payload(st, mapping, payload)
    except ConversationNotFound:
        # The OpenDesk conversation vanished (privacy erasure / retention):
        # permanent — acknowledge so Chatwoot stops retrying.
        outcome = "ignored_conversation_gone"
    except ConversationServiceError as exc:
        log.error("turn append failed; asking chatwoot to retry",
                  error=str(exc))
        raise HTTPException(
            status.HTTP_503_SERVICE_UNAVAILABLE,
            "conversation-service unavailable",
        ) from None
    return {"status": outcome}


async def process_webhook_payload(
    st: Any, mapping: dict[str, Any], payload: dict[str, Any]
) -> str:
    """One webhook payload -> zero or one appended agent turn. Pure of
    transport (test-pinned outcomes)."""
    if payload.get("event") != "message_type":
        return "ignored_event"
    if payload.get("private"):
        return "ignored_private"
    if payload.get("content_type") not in (None, "text"):
        return "ignored_content_type"
    if payload.get("message_type") != "outgoing":
        return "ignored_direction"

    tenant_id = str(mapping["tenant_id"])
    source_id = payload.get("source_id")
    if source_id and await st.store.sent_lookup(tenant_id, str(source_id)):
        return "ignored_echo"

    conv = payload.get("conversation") or {}
    cw_conv_id = conv.get("id")
    content = (payload.get("content") or "").strip()
    if not content:
        return "ignored_empty"

    try:
        cw_conv_id = int(cw_conv_id)
    except (TypeError, ValueError):
        return "ignored_malformed"

    link = await st.store.get_link_by_chatwoot_conversation(tenant_id, cw_conv_id)
    if link is None:
        return "ignored_unlinked"

    msg_id = payload.get("id")
    idem = (
        f"chatwoot-{mapping['account_id']}-{cw_conv_id}-{msg_id}"
    )
    res = await st.conv_client.append_agent_turn(
        str(link["conversation_id"]),
        tenant_id,
        text=content,
        idempotency_key=idem,
    )
    return "appended" if res.get("created") else "duplicate"
