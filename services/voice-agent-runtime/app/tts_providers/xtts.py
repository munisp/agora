"""XTTS (Coqui XTTS-v2 voice cloning) provider — HTTP client for the
xtts-tts sidecar (services/xtts-tts, SPEC-W10 cross-agent contract):

- ``POST   {XTTS_TTS_URL}/tts {"text", "voice_id", "language"}`` -> ``audio/wav``
- ``POST   {XTTS_TTS_URL}/voices {"name", "sample_base64"}`` -> ``{"voice_id"}``
- ``GET    {XTTS_TTS_URL}/voices`` -> ``{"voices": [...]}``
- ``DELETE {XTTS_TTS_URL}/voices/{id}``

30s timeout: XTTS runs on GPU with real synthesis latency (CPU unusable —
see docs/voices.md). Brand-voice enrollment is consent-gated at the admin
UI (NDPA); the runtime simply forwards the sample.
"""

from __future__ import annotations

import httpx

from ..logging import get_logger
from .base import Voice

log = get_logger("tts.xtts")


class XttsTTS:
    name = "xtts"

    def __init__(
        self,
        base_url: str = "http://xtts-tts:5810",
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
            raise RuntimeError("xtts synthesize requires a voice_id")
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

    async def enroll_voice(self, name: str, sample_base64: str) -> str:
        """Enroll a brand voice from a base64 audio sample -> voice_id."""
        resp = await self._http().post(
            f"{self.base_url}/voices",
            json={"name": name.strip(), "sample_base64": sample_base64},
        )
        resp.raise_for_status()
        data = resp.json()
        voice_id = data.get("voice_id") if isinstance(data, dict) else None
        if not voice_id:
            raise RuntimeError("xtts enrollment response missing voice_id")
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
            if not isinstance(item, dict) or not item.get("id"):
                continue
            out.append(
                Voice(
                    id=str(item["id"]),
                    languages=[str(l) for l in item.get("languages", []) or []],
                    gender=str(item.get("gender", "") or ""),
                    labels=dict(item.get("labels", {}) or {}),
                )
            )
        return out

    async def available(self) -> bool:
        try:
            resp = await self._http().get(f"{self.base_url}/voices")
            return resp.status_code == 200
        except Exception:  # noqa: BLE001 - probe must never raise
            return False
