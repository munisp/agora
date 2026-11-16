"""Nigerian-language calls: the caller's choice, the naija-speech voice provider and speech recognition."""
import asyncio
import io
import json
import wave

import httpx

from app import call_language
from app.pipeline.naija_stt import NaijaSpeechSTT, pcm_to_wav
from app.tts_providers.naija import NaijaSpeechTTS


def _wav(rate=22050, seconds=0.1):
    return pcm_to_wav(b"\x00\x01" * int(rate * seconds), rate)


def test_allowed_and_normalize():
    assert call_language.allowed("en,pcm,yo,ha,ig") == ["en", "pcm", "yo", "ha", "ig"]
    assert call_language.allowed("ig, xx ,yo") == ["en", "ig", "yo"]  # unknown codes dropped, English always first
    assert call_language.normalize("IG", "en,ig") == "ig"
    assert call_language.normalize("ful", "en,pcm,yo,ha,ig") == ""  # not offered by default
    assert call_language.normalize("en", "en,ig") == ""  # English = no special handling
    assert call_language.normalize(None, "en,ig") == ""


def test_metadata_round_trip():
    assert call_language.from_metadata(call_language.token_metadata("ha")) == "ha"
    assert call_language.token_metadata("") == ""
    assert call_language.from_metadata("") == ""
    assert call_language.from_metadata("not json") == ""
    assert call_language.from_metadata('{"language": "klingon"}') == ""


def test_greeting_and_instruction():
    assert call_language.greeting("ig", "Harbour Assurance", "Hello") == "Nnọọ na Harbour Assurance! Kedu ka m ga-esi nyere gị aka taa?"
    assert call_language.greeting("", "Harbour Assurance", "Hello") == "Hello"
    assert call_language.greeting("ful", "Harbour Assurance", "Hello") == "Hello"  # no Fulfulde greeting written
    assert "Reply ONLY in Hausa" in call_language.prompt_instruction("ha")
    assert call_language.prompt_instruction("") == "" and call_language.prompt_instruction("en") == ""


def test_tts_provider_asks_for_the_call_sample_rate():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen.update(json.loads(request.content))
        return httpx.Response(200, content=_wav(), headers={"content-type": "audio/wav"})

    tts = NaijaSpeechTTS("http://naija:5820", sample_rate=22050,
                         client=httpx.AsyncClient(transport=httpx.MockTransport(handler)))
    wav = asyncio.run(tts.synthesize("Nnọọ!", "", "ig"))
    assert wav.startswith(b"RIFF")
    assert seen == {"text": "Nnọọ!", "language": "ig", "sample_rate": 22050}


def test_stt_sends_wav_with_language():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["language"] = request.url.params["language"]
        with wave.open(io.BytesIO(request.content)) as w:
            seen["rate"] = w.getframerate()
        return httpx.Response(200, json={"text": "kedu ka i mere"})

    stt = NaijaSpeechSTT("http://naija:5820", client=httpx.AsyncClient(transport=httpx.MockTransport(handler)))
    text = asyncio.run(stt.transcribe_pcm(b"\x00\x00" * 4800, sample_rate=48000, language="ig"))
    assert text == "kedu ka i mere"
    assert seen == {"language": "ig", "rate": 48000}


def test_chain_sends_a_naija_reply_whole():
    from app.tts_providers.chain import FallbackTTS

    calls = []

    class Fake:
        name, default_voice = "naija", ""

        async def synthesize(self, text, voice, language):
            calls.append((text, language))
            return _wav()

        async def aclose(self):
            pass

    chain = FallbackTTS({"naija": Fake()}, ["naija"], sample_rate=22050)
    chain.voice, chain.language = "naija:auto", "yo"

    async def run():
        return [pcm async for pcm in chain.stream_pcm("Ẹ kú àbọ̀. Báwo ni? Ẹ ṣé.")]

    out = asyncio.run(run())
    assert len(out) == 1 and calls == [("Ẹ kú àbọ̀. Báwo ni? Ẹ ṣé.", "yo")]


def test_naija_voice_is_pinned_ahead_of_piper():
    """A Nigerian-language call must speak through naija, not the chain's first provider (piper was asked for a voice
    called "naija:" and the reply fell through to the English voices)."""
    from app.tts_providers.chain import FallbackTTS

    spoken = []

    class Fake:
        default_voice = ""

        def __init__(self, name):
            self.name = name

        async def synthesize(self, text, voice, language):
            spoken.append((self.name, voice, language))
            return _wav()

        async def aclose(self):
            pass

    chain = FallbackTTS({"piper": Fake("piper"), "naija": Fake("naija")}, ["piper", "naija"], sample_rate=22050)
    chain.voice, chain.language = "naija:auto", "ig"
    asyncio.run(chain.synthesize_pcm("Nnọọ!"))
    assert spoken == [("naija", "auto", "ig")]


def test_auto_voice_lets_the_service_choose():
    seen = {}

    def handler(request):
        seen.update(json.loads(request.content))
        return httpx.Response(200, content=_wav())

    tts = NaijaSpeechTTS("http://naija:5820", client=httpx.AsyncClient(transport=httpx.MockTransport(handler)))
    asyncio.run(tts.synthesize("Sannu", "auto", "ha"))
    assert "voice" not in seen


def test_chosen_language_stt_falls_back_to_whisper():
    from app.livekit_worker import ChosenLanguageSTT

    class Down:
        async def transcribe_pcm(self, *a, **k):
            raise httpx.ConnectError("gpu host down")

    class Whisper:
        async def transcribe_pcm(self, pcm, *, sample_rate=16000, channels=1, language=None):
            return "hello"

    stt = ChosenLanguageSTT(Down(), Whisper(), "ha")
    assert asyncio.run(stt.transcribe_pcm(b"\x00\x00", sample_rate=16000)) == "hello"
    assert stt.last_detected_language == "ha"
