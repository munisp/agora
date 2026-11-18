"""SPEC-W82 A1/A2/A3/A5: translation layer unit tests (mock-only — no
network, no GPU, no credentials).

Covers:
- detect_language: table-driven over en/pcm/yo/ig/ha incl. tricky short
  texts, diacritics in precomposed AND NFD combining forms, Hausa hooked
  letters, determinism, and the < 1 ms on 500 chars budget;
- HTTPTranslator: fail-open on timeout / 5xx / bad JSON / missing key /
  unreachable URL (original text returned, error metric incremented);
- MockTranslator: echo default for any lang pair;
- build_translator: TRANSLATE_MOCK=1 forces mock regardless of provider,
  provider selection + unknown-provider fallback, config parsing via
  load_settings (VOICE_TRANSLATE_ENABLED/TRANSLATE_*);
- process_inbound_text / process_outbound_text turn semantics: translation
  before routing, fail-open, code-switch pass-through, disabled-flag no-op,
  skip-when-already-non-English, metrics labels.
"""

from __future__ import annotations

import time
import unicodedata

import httpx
import pytest

from app import metrics
from app.config import Settings, load_settings
from app.translation import (
    HTTPTranslator,
    MockTranslator,
    TranslationResult,
    build_translator,
    detect_language,
    process_inbound_text,
    process_outbound_text,
)


@pytest.fixture(autouse=True)
def _fresh_registry():
    metrics.reset_registry()
    yield
    metrics.reset_registry()


# ---------------------------------------------------------------------------
# detect_language: table tests (>= 15 utterances)
# ---------------------------------------------------------------------------
DETECTOR_TABLE = [
    # English baseline (no signal -> en)
    ("Hello, I would like to book an appointment for tomorrow.", "en"),
    ("What time do you open on Saturday?", "en"),
    ("", "en"),
    ("Please hold on for a moment.", "en"),
    # Nigerian Pidgin
    ("Wetin dey happen for this place?", "pcm"),
    ("How far? I wan book appointment.", "pcm"),
    ("Abeg, no dey waste my time.", "pcm"),
    ("Na so e be, no wahala at all.", "pcm"),
    ("Una dey open on Sunday?", "pcm"),
    # Yoruba — diacritics (precomposed)
    ("Ẹ kaaro, mo fe book appointment.", "yo"),
    ("Ṣe o wa ni iṣẹ lọla?", "yo"),
    # Yoruba — lexicon only, no diacritics (tricky short text)
    ("Bawo ni?", "yo"),
    ("Jowo, kini o nlo?", "yo"),
    # Igbo — diacritics
    ("Ị kwenu, achọrọ m ịhụ ya.", "ig"),
    ("Kedu ka ị mere?", "ig"),
    # Igbo — lexicon only (tricky short text)
    ("Biko, dalu rinne.", "ig"),
    ("Ndeewo, kedu ihe o gini?", "ig"),
    # Hausa — hooked letters
    ("Sannu, ina son ɗakin karatu.", "ha"),
    ("Ƙun ba ni lokaci.", "ha"),
    # Hausa — lexicon only (tricky short text)
    ("Sannu! Yaya lafiya?", "ha"),
    ("Nagode, gobe sai yau.", "ha"),
]


@pytest.mark.parametrize(("text", "expected"), DETECTOR_TABLE)
def test_detect_language_table(text, expected):
    assert detect_language(text) == expected


def test_detect_language_nfd_combining_diacritics():
    # o/e/s with a COMBINING dot below (NFD form) must score like the
    # precomposed letters.
    nfd = unicodedata.normalize("NFD", "Ẹ kaaro ṣe o")
    assert "̣" in nfd  # sanity: really decomposed
    assert detect_language(nfd) == "yo"


def test_detect_language_deterministic_and_fast():
    text = "Wetin dey happen? " * 28  # ~500 chars
    assert len(text) >= 500
    first = detect_language(text)
    for _ in range(50):
        assert detect_language(text) == first
    start = time.perf_counter()
    for _ in range(100):
        detect_language(text)
    per_call_ms = (time.perf_counter() - start) / 100 * 1000
    assert per_call_ms < 1.0, f"detector too slow: {per_call_ms:.3f} ms/call"


# ---------------------------------------------------------------------------
# MockTranslator / build_translator / config parsing
# ---------------------------------------------------------------------------
async def test_mock_translator_echoes_any_pair():
    mock = MockTranslator()
    result = await mock.translate("Wetin dey?", "pcm", "en")
    assert isinstance(result, TranslationResult)
    assert result.text == "Wetin dey?"
    assert result.provider == "mock"
    assert result.source_lang == "pcm"
    assert result.target_lang == "en"
    assert result.cached is False
    result2 = await mock.translate("Hello", "en", "ha")
    assert result2.text == "Hello"


def test_build_translator_mock_default_regardless_of_provider():
    settings = Settings(translate_provider="http", translate_url="http://t:1")
    # TRANSLATE_MOCK defaults to True -> mock wins even over provider=http.
    assert isinstance(build_translator(settings), MockTranslator)


def test_build_translator_http_when_mock_off():
    settings = Settings(
        translate_mock=False,
        translate_provider="http",
        translate_url="http://translator:9000",
        translate_api_key="k",
        translate_timeout_s=1.5,
    )
    translator = build_translator(settings)
    assert isinstance(translator, HTTPTranslator)
    assert translator.provider == "http"


def test_build_translator_unknown_provider_falls_back_to_mock():
    settings = Settings(translate_mock=False, translate_provider="klingon")
    assert isinstance(build_translator(settings), MockTranslator)


def test_config_env_parsing(monkeypatch):
    monkeypatch.setenv("VOICE_TRANSLATE_ENABLED", "0")
    monkeypatch.setenv("TRANSLATE_PROVIDER", "http")
    monkeypatch.setenv("TRANSLATE_MOCK", "0")
    monkeypatch.setenv("TRANSLATE_URL", "http://translator:9000/translate")
    monkeypatch.setenv("TRANSLATE_API_KEY", "sekret")
    monkeypatch.setenv("TRANSLATE_TIMEOUT_S", "1.25")
    settings = load_settings()
    assert settings.voice_translate_enabled is False
    assert settings.translate_provider == "http"
    assert settings.translate_mock is False
    assert settings.translate_url == "http://translator:9000/translate"
    assert settings.translate_api_key == "sekret"
    assert settings.translate_timeout_s == 1.25


def test_config_defaults():
    settings = Settings()
    assert settings.voice_translate_enabled is True
    assert settings.translate_provider == "mock"
    assert settings.translate_mock is True
    assert settings.translate_url == ""
    assert settings.translate_api_key == ""
    assert settings.translate_timeout_s == 3.0


# ---------------------------------------------------------------------------
# HTTPTranslator fail-open (httpx.MockTransport — no network)
# ---------------------------------------------------------------------------
def _http_translator(handler, **kw):
    transport = httpx.MockTransport(handler)
    client = httpx.AsyncClient(transport=transport)
    return HTTPTranslator(
        kw.pop("url", "http://translator.test/translate"), client=client, **kw
    )


async def test_http_translator_success():
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.headers["Authorization"] == "Bearer key-1"
        import json as _json

        body = _json.loads(request.content)
        assert body == {"text": "Sannu", "source_lang": "ha", "target_lang": "en"}
        return httpx.Response(200, json={"text": "Hello"})

    translator = _http_translator(handler, api_key="key-1")
    result = await translator.translate("Sannu", "ha", "en")
    assert result.text == "Hello"
    assert result.provider == "http"
    # No error metric series on success (HELP/TYPE lines always render).
    assert "voice_translation_errors_total{" not in metrics.render()


async def test_http_translator_fail_open_on_5xx():
    translator = _http_translator(lambda req: httpx.Response(500, text="boom"))
    result = await translator.translate("Sannu", "ha", "en")
    assert result.text == "Sannu"  # original text returned
    assert 'voice_translation_errors_total{provider="http"} 1' in metrics.render()


async def test_http_translator_fail_open_on_bad_json():
    translator = _http_translator(
        lambda req: httpx.Response(200, text="<not json>")
    )
    result = await translator.translate("Sannu", "ha", "en")
    assert result.text == "Sannu"
    assert 'voice_translation_errors_total{provider="http"} 1' in metrics.render()


async def test_http_translator_fail_open_on_missing_text_key():
    translator = _http_translator(lambda req: httpx.Response(200, json={"ok": True}))
    result = await translator.translate("Sannu", "ha", "en")
    assert result.text == "Sannu"


async def test_http_translator_fail_open_on_timeout():
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ReadTimeout("slow", request=request)

    translator = _http_translator(handler, timeout_s=0.05)
    result = await translator.translate("Sannu", "ha", "en")
    assert result.text == "Sannu"
    assert 'voice_translation_errors_total{provider="http"} 1' in metrics.render()


async def test_http_translator_fail_open_without_url():
    translator = HTTPTranslator("", api_key="")
    result = await translator.translate("Sannu", "ha", "en")
    assert result.text == "Sannu"
    assert 'voice_translation_errors_total{provider="http"} 1' in metrics.render()


async def test_http_translator_accepts_alternate_response_keys():
    translator = _http_translator(
        lambda req: httpx.Response(200, json={"translated_text": "How are you"})
    )
    result = await translator.translate("Bawo ni", "yo", "en")
    assert result.text == "How are you"


# ---------------------------------------------------------------------------
# process_inbound_text / process_outbound_text turn semantics
# ---------------------------------------------------------------------------
class _SpyTranslator:
    provider = "spy"

    def __init__(self, mapping=None, fail=False):
        self.calls: list[tuple[str, str, str]] = []
        self._mapping = mapping or {}
        self._fail = fail

    async def translate(self, text, source_lang, target_lang):
        self.calls.append((text, source_lang, target_lang))
        if self._fail:
            raise RuntimeError("translator exploded")
        return TranslationResult(
            text=self._mapping.get(text, text),
            source_lang=source_lang,
            target_lang=target_lang,
            provider=self.provider,
        )


async def test_inbound_translates_pure_pcm_to_en():
    spy = _SpyTranslator({"Wetin dey happen?": "What is happening?"})
    result = await process_inbound_text("Wetin dey happen?", spy, path="chat")
    assert result.detected_language == "pcm"
    assert result.original_text == "Wetin dey happen?"
    assert result.text == "What is happening?"
    assert result.translated is True
    assert result.code_switch is False
    assert spy.calls == [("Wetin dey happen?", "pcm", "en")]
    rendered = metrics.render()
    assert (
        'voice_translation_total{direction="inbound",provider="spy",'
        'source_lang="pcm",target_lang="en"} 1' in rendered
    )
    assert 'voice_language_detected_total{lang="pcm",path="chat"} 1' in rendered


async def test_inbound_english_passthrough_no_translation():
    spy = _SpyTranslator()
    result = await process_inbound_text("Hello, I want to book.", spy)
    assert result.detected_language == "en"
    assert result.text == "Hello, I want to book."
    assert spy.calls == []


async def test_inbound_fail_open_on_translator_error():
    spy = _SpyTranslator(fail=True)
    result = await process_inbound_text("Wetin dey happen?", spy)
    # Original text passes through; the turn is NOT failed.
    assert result.text == "Wetin dey happen?"
    assert 'voice_translation_errors_total{provider="spy"} 1' in metrics.render()


async def test_inbound_disabled_flag_is_noop():
    spy = _SpyTranslator()
    result = await process_inbound_text("Wetin dey happen?", spy, enabled=False)
    assert result.text == "Wetin dey happen?"
    assert result.detected_language == ""
    assert spy.calls == []
    assert "voice_language_detected_total{" not in metrics.render()


async def test_inbound_codeswitch_turn_not_translated():
    spy = _SpyTranslator()
    mixed = "Wetin be the earliest appointment for tomorrow morning?"
    result = await process_inbound_text(mixed, spy)
    assert result.detected_language == "pcm"
    assert result.code_switch is True
    assert result.text == mixed  # NOT force-translated wholesale
    assert result.segments  # segment annotation present
    assert spy.calls == []


async def test_outbound_translates_en_to_caller_language():
    spy = _SpyTranslator({"Your booking is confirmed.": "Booking don confirm."})
    out = await process_outbound_text("Your booking is confirmed.", "pcm", spy)
    assert out == "Booking don confirm."
    assert spy.calls == [("Your booking is confirmed.", "en", "pcm")]
    rendered = metrics.render()
    assert (
        'voice_translation_total{direction="outbound",provider="spy",'
        'source_lang="en",target_lang="pcm"} 1' in rendered
    )


async def test_outbound_skips_english_target_and_empty():
    spy = _SpyTranslator()
    assert await process_outbound_text("Hello", "en", spy) == "Hello"
    assert await process_outbound_text("Hello", None, spy) == "Hello"
    assert await process_outbound_text("", "pcm", spy) == ""
    assert spy.calls == []


async def test_outbound_skips_already_non_english_reply():
    spy = _SpyTranslator()
    out = await process_outbound_text("Wetin dey? Your booking don ready.", "pcm", spy)
    assert out == "Wetin dey? Your booking don ready."
    assert spy.calls == []  # already substantially non-English -> pass through


async def test_outbound_fail_open_english_on_error():
    spy = _SpyTranslator(fail=True)
    out = await process_outbound_text("Your booking is confirmed.", "yo", spy)
    assert out == "Your booking is confirmed."
    assert 'voice_translation_errors_total{provider="spy"} 1' in metrics.render()


async def test_outbound_disabled_flag_is_noop():
    spy = _SpyTranslator()
    out = await process_outbound_text("Your booking is confirmed.", "pcm", spy, enabled=False)
    assert out == "Your booking is confirmed."
    assert spy.calls == []


@pytest.mark.parametrize("lang", ["yo", "ig", "ha"])
async def test_inbound_translates_each_nigerian_language(lang):
    texts = {
        "yo": "Ẹ kaaro, mo fe book.",
        "ig": "Ị kwenu oke ụda.",
        "ha": "Sannu, ɗakin nan yaya?",
    }
    spy = _SpyTranslator({texts[lang]: "EN"})
    result = await process_inbound_text(texts[lang], spy, path="voice")
    assert result.detected_language == lang
    assert result.text == "EN"
    assert f'lang="{lang}",path="voice"' in metrics.render()
