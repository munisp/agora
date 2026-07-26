"""Spitch TTS provider — Nigerian language voices (Yoruba/Hausa/Igbo/English)
via the Spitch REST API.

Request shape was verified against the official Spitch SDK source and docs:
- docs:  https://docs.spitch.app/api/speech/tts  (SDK-level reference)
- SDK:   https://github.com/spi-tch/spitch-python (src/spitch/resources/speech.py)

``POST {SPITCH_BASE_URL=https://api.spitch.app}/v1/speech`` with
``Authorization: Bearer {SPITCH_API_KEY}``, ``Accept: audio/wav`` and JSON
``{text, voice, format: "wav", language, speed}`` -> ``audio/wav`` bytes.

The documented language set is en/ha/ig/yo with named character voices
(e.g. "sade", "femi"). The request shape lives in ONE isolated
:meth:`SpitchTTS.build_request` so a contract change is a one-line fix.
"""

from __future__ import annotations

import httpx

from ..logging import get_logger
from .base import Voice

log = get_logger("tts.spitch")

# Documented Spitch languages (docs.spitch.app/features/speech).
SPITCH_LANGUAGES = ["en", "yo", "ha", "ig"]
# Named character voices confirmed in the official docs/SDK examples. The
# docs mention 8 characters but only these names are published; extend here
# when Spitch publishes the full catalog.
SPITCH_VOICES: tuple[Voice, ...] = (
    Voice(id="sade", languages=list(SPITCH_LANGUAGES), gender="Female"),
    Voice(id="femi", languages=list(SPITCH_LANGUAGES), gender="Male"),
)


class SpitchTTS:
    name = "spitch"

    def __init__(
        self,
        *,
        api_key: str = "",
        base_url: str = "https://api.spitch.app",
        timeout_s: float = 20.0,
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self.api_key = api_key
        self.base_url = base_url.rstrip("/")
        self.default_voice = "sade"
        self._timeout = timeout_s
        self._client = client

    @property
    def configured(self) -> bool:
        return bool(self.api_key)

    def _http(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = httpx.AsyncClient(timeout=httpx.Timeout(self._timeout))
        return self._client

    async def aclose(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    def build_request(
        self, text: str, voice: str, language: str
    ) -> tuple[str, dict[str, str], dict]:
        """ONE isolated place encoding the Spitch HTTP contract.

        Shape per the official SDK (spi-tch/spitch-python, resources/speech.py
        ``generate``) and https://docs.spitch.app/api/speech/tts:
        POST /v1/speech, Bearer auth, Accept: audio/wav,
        body {text, voice, format, language, speed}.
        """
        lang = (language or "").strip().split("-", 1)[0].lower()
        url = f"{self.base_url}/v1/speech"
        headers = {
            "Authorization": f"Bearer {self.api_key}",
            "Accept": "audio/wav",
        }
        body: dict = {
            "text": text,
            "voice": (voice or "").strip() or self.default_voice,
            "format": "wav",
            "speed": 1.0,
        }
        if lang:
            body["language"] = lang
        return url, headers, body

    async def synthesize(self, text: str, voice: str, language: str) -> bytes:
        text = text.strip()
        if not text:
            return b""
        if not self.configured:
            raise RuntimeError("spitch TTS not configured (set SPITCH_API_KEY)")
        url, headers, body = self.build_request(text, voice, language)
        resp = await self._http().post(url, headers=headers, json=body)
        resp.raise_for_status()
        return resp.content

    async def list_voices(self) -> list[Voice]:
        return list(SPITCH_VOICES)

    async def available(self) -> bool:
        """Configuration probe (no cheap unauthenticated ping; synthesis
        failures trip the chain's circuit breaker)."""
        return self.configured
