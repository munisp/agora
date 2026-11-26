"""2026-11-20: Yoruba/Hausa/Igbo call quality — what callers say reaches the receptionist in English (with the call's
context), replies are said all in the caller's language (phone numbers in native number words), and the clearest of
several YarnGPT takes is played."""
import base64
import json

import httpx
import pytest

from app.translation import (
    LLMTranslator,
    clean_spoken,
    digit_words_to_digits,
    emails_spoken,
    native_digits,
    process_inbound_text,
)
from app.tts_providers.naija import NaijaSpeechTTS


def test_digit_words_back_to_digits_keep_grouping():
    t = "phone zero eight zero two, five five five, four four one seven. Is that correct? One moment."
    assert digit_words_to_digits(t) == "phone 0802 555 4417. Is that correct? One moment."


def test_short_word_runs_are_left_alone():
    assert digit_words_to_digits("one moment, two people") == "one moment, two people"


@pytest.mark.parametrize("lang,first", [("yo", "òdo, ẹ̀jọ"), ("ha", "sifili, takwas"), ("ig", "efu, asatọ")])
def test_native_digits_for_phone_numbers(lang, first):
    out = native_digits("x 0802 555 4417.", lang)
    assert out.startswith("x " + first)
    assert "; " in out and not any(c.isdigit() for c in out)


def test_short_numbers_left_to_the_translation():
    assert native_digits("ní aago 12", "yo") == "ní aago 12"


def test_emails_spoken():
    assert emails_spoken("email kemi.ojo@gmail.com.") == "email kemi dot ojo at gmail dot com."


def test_foreign_script_dropped():
    assert clean_spoken("Ṣé ìתẹ́ mìíràn wà?") == "Ṣé ìẹ́ mìíràn wà?"
    assert clean_spoken("Ina ɗaukar ƙaramin aiki") == "Ina ɗaukar ƙaramin aiki"


def _chat_client(captured: list, reply: str) -> httpx.AsyncClient:
    def handler(request: httpx.Request) -> httpx.Response:
        captured.append(json.loads(request.content))
        return httpx.Response(200, json={"choices": [{"message": {"content": reply}}]})

    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


async def test_inbound_uses_the_caller_prompt_with_call_context():
    seen: list = []
    ctx = lambda: {"business": "Leadway Pensure PFA", "industry": "pension-fund",  # noqa: E731
                   "services": ["RSA Registration & Onboarding Call"], "last_line": "How can I help you today?"}
    tr = LLMTranslator("http://llm/v1", "m", client=_chat_client(seen, "I want to open a pension account."), context=ctx)
    res = await process_inbound_text("okú rò oyàn o bẹ iṣẹ tutu", tr, path="voice", language_hint="yo")
    assert res.text == "I want to open a pension account." and res.detected_language == "yo"
    system = seen[0]["messages"][0]["content"]
    assert "Leadway Pensure PFA" in system and "RSA Registration" in system and "How can I help you today?" in system
    assert "speech recognition" in system


async def test_hint_beats_spelling_heuristics_for_hausa_without_hooked_letters():
    seen: list = []
    tr = LLMTranslator("http://llm/v1", "m", client=_chat_client(seen, "I just started a new job."))
    res = await process_inbound_text("na fara sabon aiki yanzu", tr, path="voice", language_hint="ha")
    assert res.detected_language == "ha" and res.translated


async def test_unclear_becomes_a_plain_note():
    tr = LLMTranslator("http://llm/v1", "m", client=_chat_client([], "(unclear)"))
    res = await process_inbound_text("go se gon jodabo", tr, path="voice", language_hint="yo")
    assert "could not be made out" in res.text


async def test_outbound_prompt_keeps_only_names_and_emails_in_english():
    seen: list = []
    tr = LLMTranslator("http://llm/v1", "m", client=_chat_client(seen, "Ẹ dúró díẹ̀."))
    await tr.translate("One moment while I check.", "en", "yo")
    system = seen[0]["messages"][0]["content"]
    assert "Keep in English ONLY" in system and "digit strings exactly as digits" in system
    assert len(seen) == 1  # no review pass by default (it added 5-10 s and undid good translations)


async def test_best_of_three_takes_plays_the_clearest():
    takes = [b"RIFF-take-0", b"RIFF-take-1", b"RIFF-take-2"]
    heard = {takes[0]: "mumble", takes[1]: "e ku abo si leadway", takes[2]: "e ku"}

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/tts_batch":
            assert len(json.loads(request.content)["items"]) == 3
            return httpx.Response(200, json={"wavs": [base64.b64encode(t).decode() for t in takes]})
        if request.url.path == "/asr":
            return httpx.Response(200, json={"text": heard[request.content]})
        return httpx.Response(404)

    tts = NaijaSpeechTTS("http://naija", client=httpx.AsyncClient(transport=httpx.MockTransport(handler)), takes=3)
    assert await tts.synthesize("Ẹ kú àbọ̀ sí Leadway", "yoruba_male2", "yo") == takes[1]


async def test_english_and_pidgin_keep_a_single_take():
    paths: list = []

    def handler(request: httpx.Request) -> httpx.Response:
        paths.append(request.url.path)
        return httpx.Response(200, content=b"RIFF-one")

    tts = NaijaSpeechTTS("http://naija", client=httpx.AsyncClient(transport=httpx.MockTransport(handler)), takes=3)
    assert await tts.synthesize("Hello", "", "pcm") == b"RIFF-one" and paths == ["/tts"]


async def test_batch_failure_falls_back_to_one_take():
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/tts_batch":
            return httpx.Response(500)
        return httpx.Response(200, content=b"RIFF-one")

    tts = NaijaSpeechTTS("http://naija", client=httpx.AsyncClient(transport=httpx.MockTransport(handler)), takes=3)
    assert await tts.synthesize("Ẹ kú àbọ̀", "", "yo") == b"RIFF-one"
