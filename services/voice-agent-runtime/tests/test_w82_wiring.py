"""SPEC-W82 A2/A3/A4.2/A4.4 wiring tests (mock-only — no network/GPU/creds).

Covers:
- chat path: translation BEFORE route_intent (spy ordering proof),
  active_language SET on the chat path (previously never set),
  detected_language/original_text stashed on the per-turn ctx, code-switch
  addendum in the system prompt, outbound reply translation, disabled-flag
  no-op (VOICE_TRANSLATE_ENABLED=0);
- voice path: post-STT hook (pcm transcript -> en seam, ctx stash, chain
  language + voice follow), FallbackTTS.language set from the session
  active language (chain.py:113 gap), pack voice:/i18n: precedence
  (ctx.tts_voice -> pack map -> TTS_VOICE_MAP -> PIPER_VOICE_MAP), unknown
  pack provider voice id ignored with fallback, pre-TTS transform
  (outbound translation + tts_normalize + pronunciation);
- STT hints (A4.4): config parsing, forced language= + initial_prompt
  plumbed into WhisperSTTNode.recognize, unset no-op.
"""

from __future__ import annotations

import sys
from types import SimpleNamespace

import pytest

try:  # pragma: no cover - import shim (real SDK absent in the unit-test env)
    import app.livekit_worker as lw
except ModuleNotFoundError:  # pragma: no cover
    from test_live_tts_chain import _install_livekit_stubs

    _install_livekit_stubs()
    import app.livekit_worker as lw

from app import chat as chat_module
from app import metrics
from app.chat import ChatService
from app.config import Settings, parse_stt_hints
from app.multilang import LANGUAGE_NAMES, locale_instruction
from app.pronunciation import parse_tenant_lexicons
from app.session_state import SessionStore
from app.tenant_context import TenantContext
from app.translation import TranslationResult
from app.tts_providers.chain import FallbackTTS
from conftest import FakeDapr


@pytest.fixture(autouse=True)
def _clean_state():
    metrics.reset_registry()
    lw._PREWARMED.clear()
    yield
    lw._PREWARMED.clear()
    metrics.reset_registry()


def _ctx(**over) -> TenantContext:
    base = dict(
        site_slug="acme",
        tenant_id="00000000-0000-0000-0000-000000000001",
        tenant_slug="acme",
        display_name="Acme",
        locale="en-US",
    )
    base.update(over)
    return TenantContext(**base)


# ---------------------------------------------------------------------------
# Chat path (A2 inbound ordering, A3 outbound)
# ---------------------------------------------------------------------------
class _MappingTranslator:
    provider = "mock-map"

    def __init__(self, mapping):
        self._mapping = dict(mapping)
        self.calls: list[tuple[str, str, str]] = []

    async def translate(self, text, source_lang, target_lang):
        self.calls.append((text, source_lang, target_lang))
        return TranslationResult(
            text=self._mapping.get(text, text),
            source_lang=source_lang,
            target_lang=target_lang,
            provider=self.provider,
        )


class _FakeLLM:
    def __init__(self, reply="ok"):
        self._reply = reply

    async def chat_with_tools(self, messages, tools):
        return SimpleNamespace(content=self._reply, tool_calls=[])


def _chat_service(monkeypatch, settings, translator, ctx):
    seen = {}

    async def fake_fetch(dapr, settings_, site_slug):
        seen["ctx"] = ctx
        return ctx

    monkeypatch.setattr(chat_module, "fetch_tenant_context", fake_fetch)
    service = ChatService(
        settings=settings,
        dapr=FakeDapr(),
        llm=_FakeLLM(),
        sessions=SessionStore(),
        translator=translator,
    )
    return service, seen


async def test_chat_translation_before_route_intent(monkeypatch):
    """Ordering proof: route_intent MUST receive the translated (English)
    message — only possible when translation runs first."""
    translator = _MappingTranslator({"Wetin dey happen?": "What is happening?"})
    service, seen = _chat_service(monkeypatch, Settings(), translator, _ctx())

    routed_with = {}
    real_route = chat_module.route_intent

    def spy_route(message, agents):
        routed_with["message"] = message
        return real_route(message, agents)

    monkeypatch.setattr(chat_module, "route_intent", spy_route)
    resp = await service.handle_message(
        site_slug="acme", message="Wetin dey happen?", conversation_id=None
    )
    # Translation happened BEFORE routing.
    assert routed_with["message"] == "What is happening?"
    # The LLM-facing history carries the translated text.
    history = service._histories.get(resp["conversation_id"])
    user_msgs = [m for m in history if m["role"] == "user"]
    assert user_msgs[-1]["content"] == "What is happening?"
    # Stash on the per-turn context.
    ctx = seen["ctx"]
    assert ctx.detected_language == "pcm"
    assert ctx.original_text == "Wetin dey happen?"
    # active_language is SET on the chat path (previously never set).
    session = service._sessions.get(resp["conversation_id"])
    assert session.active_language == "pcm"


async def test_chat_active_language_not_set_for_english(monkeypatch):
    translator = _MappingTranslator({})
    service, _seen = _chat_service(monkeypatch, Settings(), translator, _ctx())
    resp = await service.handle_message(
        site_slug="acme", message="Hello there", conversation_id=None
    )
    session = service._sessions.get(resp["conversation_id"])
    assert session.active_language is None
    assert translator.calls == []


async def test_chat_outbound_reply_translated(monkeypatch):
    translator = _MappingTranslator({"ok": "ok na, wetin else?"})
    service, _seen = _chat_service(monkeypatch, Settings(), translator, _ctx())
    resp = await service.handle_message(
        site_slug="acme", message="Wetin dey?", conversation_id=None
    )
    # Caller-visible reply is translated en -> pcm...
    assert resp["reply"] == "ok na, wetin else?"
    # ...while the history keeps the English reply.
    history = service._histories.get(resp["conversation_id"])
    assistant = [m for m in history if m["role"] == "assistant"]
    assert assistant[-1]["content"] == "ok"


async def test_chat_codeswitch_turn_not_translated_and_instructs(monkeypatch):
    translator = _MappingTranslator({})
    service, seen = _chat_service(monkeypatch, Settings(), translator, _ctx())
    routed_with = {}

    def _spy_route(message, agents):
        routed_with["message"] = message
        return None

    monkeypatch.setattr(chat_module, "route_intent", _spy_route)
    mixed = "Wetin be the earliest appointment for tomorrow morning?"
    resp = await service.handle_message(
        site_slug="acme", message=mixed, conversation_id=None
    )
    # Mixed turn passes through UNTRANSLATED (no inbound translate call for
    # it; the outbound en -> pcm reply translation is a separate seam).
    assert routed_with["message"] == mixed
    assert all(call[0] != mixed for call in translator.calls)
    # ...annotated on the context...
    ctx = seen["ctx"]
    assert ctx.code_switch is True
    assert ctx.code_switch_segments
    session = service._sessions.get(resp["conversation_id"])
    assert session.active_language == "pcm"
    # ...and the system prompt tells the LLM to answer in the same register.
    history = service._histories.get(resp["conversation_id"])
    assert "code-switching" in history[0]["content"]
    assert "Pidgin" in history[0]["content"]


async def test_chat_disabled_flag_is_noop(monkeypatch):
    translator = _MappingTranslator({"Wetin dey?": "What is up?"})
    settings = Settings(voice_translate_enabled=False)
    service, seen = _chat_service(monkeypatch, settings, translator, _ctx())
    routed_with = {}

    def _spy_route(message, agents):
        routed_with["message"] = message
        return None

    monkeypatch.setattr(chat_module, "route_intent", _spy_route)
    resp = await service.handle_message(
        site_slug="acme", message="Wetin dey?", conversation_id=None
    )
    assert routed_with["message"] == "Wetin dey?"  # untranslated
    assert translator.calls == []
    assert seen["ctx"].detected_language == ""
    session = service._sessions.get(resp["conversation_id"])
    assert session.active_language is None


class _FakeStreamLLM:
    def __init__(self, reply):
        self._reply = reply

    async def stream_with_tools(self, messages, tools):
        reply = self._reply

        async def gen():
            yield SimpleNamespace(
                choices=[SimpleNamespace(delta=SimpleNamespace(content=reply, tool_calls=[]))]
            )

        return gen()


async def test_chat_stream_done_frame_carries_translated_reply(monkeypatch):
    translator = _MappingTranslator({"all good": "everything dey alright"})
    ctx = _ctx()

    async def fake_fetch(dapr, settings_, site_slug):
        return ctx

    monkeypatch.setattr(chat_module, "fetch_tenant_context", fake_fetch)
    service = ChatService(
        settings=Settings(),
        dapr=FakeDapr(),
        llm=_FakeStreamLLM("all good"),
        sessions=SessionStore(),
        translator=translator,
    )
    events = [
        e
        async for e in service.handle_message_stream(
            site_slug="acme", message="Wetin dey?", conversation_id=None
        )
    ]
    done = events[-1]
    assert done["done"] is True
    # Additive translated reply on the terminal frame (deltas streamed en).
    assert done["reply"] == "everything dey alright"


# ---------------------------------------------------------------------------
# Voice path (A2 post-STT hook, A3/A4.2/A4.3/A4.5 pre-TTS)
# ---------------------------------------------------------------------------
class _FakeAgent:
    def __init__(self, **kwargs):
        self.kwargs = kwargs

    @property
    def chat_ctx(self):
        return self.kwargs["chat_ctx"]

    async def say(self, text, allow_interruptions=False):
        return None

    def start(self, room):
        pass

    def on(self, event, cb=None):
        pass


async def _build_agent(monkeypatch, settings, ctx, tts_impl=None):
    lw._PREWARMED["tts"] = tts_impl or lw._build_tts(settings)

    async def _fake_fetch(dapr, settings_, site_slug):
        return ctx

    monkeypatch.setattr(lw, "fetch_tenant_context", _fake_fetch)
    monkeypatch.setattr(lw, "VoicePipelineAgent", _FakeAgent)
    monkeypatch.setattr(
        lw, "silero", SimpleNamespace(VAD=SimpleNamespace(load=lambda **kw: object()))
    )
    monkeypatch.setattr(
        lw, "lk_openai", SimpleNamespace(LLM=lambda **kw: SimpleNamespace(**kw))
    )
    agent, tenant_ctx, session = await lw.build_voice_agent(
        settings, FakeDapr(), ctx.site_slug, "conv-1"
    )
    return agent.kwargs, tenant_ctx, session


async def test_worker_sets_chain_language_at_bootstrap(monkeypatch):
    """A4.2 CRITICAL FIX: FallbackTTS.language is SET from the session
    active language (chain.py:113 gap closed)."""
    kwargs, _ctx_out, session = await _build_agent(monkeypatch, Settings(), _ctx())
    chain = kwargs["tts"]._impl
    assert isinstance(chain, FallbackTTS)
    assert chain.language == "en"
    assert session.active_language == "en"
    # The pre-TTS transform and post-STT hook are wired.
    assert kwargs["tts"]._on_text is not None
    assert kwargs["stt"]._on_transcript is not None


async def test_worker_pack_i18n_default_language(monkeypatch):
    ctx = _ctx(pack_default_language="yo")
    kwargs, _c, session = await _build_agent(monkeypatch, Settings(), ctx)
    assert session.active_language == "yo"
    assert kwargs["tts"]._impl.language == "yo"


async def test_worker_pack_voice_map_beats_tts_voice_map(monkeypatch):
    settings = Settings(
        tts_provider_chain="mms,piper",
        tts_voice_map={"pcm": "mms:pcm-env"},
    )
    ctx = _ctx(pack_voice_map={"pcm": "mms:pcm-pack"})
    kwargs, _c, _s = await _build_agent(monkeypatch, settings, ctx)
    kwargs["stt"]._on_language("pcm")
    # Pack per-language map wins over TTS_VOICE_MAP.
    assert kwargs["tts"]._impl.voice == "mms:pcm-pack"


async def test_worker_tts_voice_map_used_without_pack(monkeypatch):
    settings = Settings(
        tts_provider_chain="mms,piper",
        tts_voice_map={"pcm": "mms:pcm-env"},
    )
    kwargs, _c, _s = await _build_agent(monkeypatch, settings, _ctx())
    kwargs["stt"]._on_language("pcm")
    assert kwargs["tts"]._impl.voice == "mms:pcm-env"


async def test_worker_ctx_tts_voice_beats_pack(monkeypatch):
    ctx = _ctx(pack_voice_map={"en": "mms:pcm-pack"})
    ctx.tts_voice = "azure:en-NG-EzinneNeural"  # per-tenant override seam
    kwargs, _c, _s = await _build_agent(monkeypatch, Settings(), ctx)
    assert kwargs["tts"]._impl.voice == "azure:en-NG-EzinneNeural"


async def test_worker_unknown_pack_provider_ignored(monkeypatch):
    """Unknown pack voice ids (provider not in the chain) are ignored with a
    warning — resolution falls through, never crashes."""
    settings = Settings(tts_provider_chain="piper")  # spitch NOT in chain
    ctx = _ctx(pack_voice_map={"pcm": "spitch:sade"})
    kwargs, _c, _s = await _build_agent(monkeypatch, settings, ctx)
    kwargs["stt"]._on_language("pcm")
    # Ignored: piper default stands (chain language still tracks).
    assert kwargs["tts"]._impl.voice == settings.piper_voice


async def test_worker_post_stt_hook_pcm_turn(monkeypatch):
    """Post-STT hook: a Pidgin transcript is detected (whisper reports en),
    stashed, and drives session language + chain language + locale swap."""
    settings = Settings(
        tts_provider_chain="mms,piper", tts_voice_map={"pcm": "mms:pcm"}
    )
    kwargs, ctx, session = await _build_agent(monkeypatch, settings, _ctx())
    on_transcript = kwargs["stt"]._on_transcript
    out = await on_transcript("Wetin dey happen?", "en")
    # MockTranslator echoes -> transcript text passes through the seam.
    assert out == "Wetin dey happen?"
    assert ctx.detected_language == "pcm"
    assert ctx.original_text == "Wetin dey happen?"
    assert session.active_language == "pcm"
    chain = kwargs["tts"]._impl
    assert chain.language == "pcm"
    assert chain.voice == "mms:pcm"
    # Locale instruction swapped into the live chat context.
    system = kwargs["chat_ctx"].messages[0].content[0]
    assert "Nigerian Pidgin" in system
    assert 'voice_language_detected_total{lang="pcm",path="voice"}' in metrics.render()


async def test_worker_post_stt_hook_english_passthrough(monkeypatch):
    kwargs, ctx, session = await _build_agent(monkeypatch, Settings(), _ctx())
    out = await kwargs["stt"]._on_transcript("I want to book please", "en")
    assert out == "I want to book please"
    assert ctx.detected_language == ""
    assert session.active_language == "en"


async def test_worker_pre_tts_transform_normalize_and_pronunciation(monkeypatch):
    settings = Settings(
        voice_pronunciation_lexicon=parse_tenant_lexicons(
            '{"acme": {"Adaeze": "ah-DAH-ay-zeh"}}'
        )
    )
    kwargs, _ctx_out, session = await _build_agent(monkeypatch, settings, _ctx())
    session.active_language = "pcm"  # as the post-STT hook would set
    on_text = kwargs["tts"]._on_text
    # MockTranslator echoes (outbound seam engaged), then normalization and
    # the pronunciation lexicon rewrite the surface forms.
    out = await on_text("Dr Adaeze charges ₦5,000.")
    assert "five thousand naira" in out
    assert "ah-DAH-ay-zeh" in out
    assert "₦" not in out


async def test_worker_pre_tts_transform_fail_open(monkeypatch):
    settings = Settings(voice_translate_enabled=False)
    kwargs, _c, _s = await _build_agent(monkeypatch, settings, _ctx())
    out = await kwargs["tts"]._on_text("hello there")
    assert out == "hello there"


# ---------------------------------------------------------------------------
# PiperTTSNode on_text seam (unit level)
# ---------------------------------------------------------------------------
class _FakeTTSImpl:
    sample_rate = 22050

    def __init__(self):
        self.texts: list[str] = []

    async def synthesize_pcm(self, text: str) -> bytes:
        self.texts.append(text)
        return b"\x01\x00" * 8


async def test_tts_node_applies_on_text_before_dispatch():
    impl = _FakeTTSImpl()

    async def shout(text):
        return text.upper()

    node = lw.PiperTTSNode(impl, chunked=True, on_text=shout)
    chunks = [a async for a in node._stream("hello")]
    assert chunks
    assert impl.texts == ["HELLO"]


async def test_tts_node_on_text_failure_falls_back():
    impl = _FakeTTSImpl()

    async def boom(text):
        raise RuntimeError("transform exploded")

    node = lw.PiperTTSNode(impl, chunked=True, on_text=boom)
    chunks = [a async for a in node._stream("hello")]
    assert chunks
    assert impl.texts == ["hello"]  # original text spoken


# ---------------------------------------------------------------------------
# STT hints (A4.4)
# ---------------------------------------------------------------------------
def test_parse_stt_hints_valid_and_tolerant():
    hints = parse_stt_hints(
        '{"acme": {"language": "yo", "initial_prompt": "Acme Clinic"},'
        ' "bad": {"language": "!!"}, "empty": {}}'
    )
    assert hints == {"acme": {"language": "yo", "initial_prompt": "Acme Clinic"}}
    assert parse_stt_hints("") == {}
    assert parse_stt_hints("not json") == {}
    assert parse_stt_hints('["list"]') == {}


class _FakeSTTImpl:
    def __init__(self):
        self.calls: list[dict] = []
        self.last_detected_language = None

    async def transcribe_pcm(
        self, pcm, *, sample_rate, channels=1, language=None, initial_prompt=None
    ):
        self.calls.append(
            {"language": language, "initial_prompt": initial_prompt}
        )
        return "sannu"


def _frame():
    return SimpleNamespace(
        data=b"\x00\x00" * 1600, sample_rate=16000, num_channels=1
    )


async def test_stt_node_forces_hint_language_and_prompt():
    impl = _FakeSTTImpl()
    node = lw.WhisperSTTNode(
        impl, hints={"language": "yo", "initial_prompt": "Acme Clinic, braids"}
    )
    event = await node.recognize(_frame())
    assert impl.calls == [
        {"language": "yo", "initial_prompt": "Acme Clinic, braids"}
    ]
    assert event.alternatives[0].language == "yo"
    assert event.alternatives[0].text == "sannu"


async def test_stt_node_unset_hints_noop():
    impl = _FakeSTTImpl()
    node = lw.WhisperSTTNode(impl)
    await node.recognize(_frame())
    assert impl.calls == [{"language": None, "initial_prompt": None}]


async def test_stt_node_explicit_language_arg_wins_over_hint():
    impl = _FakeSTTImpl()
    node = lw.WhisperSTTNode(impl, hints={"language": "yo"})
    await node.recognize(_frame(), language="ha")
    assert impl.calls[0]["language"] == "ha"


async def test_worker_wires_stt_hints_by_slug(monkeypatch):
    settings = Settings(
        voice_stt_hints={"acme": {"language": "yo", "initial_prompt": "Acme"}}
    )
    kwargs, _c, _s = await _build_agent(monkeypatch, settings, _ctx())
    assert kwargs["stt"]._hints == {"language": "yo", "initial_prompt": "Acme"}


async def test_worker_no_hints_noop(monkeypatch):
    kwargs, _c, _s = await _build_agent(monkeypatch, Settings(), _ctx())
    assert kwargs["stt"]._hints is None


# ---------------------------------------------------------------------------
# multilang yo/ig/ha coverage (A2 locale instruction)
# ---------------------------------------------------------------------------
def test_language_names_cover_nigerian_languages():
    assert LANGUAGE_NAMES["yo"] == "Yoruba"
    assert LANGUAGE_NAMES["ig"] == "Igbo"
    assert LANGUAGE_NAMES["ha"] == "Hausa"


@pytest.mark.parametrize(
    ("code", "name"), [("yo", "Yoruba"), ("ig", "Igbo"), ("ha", "Hausa")]
)
def test_locale_instruction_names_language(code, name):
    text = locale_instruction(code)
    assert f"speaking {name}" in text
    assert f"Respond in {name}" in text


# ---------------------------------------------------------------------------
# tenant_context pack voice:/i18n: parsing (A4.2 prepared seam)
# ---------------------------------------------------------------------------
def test_apply_pack_reads_voice_and_i18n_blocks():
    from app.tenant_context import _apply_pack

    ctx = _ctx()
    _apply_pack(
        ctx,
        {
            "industry": "salon",
            "pack": {
                "voice": {
                    "languages": {"pcm": "mms:pcm", "yo": "mms:yor", "bad key !": "x"},
                    "lexicon": {"Adaeze": "ah-DAH-ay-zeh"},
                },
                "i18n": {"default_language": "yo"},
            },
        },
    )
    assert ctx.pack_voice_map == {"pcm": "mms:pcm", "yo": "mms:yor"}
    assert ctx.pack_default_language == "yo"
    assert ctx.pack_pronunciation_lexicon["Adaeze"].respell == "ah-DAH-ay-zeh"


def test_apply_pack_voice_flat_form_and_garbage():
    from app.tenant_context import _apply_pack

    ctx = _ctx()
    _apply_pack(
        ctx,
        {"pack": {"voice": {"pcm": "mms:pcm"}, "i18n": {"default_language": "!!"}}},
    )
    assert ctx.pack_voice_map == {"pcm": "mms:pcm"}
    assert ctx.pack_default_language == ""  # invalid -> dropped, never fatal


def test_apply_pack_without_voice_block_untouched():
    from app.tenant_context import _apply_pack

    ctx = _ctx()
    _apply_pack(ctx, {"industry": "salon", "pack": {"agentPersona": "hi"}})
    assert ctx.pack_voice_map == {}
    assert ctx.pack_default_language == ""
    assert ctx.pack_pronunciation_lexicon == {}
