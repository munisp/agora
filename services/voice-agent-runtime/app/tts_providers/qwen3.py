"""Qwen3-TTS (nari-labs/nari-qwen3-tts serving stack) TTS provider — HTTP
client for the published GPU serving container (SPEC-W74 A1). The published
image ``ghcr.io/nari-labs/nari-qwen3-tts:latest`` IS the sidecar (no shim):
the provider talks DIRECTLY to the nari server's OpenAI-shaped HTTP API:

- ``POST {QWEN3_TTS_URL}/v1/audio/speech``
  ``{"model", "input", "voice", "language", "response_format", "stream"}``
  -> ``audio/wav`` bytes (OpenAI Audio Speech request shape; verified against
  the nari-qwen3-tts repo README curl example)
- ``GET  {QWEN3_TTS_URL}/ready`` -> readiness probe. The server holds /ready
  false until model load + CUDA-graph capture + a warm-up TTS request have
  ALL completed (slow boot — compose healthcheck uses a long start_period).
- ``GET  {QWEN3_TTS_URL}/v1/models`` -> model catalog (informational).

Deployment caveats (docs/voices-sidecars.md): the container requires an
NVIDIA H100 (CUDA 13.0 driver) and the engine has been TESTED ONLY WITH
ENGLISH as the primary language — this is NOT a Nigerian-language path
(cosyvoice/mms/spitch remain those). The serving stack AND the Qwen3-TTS
model are Apache-2.0 (docs/voice-licensing.md).
"""

from __future__ import annotations

import httpx

from ..logging import get_logger
from .base import Voice

log = get_logger("tts.qwen3")


class Qwen3TTS:
    """TTSProvider impl for the nari Qwen3-TTS GPU serving container."""

    name = "qwen3"

    def __init__(
        self,
        base_url: str = "http://qwen3-tts:8000",
        *,
        model: str = "Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice",
        default_voice: str = "ryan",
        default_language: str = "english",
        timeout_s: float = 30.0,
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.default_voice = default_voice
        self.default_language = default_language
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
        """Synthesize `text` -> RIFF wav bytes (non-streaming OpenAI shape)."""
        text = text.strip()
        if not text:
            return b""
        resp = await self._http().post(
            f"{self.base_url}/v1/audio/speech",
            json={
                "model": self.model,
                "input": text,
                "voice": (voice or "").strip() or self.default_voice,
                "language": (language or "").strip() or self.default_language,
                "response_format": "wav",
                "stream": False,
            },
        )
        resp.raise_for_status()
        return resp.content

    async def list_voices(self) -> list[Voice]:
        """Static default-voice catalog (kept simple and honest: the nari
        server has no voice-listing endpoint — /v1/models lists the MODEL,
        and the CustomVoice timbres are selected via the request's `voice`
        field; the configured default is the known-good entry)."""
        return [
            Voice(
                id=self.default_voice,
                languages=[self.default_language],
                labels={"engine": "qwen3-tts", "model": self.model},
            )
        ]

    async def available(self) -> bool:
        """GET /ready — the server reports ready only after CUDA-graph
        capture + warm-up complete, so a False here usually means 'still
        booting', not 'down'. Probe must never raise."""
        try:
            resp = await self._http().get(f"{self.base_url}/ready")
            return resp.status_code == 200
        except Exception:  # noqa: BLE001 - probe must never raise
            return False
