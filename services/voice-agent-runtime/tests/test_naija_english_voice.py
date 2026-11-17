from app.config import load_settings
from app.livekit_worker import call_voice


def _piper(code):
    return f"piper-{code}"


def test_english_and_pidgin_use_the_nigerian_english_voice():
    for code in ("en", "en-NG", "pcm"):
        assert call_voice(code, english_voice="idera", naija=True, voice_spec=_piper) == ("naija:idera", "en")


def test_other_nigerian_languages_keep_their_own_voices():
    for code in ("yo", "ig", "ha", "ful"):
        assert call_voice(code, english_voice="idera", naija=True, voice_spec=_piper) == ("naija:auto", code)


def test_without_the_setting_english_stays_on_piper_and_pidgin_on_mms():
    assert call_voice("en", english_voice="", naija=True, voice_spec=_piper) == ("piper-en", "")
    assert call_voice("pcm", english_voice="", naija=True, voice_spec=_piper) == ("naija:auto", "pcm")


def test_without_the_naija_service_everything_falls_back_to_piper():
    assert call_voice("en", english_voice="idera", naija=False, voice_spec=_piper) == ("piper-en", "")
    assert call_voice("pcm", english_voice="idera", naija=False, voice_spec=_piper) == ("piper-en", "")
    assert call_voice("fr", english_voice="idera", naija=True, voice_spec=_piper) == ("piper-fr", "")


def test_setting_is_read_from_the_environment(monkeypatch):
    monkeypatch.setenv("NAIJA_ENGLISH_VOICE", " idera ")
    assert load_settings().naija_english_voice == "idera"


def test_a_closed_keepalive_connection_is_retried_once():
    import asyncio

    import httpx

    from app.tts_providers.naija import NaijaSpeechTTS

    calls = {"n": 0}

    def handler(request):
        calls["n"] += 1
        if calls["n"] == 1:
            raise httpx.RemoteProtocolError("")
        return httpx.Response(200, content=b"RIFFwav")

    tts = NaijaSpeechTTS("http://naija", client=httpx.AsyncClient(transport=httpx.MockTransport(handler)))
    assert asyncio.run(tts.synthesize("Hello", "idera", "en")) == b"RIFFwav"
    assert calls["n"] == 2


def test_per_language_voices_for_the_other_nigerian_languages():
    voices = {"yo": "yoruba_male1", "ig": "igbo_male2"}
    assert call_voice("yo", english_voice="osagie", naija=True, voice_spec=_piper,
                      language_voices=voices) == ("naija:yoruba_male1", "yo")
    assert call_voice("ig", english_voice="osagie", naija=True, voice_spec=_piper,
                      language_voices=voices) == ("naija:igbo_male2", "ig")
    # not listed: the service's default voice
    assert call_voice("ha", english_voice="osagie", naija=True, voice_spec=_piper,
                      language_voices=voices) == ("naija:auto", "ha")


def test_language_voice_setting_parses_and_skips_junk():
    from app.config import parse_naija_language_voices

    assert parse_naija_language_voices(" yo=yoruba_male1, IG = igbo_male2 ,bad, =x, ha=") == {
        "yo": "yoruba_male1", "ig": "igbo_male2"}
    assert parse_naija_language_voices("") == {}
