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

from . import ivr_menu
from .config import Settings
from .dapr_client import DaprClient
from .logging import get_logger
from .session_state import SessionStore, store_get_or_create, store_save_session
from .tenant_context import fetch_tenant_context
from .tools import ToolLayer

log = get_logger("elevenlabs")

SIGNED_URL_ENDPOINT = "https://api.elevenlabs.io/v1/convai/conversation/get-signed-url"
# SPEC-W93: hosted-agent config endpoint (GET + PATCH) used to provision
# native DTMF input.
AGENTS_ENDPOINT = "https://api.elevenlabs.io/v1/convai/agents/{agent_id}"

# ElevenLabs dtmf_input_settings bounds (their API rejects values outside
# 0.5..10.0 seconds).
DTMF_TIMEOUT_MIN_S = 0.5
DTMF_TIMEOUT_MAX_S = 10.0


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

    def desired_dtmf_settings(self) -> "dict[str, Any] | None":
        """The dtmf_input_settings the hosted agent SHOULD run (SPEC-W93).

        None = DTMF input disabled (their API shape: the field is null).
        Otherwise the full settings object, timeout clamped into the
        0.5..10.0s ElevenLabs range, redact_input defaulting ON so typed
        codes never persist in ElevenLabs stored transcripts.
        """
        s = self._settings
        if not s.elevenlabs_dtmf_enabled:
            return None
        timeout = s.elevenlabs_dtmf_timeout_s
        timeout = max(DTMF_TIMEOUT_MIN_S, min(DTMF_TIMEOUT_MAX_S, timeout))
        return {
            "dtmf_input_timeout": timeout,
            "hash_terminator": s.elevenlabs_dtmf_hash_terminator,
            "redact_input": s.elevenlabs_dtmf_redact,
        }

    async def sync_dtmf_input(self) -> dict[str, Any]:
        """Converge the hosted agent's DTMF input config (SPEC-W93).

        GET the agent, compare
        conversation_config.conversation.dtmf_input_settings with
        desired_dtmf_settings(); PATCH only on drift (in_sync performs NO
        write). Disabled + remote null = in_sync; disabled + remote set =
        explicit PATCH of null (no silent leftover config). Raises on HTTP
        errors; the control plane treats provisioning as best-effort.
        """
        if not self._settings.elevenlabs_api_key or not self._settings.elevenlabs_agent_id:
            raise RuntimeError(
                "ELEVENLABS_API_KEY and ELEVENLABS_AGENT_ID are required for AGENT_BACKEND=elevenlabs"
            )
        desired = self.desired_dtmf_settings()
        url = AGENTS_ENDPOINT.format(agent_id=self._settings.elevenlabs_agent_id)
        headers = {"xi-api-key": self._settings.elevenlabs_api_key}
        resp = await self._http.get(url, headers=headers)
        resp.raise_for_status()
        conversation = (resp.json().get("conversation_config") or {}).get(
            "conversation"
        ) or {}
        current = conversation.get("dtmf_input_settings")
        if current == desired:
            return {"status": "in_sync", "settings": desired}
        patch = await self._http.patch(
            url,
            headers=headers,
            json={
                "conversation_config": {
                    "conversation": {"dtmf_input_settings": desired}
                }
            },
        )
        patch.raise_for_status()
        log.info("elevenlabs dtmf settings synced", settings=desired)
        return {"status": "updated", "settings": desired}

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

        session = await store_get_or_create(
            self._sessions, conversation_id, site_slug, session_secret=session_secret
        )
        ctx = await fetch_tenant_context(self._dapr, self._settings, site_slug)
        tool_layer = ToolLayer(
            dapr=self._dapr,
            settings=self._settings,
            ctx=ctx,
            session=session,
            # SPEC-W93: the hosted path has no DtmfBuffer — ElevenLabs
            # collects keypad digits natively and relays them to the agent
            # as a user turn. read_keypad_entry reports keypad_native.
            native_keypad=self._settings.elevenlabs_dtmf_enabled,
            # SPEC-W96 IVR-A: the hosted agent learns the keypad menu from
            # the keypad_native tool answer (the menu sentence is
            # provisioned there because digits never transit this runtime
            # and the hosted greeting prompt lives outside the repo).
            ivr_menu_hint=(
                ivr_menu.native_menu_sentence(
                    ivr_menu.parse_ivr_menu(self._settings.ivr_menu)
                )
                if self._settings.ivr_menu_enabled
                else ""
            ),
        )
        result = await tool_layer.dispatch(tool_name, parameters)
        # SPEC-W97 G8: persist the session after each hosted tool call
        # (OTP verification/phone confirmation state survives restarts).
        # Fail-open — never breaks the webhook response.
        try:
            await store_save_session(self._sessions, session)
        except Exception as exc:  # noqa: BLE001 - persistence never breaks a turn
            log.warning("session persist failed", error=str(exc)[:200])
        # Round-trip the resume credentials (K15(b)) — additive keys.
        result.setdefault("conversation_id", session.conversation_id)
        result.setdefault("session_secret", session.session_secret)
        log.info("elevenlabs tool dispatched", tool=tool_name, status=result.get("status"))
        return result
