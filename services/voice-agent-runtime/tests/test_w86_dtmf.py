"""SPEC-W86 tests: DTMF keypad capture (OTP entry via phone keypad).

- parse_dtmf_payload: the LiveKit SIP relay shapes (topic "dtmf" plain
  digits; JSON {"digit"}/{"dtmf"} forms on any topic); everything else
  rejected.
- DtmfBuffer: accumulate / * clear / # submit / expected_length
  auto-submit (6-digit portal codes need no #) / timeout -> None /
  32-digit bound / one-shot wait semantics.
- attach_dtmf_listener: only SIP-kind participants feed the buffer (a web
  widget can never inject keypad digits into a PSTN session).
- ToolLayer.read_keypad_entry: keypad_unavailable without a buffer
  (honest channel-capability report), keypad_entry with a buffer, timeout
  and argument-clamping behavior.
- Static wiring: tool registered (TOOL_NAMES / SLOW_TOOLS / tier 0),
  prompt instruction, worker timeout override + ai_callable wrapper
  (source inspect, W81 pattern).
- Metrics: voice_dtmf_digits_total / voice_dtmf_entries_total /
  voice_dtmf_timeouts_total.

REDIRECTION PROOF: fails on the base tree (app/dtmf.py absent, no
read_keypad_entry tool).
"""

from __future__ import annotations

import asyncio
import inspect
from types import SimpleNamespace

import pytest

try:  # pragma: no cover - import shim (real SDK present in the worker image)
    import app.livekit_worker as lw
except ModuleNotFoundError:  # pragma: no cover - import shim
    import test_live_tts_chain  # noqa: F401 - installs the livekit stubs
    import app.livekit_worker as lw

from app import dtmf, metrics, prompts
from app import tools as tools_module
from app.async_tools import SLOW_TOOLS
from app.config import DEFAULT_VOICE_TOOL_TIERS, TIER_LOW, Settings
from app.dtmf import DtmfBuffer, attach_dtmf_listener, parse_dtmf_payload
from app.session_state import SessionState
from app.tenant_context import TenantContext
from app.tools import TOOL_NAMES, ToolLayer

from conftest import FakeDapr


@pytest.fixture(autouse=True)
def _fresh_registry():
    metrics.reset_registry()
    yield


def _ctx() -> TenantContext:
    return TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")


def _settings(**over) -> Settings:
    base = dict(
        booking_url="http://booking:7002",
        voice_booking_internal_token="book-tok",
        phone_hash_salt="test-salt",
    )
    base.update(over)
    return Settings(**base)


def _layer(buffer: DtmfBuffer | None = None, **settings_over) -> ToolLayer:
    return ToolLayer(
        dapr=FakeDapr(),  # type: ignore[arg-type]
        settings=_settings(**settings_over),
        ctx=_ctx(),
        session=SessionState(conversation_id="conv-1", site_slug="demo"),
        dtmf_buffer=buffer,
    )


async def _drain_events() -> None:
    """ToolInvoked publishes are fire-and-forget (SPEC-W46 P9) — yield to
    the loop so scheduled publishes land in FakeDapr."""
    await asyncio.sleep(0)
    await asyncio.sleep(0)


def _sip_participant():
    return SimpleNamespace(
        kind=SimpleNamespace(name="SIP"),
        identity="sip_+2348035550101_ab12",
        attributes={},
    )


def _web_participant():
    return SimpleNamespace(
        kind=SimpleNamespace(name="STANDARD"),
        identity="web-visitor-1",
        attributes={},
    )


class _FakeRoom:
    """Duck-typed LiveKit room: on(event[, callback]) + manual emit."""

    def __init__(self) -> None:
        self._handlers: dict[str, list] = {}

    def on(self, event, callback=None):
        if callback is not None:
            self._handlers.setdefault(event, []).append(callback)
            return callback

        def decorator(cb):
            self._handlers.setdefault(event, []).append(cb)
            return cb

        return decorator

    def emit(self, event, packet) -> None:
        for cb in self._handlers.get(event, []):
            cb(packet)


class TestParseDtmfPayload:
    def test_dtmf_topic_plain_digit(self):
        assert parse_dtmf_payload(b"5", "dtmf") == "5"

    def test_dtmf_topic_multi_digit(self):
        assert parse_dtmf_payload(b"53#", "dtmf") == "53#"

    def test_dtmf_topic_star(self):
        assert parse_dtmf_payload(b"*", "dtmf") == "*"

    def test_json_digit_any_topic(self):
        assert parse_dtmf_payload(b'{"digit": "7"}', "custom") == "7"

    def test_json_dtmf_key_any_topic(self):
        assert parse_dtmf_payload(b'{"dtmf": "2"}', "") == "2"

    def test_json_form_on_dtmf_topic(self):
        # Relay variants that wrap the digit still parse on topic "dtmf".
        assert parse_dtmf_payload(b'{"digit": "9"}', "dtmf") == "9"

    def test_json_int_value(self):
        assert parse_dtmf_payload(b'{"digit": 4}', "x") == "4"

    def test_plain_payload_on_non_dtmf_topic_rejected(self):
        assert parse_dtmf_payload(b"5", "chat") is None

    def test_garbage_rejected(self):
        assert parse_dtmf_payload(b"\xff\xfe\x00", "dtmf") is None
        assert parse_dtmf_payload(b"hello", "dtmf") is None
        assert parse_dtmf_payload(b"", "dtmf") is None

    def test_wrong_json_shapes_rejected(self):
        assert parse_dtmf_payload(b'{"other": "5"}', "x") is None
        assert parse_dtmf_payload(b"[1, 2]", "x") is None
        assert parse_dtmf_payload(b'{"digit": "ab"}', "x") is None
        assert parse_dtmf_payload(b'{"digit": true}', "x") is None


class TestDtmfBuffer:
    async def test_accumulate_and_hash_submit(self):
        buf = DtmfBuffer()

        async def finisher():
            await asyncio.sleep(0.01)
            buf.feed("3")
            buf.feed("#")

        task = asyncio.create_task(finisher())
        buf.feed("12")
        entry = await buf.wait_for_entry(timeout_s=1.0)
        await task
        assert entry == "123"

    async def test_star_clears(self):
        buf = DtmfBuffer()
        buf.feed("12")
        buf.feed("*")
        assert buf.pending == ""
        buf.feed("3#")
        entry = await buf.wait_for_entry(timeout_s=0.5)
        assert entry == "3"

    async def test_expected_length_auto_submit_no_hash(self):
        buf = DtmfBuffer()

        async def typer():
            await asyncio.sleep(0.01)
            buf.feed("483920")  # 6 digits -> auto-submit, no '#'

        task = asyncio.create_task(typer())
        entry = await buf.wait_for_entry(timeout_s=1.0, expected_length=6)
        await task
        assert entry == "483920"

    async def test_timeout_returns_none(self):
        buf = DtmfBuffer()
        assert await buf.wait_for_entry(timeout_s=0.05) is None

    async def test_max_digits_bound(self):
        buf = DtmfBuffer()
        buf.feed("1" * 40)
        assert len(buf.pending) == dtmf.MAX_DIGITS == 32

    async def test_wait_consumes_entry_once(self):
        buf = DtmfBuffer()
        buf.feed("55#")
        assert await buf.wait_for_entry(timeout_s=0.5) == "55"
        # The entry is consumed: a second wait with no new input times out.
        assert await buf.wait_for_entry(timeout_s=0.05) is None

    async def test_digits_typed_before_wait_satisfy_immediately(self):
        # Caller typed while the agent was still speaking.
        buf = DtmfBuffer()
        buf.feed("123456")
        entry = await buf.wait_for_entry(timeout_s=0.5, expected_length=6)
        assert entry == "123456"

    async def test_timeout_discards_partial(self):
        # A retry must never mix digits from two attempts.
        buf = DtmfBuffer()
        buf.feed("123")
        assert await buf.wait_for_entry(timeout_s=0.05, expected_length=6) is None
        assert buf.pending == ""

    async def test_hash_with_empty_buffer_is_noop(self):
        buf = DtmfBuffer()
        buf.feed("#")
        assert await buf.wait_for_entry(timeout_s=0.05) is None


class TestAttachDtmfListener:
    def test_sip_participant_feeds_buffer(self):
        room, buf = _FakeRoom(), DtmfBuffer()
        attach_dtmf_listener(room, buf)
        room.emit(
            "data_received",
            SimpleNamespace(data=b"4", topic="dtmf", participant=_sip_participant()),
        )
        assert buf.pending == "4"

    def test_web_participant_never_feeds_buffer(self):
        room, buf = _FakeRoom(), DtmfBuffer()
        attach_dtmf_listener(room, buf)
        room.emit(
            "data_received",
            SimpleNamespace(data=b"4", topic="dtmf", participant=_web_participant()),
        )
        assert buf.pending == ""

    def test_none_participant_ignored(self):
        room, buf = _FakeRoom(), DtmfBuffer()
        attach_dtmf_listener(room, buf)
        room.emit(
            "data_received",
            SimpleNamespace(data=b"4", topic="dtmf", participant=None),
        )
        assert buf.pending == ""

    def test_non_dtmf_payload_ignored(self):
        room, buf = _FakeRoom(), DtmfBuffer()
        attach_dtmf_listener(room, buf)
        room.emit(
            "data_received",
            SimpleNamespace(
                data=b'{"action": "navigate"}', topic="ui-actions",
                participant=_sip_participant(),
            ),
        )
        assert buf.pending == ""

    def test_decorator_style_registration(self):
        class _DecoratorOnlyRoom:
            def __init__(self):
                self._handlers = {}

            def on(self, event):
                def decorator(cb):
                    self._handlers[event] = cb
                    return cb

                return decorator

        room, buf = _DecoratorOnlyRoom(), DtmfBuffer()
        attach_dtmf_listener(room, buf)
        room._handlers["data_received"](
            SimpleNamespace(data=b"7", topic="dtmf", participant=_sip_participant())
        )
        assert buf.pending == "7"


class TestReadKeypadEntryTool:
    async def test_keypad_unavailable_without_buffer(self):
        layer = _layer(buffer=None)
        result = await layer.dispatch("read_keypad_entry", {})
        assert result["status"] == "keypad_unavailable"
        assert result["error"] == "keypad_unavailable"
        assert result["digits"] == ""

    async def test_keypad_entry_with_buffer(self):
        buf = DtmfBuffer()
        layer = _layer(buffer=buf)

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

    async def test_keypad_timeout(self, monkeypatch):
        # Compress the 5s floor to 0 so the test does not wait in real time.
        monkeypatch.setattr(tools_module, "KEYPAD_TIMEOUT_MIN_S", 0)
        buf = DtmfBuffer()
        layer = _layer(buffer=buf)
        result = await layer.dispatch("read_keypad_entry", {"timeout_s": 0})
        assert result["status"] == "keypad_timeout"
        assert result["digits"] == ""

    async def test_argument_bounds_clamped(self):
        # The clamped values are echoed in every payload (observable here
        # via the fast keypad_unavailable path — no waiting).
        layer = _layer(buffer=None)
        result = await layer.dispatch(
            "read_keypad_entry", {"timeout_s": 1, "expected_length": 99}
        )
        assert result["timeout_s"] == 5  # clamped up to the floor
        assert result["expected_length"] == 8  # clamped down to the ceiling
        result = await layer.dispatch(
            "read_keypad_entry", {"timeout_s": 9999, "expected_length": 1}
        )
        assert result["timeout_s"] == 60
        assert result["expected_length"] == 4

    async def test_garbage_arguments_fall_back_to_defaults(self):
        layer = _layer(buffer=None)
        result = await layer.dispatch(
            "read_keypad_entry", {"timeout_s": "soon", "expected_length": "six"}
        )
        assert result["timeout_s"] == 30
        assert result["expected_length"] is None

    async def test_event_carries_no_digits(self):
        buf = DtmfBuffer()
        layer = _layer(buffer=buf)
        buf.feed("777#")
        result = await layer.dispatch("read_keypad_entry", {"timeout_s": 10})
        assert result["status"] == "keypad_entry"
        await _drain_events()
        dapr = layer._dapr
        events = [
            evt for _ps, _t, evt in dapr.best_effort
            if evt.get("type") == "com.opendesk.conversation.ToolInvoked"
        ]
        assert events, "ToolInvoked event expected"
        for event in events:
            # The digits never leave the runtime via events — status only.
            assert "777" not in str(event)


class TestStaticWiring:
    def test_tool_registered(self):
        assert "read_keypad_entry" in TOOL_NAMES
        assert "read_keypad_entry" in SLOW_TOOLS
        assert DEFAULT_VOICE_TOOL_TIERS["read_keypad_entry"] == TIER_LOW

    def test_prompt_mentions_keypad_tool(self):
        src = inspect.getsource(prompts)
        assert "read_keypad_entry" in src

    def test_worker_wires_keypad_timeout_override(self):
        src = inspect.getsource(lw.build_voice_agent)
        assert '"read_keypad_entry": settings.keypad_tool_timeout_s' in src
        # The W81 OTP override is still present (unchanged behavior).
        assert "verification_tool_timeout_s" in src

    def test_receptionist_wrapper_present(self):
        src = inspect.getsource(lw.ReceptionistFunctions)
        assert "async def read_keypad_entry" in src

    def test_entrypoint_attaches_listener(self):
        src = inspect.getsource(lw)
        assert "dtmf.attach_dtmf_listener(ctx.room, dtmf_buffer)" in src

    def test_keypad_timeout_setting_default(self):
        s = Settings()
        assert s.keypad_tool_timeout_s == 65.0
        # The override must exceed the tool-side max wait (60s).
        assert s.keypad_tool_timeout_s > 60.0
        # The global tool timeout is untouched.
        assert s.tool_timeout_s == 4.0


class TestDtmfMetrics:
    async def test_digits_and_entries_counted(self):
        buf = DtmfBuffer()
        buf.feed("12")
        buf.feed("#")
        assert await buf.wait_for_entry(timeout_s=0.5) == "12"
        rendered = metrics.render()
        assert "voice_dtmf_digits_total 2" in rendered
        assert "voice_dtmf_entries_total 1" in rendered

    async def test_timeouts_counted(self):
        buf = DtmfBuffer()
        assert await buf.wait_for_entry(timeout_s=0.05) is None
        assert "voice_dtmf_timeouts_total 1" in metrics.render()
