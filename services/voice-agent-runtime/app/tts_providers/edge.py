"""edge provider — Microsoft neural voices through the Edge "read aloud" service (the `edge-tts` package), e.g.
``edge:en-NG-AbeoNeural`` (Nigerian English, the voice the demo films are narrated in).

DEMO-GRADE: the service is free but unofficial (no SLA; Microsoft can change or block it). For real customers use
the ``azure`` provider with an Azure Speech key — the same voices, officially. The chain falls back to piper when
this provider fails.

Returns RIFF wav (16-bit mono) at ``sample_rate``: the call pipeline plays one fixed rate.
"""
from __future__ import annotations

import io
import wave

from .base import Voice

EDGE_VOICES = {
    "en-NG-AbeoNeural": ("en", "male"),
    "en-NG-EzinneNeural": ("en", "female"),
}


def _mp3_to_wav(mp3: bytes, sample_rate: int) -> bytes:
    """Decode the service's MP3 to 16-bit mono PCM at `sample_rate` (PyAV ships with faster-whisper)."""
    import av

    out = io.BytesIO()
    pcm = bytearray()
    with av.open(io.BytesIO(mp3)) as container:
        resampler = av.audio.resampler.AudioResampler(format="s16", layout="mono", rate=sample_rate)
        for frame in container.decode(audio=0):
            for f in resampler.resample(frame):
                pcm += f.to_ndarray().tobytes()
        for f in resampler.resample(None):
            pcm += f.to_ndarray().tobytes()
    with wave.open(out, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sample_rate)
        w.writeframes(bytes(pcm))
    return out.getvalue()


class EdgeTTS:
    name = "edge"

    def __init__(self, *, sample_rate: int = 22050, default_voice: str = "en-NG-AbeoNeural", communicate=None) -> None:
        self.sample_rate = sample_rate
        self.default_voice = default_voice
        self._communicate = communicate  # injectable for tests

    async def synthesize(self, text: str, voice: str, language: str) -> bytes:  # noqa: ARG002 - voice picks the language
        text = text.strip()
        if not text:
            return b""
        voice = (voice or "").strip() or self.default_voice
        if voice == "auto":
            voice = self.default_voice
        if self._communicate is None:
            import edge_tts

            self._communicate = edge_tts.Communicate
        mp3 = bytearray()
        async for chunk in self._communicate(text, voice).stream():
            if chunk.get("type") == "audio":
                mp3 += chunk["data"]
        if not mp3:
            raise RuntimeError("edge tts returned no audio")
        return _mp3_to_wav(bytes(mp3), self.sample_rate)

    async def list_voices(self) -> list[Voice]:
        return [Voice(id=v, languages=[lang], gender=g, labels=["edge", "demo"]) for v, (lang, g) in EDGE_VOICES.items()]

    async def available(self) -> bool:
        try:
            import edge_tts  # noqa: F401
            return True
        except Exception:  # noqa: BLE001 - probe must never raise
            return False
