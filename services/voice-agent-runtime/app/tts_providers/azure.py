"""Azure Cognitive Speech TTS provider (direct REST, no SDK).

- ``POST https://{AZURE_SPEECH_REGION}.tts.speech.microsoft.com/cognitiveservices/v1``
  with an SSML body, ``Ocp-Apim-Subscription-Key: {AZURE_SPEECH_KEY}`` and
  ``X-Microsoft-OutputFormat: riff-24khz16bit-mono-pcm`` -> RIFF wav
  (24 kHz 16-bit mono) passed through unchanged.
  REST contract: https://learn.microsoft.com/en-us/azure/ai-services/speech-service/rest-text-to-speech

Voice coverage note (verified 2026-07 against the Microsoft voice list,
source below): Azure offers Nigerian ENGLISH voices (en-NG) but NO Yoruba
(yo-NG) or Hausa (ha-NG) voices — route those languages to mms/spitch.
"""

from __future__ import annotations

from xml.sax.saxutils import escape

import httpx

from ..logging import get_logger
from .base import Voice

log = get_logger("tts.azure")

# Curated African-language/neural voice list — THE single source of truth for
# azure voice ids. Verified against the official Microsoft Speech language
# support page (GA voices):
# https://learn.microsoft.com/en-us/azure/ai-services/speech-service/language-support
AZURE_VOICES: tuple[Voice, ...] = (
    Voice(
        id="en-NG-EzinneNeural",
        languages=["en-NG", "en"],
        gender="Female",
        labels={"locale_name": "English (Nigeria)"},
    ),
    Voice(
        id="en-NG-AbeoNeural",
        languages=["en-NG", "en"],
        gender="Male",
        labels={"locale_name": "English (Nigeria)"},
    ),
    Voice(
        id="sw-KE-ZuriNeural",
        languages=["sw-KE", "sw"],
        gender="Female",
        labels={"locale_name": "Swahili (Kenya)"},
    ),
    Voice(
        id="sw-KE-RafikiNeural",
        languages=["sw-KE", "sw"],
        gender="Male",
        labels={"locale_name": "Swahili (Kenya)"},
    ),
    Voice(
        id="am-ET-MekdesNeural",
        languages=["am-ET", "am"],
        gender="Female",
        labels={"locale_name": "Amharic (Ethiopia)"},
    ),
    Voice(
        id="am-ET-AmehaNeural",
        languages=["am-ET", "am"],
        gender="Male",
        labels={"locale_name": "Amharic (Ethiopia)"},
    ),
)

_OUTPUT_FORMAT = "riff-24khz16bit-mono-pcm"


class AzureTTS:
    name = "azure"

    def __init__(
        self,
        *,
        api_key: str = "",
        region: str = "",
        timeout_s: float = 20.0,
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self.api_key = api_key
        self.region = region.strip()
        self.default_voice = "en-NG-EzinneNeural"
        self._timeout = timeout_s
        self._client = client

    @property
    def configured(self) -> bool:
        return bool(self.api_key and self.region)

    def _http(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = httpx.AsyncClient(timeout=httpx.Timeout(self._timeout))
        return self._client

    async def aclose(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    def build_ssml(self, text: str, voice: str, language: str) -> str:
        """Minimal SSML document for the synthesis endpoint."""
        locale = (language or "").strip() or "en-NG"
        return (
            "<speak version='1.0' xml:lang='{locale}'>"
            "<voice xml:lang='{locale}' name='{voice}'>{text}</voice>"
            "</speak>"
        ).format(locale=escape(locale), voice=escape(voice), text=escape(text))

    async def synthesize(self, text: str, voice: str, language: str) -> bytes:
        text = text.strip()
        if not text:
            return b""
        if not self.configured:
            raise RuntimeError(
                "azure TTS not configured (set AZURE_SPEECH_KEY and AZURE_SPEECH_REGION)"
            )
        voice = (voice or "").strip() or self.default_voice
        resp = await self._http().post(
            f"https://{self.region}.tts.speech.microsoft.com/cognitiveservices/v1",
            headers={
                "Ocp-Apim-Subscription-Key": self.api_key,
                "Content-Type": "application/ssml+xml",
                "X-Microsoft-OutputFormat": _OUTPUT_FORMAT,
                "User-Agent": "opendesk-voice-agent-runtime",
            },
            content=self.build_ssml(text, voice, language).encode("utf-8"),
        )
        resp.raise_for_status()
        return resp.content

    async def list_voices(self) -> list[Voice]:
        """Static curated catalog (AZURE_VOICES). The full Azure voice list is
        huge; we only surface the curated African set."""
        return list(AZURE_VOICES)

    async def available(self) -> bool:
        """Configuration probe: Azure has no cheap unauthenticated ping, so
        availability = credentials present (synthesize failures still trip the
        chain's circuit breaker)."""
        return self.configured
