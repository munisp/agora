"""Per-session TURN credentials for browsers (app/ice.py)."""
import json

import httpx

from app.ice import IceServerSource, normalize_ice_servers


def test_normalize_keeps_browser_usable_urls():
    payload = {"iceServers": [
        {"urls": ["stun:stun.cloudflare.com:3478", "stun:stun.cloudflare.com:53"]},
        {"urls": ["turn:turn.cloudflare.com:3478?transport=udp", "turn:turn.cloudflare.com:53?transport=udp",
                  "turns:turn.cloudflare.com:443?transport=tcp"], "username": "u", "credential": "c"},
    ]}
    assert normalize_ice_servers(payload) == [
        {"urls": ["stun:stun.cloudflare.com:3478"]},
        {"urls": ["turn:turn.cloudflare.com:3478?transport=udp", "turns:turn.cloudflare.com:443?transport=tcp"],
         "username": "u", "credential": "c"},
    ]
    # Older single-object shape.
    assert normalize_ice_servers({"iceServers": {"urls": "turn:x:3478", "username": "u", "credential": "c"}}) == [
        {"urls": ["turn:x:3478"], "username": "u", "credential": "c"}]
    assert normalize_ice_servers({}) == []


async def test_source_mints_per_session_and_fails_soft():
    seen = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append((request.url.path, request.headers["authorization"], json.loads(request.content)))
        return httpx.Response(201, json={"iceServers": [{"urls": ["turn:t:3478"], "username": "u", "credential": "c"}]})

    src = IceServerSource("key1", "tok", ttl_s=600, client=httpx.AsyncClient(transport=httpx.MockTransport(handler)))
    assert await src.get() == [{"urls": ["turn:t:3478"], "username": "u", "credential": "c"}]
    assert seen == [("/v1/turn/keys/key1/credentials/generate-ice-servers", "Bearer tok", {"ttl": 600})]

    assert await IceServerSource("", "").get() is None  # not configured
    down = IceServerSource("k", "t", client=httpx.AsyncClient(transport=httpx.MockTransport(lambda r: httpx.Response(503))))
    assert await down.get() is None  # TURN outage: session still created, client tries direct
