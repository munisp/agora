"""Agent-driven UI action tests (SPEC-W9 Part B): validation rules in
app/ui_actions.py, tool registration + per-turn collection in the tool
layer, and the chat transport shape (buffered `ui_actions` payload and SSE
`{"ui_action": ...}` frames before done)."""

from __future__ import annotations

import json
from types import SimpleNamespace

import pytest

from app import chat as chat_module
from app.chat import ChatService
from app.config import Settings
from app.session_state import SessionState, SessionStore
from app.tenant_context import TenantContext
from app.tools import TOOL_NAMES, UI_ACTION_TOOL_NAMES, ToolLayer
from app.ui_actions import (
    validate_highlight,
    validate_navigate,
    validate_prefill_booking,
)

from conftest import FakeDapr

VALID_UUID = "123e4567-e89b-42d3-a456-426614174000"


# ------------------------------------------------------------- validation
class TestNavigateValidation:
    @pytest.mark.parametrize(
        "path",
        ["/", "/rooms", "/rooms/deluxe?night=1", "/#booking", "/a/b_c-d.e~f"],
    )
    def test_accepts_same_origin_paths(self, path):
        assert validate_navigate(path) == {"type": "navigate", "path": path}

    @pytest.mark.parametrize(
        "path",
        [
            "",  # empty
            "rooms",  # no leading slash
            "https://evil.example/rooms",  # scheme + host
            "http://evil.example",  # scheme + host
            "//evil.example/rooms",  # protocol-relative host smuggling
            "javascript:alert(1)",  # scheme
            "/rooms with space",  # whitespace
            None,
            42,
            "/" + "a" * 2048,  # over length cap
        ],
    )
    def test_rejects_non_same_origin(self, path):
        assert validate_navigate(path) is None


class TestHighlightValidation:
    @pytest.mark.parametrize(
        "selector",
        [
            "#booking-form",
            ".offerings",
            "main .card > button",
            "[data-offering='abc']",
            'input[name="phone"]',
            "#a .b:c",
            "x" * 120,
        ],
    )
    def test_accepts_sanitized_selectors(self, selector):
        assert validate_highlight(selector) == {"type": "highlight", "selector": selector}

    @pytest.mark.parametrize(
        "selector",
        [
            "",  # empty
            "x" * 121,  # over 120 chars
            "div;position:fixed",  # ';' not in charset
            "img{x:expression(alert(1))}",  # braces/parens not in charset
            "<script>alert(1)</script>",  # '<' not in charset
            "a\\ b",  # backslash not in charset
            "div, span",  # ',' not in charset
            "*",  # '*' not in charset
            None,
            7,
        ],
    )
    def test_rejects_unsanitized_selectors(self, selector):
        assert validate_highlight(selector) is None


class TestPrefillValidation:
    def test_accepts_uuid_and_normalizes(self):
        action = validate_prefill_booking(VALID_UUID.upper())
        assert action == {"type": "prefill_booking", "offering_id": VALID_UUID}

    @pytest.mark.parametrize(
        "value",
        [
            "",
            "not-a-uuid",
            "123e4567-e89b-42d3-a456-42661417400",  # too short
            "123e4567-e89b-42d3-a456-426614174000-extra",
            None,
            123,
            {"id": VALID_UUID},
        ],
    )
    def test_rejects_non_uuids(self, value):
        assert validate_prefill_booking(value) is None


# ------------------------------------------------------------- tool layer
def _ctx() -> TenantContext:
    return TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")


def _tool_layer(dapr: FakeDapr, sink: list | None = None) -> ToolLayer:
    return ToolLayer(
        dapr=dapr,  # type: ignore[arg-type]
        settings=Settings(),
        ctx=_ctx(),
        session=SessionState(conversation_id="conv-ui", site_slug="demo"),
        ui_action_sink=sink,
    )


def test_ui_action_tools_registered():
    for name in UI_ACTION_TOOL_NAMES:
        assert name in TOOL_NAMES


async def test_navigate_tool_queues_action_and_acks():
    sink: list = []
    layer = _tool_layer(FakeDapr(), sink)
    result = await layer.dispatch("navigate_to_page", {"path": "/rooms"})
    assert result["status"] == "ok"
    assert result["ui_action"] == {"type": "navigate", "path": "/rooms"}
    assert sink == [{"type": "navigate", "path": "/rooms"}]
    assert layer.collected_ui_actions is sink


async def test_highlight_tool_queues_action():
    sink: list = []
    layer = _tool_layer(FakeDapr(), sink)
    result = await layer.dispatch("highlight_element", {"selector": "#booking-form"})
    assert result["status"] == "ok"
    assert sink == [{"type": "highlight", "selector": "#booking-form"}]


async def test_prefill_tool_queues_action():
    sink: list = []
    layer = _tool_layer(FakeDapr(), sink)
    result = await layer.dispatch("prefill_booking", {"offering_id": VALID_UUID})
    assert result["status"] == "ok"
    assert sink == [{"type": "prefill_booking", "offering_id": VALID_UUID}]


async def test_invalid_actions_return_error_string_and_are_dropped():
    sink: list = []
    layer = _tool_layer(FakeDapr(), sink)
    r1 = await layer.dispatch("navigate_to_page", {"path": "https://evil.example/"})
    r2 = await layer.dispatch("highlight_element", {"selector": "<script>alert(1)</script>"})
    r3 = await layer.dispatch("prefill_booking", {"offering_id": "nope"})
    for r in (r1, r2, r3):
        assert r["status"] == "error"
        assert isinstance(r["message"], str) and r["message"]
        assert "ui_action" not in r
    assert sink == []  # nothing reached the outgoing payload


# ------------------------------------------------------- chat transport
def _ctx_for_chat() -> TenantContext:
    return TenantContext(
        site_slug="demo",
        tenant_id="t-uuid",
        tenant_slug="acme",
        offerings=[{"id": VALID_UUID, "name": "Deluxe night"}],
    )


class FakeLLM:
    """Scripted non-streaming rounds for run_tool_loop."""

    def __init__(self, rounds):
        self._rounds = list(rounds)

    async def chat_with_tools(self, messages, tools):
        return self._rounds.pop(0)


class FakeStreamLLM:
    """Scripted streaming rounds for run_tool_loop_stream."""

    def __init__(self, rounds):
        self._rounds = list(rounds)

    async def stream_with_tools(self, messages, tools):
        chunks = self._rounds.pop(0)

        async def gen():
            for c in chunks:
                yield c

        return gen()


def _tool_call_msg(calls):
    tcs = [
        SimpleNamespace(
            id=f"call_{i}",
            function=SimpleNamespace(name=name, arguments=json.dumps(args)),
        )
        for i, (name, args) in enumerate(calls)
    ]
    return SimpleNamespace(content="", tool_calls=tcs)


def _text_msg(text):
    return SimpleNamespace(content=text, tool_calls=[])


def _chunk(content=None, tool_calls=None):
    delta = SimpleNamespace(content=content, tool_calls=tool_calls or [])
    return SimpleNamespace(choices=[SimpleNamespace(delta=delta)])


def _stream_tc(index, tc_id=None, name=None, arguments=None):
    fn = SimpleNamespace(name=name, arguments=arguments)
    return SimpleNamespace(index=index, id=tc_id, function=fn)


def _chat_service(llm, monkeypatch) -> ChatService:
    async def fake_fetch(dapr, settings, site_slug):
        return _ctx_for_chat()

    monkeypatch.setattr(chat_module, "fetch_tenant_context", fake_fetch)
    return ChatService(
        settings=Settings(),
        dapr=FakeDapr(),  # type: ignore[arg-type]
        llm=llm,  # type: ignore[arg-type]
        sessions=SessionStore(),
    )


async def test_buffered_response_carries_ui_actions(monkeypatch):
    llm = FakeLLM(
        [
            _tool_call_msg(
                [
                    ("navigate_to_page", {"path": "/rooms"}),
                    ("highlight_element", {"selector": "#booking-form"}),
                    # Invalid action: the LLM gets an error string, the
                    # action is dropped from the outgoing payload.
                    ("prefill_booking", {"offering_id": "not-a-uuid"}),
                ]
            ),
            _text_msg("Let me show you our rooms page."),
        ]
    )
    service = _chat_service(llm, monkeypatch)
    resp = await service.handle_message(site_slug="demo", message="show me rooms", conversation_id=None)

    assert resp["reply"] == "Let me show you our rooms page."
    assert resp["ui_actions"] == [
        {"type": "navigate", "path": "/rooms"},
        {"type": "highlight", "selector": "#booking-form"},
    ]


async def test_buffered_response_ui_actions_empty_by_default(monkeypatch):
    llm = FakeLLM([_text_msg("Hello!")])
    service = _chat_service(llm, monkeypatch)
    resp = await service.handle_message(site_slug="demo", message="hi", conversation_id=None)
    assert resp["ui_actions"] == []


async def test_buffered_system_prompt_has_ui_action_addendum(monkeypatch):
    llm = FakeLLM([_text_msg("ok")])
    service = _chat_service(llm, monkeypatch)
    await service.handle_message(site_slug="demo", message="hi", conversation_id="conv-prompt")
    history = service._histories.get("conv-prompt")
    assert history[0]["role"] == "system"
    assert "navigate_to_page" in history[0]["content"]
    assert "highlight" in history[0]["content"]


async def test_stream_emits_ui_action_frames_before_done(monkeypatch):
    llm = FakeStreamLLM(
        [
            [
                _chunk(tool_calls=[_stream_tc(0, tc_id="call_1")]),
                _chunk(tool_calls=[_stream_tc(0, name="navigate_to_page")]),
                _chunk(tool_calls=[_stream_tc(0, arguments='{"path": "/rooms"}')]),
            ],
            [_chunk(content="Here "), _chunk(content="you go.")],
        ]
    )
    service = _chat_service(llm, monkeypatch)
    events = [
        e
        async for e in service.handle_message_stream(
            site_slug="demo", message="show rooms", conversation_id=None
        )
    ]

    ui_frames = [e["ui_action"] for e in events if "ui_action" in e]
    assert ui_frames == [{"type": "navigate", "path": "/rooms"}]
    # ui_action frames arrive after the deltas/tool frames and strictly
    # before the terminal done frame.
    kinds = [
        (
            "delta"
            if "delta" in e
            else "ui_action"
            if "ui_action" in e
            else "done"
            if "done" in e
            else "other"
        )
        for e in events
    ]
    assert kinds[-1] == "done"
    assert kinds.index("ui_action") < kinds.index("done")
    assert kinds.index("ui_action") > kinds.index("delta")
    assert events[-1]["done"] is True


async def test_stream_without_ui_actions_has_no_frames(monkeypatch):
    llm = FakeStreamLLM([[_chunk(content="hi")]])
    service = _chat_service(llm, monkeypatch)
    events = [
        e
        async for e in service.handle_message_stream(
            site_slug="demo", message="hi", conversation_id=None
        )
    ]
    assert not any("ui_action" in e for e in events)
    assert events[-1]["done"] is True
