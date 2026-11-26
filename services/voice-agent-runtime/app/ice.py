"""ICE servers (STUN/TURN) for browsers joining LiveKit rooms.

Where the LiveKit server can't take inbound UDP (behind NAT with no port forward, e.g. a home router or a
Cloudflare-Tunnel-only host), browsers relay media through a TURN service. This mints short-lived Cloudflare
Realtime TURN credentials, one set per session, so nothing long-lived is ever handed to a browser:

    POST https://rtc.live.cloudflare.com/v1/turn/keys/{CF_TURN_KEY_ID}/credentials/generate-ice-servers
         Authorization: Bearer {CF_TURN_API_TOKEN}   {"ttl": seconds}

Unset keys -> None: the client keeps its default ICE configuration (direct UDP to LiveKit).
"""
from __future__ import annotations

from typing import Any

import httpx

from .logging import get_logger

log = get_logger("ice")

CF_TURN_ENDPOINT = "https://rtc.live.cloudflare.com/v1/turn/keys/{key_id}/credentials/generate-ice-servers"


def _usable(url: str) -> bool:
    # Cloudflare also lists :53 alternates; browsers block port 53, so offering them only slows ICE down.
    return ":53?" not in url and not url.endswith(":53")


def normalize_ice_servers(payload: Any) -> list[dict[str, Any]]:
    """Cloudflare answers {"iceServers": [...]} (or one object): the RTCIceServer list a browser takes."""
    raw = payload.get("iceServers") if isinstance(payload, dict) else None
    entries = raw if isinstance(raw, list) else [raw] if isinstance(raw, dict) else []
    out: list[dict[str, Any]] = []
    for entry in entries:
        if not isinstance(entry, dict):
            continue
        urls = entry.get("urls")
        urls = [urls] if isinstance(urls, str) else [u for u in (urls or []) if isinstance(u, str)]
        urls = [u for u in urls if _usable(u)]
        if not urls:
            continue
        server: dict[str, Any] = {"urls": urls}
        if entry.get("username"):
            server["username"] = str(entry["username"])
        if entry.get("credential"):
            server["credential"] = str(entry["credential"])
        out.append(server)
    return out


class IceServerSource:
    def __init__(
        self,
        key_id: str = "",
        api_token: str = "",
        *,
        ttl_s: int = 14400,
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self.key_id = key_id.strip()
        self.api_token = api_token.strip()
        self.ttl_s = ttl_s
        self._client = client

    @property
    def configured(self) -> bool:
        return bool(self.key_id and self.api_token)

    async def get(self) -> list[dict[str, Any]] | None:
        """Fresh credentials for one session, or None (not configured / TURN unavailable: the call still tries
        direct connectivity rather than failing)."""
        if not self.configured:
            return None
        client = self._client or httpx.AsyncClient(timeout=httpx.Timeout(10.0))
        try:
            resp = await client.post(
                CF_TURN_ENDPOINT.format(key_id=self.key_id),
                headers={"Authorization": f"Bearer {self.api_token}"},
                json={"ttl": self.ttl_s},
            )
            resp.raise_for_status()
            servers = normalize_ice_servers(resp.json())
            return servers or None
        except Exception as exc:  # noqa: BLE001 - a TURN outage must not break session creation
            log.warning("turn credentials unavailable", error=str(exc)[:200] or type(exc).__name__)
            return None
        finally:
            if self._client is None:
                await client.aclose()
