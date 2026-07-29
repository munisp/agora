"""Shared fixtures: stub the optional livekit modules so the escalation code
paths are testable without the server SDK installed."""

from __future__ import annotations

import sys
import types

import pytest

sys.path.insert(0, ".")


class _CreateRoomRequest:
    def __init__(self, name: str, empty_timeout: int = 0) -> None:
        self.name = name
        self.empty_timeout = empty_timeout


class _SendDataRequest:
    def __init__(self, room: str, data: bytes, kind=None, topic: str = "") -> None:
        self.room = room
        self.data = data
        self.kind = kind
        self.topic = topic


class _Kind:
    @staticmethod
    def Value(name: str) -> int:  # noqa: N802 - mirrors protobuf API
        return {"RELIABLE": 1, "LOSSY": 0}.get(name, 1)


class _DataPacket:
    Kind = _Kind


class _VideoGrants:
    def __init__(self, room_join: bool = False, room: str = "") -> None:
        self.room_join = room_join
        self.room = room


class _AccessToken:
    def __init__(self, api_key: str, api_secret: str) -> None:
        self.api_key = api_key
        self.claims: dict = {}

    def with_identity(self, identity: str):
        self.claims["identity"] = identity
        return self

    def with_name(self, name: str):
        self.claims["name"] = name
        return self

    def with_grants(self, grants):
        self.claims["grants"] = grants
        return self

    def with_ttl(self, ttl):
        self.claims["ttl"] = ttl
        return self

    def to_jwt(self) -> str:
        return f"stub-jwt:{self.claims.get('identity', '?')}"


class _RoomService:
    def __init__(self, stub: "LiveKitStub") -> None:
        self._stub = stub

    async def create_room(self, req):
        self._stub.create_room_calls.append(req.name)
        if self._stub.fail:
            raise ConnectionError("livekit unreachable (stub)")
        return types.SimpleNamespace(name=req.name)

    async def send_data(self, req):
        self._stub.send_data_calls.append(req)
        if self._stub.fail:
            raise ConnectionError("livekit unreachable (stub)")


class LiveKitStub:
    """Behaviour switchboard behind the stubbed livekit.api module."""

    def __init__(self) -> None:
        self.fail = False
        self.create_room_calls: list[str] = []
        self.send_data_calls: list = []

    def api(self, url, api_key, api_secret) -> "_LiveKitAPI":
        return _LiveKitAPI(self)


class _LiveKitAPI:
    def __init__(self, stub: LiveKitStub) -> None:
        self.room = _RoomService(stub)
        self.closed = False

    async def aclose(self) -> None:
        self.closed = True


@pytest.fixture()
def livekit_stub(monkeypatch) -> LiveKitStub:
    stub = LiveKitStub()

    api_mod = types.ModuleType("livekit.api")
    api_mod.LiveKitAPI = stub.api
    api_mod.AccessToken = _AccessToken
    api_mod.VideoGrants = _VideoGrants
    api_mod.CreateRoomRequest = _CreateRoomRequest
    api_mod.SendDataRequest = _SendDataRequest

    protocol_mod = types.ModuleType("livekit.protocol")
    room_mod = types.ModuleType("livekit.protocol.room")
    room_mod.DataPacket = _DataPacket
    protocol_mod.room = room_mod

    livekit_mod = types.ModuleType("livekit")
    livekit_mod.api = api_mod
    livekit_mod.protocol = protocol_mod

    monkeypatch.setitem(sys.modules, "livekit", livekit_mod)
    monkeypatch.setitem(sys.modules, "livekit.api", api_mod)
    monkeypatch.setitem(sys.modules, "livekit.protocol", protocol_mod)
    monkeypatch.setitem(sys.modules, "livekit.protocol.room", room_mod)
    return stub


class FakeDapr:
    """Records publishes/invocations like the real DaprClient."""

    def __init__(self) -> None:
        self.published: list[tuple[str, str, dict]] = []
        self.best_effort: list[tuple[str, str, dict]] = []
        # SPEC-W11 Part C: scriptable GET responses (method path -> payload
        # or Exception) + PUT recording, for the capture_location tool.
        self.get_responses: dict = {}
        self.invoke_get_calls: list[tuple] = []
        self.put_calls: list[tuple] = []

    async def publish(self, pubsub: str, topic: str, event: dict) -> None:
        self.published.append((pubsub, topic, event))

    async def publish_best_effort(self, pubsub: str, topic: str, event: dict, kind: str = "") -> None:
        self.best_effort.append((pubsub, topic, event))

    async def invoke_get(self, app_id, method, *, params=None, headers=None):
        self.invoke_get_calls.append((app_id, method, params, headers))
        resp = self.get_responses.get(method, {})
        if isinstance(resp, Exception):
            raise resp
        return resp

    async def invoke_post(self, app_id, method, *, payload=None, headers=None):
        return {}

    async def invoke_put(self, app_id, method, *, payload=None, headers=None):
        self.put_calls.append((app_id, method, payload, headers))
        return {"ok": True}
