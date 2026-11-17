"""naija provider — HTTP client for the naija-speech service on the GPU host (services/naija-speech):

- ``POST {NAIJA_SPEECH_URL}/tts {"text", "language", "voice"?, "sample_rate"}`` -> ``audio/wav``

Igbo, Yoruba, Hausa (and Nigerian-accented English) are YarnGPT voices; Pidgin and Fulfulde are MMS voices. The
service resamples to ``sample_rate`` — the call pipeline plays one fixed rate, and audio at any other rate is pitched.
"""
from __future__ import annotations

import httpx

from .base import Voice, labels_of


class NaijaSpeechTTS:
    name = "naija"

    def __init__(self, base_url: str, *, sample_rate: int = 22050, timeout_s: float = 30.0,
                 client: httpx.AsyncClient | None = None) -> None:
        self.base_url = (base_url or "").rstrip("/")
        self.sample_rate = sample_rate
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
        body = {"text": text, "language": lang, "sample_rate": self.sample_rate}
        if voice and voice.strip() and voice.strip() != "auto":  # "auto": the service's best voice for the language
            body["voice"] = voice.strip()
        try:
            resp = await self._http().post(f"{self.base_url}/tts", json=body)
        except httpx.TransportError:
            # The service closes idle keep-alive connections after a few seconds; the first sentence after a pause in
            # the call could reuse a closed one and fail with an empty error (that sentence then fell back to piper).
            resp = await self._http().post(f"{self.base_url}/tts", json=body)
        resp.raise_for_status()
        return resp.content

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
