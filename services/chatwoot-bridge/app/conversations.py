"""conversation-service internal-surface client (SPEC-W118 §3).

Calls the X-Internal-Token routes added by W118 — never the public
gateway-bound surface. Fail-closed at construction when the token is unset
(mirroring the server-side K19 rule): better a dead bridge than an
unauthenticated one.
"""

from __future__ import annotations

from typing import Any

import httpx

from .logging import get_logger

log = get_logger(__name__)


class ConversationServiceError(Exception):
    def __init__(self, status: int, body: str):
        super().__init__(f"conversation-service error {status}: {body[:200]}")
        self.status = status
        self.body = body


class ConversationNotFound(Exception):
    pass


class ConversationServiceClient:
    def __init__(
        self,
        base_url: str,
        *,
        token: str,
        timeout: float = 10.0,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> None:
        if not token:
            raise ConversationServiceError(
                0, "CHATWOOT_CONVERSATION_INTERNAL_TOKEN unset"
            )
        self._http = httpx.AsyncClient(
            base_url=base_url.rstrip("/"),
            timeout=timeout,
            headers={"X-Internal-Token": token},
            transport=transport,
        )

    async def aclose(self) -> None:
        await self._http.aclose()

    async def get_conversation(
        self, conversation_id: str, tenant_id: str
    ) -> dict[str, Any]:
        resp = await self._http.get(
            f"/internal/conversations/{conversation_id}",
            params={"tenant_id": tenant_id},
        )
        if resp.status_code == 404:
            raise ConversationNotFound(conversation_id)
        if resp.status_code >= 300:
            raise ConversationServiceError(resp.status_code, resp.text)
        return resp.json()

    async def append_agent_turn(
        self,
        conversation_id: str,
        tenant_id: str,
        *,
        text: str,
        idempotency_key: str,
    ) -> dict[str, Any]:
        resp = await self._http.post(
            f"/internal/conversations/{conversation_id}/turns",
            json={
                "tenant_id": tenant_id,
                "role": "agent",
                "text": text,
                "idempotency_key": idempotency_key,
            },
        )
        if resp.status_code == 404:
            raise ConversationNotFound(conversation_id)
        if resp.status_code >= 300:
            raise ConversationServiceError(resp.status_code, resp.text)
        return resp.json()
