"""SPEC-W96 tests: IVR-A DTMF accelerator (keypad menu + intent hints).

- parse_ivr_menu: default, custom, "none", malformed skips (bad digit /
  missing colon / empty parts / duplicate digit), >8 cap.
- menu_line: exact default rendering; empty menu -> "".
- build_greeting: byte-identical without the kwarg; menu line appended
  LAST with and without the disclosure block.
- build_system_prompt: byte-identical without the kwarg; KEYPAD MENU
  block present with the mapping; a human entry mentions request_human.
- DtmfBuffer: menu digit idle -> callback fired + not buffered; armed
  expected_length wait -> buffered (OTP safe); active no-length wait ->
  buffered; partial entry then menu digit -> buffered; raising callback
  -> feed unaffected.
- attach_ivr_menu (fake agent): digit -> ack spoken + system hint
  appended + metric bumped; empty menu -> not armed.
- tools: keypad_native message gains the menu sentence when
  ivr_menu_hint is set; unchanged when unset (W93 contract preserved).
- config: IVR_MENU_ENABLED / IVR_MENU load with documented defaults.

REDIRECTION PROOFS (SPEC-W96 gates):
  R1 feed() interception removed -> TestBufferMenuInterception FAILs;
  R2 build_greeting menu append removed -> TestGreetingMenuLine FAILs;
  R3 keypad_native menu sentence removed -> TestNativeMenuMessage FAILs.
"""

from __future__ import annotations

import asyncio

import pytest

from app import dtmf, ivr_menu, metrics
from app.config import load_settings
from app.dtmf import DtmfBuffer
from app.prompts import build_greeting, build_system_prompt
from app.session_state import SessionState
from app.tenant_context import TenantContext
from app.tools import ToolLayer

from conftest import FakeDapr

from test_w93_elevenlabs_dtmf import _ctx, _settings


@pytest.fixture(autouse=True)
def _fresh_registry():
    metrics.reset_registry()
    yield


# ---------------------------------------------------------------- parse


class TestParseIvrMenu:
    def test_empty_string_selects_default_menu(self):
        menu = ivr_menu.parse_ivr_menu("")
        assert menu == ivr_menu.DEFAULT_MENU
        assert [(e.digit, e.intent) for e in menu] == [
            ("1", "book"),
            ("2", "reschedule"),
            ("3", "human"),
        ]

    def test_none_disables(self):
        assert ivr_menu.parse_ivr_menu("none") == ()
        assert ivr_menu.parse_ivr_menu("NONE") == ()

    def test_custom_menu(self):
        menu = ivr_menu.parse_ivr_menu(
            "7=book:book a table,9=human:talk to the manager"
        )
        assert [(e.digit, e.intent, e.spoken) for e in menu] == [
            ("7", "book", "book a table"),
            ("9", "human", "talk to the manager"),
        ]

    def test_malformed_entries_skipped_individually(self):
        menu = ivr_menu.parse_ivr_menu(
            "1=book:book it,"  # valid
            "x=nope:nope,"  # bad digit (not 0-9)
            "12=nope:nope,"  # bad digit (multi-char)
            "2=nocolon,"  # missing spoken phrase colon
            "3=:empty intent,"  # empty intent
            "4=human:,"  # empty phrase
            "5=nosep"  # missing '=' entirely
        )
        assert [e.digit for e in menu] == ["1"]

    def test_duplicate_digit_first_wins(self):
        menu = ivr_menu.parse_ivr_menu("1=book:book it,1=human:human dup")
        assert [(e.digit, e.intent) for e in menu] == [("1", "book")]

    def test_cap_at_max_entries(self):
        raw = ",".join(f"{d}=intent{i}:phrase {i}" for i, d in enumerate("0123456789"))
        menu = ivr_menu.parse_ivr_menu(raw)
        assert len(menu) == ivr_menu.MAX_MENU_ENTRIES
        assert [e.digit for e in menu] == list("01234567")


# ---------------------------------------------------------------- lines


class TestMenuLine:
    def test_default_menu_exact_string(self):
        assert ivr_menu.menu_line(ivr_menu.DEFAULT_MENU) == (
            "You can press 1 to book an appointment, 2 to reschedule an "
            "appointment, or 3 to speak to a team member at any time."
        )

    def test_empty_menu_empty_line(self):
        assert ivr_menu.menu_line(()) == ""

    def test_single_and_two_entry_renderings(self):
        one = (ivr_menu.IvrEntry("1", "book", "book an appointment"),)
        assert ivr_menu.menu_line(one) == (
            "You can press 1 to book an appointment at any time."
        )
        two = one + (ivr_menu.IvrEntry("2", "human", "speak to a team member"),)
        assert ivr_menu.menu_line(two) == (
            "You can press 1 to book an appointment or 2 to speak to a "
            "team member at any time."
        )


# ---------------------------------------------------------------- greeting


class _GreetingCtx:
    """Duck-typed build_greeting context (attribute access only)."""

    def __init__(self, disclosure=None):
        self.display_name = "Ada Clinic"
        self.disclosure = disclosure


class TestGreetingMenuLine:
    def test_byte_identical_without_kwarg(self):
        ctx = _GreetingCtx()
        assert build_greeting(ctx, menu_line=None) == build_greeting(ctx)

    def test_menu_line_appended_without_disclosure(self):
        ctx = _GreetingCtx()
        line = ivr_menu.menu_line(ivr_menu.DEFAULT_MENU)
        assert build_greeting(ctx, menu_line=line) == (
            f"{build_greeting(ctx)} {line}"
        )

    def test_menu_line_appended_last_with_disclosure(self):
        ctx = _GreetingCtx(
            disclosure={"spokenAiDisclosure": True, "recordingConsent": True}
        )
        line = ivr_menu.menu_line(ivr_menu.DEFAULT_MENU)
        greeting = build_greeting(ctx, menu_line=line)
        assert greeting.endswith(f" {line}")
        # disclosure lines still applied around the base greeting
        assert greeting.startswith("You're speaking with an automated assistant.")
        assert "This call may be recorded" in greeting

    def test_empty_menu_line_treated_as_absent(self):
        ctx = _GreetingCtx()
        assert build_greeting(ctx, menu_line="") == build_greeting(ctx)


# ---------------------------------------------------------------- prompt


class TestPromptMenuHint:
    def test_byte_identical_without_kwarg(self):
        ctx = _ctx()
        assert build_system_prompt(
            ctx, conversation_id="c1", keypad_menu_hint=None
        ) == build_system_prompt(ctx, conversation_id="c1")

    def test_hint_block_present_with_mapping(self):
        ctx = _ctx()
        hint = ivr_menu.prompt_addendum(ivr_menu.DEFAULT_MENU)
        prompt = build_system_prompt(ctx, conversation_id="c1", keypad_menu_hint=hint)
        assert "KEYPAD MENU" in prompt
        assert "1 = book an appointment" in prompt
        assert "2 = reschedule an appointment" in prompt
        assert "3 = speak to a team member" in prompt

    def test_human_entry_mentions_request_human(self):
        hint = ivr_menu.prompt_addendum(ivr_menu.DEFAULT_MENU)
        assert "request_human" in hint
        # a menu WITHOUT a human entry does not mention it
        no_human = (ivr_menu.IvrEntry("1", "book", "book an appointment"),)
        assert "request_human" not in ivr_menu.prompt_addendum(no_human)

    def test_empty_menu_empty_addendum(self):
        assert ivr_menu.prompt_addendum(()) == ""


# ---------------------------------------------------------------- buffer


class TestBufferMenuInterception:
    def test_menu_digit_idle_fires_callback_not_buffered(self):
        buf = DtmfBuffer()
        fired: list[str] = []
        buf.arm_menu(frozenset({"1", "2", "3"}), fired.append)
        buf.feed("1")
        assert fired == ["1"]
        assert buf.pending == ""
        # a non-menu digit still buffers
        buf.feed("5")
        assert buf.pending == "5"

    async def test_menu_digit_during_expected_length_wait_is_buffered(self):
        buf = DtmfBuffer()
        fired: list[str] = []
        buf.arm_menu(frozenset({"1"}), fired.append)

        async def typer():
            await asyncio.sleep(0.02)
            buf.feed("1")
            await asyncio.sleep(0.01)
            buf.feed("23456")

        task = asyncio.create_task(typer())
        digits = await buf.wait_for_entry(timeout_s=2.0, expected_length=6)
        await task
        assert digits == "123456"  # OTP capture wins — never intercepted
        assert fired == []

    async def test_menu_digit_during_plain_wait_is_buffered(self):
        buf = DtmfBuffer()
        fired: list[str] = []
        buf.arm_menu(frozenset({"1"}), fired.append)

        async def typer():
            await asyncio.sleep(0.02)
            buf.feed("1")
            await asyncio.sleep(0.01)
            buf.feed("#")

        task = asyncio.create_task(typer())
        digits = await buf.wait_for_entry(timeout_s=2.0)
        await task
        assert digits == "1"
        assert fired == []

    def test_menu_digit_after_partial_entry_is_buffered(self):
        buf = DtmfBuffer()
        fired: list[str] = []
        buf.arm_menu(frozenset({"1"}), fired.append)
        buf.feed("5")
        buf.feed("1")  # mid-entry: NOT a menu choice
        assert fired == []
        assert buf.pending == "51"

    def test_raising_callback_does_not_break_feed(self):
        buf = DtmfBuffer()

        def boom(_digit: str) -> None:
            raise RuntimeError("menu callback exploded")

        buf.arm_menu(frozenset({"1"}), boom)
        buf.feed("1")  # swallowed — keypad capture unaffected
        buf.feed("5")
        assert buf.pending == "5"

    def test_unarmed_buffer_unchanged(self):
        buf = DtmfBuffer()
        buf.feed("1")
        assert buf.pending == "1"


# ---------------------------------------------------------------- attach


class _FakeChatCtx:
    def __init__(self):
        self.appended: list[tuple[str, str]] = []

    def append(self, *, role: str, text: str):
        self.appended.append((role, text))
        return self


class _FakeAgent:
    def __init__(self):
        self.said: list[str] = []
        self.chat_ctx = _FakeChatCtx()

    async def say(self, text: str, allow_interruptions: bool = True):
        self.said.append(text)


async def _drain() -> None:
    await asyncio.sleep(0)
    await asyncio.sleep(0)


class TestAttachIvrMenu:
    async def test_digit_acks_hints_and_counts(self):
        buf = DtmfBuffer()
        agent = _FakeAgent()
        ivr_menu.attach_ivr_menu(buf, ivr_menu.DEFAULT_MENU, agent)
        buf.feed("1")
        await _drain()
        assert agent.said == [ivr_menu.ack_phrase(ivr_menu.DEFAULT_MENU[0])]
        assert agent.chat_ctx.appended == [
            ("system", ivr_menu.intent_hint(ivr_menu.DEFAULT_MENU[0]))
        ]
        rendered = "\n".join(metrics.get_registry().ivr_menu_digits.render())
        assert 'voice_ivr_menu_digits_total{intent="book"} 1' in rendered

    async def test_human_hint_mentions_request_human(self):
        buf = DtmfBuffer()
        agent = _FakeAgent()
        ivr_menu.attach_ivr_menu(buf, ivr_menu.DEFAULT_MENU, agent)
        buf.feed("3")
        await _drain()
        hint = agent.chat_ctx.appended[0][1]
        assert "request_human" in hint
        assert agent.said == [ivr_menu.ack_phrase(ivr_menu.DEFAULT_MENU[2])]

    async def test_non_menu_digit_still_buffers(self):
        buf = DtmfBuffer()
        agent = _FakeAgent()
        ivr_menu.attach_ivr_menu(buf, ivr_menu.DEFAULT_MENU, agent)
        buf.feed("5")
        await _drain()
        assert agent.said == []
        assert agent.chat_ctx.appended == []
        assert buf.pending == "5"

    def test_empty_menu_not_armed(self):
        buf = DtmfBuffer()
        agent = _FakeAgent()
        ivr_menu.attach_ivr_menu(buf, (), agent)
        buf.feed("1")  # nothing armed -> plain buffering
        assert buf.pending == "1"


# ---------------------------------------------------------------- native


class TestNativeMenuMessage:
    def _layer(self, hint: str) -> ToolLayer:
        return ToolLayer(
            dapr=FakeDapr(),  # type: ignore[arg-type]
            settings=_settings(),
            ctx=_ctx(),
            session=SessionState(conversation_id="conv-1", site_slug="demo"),
            native_keypad=True,
            ivr_menu_hint=hint,
        )

    async def test_keypad_native_message_gains_menu_sentence(self):
        hint = ivr_menu.native_menu_sentence(ivr_menu.DEFAULT_MENU)
        layer = self._layer(hint)
        result = await layer.dispatch("read_keypad_entry", {})
        assert result["status"] == "keypad_native"
        assert "keypad menu" in result["message"]
        assert "1 = book an appointment" in result["message"]
        assert "3 = speak to a team member" in result["message"]
        # W93 relay contract still intact
        assert "verify_caller_code" in result["message"]

    async def test_keypad_native_message_unchanged_without_hint(self):
        layer = self._layer("")
        result = await layer.dispatch("read_keypad_entry", {})
        assert result["status"] == "keypad_native"
        assert "keypad menu" not in result["message"]

    def test_native_menu_sentence_empty_menu(self):
        assert ivr_menu.native_menu_sentence(()) == ""


# ---------------------------------------------------------------- config


class TestIvrMenuConfig:
    def test_defaults(self, monkeypatch):
        monkeypatch.delenv("IVR_MENU_ENABLED", raising=False)
        monkeypatch.delenv("IVR_MENU", raising=False)
        settings = load_settings()
        assert settings.ivr_menu_enabled is True
        assert settings.ivr_menu == ""

    def test_env_override(self, monkeypatch):
        monkeypatch.setenv("IVR_MENU_ENABLED", "0")
        monkeypatch.setenv("IVR_MENU", "none")
        settings = load_settings()
        assert settings.ivr_menu_enabled is False
        assert settings.ivr_menu == "none"

    def test_session_state_field_defaults_none(self):
        session = SessionState(conversation_id="c", site_slug="demo")
        assert session.ivr_menu is None
