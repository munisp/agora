"""CosyVoice (FunAudioLLM CosyVoice-2, Apache-2.0) TTS provider — HTTP client
for the cosyvoice-tts sidecar (services/cosyvoice-tts, SPEC-W72 A1/A2):

- ``POST   {COSYVOICE_TTS_URL}/tts {"text", "voice_id", "language"}`` -> ``audio/wav``
- ``POST   {COSYVOICE_TTS_URL}/voices {"name", "sample_base64", "consent_basis", "source"}``
  -> ``{"voice_id"}`` (sidecar fails closed: blank consent_basis -> 422)
- ``GET    {COSYVOICE_TTS_URL}/voices`` -> ``{"voices": [...]}``
- ``DELETE {COSYVOICE_TTS_URL}/voices/{id}``

30s timeout: real CosyVoice-2 runs on GPU with real synthesis latency (CPU
unusable — see docs/voices-sidecars.md). CosyVoice-2 is Apache-2.0
(commercial-safe), the recommended path for OWNED Nigerian voices; every
sidecar voice record carries consent metadata (docs/VOICES.md).
"""

from __future__ import annotations

import httpx

from ..logging import get_logger
from .base import Voice

log = get_logger("tts.cosyvoice")


class CosyVoiceTTS:
    name = "cosyvoice"

    def __init__(
        self,
        base_url: str = "http://cosyvoice-tts:5820",
        *,
        timeout_s: float = 30.0,
        default_voice: str = "",
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.default_voice = default_voice
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
        voice_id = (voice or "").strip() or self.default_voice
        if not voice_id:
            raise RuntimeError("cosyvoice synthesize requires a voice_id")
        resp = await self._http().post(
            f"{self.base_url}/tts",
            json={
                "text": text,
                "voice_id": voice_id,
                "language": (language or "").strip() or "en",
            },
        )
        resp.raise_for_status()
        return resp.content

    async def enroll_voice(
        self, name: str, sample_base64: str, *, consent_basis: str, source: str
    ) -> str:
        """Enroll a voice from a base64 audio sample -> voice_id.

        ``consent_basis`` is forwarded verbatim; the sidecar fails closed
        (422) on a missing/blank value, so consent cannot be skipped here.
        """
        resp = await self._http().post(
            f"{self.base_url}/voices",
            json={
                "name": name.strip(),
                "sample_base64": sample_base64,
                "consent_basis": consent_basis,
                "source": source,
            },
        )
        resp.raise_for_status()
        data = resp.json()
        voice_id = data.get("voice_id") if isinstance(data, dict) else None
        if not voice_id:
            raise RuntimeError("cosyvoice enrollment response missing voice_id")
        return str(voice_id)

    async def delete_voice(self, voice_id: str) -> None:
        resp = await self._http().delete(f"{self.base_url}/voices/{voice_id}")
        resp.raise_for_status()

    async def list_voices(self) -> list[Voice]:
        resp = await self._http().get(f"{self.base_url}/voices")
        resp.raise_for_status()
        data = resp.json()
        out: list[Voice] = []
        for item in data.get("voices", []) if isinstance(data, dict) else []:
            voice_id = item.get("voice_id") if isinstance(item, dict) else None
            if not voice_id:
                continue
            out.append(
                Voice(
                    id=str(voice_id),
                    languages=[str(l) for l in item.get("languages", []) or []],
                    labels={
                        "name": str(item.get("name", "") or ""),
                        "source": str(item.get("source", "") or ""),
                        "license": str(item.get("license", "") or ""),
                    },
                )
            )
        return out

    async def available(self) -> bool:
        try:
            resp = await self._http().get(f"{self.base_url}/voices")
            return resp.status_code == 200
        except Exception:  # noqa: BLE001 - probe must never raise
            return False
