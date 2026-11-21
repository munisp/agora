"""Think in English, speak Yoruba/Hausa/Igbo (NAIJA_THINK_IN_ENGLISH)."""
import asyncio
import json

import httpx

from app import call_language
from app.config import load_settings
from app.translation import LLMTranslator


def test_prompt_asks_for_english_and_tools_in_translated_languages():
    p = call_language.prompt_instruction("yo", think_in_english=True)
    assert "Write every reply in plain, simple ENGLISH" in p and "get_availability" in p and "Yoruba" in p
    # Pidgin is written by the model itself, as before
    assert "Reply ONLY in Nigerian Pidgin" in call_language.prompt_instruction("pcm", think_in_english=True)
    assert "Reply ONLY in Yoruba" in call_language.prompt_instruction("yo")  # mode off: unchanged
    assert call_language.prompt_instruction("en", think_in_english=True) == ""


def test_setting(monkeypatch):
    monkeypatch.setenv("NAIJA_THINK_IN_ENGLISH", "true")
    assert load_settings().naija_think_in_english is True
    monkeypatch.setenv("NAIJA_THINK_IN_ENGLISH", "0")
    assert load_settings().naija_think_in_english is False


def _translator(reply, status=200, seen=None):
    def handler(request):
        if seen is not None:
            seen.append(json.loads(request.content))
        return httpx.Response(status, json={"choices": [{"message": {"content": reply}}]})
    return LLMTranslator("http://llm/v1", "m", client=httpx.AsyncClient(transport=httpx.MockTransport(handler)))


def test_translates_caches_and_cleans():
    seen = []
    t = _translator('"Mo ti ṣètò ìpàdé yín.<0xC7><0xB8>"', seen=seen)
    r1 = asyncio.run(t.translate("You are booked.", "en", "yo"))
    r2 = asyncio.run(t.translate("You are booked.", "en", "yo"))
    assert r1.text == "Mo ti ṣètò ìpàdé yín." and r2.cached and len(seen) == 1
    system = seen[0]["messages"][0]["content"]
    assert "Yoruba" in system and "Mo ti ṣètò ìpàdé yín pẹ̀lú Aisha Bello" in system  # examples included


def test_fails_open_to_english():
    t = _translator("x", status=500)
    assert asyncio.run(t.translate("One moment.", "en", "ha")).text == "One moment."


def test_caller_date_and_mismatch_note():
    from types import SimpleNamespace as M

    from app.livekit_worker import caller_date, date_mismatch_note

    lines = [M(role="user", content="The Union Book in the Circle, Lana Tuesday"),
             M(role="assistant", content="next Tuesday, November twenty-fifth"),
             M(role="user", content="For my pension account registration call, please, Tuesday, 24 November, at 1 in the afternoon.")]
    assert caller_date(lines) == (11, 24)
    assert caller_date([M(role="user", content="Tuesday, the twenty-fourth of November, at noon")]) == (11, 24)
    assert caller_date([M(role="user", content="on November 3rd please")]) == (11, 3)
    assert caller_date([M(role="user", content="next Tuesday morning")]) is None
    note = date_mismatch_note(lines, "2026-11-25T13:00:00", "Africa/Lagos")
    assert note and "Tuesday 24 November" in note and "Wednesday 25 November" in note and "2026-11-24" in note
    assert date_mismatch_note(lines, "2026-11-24T13:00:00+01:00", "Africa/Lagos") is None
    assert date_mismatch_note([M(role="user", content="no date")], "2026-11-25T13:00:00", "Africa/Lagos") is None


def test_leaked_model_token_is_dropped():
    from app.livekit_worker import _LEAKED_TOKEN

    s = "Let me check Tuesday at twelve noon. lvisertguard I have found a slot for you."
    assert _LEAKED_TOKEN.sub("", s) == "Let me check Tuesday at twelve noon. I have found a slot for you."
    keep = "Thank you. Your appointment is booked. Aisha will call."
    assert _LEAKED_TOKEN.sub("", keep) == keep


def test_read_back_in_words_counts_as_confirmed():
    from types import SimpleNamespace as M

    from app.livekit_worker import read_back_confirmed

    lines = [M(role="assistant", content="I have Kemi Ojo, email kemi dot ojo at gmail dot com, phone zero eight zero "
                                         "two, five, five, five, four, four, one, seven. Is that all correct?"),
             M(role="user", content="Yes, that's correct.")]
    assert read_back_confirmed(lines, "08025554417")
    assert not read_back_confirmed(lines[:1] + [M(role="user", content="No, it's K E M I")], "08025554417")


def test_pidgin_set_claims_are_caught():
    from app.lk_tool_loop import booking_claim

    assert booking_claim("No wahala, Grace. Your appointment for RSA Registration and Onboarding Call with Aisha Bello "
                         "don set for Tuesday, twenty-fourth November.")
    assert booking_claim("Your appointment is set for Tuesday at eleven.")
    assert booking_claim("You're all set for your registration call.")
    assert not booking_claim("Aisha Bello is free on Tuesday at eleven. Shall I book that?")
    assert not booking_claim("I have Grace Okon, email grace dot okon at gmail dot com. Is that all correct?")


def test_weekday_dates_are_made_consistent():
    from datetime import datetime
    from zoneinfo import ZoneInfo

    from app.livekit_worker import fix_weekday_dates

    now = datetime(2026, 11, 19, 19, 0, tzinfo=ZoneInfo("Africa/Lagos"))  # a Thursday
    f = lambda t: fix_weekday_dates(t, "Africa/Lagos", now)
    assert f("an RSA call for next Tuesday, November twenty-fifth, at three") == \
        "an RSA call for next Tuesday, November twenty-fourth, at three"
    assert f("on Tuesday, the 25th of November") == "on Tuesday, the 24th of November"
    assert f("on Tuesday, 24 November at noon") == "on Tuesday, 24 November at noon"  # already right
    assert f("Wednesday, November twenty-fifth") == "Wednesday, November twenty-fifth"
    assert f("no dates here") == "no dates here"


def test_near_miss_ids_are_corrected():
    from app.tools import known_id

    offerings = ["4a5dca9a-3963-499f-ab30-8abf2b097816", "af66a5d6-62ee-448f-97d5-72c46b22da76"]
    assert known_id("4a5dca9a-3663-499f-ab30-8abf2b097816", offerings) == offerings[0]  # the slip from a live call
    assert known_id(offerings[1], offerings) == offerings[1]
    assert known_id("totally-different-id", offerings) == "totally-different-id"
    assert known_id("", offerings) == ""


def test_spoken_language_requests_switch_the_call():
    from app.livekit_worker import SwitchingLanguageSTT

    switched = []
    stt = SwitchingLanguageSTT.__new__(SwitchingLanguageSTT)
    stt.language, stt._allowed, stt._english_side = "pcm", ["en", "pcm", "yo", "ha", "ig"], "pcm"
    stt._on_switch = switched.append
    stt._follow_request("Please, can I switch to Hausa? I feel more comfortable speaking Hausa.")
    assert switched == ["ha"] and stt.language == "ha"
    stt._follow_request("Sorry, can we switch to Igbo instead? I'm more comfortable in Igbo.")
    assert switched == ["ha", "ig"]
    stt._follow_request("I want to open my pension account")
    assert switched == ["ha", "ig"]


def test_phonetic_switch_requests():
    from app.livekit_worker import SwitchingLanguageSTT

    switched = []
    stt = SwitchingLanguageSTT.__new__(SwitchingLanguageSTT)
    stt.language, stt._allowed, stt._english_side = "pcm", ["en", "pcm", "yo", "ha", "ig"], "pcm"
    stt._on_switch = switched.append
    stt._follow_request("please kan i swish to hausa i feel more komfortabol speakin housa")
    stt._follow_request("surry kan wi swish to ebol instead i am more komfortabol en ebol")
    assert switched == ["ha", "ig"]


def test_misheard_language_names_are_fixed():
    from app.livekit_worker import _LANGUAGE_NAME_FIX, _LANGUAGE_NAMES_HEARD

    fix = lambda t: _LANGUAGE_NAMES_HEARD.sub(lambda m: _LANGUAGE_NAME_FIX[m.group(0).lower()], t)
    assert fix("No problem at all, we can continue in Eboe.") == "No problem at all, we can continue in Igbo."
    assert fix("We can continue in Housa.") == "We can continue in Hausa."
    assert fix("Thank you, Ebuka.") == "Thank you, Ebuka."


def test_switch_requests_with_odd_endings():
    from app.livekit_worker import SwitchingLanguageSTT

    switched = []
    stt = SwitchingLanguageSTT.__new__(SwitchingLanguageSTT)
    stt.language, stt._allowed, stt._english_side = "pcm", ["en", "pcm", "yo", "ha", "ig"], "pcm"
    stt._on_switch = switched.append
    stt._follow_request("please kan i swish to hausar i feel more komfortabol speaking housar")
    stt._follow_request("surry kan wi swish to ebol instead i am more comfortable in ebl")
    assert switched == ["ha", "ig"]
    stt._follow_request("I go come tomorrow")  # no language named: no switch
    assert switched == ["ha", "ig"]


def test_your_is_not_yoruba():
    from app.livekit_worker import SwitchingLanguageSTT

    switched = []
    stt = SwitchingLanguageSTT.__new__(SwitchingLanguageSTT)
    stt.language, stt._allowed, stt._english_side = "pcm", ["en", "pcm", "yo", "ha", "ig"], "pcm"
    stt._on_switch = switched.append
    stt._follow_request("i go open your account tomorrow, can you change your time")
    assert switched == []
    stt._follow_request("abeg make we switch to yoruba")
    assert switched == ["yo"]


def test_fixed_switch_agreement():
    import asyncio

    from app import call_language
    from app.livekit_worker import fix_continue_in

    assert fix_continue_in("Of course, we can continue in Ebon. Please go ahead.", "ig") == \
        call_language.agreement_en("ig")
    assert fix_continue_in("We can continue in Hausa.", "ha") == "We can continue in Hausa."
    t = _translator("should not be used", status=500)
    out = asyncio.run(t.translate(call_language.agreement_en("ig"), "en", "ig")).text
    assert out == "N'ezie, anyị nwere ike ịga n'ihu n'asụsụ Igbo. Biko gaa n'ihu."


def test_requested_language_sticks_through_english_sounding_turns():
    import asyncio

    from app.livekit_worker import SwitchingLanguageSTT

    class Whisper:
        async def transcribe_pcm(self, pcm, **kw):
            return "i feel more komfortabol speaking hausa"

    switched = []
    stt = SwitchingLanguageSTT.__new__(SwitchingLanguageSTT)
    stt.language, stt._home, stt._allowed, stt._english_side = "pcm", "pcm", ["en", "pcm", "yo", "ha", "ig"], "pcm"
    stt._on_switch, stt._whisper = switched.append, Whisper()
    stt.last_detected_language = "pcm"

    async def sounds_pidgin(pcm, sample_rate, channels):
        return "pcm", 0.997
    stt._identify = sounds_pidgin
    stt._follow_request("please kan i swish to hausa")
    assert switched == ["ha"]
    pcm = b"\0" * (16000 * 2 * 3)  # 3 s of audio that sounds like Pidgin
    asyncio.run(stt.transcribe_pcm(pcm, sample_rate=16000, channels=1))
    assert stt.language == "ha" and switched == ["ha"]  # not flipped back


def test_set_up_claims_are_caught():
    from app.lk_tool_loop import booking_claim

    assert booking_claim("I have set up an RSA Registration and Onboarding call so our consultant can explain.")
    assert booking_claim("I've arranged your registration call for Tuesday.")
    assert not booking_claim("I can set up an RSA Registration and Onboarding call for you. Which day works?")
    assert not booking_claim("Shall I set up a registration call for you?")
