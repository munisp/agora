"""Docling doc-intel sidecar client (SPEC-W107).

The ONLY document-extraction engine on the knowledge ingest path: uploads are
converted by a docling-serve sidecar (docling layout-aware parsing, RapidOCR
for scanned content, tables -> Markdown). There is intentionally no local
fallback parser — a silent text-only degradation would be a mock by another
name. When the sidecar is unreachable the endpoint answers 503 honestly.

API target: docling-serve v1 (POST {base}/v1/convert/source), unified
`sources` array with {"kind": "file", "base64_string", "filename"} and
inbody target. Response: {"document": {"md_content": ...}, "status": ...}.
"""

from __future__ import annotations

import base64
from dataclasses import dataclass

import httpx


class DocIntelUnavailable(Exception):
    """Sidecar unreachable or timed out (mapped to 503)."""


class DocIntelRejected(Exception):
    """Sidecar refused/failed the conversion (mapped to 422)."""


class DocIntelEmpty(Exception):
    """Conversion succeeded but produced no text (mapped to 422)."""


@dataclass(frozen=True)
class Extraction:
    text: str
    processing_time_s: float


class DocIntelClient:
    def __init__(self, base_url: str, *, api_key: str = "", timeout_s: float = 300.0):
        self._base = base_url.rstrip("/")
        self._api_key = api_key
        self._timeout = timeout_s

    def _headers(self) -> dict[str, str]:
        return {"X-Api-Key": self._api_key} if self._api_key else {}

    async def convert(self, filename: str, data: bytes) -> Extraction:
        payload = {
            "options": {
                "to_formats": ["md"],
                "do_ocr": True,
                "ocr_engine": "rapidocr",
                "do_table_structure": True,
                "table_mode": "fast",
                "image_export_mode": "placeholder",
            },
            "sources": [
                {
                    "kind": "file",
                    "base64_string": base64.b64encode(data).decode("ascii"),
                    "filename": filename,
                }
            ],
            "target": {"kind": "inbody"},
        }
        try:
            async with httpx.AsyncClient(timeout=self._timeout) as client:
                resp = await client.post(
                    f"{self._base}/v1/convert/source",
                    json=payload,
                    headers=self._headers(),
                )
        except (httpx.ConnectError, httpx.TimeoutException, httpx.NetworkError) as exc:
            raise DocIntelUnavailable(
                f"doc-intel sidecar unreachable at {self._base}: {exc!r}"
            ) from exc
        if resp.status_code != 200:
            raise DocIntelRejected(
                f"doc-intel conversion refused (HTTP {resp.status_code}): "
                f"{resp.text[:500]}"
            )
        body = resp.json()
        status = body.get("status")
        if status == "failure":
            errors = body.get("errors") or []
            raise DocIntelRejected(f"doc-intel conversion failed: {errors!r}")
        document = body.get("document") or {}
        text = (document.get("md_content") or "").strip()
        if not text:
            text = (document.get("text_content") or "").strip()
        if not text:
            raise DocIntelEmpty(
                "doc-intel extracted no text. For scanned documents enable the "
                "OCR path (RapidOCR/PaddleOCR models, force_ocr) in the "
                "doc-intel sidecar — see docs/document-intelligence.md."
            )
        return Extraction(
            text=text,
            processing_time_s=float(body.get("processing_time") or 0.0),
        )

    async def health(self) -> bool:
        try:
            async with httpx.AsyncClient(timeout=5.0) as client:
                resp = await client.get(f"{self._base}/health", headers=self._headers())
            return resp.status_code == 200
        except httpx.HTTPError:
            return False
