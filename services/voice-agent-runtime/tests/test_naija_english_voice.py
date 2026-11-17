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
