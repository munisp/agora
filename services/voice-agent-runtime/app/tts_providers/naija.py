"""naija provider — HTTP client for the naija-speech service on the GPU host (services/naija-speech):

- ``POST {NAIJA_SPEECH_URL}/tts {"text", "language", "voice"?, "sample_rate"}`` -> ``audio/wav``

Igbo, Yoruba, Hausa (and Nigerian-accented English) are YarnGPT voices; Pidgin and Fulfulde are MMS voices. The
service resamples to ``sample_rate`` — the call pipeline plays one fixed rate, and audio at any other rate is pitched.
"""
from __future__ import annotations

import asyncio
import base64
import difflib
import os
import unicodedata

import httpx

from .base import Voice, labels_of
from .spoken import spoken


class NaijaSpeechTTS:
    name = "naija"

    # Yoruba/Hausa/Igbo YarnGPT voices garble a sentence now and then: the same line in the same voice scored 0.16 on
    # one take and 0.80 on the next (2026-11-20, scored by recognising the voice's own audio). For those languages
    # several takes are made in one batch and the one the recogniser hears closest to the text is played.
    BEST_OF_LANGUAGES = ("yo", "ha", "ig")

    def __init__(self, base_url: str, *, sample_rate: int = 22050, timeout_s: float = 30.0,
                 client: httpx.AsyncClient | None = None, takes: int | None = None) -> None:
        self.base_url = (base_url or "").rstrip("/")
        self.sample_rate = sample_rate
        self.takes = max(1, takes if takes is not None else int(os.environ.get("NAIJA_TTS_TAKES", "3") or 3))
        self.default_voice = ""  # the service picks the language's best voice
        self._timeout = timeout_s
        self._client = client

    def _http(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = httpx.AsyncClient(timeout=httpx.Timeout(self._timeout))
        return self._client

    async def aclose(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    async def synthesize(self, text: str, voice: str, language: str) -> bytes:
        text = text.strip()
        if not text:
            return b""
        if not self.base_url:
            raise RuntimeError("NAIJA_SPEECH_URL is not set")
        lang = (language or "").strip().lower()
        if not lang:
            raise RuntimeError("naija synthesize needs a language")
        if lang in ("en", "pcm"):
            text = spoken(text)  # YarnGPT garbles digits ("9:30" -> "thirty thoughts"): times and numbers as words
        body = {"text": text, "language": lang, "sample_rate": self.sample_rate}
        if voice and voice.strip() and voice.strip() != "auto":  # "auto": the service's best voice for the language
            body["voice"] = voice.strip()
        if self.takes > 1 and lang in self.BEST_OF_LANGUAGES:
            try:
                return await self._best_take(body)
            except Exception:  # noqa: BLE001 - one plain take below
                pass
        try:
            resp = await self._http().post(f"{self.base_url}/tts", json=body)
        except httpx.TransportError:
            # The service closes idle keep-alive connections after a few seconds; the first sentence after a pause in
            # the call could reuse a closed one and fail with an empty error (that sentence then fell back to piper).
            resp = await self._http().post(f"{self.base_url}/tts", json=body)
        resp.raise_for_status()
        return resp.content

    async def _best_take(self, body: dict) -> bytes:
        resp = await self._http().post(f"{self.base_url}/tts_batch", json={"items": [body] * self.takes})
        resp.raise_for_status()
        wavs = [base64.b64decode(b) for b in resp.json()["wavs"]]
        if len(wavs) == 1:
            return wavs[0]
        target = _letters(body["text"])

        async def score(wav: bytes) -> float:
            try:
                r = await self._http().post(f"{self.base_url}/asr", params={"language": body["language"]},
                                            content=wav, headers={"content-type": "audio/wav"})
                r.raise_for_status()
                return difflib.SequenceMatcher(None, target, _letters(r.json().get("text", ""))).ratio()
            except Exception:  # noqa: BLE001 - an unscored take ranks last
                return -1.0

        scores = await asyncio.gather(*(score(w) for w in wavs))
        return wavs[max(range(len(wavs)), key=lambda i: scores[i])]

    async def list_voices(self) -> list[Voice]:
        if not self.base_url:
            return []
        resp = await self._http().get(f"{self.base_url}/voices")
        resp.raise_for_status()
        data = resp.json()
        return [Voice(id=str(v["id"]), languages=[str(x) for x in v.get("languages", []) or []],
                      gender=str(v.get("gender", "") or ""), labels=labels_of(v))
                for v in (data.get("voices", []) if isinstance(data, dict) else []) if isinstance(v, dict) and v.get("id")]

    async def available(self) -> bool:
        if not self.base_url:
            return False
        try:
            resp = await self._http().get(f"{self.base_url}/healthz")
            return resp.status_code == 200 and resp.json().get("status") == "ok"
        except Exception:  # noqa: BLE001 - probe must never raise
            return False


def _letters(text: str) -> str:
    """Lowercase letters and spaces, tone marks stripped: the recogniser rarely gets tone marks right."""
    t = unicodedata.normalize("NFD", (text or "").lower())
    t = "".join(c for c in t if unicodedata.category(c)[0] in "LZ" and unicodedata.category(c) != "Mn")
    return " ".join(t.split())
