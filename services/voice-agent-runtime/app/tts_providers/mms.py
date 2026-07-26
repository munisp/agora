"""MMS (Meta Massively Multilingual Speech) TTS provider — HTTP client for the
mms-tts sidecar (services/mms-tts, SPEC-W10 cross-agent contract):

- ``POST {MMS_TTS_URL}/tts {"text", "lang"}`` -> ``audio/wav``
  (langs are ISO-639-3: eng, pcm, yor, ibo, hau)
- ``GET  {MMS_TTS_URL}/voices`` -> ``{"voices": [{id, languages, gender, labels}]}``
- ``GET  {MMS_TTS_URL}/healthz``

10s timeout with one retry on transport errors / 5xx (sidecar cold model
loads are the common transient failure).
"""

from __future__ import annotations

import httpx

from ..logging import get_logger
from .base import Voice

log = get_logger("tts.mms")

# Runtime language tags (ISO-639 primary) -> MMS sidecar lang ids (ISO-639-3).
MMS_LANG_CODES = {"eng", "pcm", "yor", "ibo", "hau"}
LANG_TO_MMS = {
    "en": "eng",
    "pcm": "pcm",
    "yo": "yor",
    "ig": "ibo",
    "ha": "hau",
}


class MmsTTS:
    name = "mms"

    def __init__(
        self,
        base_url: str = "http://mms-tts:5800",
        *,
        timeout_s: float = 10.0,
        retries: int = 1,
        client: httpx.AsyncClient | None = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.default_voice = "eng"
        self._timeout = timeout_s
        self._retries = max(0, retries)
        self._client = client

    def _http(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = httpx.AsyncClient(timeout=httpx.Timeout(self._timeout))
        return self._client

    async def aclose(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    def _resolve_lang(self, voice: str, language: str) -> str:
        """`voice` on MMS is a sidecar lang id (mms:pcm -> "pcm"); fall back
        to mapping the runtime language tag, else the provider default."""
        voice = (voice or "").strip()
        if voice:
            return voice
        lang = (language or "").strip().split("-", 1)[0].lower()
        return LANG_TO_MMS.get(lang, self.default_voice)

    async def synthesize(self, text: str, voice: str, language: str) -> bytes:
        text = text.strip()
        if not text:
            return b""
        lang = self._resolve_lang(voice, language)
        last_exc: Exception | None = None
        for attempt in range(self._retries + 1):
            try:
                resp = await self._http().post(
                    f"{self.base_url}/tts", json={"text": text, "lang": lang}
                )
                resp.raise_for_status()
                return resp.content
            except (httpx.TransportError, httpx.HTTPStatusError) as exc:
                last_exc = exc
                # Retry transport failures and 5xx only; 4xx is a contract bug.
                if isinstance(exc, httpx.HTTPStatusError) and (
                    exc.response is not None and exc.response.status_code < 500
                ):
                    raise
                log.warning(
                    "mms /tts failed", attempt=attempt, lang=lang, error=str(exc)[:200]
                )
        assert last_exc is not None
        raise last_exc

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
