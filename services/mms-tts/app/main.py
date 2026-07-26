"""MMS-TTS sidecar — HTTP contract (SPEC-W10 Part B, consumed by Agent A).

    POST /tts   {"text": str, "lang": "eng|pcm|yor|ibo|hau"}  -> audio/wav
    GET  /voices -> {"voices": [{"id", "languages", "gender", "labels"}, ...]}
    GET  /healthz -> {"status": "ok", ...}

Config (env):
    MMS_MOCK=1        (default) deterministic sine wav, no model downloads.
                      Set MMS_MOCK=0 for real facebook/mms-tts-* inference.
    MMS_LANGS=eng,pcm (default) allow-list of real-model languages.
    PORT=5800         listen port when run as `python -m app.main`.
"""

from __future__ import annotations

import os

from fastapi import FastAPI, HTTPException
from fastapi.responses import Response
from pydantic import BaseModel, Field

from .synth import (
    SUPPORTED_LANGS,
    VOICE_CATALOG,
    LangNotEnabled,
    MMSModelPool,
    encode_wav,  # noqa: F401  (re-exported for tests/tools)
    mock_synthesize,
)

MAX_TEXT_LEN = 5000


class TTSRequest(BaseModel):
    text: str = Field(min_length=1, max_length=MAX_TEXT_LEN)
    lang: str = Field(min_length=1, max_length=8)


class TTSResponseHeaders:
    CONTENT_TYPE = "audio/wav"


def _parse_langs(raw: str) -> tuple[str, ...]:
    langs = tuple(part.strip() for part in raw.split(",") if part.strip())
    unknown = [lang for lang in langs if lang not in SUPPORTED_LANGS]
    if unknown:
        raise ValueError(
            f"MMS_LANGS contains unsupported language(s): {', '.join(unknown)}; "
            f"supported: {', '.join(SUPPORTED_LANGS)}"
        )
    return langs or ("eng", "pcm")


def create_app() -> FastAPI:
    mock = os.environ.get("MMS_MOCK", "1") != "0"
    enabled_langs = _parse_langs(os.environ.get("MMS_LANGS", "eng,pcm"))
    pool = None if mock else MMSModelPool(enabled_langs)

    app = FastAPI(title="opendesk-mms-tts", version="1.0.0")

    @app.get("/healthz")
    def healthz() -> dict:
        return {
            "status": "ok",
            "service": "mms-tts",
            "mock": mock,
            "supported_langs": list(SUPPORTED_LANGS),
            "enabled_langs": list(enabled_langs),
            "loaded_langs": list(pool.loaded_langs) if pool else [],
        }

    @app.get("/voices")
    def voices() -> dict:
        return {
            "voices": [
                {
                    "id": lang,
                    "languages": VOICE_CATALOG[lang]["languages"],
                    "gender": "unspecified",
                    "labels": VOICE_CATALOG[lang]["labels"],
                }
                for lang in SUPPORTED_LANGS
            ]
        }

    @app.post("/tts", response_class=Response)
    def tts(req: TTSRequest) -> Response:
        if req.lang not in SUPPORTED_LANGS:
            raise HTTPException(
                status_code=400,
                detail=(
                    f"unsupported lang '{req.lang}'; "
                    f"supported: {', '.join(SUPPORTED_LANGS)}"
                ),
            )
        if mock:
            wav = mock_synthesize(req.text, req.lang)
        else:
            assert pool is not None
            try:
                wav = pool.synthesize(req.text, req.lang)
            except LangNotEnabled as exc:
                raise HTTPException(status_code=400, detail=str(exc)) from exc
        return Response(
            content=wav,
            media_type=TTSResponseHeaders.CONTENT_TYPE,
            headers={"Content-Disposition": 'inline; filename="tts.wav"'},
        )

    return app


app = create_app()


def main() -> None:  # pragma: no cover - process entrypoint
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=int(os.environ.get("PORT", "5800")))


if __name__ == "__main__":  # pragma: no cover
    main()
