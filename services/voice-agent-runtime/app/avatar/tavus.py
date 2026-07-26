"""Tavus CVI avatar provider (SPEC-W9 Part A / A1).

On session start we ask the Tavus Conversational Video Interface (CVI) API
to spawn a conversation whose replica joins OUR LiveKit room (the room the
browser token was just minted for). The Tavus participant then publishes a
lip-synced video track into the room; the web client renders any remote
video track as the avatar tile (apps/admin-web voice-session-button /
call-client).

Request shape is isolated behind ``build_conversation_payload`` below so a
Tavus API correction is a one-function change. Doc reference (checked for
Wave 9): https://docs.tavus.io/api-reference/conversations/create-conversation
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

import httpx

from ..logging import get_logger
from .base import AvatarStatus, register_provider

if TYPE_CHECKING:  # pragma: no cover - typing only
    from ..config import Settings

log = get_logger("avatar.tavus")

# Tavus CVI REST endpoint (api.tavus.io; the newer tavusapi.com host serves
# the same contract — one constant to flip if Tavus migrates).
TAVUS_CONVERSATIONS_URL = "https://api.tavus.io/v2/conversations"

# Hard ceiling on the Tavus API call. join_room runs as a fire-and-forget
# background task, but we still cap it so a hung Tavus edge can't leak
# tasks (SPEC-W9 A1: 10s timeout, never blocks session creation).
TAVUS_TIMEOUT_S = 10.0


def build_conversation_payload(
    *, replica_id: str, persona_id: str, room: str
) -> dict[str, Any]:
    """POST body for the Tavus "create conversation" endpoint that makes the
    replica join an existing LiveKit room.

    Shape per SPEC-W9 A1 and the Tavus CVI docs
    (https://docs.tavus.io/api-reference/conversations/create-conversation):

        {replica_id, persona_id, properties: {livekit_room_name: <room>}}

    NOTE (flagged in docs/avatar.md): current Tavus docs steer LiveKit
    integrations at the LiveKit Agents plugin (`tavus.AvatarSession`) and
    renamed replica/persona -> face/PAL (the legacy `replica_id` /
    `persona_id` request fields remain accepted aliases). The
    `properties.livekit_room_name` join-an-existing-room property is the
    documented v2/conversations CVI shape for LiveKit rooms; if Tavus has
    retired it server-side, this is the ONLY function to change.
    """
    payload: dict[str, Any] = {
        "replica_id": replica_id,
        "properties": {"livekit_room_name": room},
    }
    if persona_id:
        # persona_id is optional on the Tavus side (stock personas exist);
        # only send it when configured.
        payload["persona_id"] = persona_id
    return payload


class TavusProvider:
    """POSTs the Tavus CVI API to bring a hosted avatar into the room."""

    name = "tavus"

    def __init__(self, settings: Settings, client: httpx.AsyncClient | None = None):
        self._settings = settings
        # Injectable for tests (httpx.MockTransport); created per-join and
        # closed afterwards when not injected.
        self._client = client

    def check_ready(self) -> str | None:
        if not getattr(self._settings, "tavus_api_key", ""):
            return "TAVUS_API_KEY is not set"
        if not getattr(self._settings, "tavus_replica_id", ""):
            return "TAVUS_REPLICA_ID is not set"
        return None

    async def join_room(self, room: str, *, tenant_ctx: Any = None) -> AvatarStatus:
        """Fire the Tavus conversation request. NEVER raises — every failure
        degrades to status unavailable + a warning log so the voice session
        proceeds audio-only."""
        not_ready = self.check_ready()
        if not_ready:
            log.warning("tavus avatar misconfigured", detail=not_ready, room=room)
            return AvatarStatus(provider=self.name, status="unavailable", detail=not_ready)

        payload = build_conversation_payload(
            replica_id=self._settings.tavus_replica_id,
            persona_id=getattr(self._settings, "tavus_persona_id", ""),
            room=room,
        )
        headers = {"x-api-key": self._settings.tavus_api_key}
        try:
            if self._client is not None:
                resp = await self._client.post(
                    TAVUS_CONVERSATIONS_URL, json=payload, headers=headers
                )
            else:
                async with httpx.AsyncClient(timeout=TAVUS_TIMEOUT_S) as client:
                    resp = await client.post(
                        TAVUS_CONVERSATIONS_URL, json=payload, headers=headers
                    )
        except Exception as exc:  # noqa: BLE001 - fire-and-forget by contract
            log.warning("tavus conversation request failed", room=room, error=str(exc))
            return AvatarStatus(
                provider=self.name, status="unavailable", detail=str(exc)
            )

        if resp.status_code >= 400:
            detail = f"tavus api http {resp.status_code}"
            log.warning(
                "tavus conversation rejected",
                room=room,
                status_code=resp.status_code,
                body=resp.text[:300],
            )
            return AvatarStatus(provider=self.name, status="unavailable", detail=detail)

        conversation_id = ""
        try:
            conversation_id = str(resp.json().get("conversation_id") or "")
        except Exception:  # noqa: BLE001 - id is nice-to-have only
            pass
        log.info(
            "tavus avatar joining room", room=room, conversation_id=conversation_id
        )
        return AvatarStatus(provider=self.name, status="joining")


register_provider(TavusProvider.name, TavusProvider)
