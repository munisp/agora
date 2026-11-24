"""Async Chatwoot Application-API client (SPEC-W118 §4).

Auth: `api_access_token` request header (per-tenant token stored in the
mapping). Only the surfaces the bridge needs:

- validate_inbox        GET  /api/v1/accounts/{aid}/inboxes
- create_contact        POST /api/v1/accounts/{aid}/contacts (inbox_id ->
                        auto contact_inbox; 422 -> search-by-identifier +
                        POST /contacts/{id}/contact_inboxes fallback)
- create_conversation   POST /api/v1/accounts/{aid}/conversations
- create_message        POST /api/v1/accounts/{aid}/conversations/{cid}/messages
- list_webhooks         GET  /api/v1/accounts/{aid}/webhooks
- create_webhook        POST /api/v1/accounts/{aid}/webhooks

Every non-2xx is a ChatwootError(status, body) — callers decide retry
semantics; this client never swallows failures.
"""

from __future__ import annotations

from typing import Any

import httpx

from .logging import get_logger

log = get_logger(__name__)


class ChatwootError(Exception):
    def __init__(self, status: int, body: str):
        super().__init__(f"chatwoot API error {status}: {body[:200]}")
        self.status = status
        self.body = body


class ChatwootClient:
    def __init__(
        self,
        base_url: str,
        *,
        timeout: float = 10.0,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> None:
        # `transport` is the test seam (httpx.MockTransport); production
        # never passes it.
        self._http = httpx.AsyncClient(
            base_url=base_url.rstrip("/"), timeout=timeout, transport=transport
        )

    async def aclose(self) -> None:
        await self._http.aclose()

    @staticmethod
    def _headers(token: str) -> dict[str, str]:
        return {"api_access_token": token}

    async def _request(
        self, method: str, path: str, token: str, **kwargs: Any
    ) -> httpx.Response:
        resp = await self._http.request(
            method, path, headers=self._headers(token), **kwargs
        )
        if resp.status_code >= 300:
            raise ChatwootError(resp.status_code, resp.text)
        return resp

    @staticmethod
    def _payload(data: Any) -> Any:
        """Chatwoot index endpoints wrap lists in {"payload": [...]}; object
        endpoints return the object bare. Accept both."""
        if isinstance(data, dict) and "payload" in data:
            return data["payload"]
        return data

    async def validate_inbox(
        self, token: str, account_id: int, inbox_id: int
    ) -> bool:
        """True when the credential can list the account's inboxes AND
        inbox_id is among them (registration-time live validation)."""
        resp = await self._request("GET", f"/api/v1/accounts/{account_id}/inboxes", token)
        inboxes = self._payload(resp.json()) or []
        return any(int(i.get("id", -1)) == int(inbox_id) for i in inboxes)

    async def create_contact(
        self,
        token: str,
        account_id: int,
        *,
        inbox_id: int,
        name: str,
        identifier: str,
        phone_number: str | None = None,
    ) -> tuple[int, str]:
        """Find-or-create a contact bound to the API inbox; returns
        (contact_id, contact_inbox source_id).

        422 (duplicate identifier) -> search by identifier, then create the
        contact_inbox explicitly (documented API-channel flow).
        """
        body: dict[str, Any] = {
            "inbox_id": inbox_id,
            "name": name,
            "identifier": identifier,
        }
        if phone_number:
            body["phone_number"] = phone_number
        try:
            resp = await self._request(
                "POST", f"/api/v1/accounts/{account_id}/contacts", token, json=body
            )
            data = self._payload(resp.json())
            return int(data["id"]), self._source_id(data, inbox_id)
        except ChatwootError as exc:
            if exc.status not in (409, 422):
                raise
        # Duplicate: locate by identifier and bind to the inbox.
        resp = await self._request(
            "GET",
            f"/api/v1/accounts/{account_id}/contacts/search",
            token,
            params={"q": identifier},
        )
        matches = self._payload(resp.json()) or []
        contact = next(
            (c for c in matches if c.get("identifier") == identifier), None
        )
        if contact is None:
            raise ChatwootError(
                422, f"contact create conflicted but identifier {identifier!r} not found"
            )
        source_id = await self.create_contact_inbox(
            token, account_id, contact_id=int(contact["id"]), inbox_id=inbox_id
        )
        return int(contact["id"]), source_id

    @staticmethod
    def _source_id(contact_data: dict[str, Any], inbox_id: int) -> str:
        for ci in contact_data.get("contact_inboxes") or []:
            inbox = ci.get("inbox") or {}
            if int(inbox.get("id", inbox_id)) == int(inbox_id) and ci.get("source_id"):
                return str(ci["source_id"])
        raise ChatwootError(502, "contact response lacks contact_inboxes source_id")

    async def create_contact_inbox(
        self, token: str, account_id: int, *, contact_id: int, inbox_id: int
    ) -> str:
        resp = await self._request(
            "POST",
            f"/api/v1/accounts/{account_id}/contacts/{contact_id}/contact_inboxes",
            token,
            json={"inbox_id": inbox_id},
        )
        data = self._payload(resp.json())
        source_id = data.get("source_id") if isinstance(data, dict) else None
        if not source_id:
            raise ChatwootError(502, "contact_inbox response lacks source_id")
        return str(source_id)

    async def create_conversation(
        self,
        token: str,
        account_id: int,
        *,
        source_id: str,
        inbox_id: int,
        opendesk_conversation_id: str,
    ) -> int:
        resp = await self._request(
            "POST",
            f"/api/v1/accounts/{account_id}/conversations",
            token,
            json={
                "source_id": source_id,
                "inbox_id": inbox_id,
                "status": "open",
                "additional_attributes": {
                    "opendesk_conversation_id": opendesk_conversation_id
                },
            },
        )
        data = self._payload(resp.json())
        return int(data["id"])

    async def create_message(
        self,
        token: str,
        account_id: int,
        conversation_id: int,
        *,
        content: str,
        message_type: str,
        source_id: str,
    ) -> dict[str, Any]:
        resp = await self._request(
            "POST",
            f"/api/v1/accounts/{account_id}/conversations/{conversation_id}/messages",
            token,
            json={
                "content": content,
                "message_type": message_type,
                "private": False,
                "source_id": source_id,
            },
        )
        return self._payload(resp.json())

    async def list_webhooks(self, token: str, account_id: int) -> list[dict[str, Any]]:
        resp = await self._request(
            "GET", f"/api/v1/accounts/{account_id}/webhooks", token
        )
        return self._payload(resp.json()) or []

    async def create_webhook(
        self,
        token: str,
        account_id: int,
        *,
        url: str,
        subscriptions: list[str],
        name: str = "OpenDesk bridge",
    ) -> int:
        resp = await self._request(
            "POST",
            f"/api/v1/accounts/{account_id}/webhooks",
            token,
            json={"url": url, "subscriptions": subscriptions, "name": name},
        )
        data = self._payload(resp.json())
        return int(data["id"])
