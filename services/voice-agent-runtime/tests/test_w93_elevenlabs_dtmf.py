"""SPEC-W93 tests: ElevenLabs native DTMF input.

- desired_dtmf_settings: defaults (redact ON), timeout clamped into the
  ElevenLabs 0.5..10.0s range, disabled -> None.
- sync_dtmf_input (httpx MockTransport): in_sync performs NO write; drift
  PATCHes the exact convergence body; disabled converges to null
  (in_sync when already null, explicit PATCH when set).
- read_keypad_entry: keypad_native on the hosted backend (honest relay
  contract, no digits), keypad_unavailable unchanged elsewhere, the
  LiveKit DtmfBuffer always wins when present.
- Webhook wiring: handle_tool_webhook forwards native_keypad from
  settings into the ToolLayer.
- Config: ELEVENLABS_DTMF_* env vars load with the documented defaults.

REDIRECTION PROOF: fails on the base tree (no desired_dtmf_settings /
sync_dtmf_input / native_keypad / keypad_native).
"""

from __future__ import annotations

import asyncio
import json

import httpx
import pytest

from app import elevenlabs_adapter
from app import metrics
from app import tools as tools_module
from app.config import load_settings
from app.dtmf import DtmfBuffer
from app.elevenlabs_adapter import ElevenLabsBackend
from app.session_state import SessionState, SessionStore
from app.tenant_context import TenantContext
from app.tools import ToolLayer

from conftest import FakeDapr


@pytest.fixture(autouse=True)
def _fresh_registry():
    metrics.reset_registry()
    yield


def _settings(**over):
    from app.config import Settings

    base = dict(
        booking_url="http://booking:7002",
        voice_booking_internal_token="book-tok",
        phone_hash_salt="test-salt",
        agent_backend="elevenlabs",
        elevenlabs_api_key="el-key",
        elevenlabs_agent_id="agent-1",
    )
    base.update(over)
    return Settings(**base)


def _ctx() -> TenantContext:
    return TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")


def _backend(requests: list, remote_settings, **over) -> ElevenLabsBackend:
    """Backend whose HTTP client is a MockTransport replaying `remote_settings`
    as the hosted agent's current dtmf_input_settings (GET) and acking PATCH."""

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        if request.method == "GET":
            return httpx.Response(
                200,
                json={
                    "conversation_config": {
                        "conversation": {"dtmf_input_settings": remote_settings}
                    }
                },
            )
        return httpx.Response(200, json={})

    be = ElevenLabsBackend(
        settings=_settings(**over), dapr=FakeDapr(), sessions=SessionStore()
    )
    be._http = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    return be


async def _drain_events() -> None:
    await asyncio.sleep(0)
    await asyncio.sleep(0)


class TestDesiredSettings:
    def test_defaults(self):
        be = _backend([], None)
        assert be.desired_dtmf_settings() == {
            "dtmf_input_timeout": 2.0,
            "hash_terminator": True,
            "redact_input": True,  # privacy default ON (SPEC-W93)
        }

    def test_timeout_clamped_to_elevenlabs_range(self):
        assert _backend([], None, elevenlabs_dtmf_timeout_s=0.4).desired_dtmf_settings()[
            "dtmf_input_timeout"
        ] == 0.5
        assert _backend([], None, elevenlabs_dtmf_timeout_s=99.0).desired_dtmf_settings()[
            "dtmf_input_timeout"
        ] == 10.0

    def test_disabled_returns_none(self):
        be = _backend([], None, elevenlabs_dtmf_enabled=False)
        assert be.desired_dtmf_settings() is None


class TestSyncDtmfInput:
    async def test_in_sync_performs_no_write(self):
        requests: list = []
        remote = {
            "dtmf_input_timeout": 2.0,
            "hash_terminator": True,
            "redact_input": True,
        }
        be = _backend(requests, remote)
        result = await be.sync_dtmf_input()
        assert result["status"] == "in_sync"
        assert [r.method for r in requests] == ["GET"]  # NO patch issued

    async def test_drift_patches_exact_body(self):
        requests: list = []
        remote = {
            "dtmf_input_timeout": 2.0,
            "hash_terminator": True,
            "redact_input": False,  # drift: redaction off remotely
        }
        be = _backend(requests, remote)
        result = await be.sync_dtmf_input()
        assert result["status"] == "updated"
        assert [r.method for r in requests] == ["GET", "PATCH"]
        patch = requests[1]
        assert patch.url.path == "/v1/convai/agents/agent-1"
        assert patch.headers["xi-api-key"] == "el-key"
        assert json.loads(patch.content) == {
            "conversation_config": {
                "conversation": {
                    "dtmf_input_settings": {
                        "dtmf_input_timeout": 2.0,
                        "hash_terminator": True,
                        "redact_input": True,
                    }
                }
            }
        }

    async def test_disabled_remote_null_in_sync(self):
        requests: list = []
        be = _backend(requests, None, elevenlabs_dtmf_enabled=False)
        result = await be.sync_dtmf_input()
        assert result["status"] == "in_sync"
        assert result["settings"] is None
        assert [r.method for r in requests] == ["GET"]

    async def test_disabled_remote_set_patches_null(self):
        requests: list = []
        remote = {"dtmf_input_timeout": 3.0, "hash_terminator": True, "redact_input": False}
        be = _backend(requests, remote, elevenlabs_dtmf_enabled=False)
        result = await be.sync_dtmf_input()
        assert result["status"] == "updated"
        assert [r.method for r in requests] == ["GET", "PATCH"]
        assert json.loads(requests[1].content) == {
            "conversation_config": {"conversation": {"dtmf_input_settings": None}}
        }

    async def test_missing_credentials_raise(self):
        be = _backend([], None, elevenlabs_api_key="")
        with pytest.raises(RuntimeError):
            await be.sync_dtmf_input()


class TestReadKeypadEntryNative:
    def _layer(self, native: bool, buffer=None) -> ToolLayer:
        return ToolLayer(
            dapr=FakeDapr(),  # type: ignore[arg-type]
            settings=_settings(),
            ctx=_ctx(),
            session=SessionState(conversation_id="conv-1", site_slug="demo"),
            dtmf_buffer=buffer,
            native_keypad=native,
        )

    async def test_keypad_native_without_buffer(self):
        layer = self._layer(native=True)
        result = await layer.dispatch("read_keypad_entry", {"expected_length": 6})
        assert result["status"] == "keypad_native"
        assert result["digits"] == ""  # digits never transit this runtime
        assert "next message" in result["message"]
        assert "#" in result["message"]
        assert "verify_caller_code" in result["message"]
        assert result["expected_length"] == 6

    async def test_keypad_unavailable_unchanged_when_not_native(self):
        layer = self._layer(native=False)
        result = await layer.dispatch("read_keypad_entry", {})
        assert result["status"] == "keypad_unavailable"
        assert result["error"] == "keypad_unavailable"

    async def test_buffer_wins_over_native(self):
        buf = DtmfBuffer()
        layer = self._layer(native=True, buffer=buf)

        async def typer():
            await asyncio.sleep(0.01)
            buf.feed("654321")

        task = asyncio.create_task(typer())
        result = await layer.dispatch(
            "read_keypad_entry", {"timeout_s": 10, "expected_length": 6}
        )
        await task
        assert result["status"] == "keypad_entry"
        assert result["digits"] == "654321"

    async def test_native_event_carries_no_digits(self):
        layer = self._layer(native=True)
        result = await layer.dispatch("read_keypad_entry", {})
        assert result["status"] == "keypad_native"
        await _drain_events()
        events = [
            evt
            for _ps, _t, evt in layer._dapr.best_effort
            if evt.get("type") == "com.opendesk.conversation.ToolInvoked"
        ]
        assert events, "expected a ToolInvoked event"
        evt = events[-1]
        assert evt["data"]["tool"] == "read_keypad_entry"
        assert evt["data"]["status"] == "keypad_native"
        assert "digits" not in json.dumps(evt)


class TestWebhookNativeWiring:
    async def _run_webhook(self, monkeypatch, enabled: bool) -> dict:
        captured: dict = {}

        class _FakeLayer:
            def __init__(self, **kwargs):
                captured.update(kwargs)

            async def dispatch(self, name, params):
                return {"status": "ok"}

        monkeypatch.setattr(elevenlabs_adapter, "ToolLayer", _FakeLayer)

        async def _fake_ctx(dapr, settings, site_slug):
            return _ctx()

        monkeypatch.setattr(elevenlabs_adapter, "fetch_tenant_context", _fake_ctx)
        be = ElevenLabsBackend(
            settings=_settings(elevenlabs_dtmf_enabled=enabled),
            dapr=FakeDapr(),
            sessions=SessionStore(),
        )
        result = await be.handle_tool_webhook(
            {"tool_name": "any", "parameters": {}, "site_slug": "demo"}
        )
        assert result["status"] == "ok"
        assert "session_secret" in result  # K15(b) round-trip preserved
        return captured

    async def test_webhook_passes_native_keypad_enabled(self, monkeypatch):
        captured = await self._run_webhook(monkeypatch, enabled=True)
        assert captured["native_keypad"] is True

    async def test_webhook_passes_native_keypad_disabled(self, monkeypatch):
        captured = await self._run_webhook(monkeypatch, enabled=False)
        assert captured["native_keypad"] is False


class TestConfigEnv:
    def test_env_defaults(self, monkeypatch):
        for var in (
            "ELEVENLABS_DTMF_ENABLED",
            "ELEVENLABS_DTMF_TIMEOUT_S",
            "ELEVENLABS_DTMF_HASH_TERMINATOR",
            "ELEVENLABS_DTMF_REDACT",
        ):
            monkeypatch.delenv(var, raising=False)
        s = load_settings()
        assert s.elevenlabs_dtmf_enabled is True
        assert s.elevenlabs_dtmf_timeout_s == 2.0
        assert s.elevenlabs_dtmf_hash_terminator is True
        assert s.elevenlabs_dtmf_redact is True

    def test_env_overrides(self, monkeypatch):
        monkeypatch.setenv("ELEVENLABS_DTMF_ENABLED", "0")
        monkeypatch.setenv("ELEVENLABS_DTMF_TIMEOUT_S", "5.5")
        monkeypatch.setenv("ELEVENLABS_DTMF_HASH_TERMINATOR", "off")
        monkeypatch.setenv("ELEVENLABS_DTMF_REDACT", "false")
        s = load_settings()
        assert s.elevenlabs_dtmf_enabled is False
        assert s.elevenlabs_dtmf_timeout_s == 5.5
        assert s.elevenlabs_dtmf_hash_terminator is False
        assert s.elevenlabs_dtmf_redact is False
