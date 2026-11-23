"""Conversation transcripts for the business: every web chat and voice call is stored in conversation-service
(POST /v1/conversations once per session, then POST /v1/conversations/{id}/turns per message), so the dashboard can
show what customers asked and what the receptionist answered.

Before this, the runtime only published SessionStarted/SessionEnded events; no conversation or turn was ever written,
and the business had no record of its chats or calls.

- Fire-and-forget: recording never delays or fails a reply (errors are logged).
- Order is kept per session (one lock per session key; turns are posted in sequence).
- Tenant scoping: in-cluster callers name the tenant in X-Tenant-Slugs (the gateway strips client copies at the edge),
  the same contract knowledge-service uses; conversation-service binds the body tenant_id to it.
"""
from __future__ import annotations

import asyncio
import json
import uuid
from collections import OrderedDict
from typing import Any

import httpx

from .logging import get_logger

log = get_logger("transcripts")

# Runtime channel names -> conversation-service channels (voice, chat, phone, api, ussd).
_CHANNELS = {"web": "chat", "chat": "chat", "whatsapp": "chat", "telegram": "chat", "sms": "chat",
             "voice": "voice", "phone": "phone", "sip": "phone", "ussd": "ussd", "api": "api"}


def conversation_channel(channel: str | None) -> str:
    return _CHANNELS.get((channel or "").lower(), "chat")


def tool_turn_text(entry: dict[str, Any]) -> str:
    """A readable line for a tool call, e.g. 'book_appointment(offering: Claim Assessment Visit, start: ...)'."""
    name = str(entry.get("tool") or "tool")
    args = entry.get("arguments")
    if isinstance(args, str):
        try:
            args = json.loads(args)
        except ValueError:
            args = {"input": args}
    if isinstance(args, dict) and args:
        shown = ", ".join(f"{k}: {v}" for k, v in list(args.items())[:6])
        return f"{name}({shown})"[:500]
    return f"{name}()"


class TranscriptRecorder:
    def __init__(self, base_url: str, *, timeout_s: float = 10.0, client: httpx.AsyncClient | None = None,
                 max_sessions: int = 5000) -> None:
        self.base_url = (base_url or "").rstrip("/")
        self._client = client
        self._timeout = timeout_s
        self._ids: OrderedDict[str, str] = OrderedDict()  # runtime session key -> stored conversation id
        self._locks: dict[str, asyncio.Lock] = {}
        self._max = max_sessions
        self._tasks: set[asyncio.Task] = set()

    @property
    def enabled(self) -> bool:
        return bool(self.base_url)

    def _http(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = httpx.AsyncClient(timeout=httpx.Timeout(self._timeout))
        return self._client

    async def aclose(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    def stored_id(self, key: str) -> str | None:
        """The conversation-service id this session's turns are stored under (None before the first turn)."""
        return self._ids.get(key)

    def _remember(self, key: str, conv_id: str) -> None:
        self._ids[key] = conv_id
        self._ids.move_to_end(key)
        while len(self._ids) > self._max:
            old, _ = self._ids.popitem(last=False)
            self._locks.pop(old, None)

    async def _ensure(self, key: str, *, tenant_id: str, site_slug: str, channel: str, contact_phone: str | None) -> str:
        conv_id = self._ids.get(key)
        if conv_id:
            return conv_id
        resp = await self._http().post(
            f"{self.base_url}/v1/conversations",
            headers={"X-Tenant-Slugs": tenant_id},
            json={"tenant_id": tenant_id, "site_slug": site_slug, "channel": conversation_channel(channel),
                  "contact_phone": contact_phone or None},
        )
        resp.raise_for_status()
        conv_id = str(resp.json()["id"])
        self._remember(key, conv_id)
        return conv_id

    async def conversation(self, conv_id: str, tenant_id: str) -> dict[str, Any] | None:
        """The stored conversation with its turns (None when unknown or the service is unreachable)."""
        if not self.enabled or not conv_id or not tenant_id:
            return None
        try:
            resp = await self._http().get(f"{self.base_url}/v1/conversations/{conv_id}",
                                          headers={"X-Tenant-ID": tenant_id, "X-Tenant-Slugs": tenant_id})
            if resp.status_code != 200:
                return None
            return resp.json()
        except Exception as exc:  # noqa: BLE001 - fail-open: the receptionist keeps answering
            log.warning("stored conversation not read", conversation_id=conv_id, error=str(exc)[:200])
            return None

    async def handoff_active(self, conv_id: str, tenant_id: str) -> bool:
        """2026-11-23: True while a person owns the conversation in the human inbox (call_details.handoff.active)."""
        conv = await self.conversation(conv_id, tenant_id)
        handoff = ((conv or {}).get("call_details") or {}).get("handoff") or {}
        return bool(isinstance(handoff, dict) and handoff.get("active"))

    async def add_turns(self, key: str, turns: list[dict[str, Any]], *, tenant_id: str, site_slug: str,
                        channel: str, contact_phone: str | None = None) -> str | None:
        """Append turns ({role, text, tool_calls?}) to the session's stored conversation, creating it first."""
        if not self.enabled or not tenant_id or not turns:
            return None
        lock = self._locks.setdefault(key, asyncio.Lock())
        async with lock:
            conv_id = await self._ensure(key, tenant_id=tenant_id, site_slug=site_slug, channel=channel,
                                         contact_phone=contact_phone)
            for turn in turns:
                text = str(turn.get("text") or "").strip()
                if not text:
                    continue
                body: dict[str, Any] = {"role": turn["role"], "text": text}
                if turn.get("tool_calls"):
                    body["tool_calls"] = turn["tool_calls"]
                if turn.get("translation_en"):
                    body["translation_en"] = turn["translation_en"]
                resp = await self._http().post(
                    f"{self.base_url}/v1/conversations/{conv_id}/turns",
                    headers={"X-Tenant-Slugs": tenant_id, "Idempotency-Key": str(uuid.uuid4())},
                    json=body,
                )
                resp.raise_for_status()
            return conv_id

    def record(self, key: str, turns: list[dict[str, Any]], **kw: Any) -> None:
        """Fire-and-forget add_turns: never blocks or fails the caller."""
        if not self.enabled:
            return

        async def _run() -> None:
            try:
                await self.add_turns(key, turns, **kw)
            except Exception as exc:  # noqa: BLE001 - a transcript outage must not break a conversation
                log.warning("transcript not recorded", session=key, error=str(exc)[:200])

        try:
            task = asyncio.get_running_loop().create_task(_run())
        except RuntimeError:
            return
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)


def exchange_turns(message: str, reply: str, trace: list[dict[str, Any]] | None) -> list[dict[str, Any]]:
    """One chat exchange as turns: the customer's message, any tool calls, the receptionist's reply."""
    turns: list[dict[str, Any]] = [{"role": "user", "text": message}]
    for entry in trace or []:
        turns.append({"role": "tool", "text": tool_turn_text(entry),
                      "tool_calls": [{"tool": entry.get("tool"), "arguments": entry.get("arguments")}]})
    turns.append({"role": "agent", "text": reply})
    return turns
