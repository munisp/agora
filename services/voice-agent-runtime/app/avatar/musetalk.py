"""Open lip-sync avatar provider: MuseTalk via the avatar-renderer sidecar
(SPEC-W9 Part A / A1).

This is the self-hosted path — no hosted API call. The actual audio ->
lip-synced-frame inference runs in the ``services/avatar-renderer`` sidecar
(GPU workload), which discovers ``site-{slug}`` rooms via the LiveKit
RoomService API and publishes a video track into them.

This provider is the *intent* half: when ``MUSETALK_ROOM_AGENT=true`` and
``AVATAR_RENDERER=enabled`` it publishes the join intent (structured log the
sidecar/operators key on) and reports ``joining``; the sidecar does the
real work. Without the renderer flag it reports ``unavailable`` so the
session response is honest about the missing GPU path.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from ..logging import get_logger
from .base import AvatarStatus, register_provider

if TYPE_CHECKING:  # pragma: no cover - typing only
    from ..config import Settings

log = get_logger("avatar.musetalk")


def _flag(value: Any) -> bool:
    return str(value).strip().lower() in ("1", "true", "yes", "on", "enabled")


class MuseTalkProvider:
    """Publishes join intent for the avatar-renderer sidecar."""

    name = "musetalk"

    def __init__(self, settings: Settings):
        self._settings = settings

    def check_ready(self) -> str | None:
        if not _flag(getattr(self._settings, "musetalk_room_agent", False)):
            return "MUSETALK_ROOM_AGENT is not enabled"
        if not _flag(getattr(self._settings, "avatar_renderer", "")):
            return "AVATAR_RENDERER is not enabled (avatar-renderer sidecar off)"
        return None

    async def join_room(self, room: str, *, tenant_ctx: Any = None) -> AvatarStatus:
        """Publish the join intent for ``room``. Never raises: the only
        failure mode is misconfiguration, which check_ready reports."""
        not_ready = self.check_ready()
        if not_ready:
            log.warning("musetalk avatar misconfigured", detail=not_ready, room=room)
            return AvatarStatus(provider=self.name, status="unavailable", detail=not_ready)
        # Intent channel: the renderer sidecar polls LiveKit RoomService for
        # active `site-*` rooms and joins them; this log line is the operator
        # audit trail tying the intent to the session. (Room discovery keeps
        # the voice runtime free of renderer RPC — see services/avatar-renderer.)
        log.info(
            "musetalk renderer intent published",
            room=room,
            mode=getattr(self._settings, "avatar_renderer_mode", "mock"),
        )
        return AvatarStatus(provider=self.name, status="joining")


register_provider(MuseTalkProvider.name, MuseTalkProvider)

