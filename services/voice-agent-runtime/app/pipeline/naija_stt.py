"""Speech recognition for callers who chose a Nigerian language: MMS-1B-all on the GPU host (services/naija-speech
``POST /asr?language=...`` with one utterance as audio/wav -> ``{"text"}``).

Whisper cannot hear Igbo or Pidgin at all and hears Yoruba/Hausa poorly; MMS on real FLEURS recordings is close to
word-perfect for Igbo and Hausa. Same ``transcribe_pcm`` shape as FasterWhisperSTT so the LiveKit bridge can swap it.
"""
from __future__ import annotations

import io
import wave

import httpx

from ..logging import get_logger

log = get_logger("naija-stt")


def pcm_to_wav(pcm: bytes, sample_rate: int, channels: int = 1) -> bytes:
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(channels)
        w.setsampwidth(2)
        w.setframerate(sample_rate)
        w.writeframes(pcm[: len(pcm) - (len(pcm) % (2 * channels))])
    return buf.getvalue()


class NaijaSpeechSTT:
    def __init__(self, base_url: str, *, timeout_s: float = 20.0, client: httpx.AsyncClient | None = None) -> None:
        self.base_url = (base_url or "").rstrip("/")
        self._timeout = timeout_s
        self._client = client
        self.last_detected_language: str | None = None

    def _http(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = httpx.AsyncClient(timeout=httpx.Timeout(self._timeout))
        return self._client

    async def aclose(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    async def identify(self, pcm: bytes, *, sample_rate: int, channels: int = 1, candidates: list[str]) -> dict:
        """Spoken-language identification among `candidates` (POST /lid) -> {"language", "scores"}."""
        resp = await self._http().post(
            f"{self.base_url}/lid", params={"candidates": ",".join(candidates)},
            content=pcm_to_wav(pcm, sample_rate, channels), headers={"Content-Type": "audio/wav"},
        )
        resp.raise_for_status()
        return resp.json()

    async def transcribe_pcm(self, pcm: bytes, *, sample_rate: int = 16000, channels: int = 1,
                             language: str | None = None) -> str:
        if not language:
            raise ValueError("naija speech recognition needs the caller's language")
        resp = await self._http().post(
            f"{self.base_url}/asr", params={"language": language},
            content=pcm_to_wav(pcm, sample_rate, channels), headers={"Content-Type": "audio/wav"},
        )
        resp.raise_for_status()
        self.last_detected_language = language
        return str(resp.json().get("text") or "").strip()
