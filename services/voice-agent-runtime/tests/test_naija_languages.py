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
    assert call_language.greeting("ig", "Harbour Assurance", "Hello") == (
        "Enwere ike ịdekọ oku a. Nnọọ na Harbour Assurance! Kedu ka m ga-esi nyere gị aka taa?")  # recording notice first
    assert call_language.greeting("", "Harbour Assurance", "Hello") == "Hello"
    assert call_language.greeting("ful", "Harbour Assurance", "Hello") == "Hello"  # no Fulfulde greeting written
    assert "Reply ONLY in Hausa" in call_language.prompt_instruction("ha")
    assert call_language.prompt_instruction("") == "" and call_language.prompt_instruction("en") == ""


def test_tts_provider_asks_for_the_call_sample_rate():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen.update(json.loads(request.content))
        return httpx.Response(200, content=_wav(), headers={"content-type": "audio/wav"})

    tts = NaijaSpeechTTS("http://naija:5820", sample_rate=22050, takes=1,  # one take: best-of-N has its own tests
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


def test_naija_replies_are_grouped_into_long_chunks():
    """Sentence-by-sentence naija synthesis left multi-second gaps; chunks of >=120 chars are generated in one batch."""
    from livekit.agents import tokenize as lk_tokenize

    from app.livekit_worker import NAIJA_MIN_CHUNK_CHARS

    reply = ("I mela, enwere m ntị. Biko, tupu anyị malite, ọ bụrụ na onye ọ bụla merụrụ ahụ, kpọọ ndị ọrụ enyemaka "
             "ozugbo. Gwa m ihe mere. Biko nye m nọmba polisi gị. Anyị ga-enyere gị aka.")
    chunks = lk_tokenize.basic.SentenceTokenizer(min_sentence_len=NAIJA_MIN_CHUNK_CHARS).tokenize(reply)
    assert len(chunks) < 5  # five sentences, fewer chunks
    assert all(len(c) >= NAIJA_MIN_CHUNK_CHARS for c in chunks[:-1])
    assert "I mela, enwere m ntị." in chunks[0] and "Biko" in chunks[0]  # the short opener rides with the next one


def test_naija_tts_streams():
    from app.livekit_worker import PiperTTSNode, _naija_tts

    class Impl:
        sample_rate = 22050

    adapter = _naija_tts(PiperTTSNode(Impl()))
    assert adapter.capabilities.streaming and adapter.sample_rate == 22050


def test_markup_is_not_read_aloud():
    from app.livekit_worker import speakable

    assert speakable("</li><li>I apologize, **Amica**. Please `wait`.") == "I apologize, Amica. Please wait."
    assert speakable("Fees: ₦10,000 — 9:00 a.m.") == "Fees: ₦10,000 — 9:00 a.m."


def test_listener_follows_a_reconnected_caller():
    """After a full reconnect the caller is a NEW participant object with the same identity; the receptionist must
    listen to it (0.10 kept the old one and went deaf)."""
    from app.livekit_worker import keep_listening_after_reconnect

    class P:
        def __init__(self, identity):
            self.identity = identity

    class Listener:
        def __init__(self, p):
            self._participant, self.subscribed = p, 0

        def _subscribe_to_microphone(self):
            self.subscribed += 1

    class Room:
        def __init__(self):
            self.handlers = {}

        def on(self, ev, fn):
            self.handlers[ev] = fn

    old = P("web-1"); agent = type("A", (), {})(); agent._human_input = Listener(old); room = Room()
    keep_listening_after_reconnect(agent, room)
    room.handlers["participant_connected"](P("someone-else"))
    assert agent._human_input._participant is old
    new = P("web-1"); room.handlers["participant_connected"](new)
    assert agent._human_input._participant is new and agent._human_input.subscribed == 1


def test_reconnect_diagnostics_never_raise():
    from app.livekit_worker import keep_listening_after_reconnect

    class Room:
        def __init__(self):
            self.handlers = {}

        def on(self, ev, fn):
            self.handlers[ev] = fn

    agent = type("A", (), {})(); agent._human_input = None; room = Room()
    keep_listening_after_reconnect(agent, room)
    track, pub, who = type("T", (), {"sid": "TR_1"})(), type("P", (), {"sid": "TP_1"})(), type("W", (), {"identity": "web-1"})()
    for ev in ("participant_disconnected", "track_published", "track_subscribed", "track_unsubscribed", "reconnecting", "reconnected"):
        room.handlers[ev](track, pub, who)  # must not raise (a logging bug used to break the room's callbacks)


def test_short_clips_use_the_call_language():
    """'Nine o'clock is fine.' (1.5 s) was transcribed as German by whisper's language guess."""
    import asyncio as _a

    from app.livekit_worker import WhisperSTTNode

    seen = []

    class Impl:
        last_detected_language = None

        async def transcribe_pcm(self, pcm, *, sample_rate, channels, language=None):
            seen.append(language)
            return "ok"

    class Frame:
        def __init__(self, seconds):
            self.sample_rate, self.num_channels = 16000, 1
            self.samples_per_channel = int(16000 * seconds)
            self.data = b"\x00\x00" * self.samples_per_channel

    import app.livekit_worker as w
    node = WhisperSTTNode(Impl(), current_language=lambda: "en")
    orig = w.lk_utils.merge_frames
    try:
        w.lk_utils.merge_frames = lambda buf: buf
        _a.run(node.recognize(Frame(1.5)))
        _a.run(node.recognize(Frame(6.0)))
    finally:
        w.lk_utils.merge_frames = orig
    assert seen == ["en", None]  # short clip pinned to the call language; long clip still auto-detected


def _switcher(detect, start="en"):
    from app.livekit_worker import SwitchingLanguageSTT

    calls = {"asr": [], "whisper": [], "switch": []}

    class Naija:
        async def identify(self, pcm, *, sample_rate, channels=1, candidates):
            return detect

        async def transcribe_pcm(self, pcm, *, sample_rate, channels=1, language=None):
            calls["asr"].append(language)
            return f"[{language}]"

    class Whisper:
        async def transcribe_pcm(self, pcm, *, sample_rate, channels=1, language=None):
            calls["whisper"].append(language)
            return "[en]"

    s = SwitchingLanguageSTT(Naija(), Whisper(), start, ["en", "pcm", "yo", "ha", "ig"], calls["switch"].append)
    return s, calls


def _pcm(seconds, rate=16000):
    return b"\x00\x00" * int(rate * seconds)


def test_long_turn_in_another_language_switches_the_call():
    s, calls = _switcher({"language": "yo", "scores": {"yo": 0.99, "en": 0.01}})
    assert asyncio.run(s.transcribe_pcm(_pcm(4), sample_rate=16000)) == "[yo]"
    assert calls["switch"] == ["yo"] and calls["asr"] == ["yo"] and s.language == "yo"


def test_short_or_unsure_turns_never_switch():
    s, calls = _switcher({"language": "ha", "scores": {"ha": 0.97}})
    asyncio.run(s.transcribe_pcm(_pcm(1.5), sample_rate=16000))
    s2, calls2 = _switcher({"language": "ha", "scores": {"ha": 0.55}})
    asyncio.run(s2.transcribe_pcm(_pcm(5), sample_rate=16000))
    assert calls["switch"] == [] and calls2["switch"] == [] and s.language == "en" and s2.language == "en"
    assert calls["whisper"] == ["en"]


def test_english_and_pidgin_are_one_family():
    s, calls = _switcher({"language": "en", "scores": {"en": 0.99}}, start="pcm")
    assert asyncio.run(s.transcribe_pcm(_pcm(4), sample_rate=16000)) == "[pcm]"  # a Pidgin call stays Pidgin
    assert calls["switch"] == []
    s.language = "ig"  # caller had moved to Igbo; English-family speech returns to their Pidgin side
    asyncio.run(s.transcribe_pcm(_pcm(4), sample_rate=16000))
    assert calls["switch"] == ["pcm"] and s.language == "pcm"


def test_read_back_then_yes_counts_as_confirmed():
    from app.livekit_worker import read_back_confirmed

    M = lambda role, text: type("Msg", (), {"role": role, "content": text})()
    convo = [M("user", "My number is 0803 555 2214."), M("assistant", "I have 0803 555 2214. Is that correct?"),
             M("user", "Yes, that is correct.")]
    assert read_back_confirmed(convo, "08035552214")
    assert read_back_confirmed(convo[:2] + [M("user", "E correct.")], "08035552214")       # Pidgin
    assert not read_back_confirmed(convo[:2], "08035552214")                                 # no reply yet
    assert not read_back_confirmed(convo[:2] + [M("user", "No, it's 0803 555 9999")], "08035552214")
    assert not read_back_confirmed(convo, "08061112233")                                    # a different number


def test_read_back_counts_even_if_times_were_offered_after_it():
    from app.livekit_worker import read_back_confirmed

    M = lambda role, text: type("Msg", (), {"role": role, "content": text})()
    convo = [M("assistant", "I have 0803 555 2214. Is that correct?"), M("user", "Yes, that is correct."),
             M("assistant", "I have 9:00, 10:00 or 11:00 AM. Which works?"), M("user", "Nine o'clock is fine.")]
    assert read_back_confirmed(convo, "08035552214")
    wrong = [convo[0], M("user", "No, it's 0803 555 9999"), convo[2], convo[3]]
    assert not read_back_confirmed(wrong, "08035552214")  # the reply right after the read-back was a correction


def test_leaving_pidgin_needs_a_confident_detection():
    # Pidgin lines were taken for Igbo at 0.78 and 0.95: the call stays in Pidgin
    for conf in (0.78, 0.95):
        s, calls = _switcher({"language": "ig", "scores": {"ig": conf}}, start="pcm")
        asyncio.run(s.transcribe_pcm(_pcm(4), sample_rate=16000))
        assert calls["switch"] == [] and s.language == "pcm"
    # a real switch (0.99) still goes through, and between Nigerian languages the usual bar applies
    s, calls = _switcher({"language": "ha", "scores": {"ha": 0.99}}, start="pcm")
    asyncio.run(s.transcribe_pcm(_pcm(4), sample_rate=16000))
    assert calls["switch"] == ["ha"]
    s, calls = _switcher({"language": "ig", "scores": {"ig": 0.8}}, start="ha")
    asyncio.run(s.transcribe_pcm(_pcm(4), sample_rate=16000))
    assert calls["switch"] == ["ig"]


def test_auto_switch_off_keeps_the_chosen_language():
    from app.config import load_settings
    import os
    os.environ["VOICE_AUTO_LANGUAGE_SWITCH"] = "0"
    try:
        assert load_settings().voice_auto_language_switch is False
    finally:
        del os.environ["VOICE_AUTO_LANGUAGE_SWITCH"]
    # a switcher allowed only the chosen language never switches, even on a confident detection
    from app.livekit_worker import SwitchingLanguageSTT

    class Naija:
        async def identify(self, *a, **k):
            return {"language": "yo", "scores": {"yo": 0.99}}

        async def transcribe_pcm(self, *a, **k):
            return "x"

    class Whisper:
        async def transcribe_pcm(self, *a, **k):
            return "hello"

    switched = []
    s = SwitchingLanguageSTT(Naija(), Whisper(), "en", ["en"], switched.append)
    asyncio.run(s.transcribe_pcm(_pcm(4), sample_rate=16000))
    assert switched == [] and s.language == "en"


def test_switch_mode_nigerian():
    from app.config import load_settings
    import os
    os.environ["VOICE_AUTO_LANGUAGE_SWITCH"] = "nigerian"
    try:
        s = load_settings()
        assert s.voice_language_switch_mode == "nigerian" and s.voice_auto_language_switch is False
    finally:
        del os.environ["VOICE_AUTO_LANGUAGE_SWITCH"]
    assert load_settings().voice_language_switch_mode == "on"


def test_english_details_inside_a_yoruba_call_are_heard_in_english_without_switching():
    import asyncio
    from app.livekit_worker import SwitchingLanguageSTT

    class Naija:
        async def identify(self, pcm, **_k):
            return {"language": "en", "scores": {"en": 0.93}}

        async def transcribe_pcm(self, pcm, **_k):
            return "go oton"

    class Whisper:
        async def transcribe_pcm(self, pcm, **_k):
            return "Yes, that's correct."

    switched = []
    stt = SwitchingLanguageSTT(Naija(), Whisper(), "yo", ["en", "pcm", "yo", "ha", "ig"], switched.append)
    pcm = b"\0\0" * 16000 * 2  # 2 s: short turns are checked too
    text = asyncio.run(stt.transcribe_pcm(pcm, sample_rate=16000))
    assert text == "Yes, that's correct." and stt.language == "yo" and switched == []


def test_asking_for_english_inside_a_yoruba_call_switches():
    import asyncio
    from app.livekit_worker import SwitchingLanguageSTT

    class Naija:
        async def identify(self, pcm, **_k):
            return {"language": "en", "scores": {"en": 0.95}}

    class Whisper:
        async def transcribe_pcm(self, pcm, **_k):
            return "Can we continue in English please?"

    switched = []
    stt = SwitchingLanguageSTT(Naija(), Whisper(), "yo", ["en", "pcm", "yo", "ha", "ig"], switched.append)
    asyncio.run(stt.transcribe_pcm(b"\0\0" * 16000 * 3, sample_rate=16000))
    assert stt.language == "en" and switched == ["en"]


def test_speakable_decodes_byte_fallback_tokens():
    """2026-11-21: '<0xC7><0xB8>' is the UTF-8 for 'Ǹ' (decoded back); a lone '<0xA0>' is dropped; NBSP -> space."""
    from app.livekit_worker import speakable
    assert speakable("zero, zero. <0xC7><0xB8>jẹ́ èyí tòótọ́?") == "zero, zero. Ǹjẹ́ èyí tòótọ́?"
    assert speakable("<0xA0> <0xC2><0xA0>Note: checking") == "Note: checking"
    assert speakable("plain <b>text</b>") == "plain text"
