"""LongCat (Meituan LongCat-AudioDiT, MIT code + weights) TTS provider —
HTTP client for the longcat-tts sidecar (services/longcat-tts, SPEC-W102 C1):

- ``POST   {LONGCAT_TTS_URL}/tts {"text", "voice_id", "language"}`` -> ``audio/wav``
- ``POST   {LONGCAT_TTS_URL}/voices {"name", "sample_base64", "consent_basis",
  "source"[, "prompt_text"]}`` -> ``{"voice_id"}`` (sidecar fails closed:
  blank consent_basis -> 422)
- ``GET    {LONGCAT_TTS_URL}/voices`` -> ``{"voices": [...]}``
- ``DELETE {LONGCAT_TTS_URL}/voices/{id}``

30s timeout: real LongCat-AudioDiT runs on GPU (>=8 GB VRAM) with real
diffusion synthesis latency (CPU unusable — see docs/voices-sidecars.md).
LongCat-AudioDiT is MIT for BOTH code and weights (commercial-safe) and is
benchmarked ZH+EN ONLY (SIM 0.818 ZH / 0.786 EN) — it is NOT a Nigerian
language path; keep Spitch/Azure for pcm/yo/ha/ig. Zero-shot cloning needs
the enrolled sample's transcript (``prompt_text``); voices enrolled without
it honestly fall back to plain TTS (never faked cloning).

``prompt_text`` is optional and forward-compatible: the runtime enroll
endpoint (/voice-admin/voices/enroll) currently routes to XTTS only and
does not carry it, so callers omit it today; when a LongCat enrollment
path lands, the kwarg and the sidecar field are already wired.
"""

from __future__ import annotations

import httpx

from ..logging import get_logger
from .base import Voice

log = get_logger("tts.longcat")


class LongCatTTS:
    name = "longcat"

    def __init__(
        self,
        base_url: str = "http://longcat-tts:5830",
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
            raise RuntimeError("longcat synthesize requires a voice_id")
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
        self,
        name: str,
        sample_base64: str,
        *,
        consent_basis: str,
        source: str,
        prompt_text: str = "",
    ) -> str:
        """Enroll a voice from a base64 audio sample -> voice_id.

        ``consent_basis`` is forwarded verbatim; the sidecar fails closed
        (422) on a missing/blank value, so consent cannot be skipped here.
        ``prompt_text`` (the sample's transcript, needed for zero-shot
        cloning) is only sent when non-empty so the wire shape matches the
        other sidecar providers when unused.
        """
        payload: dict[str, str] = {
            "name": name.strip(),
            "sample_base64": sample_base64,
            "consent_basis": consent_basis,
            "source": source,
        }
        if prompt_text.strip():
            payload["prompt_text"] = prompt_text.strip()
        resp = await self._http().post(f"{self.base_url}/voices", json=payload)
        resp.raise_for_status()
        data = resp.json()
        voice_id = data.get("voice_id") if isinstance(data, dict) else None
        if not voice_id:
            raise RuntimeError("longcat enrollment response missing voice_id")
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
