"""Optional ElevenLabs ConvAI backend (SPEC §11: adapter pattern).

Selected with AGENT_BACKEND=elevenlabs. Shows the abstraction boundary: the
same ToolLayer that backs the open-source LiveKit stack is reused; only the
voice transport/orchestration is outsourced.

- `get_signed_url`: server-side minting of a ConvAI websocket signed URL so
  the browser client can connect without exposing the API key.
- `tool_webhook`: ElevenLabs "client tools"/server webhooks passthrough —
  tool calls from the hosted agent are dispatched through our ToolLayer so
  the phone-confirmation policy and Dapr command flow stay identical.
"""

from __future__ import annotations

from typing import Any

import httpx

from .config import Settings
from .dapr_client import DaprClient
from .logging import get_logger
from .session_state import SessionStore
from .tenant_context import fetch_tenant_context
from .tools import ToolLayer

log = get_logger("elevenlabs")

SIGNED_URL_ENDPOINT = "https://api.elevenlabs.io/v1/convai/conversation/get-signed-url"


class ElevenLabsBackend:
    def __init__(
        self, *, settings: Settings, dapr: DaprClient, sessions: SessionStore
    ) -> None:
        self._settings = settings
        self._dapr = dapr
        self._sessions = sessions
        self._http = httpx.AsyncClient(timeout=httpx.Timeout(settings.http_timeout_s))

    async def aclose(self) -> None:
        await self._http.aclose()

    async def get_signed_url(self) -> dict[str, Any]:
        """Mint a ConvAI signed URL for the configured agent."""
        if not self._settings.elevenlabs_api_key or not self._settings.elevenlabs_agent_id:
            raise RuntimeError(
                "ELEVENLABS_API_KEY and ELEVENLABS_AGENT_ID are required for AGENT_BACKEND=elevenlabs"
            )
        resp = await self._http.get(
            SIGNED_URL_ENDPOINT,
            params={"agent_id": self._settings.elevenlabs_agent_id},
            headers={"xi-api-key": self._settings.elevenlabs_api_key},
        )
        resp.raise_for_status()
        return resp.json()

    async def handle_tool_webhook(self, payload: dict[str, Any]) -> dict[str, Any]:
        """Dispatch an ElevenLabs tool webhook call through the ToolLayer.

        Expected payload (ElevenLabs server-tool webhook):
        {"tool_name": str, "parameters": {...}, "conversation_id": str,
         "site_slug": str, "session_secret": str?}

        K15(b): resuming session state across webhook calls requires the
        session_secret — the response always carries the current
        conversation_id + session_secret so the ElevenLabs agent can round
        -trip them (dynamic variables / client tools). A conversation_id
        alone NEVER resumes confirmed/verified phone, history or escalation
        state; without the secret each call runs on a fresh session.
        """
        tool_name = payload.get("tool_name") or payload.get("name") or ""
        parameters = payload.get("parameters") or payload.get("args") or {}
        site_slug = payload.get("site_slug") or ""
        conversation_id = payload.get("conversation_id")
        session_secret = payload.get("session_secret")

        session = self._sessions.get_or_create(
            conversation_id, site_slug, session_secret=session_secret
        )
        ctx = await fetch_tenant_context(self._dapr, self._settings, site_slug)
        tool_layer = ToolLayer(
            dapr=self._dapr, settings=self._settings, ctx=ctx, session=session
        )
        result = await tool_layer.dispatch(tool_name, parameters)
        # Round-trip the resume credentials (K15(b)) — additive keys.
        result.setdefault("conversation_id", session.conversation_id)
        result.setdefault("session_secret", session.session_secret)
        log.info("elevenlabs tool dispatched", tool=tool_name, status=result.get("status"))
        return result
