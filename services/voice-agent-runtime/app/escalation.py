"""Warm handoff (SPEC-W3 §4, innovation 1).

`request_human` escalates a conversation to a human operator:

1. Creates a LiveKit room ``escalation-{conversation_id}`` via the
   livekit-api server SDK and mints a staff join token.
2. Publishes an ``EscalationRequested`` CloudEvent to
   ``opendesk.conversation.events`` via Dapr (consumed by the edge/dashboard
   for the staff escalation banner).
3. Whisper-copilot mode: after the handoff the agent keeps generating
   suggested replies, posted to the escalation room data channel so the
   operator sees live drafts while talking to the caller.

SPEC-W45 K15(e): the staff join token is NEVER published on the events
topic (fan-out = any consumer could join the escalation room and listen to
the caller). Delivery is staff-participant-only:

1. On-demand mint via the internal control-plane endpoint
   ``POST /voice-admin/escalations/{conversation_id}/staff-token``
   (X-Internal-Token, app/control_plane.py) — the staff dashboard calls it
   when an operator picks up the escalation banner; or
2. ``deliver_staff_token`` — a targeted LiveKit data message
   (``destination_identities=[staff_identity]``) pushed into the escalation
   room once the staff participant has joined; other participants
   (including the caller) never receive it.

Everything degrades gracefully: when the LiveKit server is unreachable the
event is still published (the dashboard banner + room name are valid) and
copilot posts are skipped with a warning — the caller never hears an error.
"""

from __future__ import annotations

from datetime import timedelta
from typing import Any

from .config import Settings
from .logging import get_logger

log = get_logger("escalation")


def escalation_room_name(conversation_id: str) -> str:
    return f"escalation-{conversation_id}"


def _http_url(ws_url: str) -> str:
    """livekit-api (Twirp/HTTP) needs an http(s) URL; LIVEKIT_URL is ws(s)."""
    if ws_url.startswith("wss://"):
        return "https://" + ws_url[len("wss://"):]
    if ws_url.startswith("ws://"):
        return "http://" + ws_url[len("ws://"):]
    return ws_url


class LiveKitEscalation:
    """Thin wrapper over livekit-api for escalation rooms.

    The livekit imports are deferred so the module (and its tests) import
    cleanly without the optional server SDK installed.
    """

    def __init__(self, settings: Settings) -> None:
        self._settings = settings

    def _api(self) -> Any:
        from livekit import api as lk_api  # deferred import (optional dep)

        return lk_api.LiveKitAPI(
            _http_url(self._settings.livekit_url),
            self._settings.livekit_api_key,
            self._settings.livekit_api_secret,
        )

    async def create_room(self, room: str) -> bool:
        """Create the escalation room. Returns False (graceful) on failure."""
        from livekit import api as lk_api

        try:
            api = self._api()
            try:
                await api.room.create_room(
                    lk_api.CreateRoomRequest(name=room, empty_timeout=1800)
                )
            finally:
                await api.aclose()
            log.info("escalation room created", room=room)
            return True
        except Exception as exc:  # noqa: BLE001 - degrade gracefully
            log.warning("escalation room creation failed", room=room, error=str(exc))
            return False

    def staff_join_token(self, room: str, *, staff_name: str = "Staff") -> str:
        """Mint a staff join token for the escalation room (offline JWT).

        K15(e): call sites must deliver the returned token to the staff
        participant ONLY (internal mint endpoint or targeted in-room data) —
        never publish it on an event topic."""
        from livekit import api as lk_api

        return (
            lk_api.AccessToken(
                self._settings.livekit_api_key, self._settings.livekit_api_secret
            )
            .with_identity(f"staff-{room[:24]}")
            .with_name(staff_name)
            .with_grants(lk_api.VideoGrants(room_join=True, room=room))
            .with_ttl(timedelta(hours=2))
            .to_jwt()
        )

    async def deliver_staff_token(
        self,
        room: str,
        staff_identity: str,
        *,
        token: str | None = None,
        staff_name: str = "Staff",
    ) -> bool:
        """K15(e): push the staff join token INTO the escalation room,
        addressed to the staff participant only
        (``SendDataRequest.destination_identities``) — the caller and any
        other room participants never receive it.

        Used when the staff participant has already joined (e.g. they
        entered via the dashboard-minted token and need a refresh, or a
        supervisor joins the whisper-copilot session). Returns False
        (graceful) when LiveKit is unreachable or the identity is absent.
        """
        import json

        from livekit import api as lk_api
        from livekit.protocol import room as lk_room

        token = token or self.staff_join_token(room, staff_name=staff_name)
        try:
            api = self._api()
            try:
                await api.room.send_data(
                    lk_api.SendDataRequest(
                        room=room,
                        data=json.dumps(
                            {
                                "type": "staff_join_token",
                                "room": room,
                                "token": token,
                            }
                        ).encode(),
                        kind=lk_room.DataPacket.Kind.Value("RELIABLE"),
                        topic="staff-credentials",
                        destination_identities=[staff_identity],
                    )
                )
            finally:
                await api.aclose()
            log.info(
                "staff token delivered in-room", room=room, identity=staff_identity
            )
            return True
        except Exception as exc:  # noqa: BLE001 - best-effort
            log.warning(
                "staff token delivery failed", room=room, error=str(exc)[:200]
            )
            return False

    async def post_suggestion(self, room: str, suggestion: dict[str, Any]) -> bool:
        """Publish a copilot suggested reply to the room data channel.

        Returns False (graceful) when LiveKit is unreachable — copilot
        suggestions are best-effort by design.
        """
        import json

        from livekit import api as lk_api
        from livekit.protocol import room as lk_room

        try:
            api = self._api()
            try:
                await api.room.send_data(
                    lk_api.SendDataRequest(
                        room=room,
                        data=json.dumps(suggestion).encode(),
                        kind=lk_room.DataPacket.Kind.Value("RELIABLE"),
                        topic="copilot",
                    )
                )
            finally:
                await api.aclose()
            return True
        except Exception as exc:  # noqa: BLE001 - best-effort
            log.warning("copilot suggestion post failed", room=room, error=str(exc))
            return False
