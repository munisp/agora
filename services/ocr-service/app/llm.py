"""MiniCPM-OCR extraction via the llama.cpp sidecar (SPEC-W53 Scope A).

Real path: rasterize PDFs with pdftoppm (poppler-utils in the image —
slim-image impact documented in README), then ONE OpenAI-compatible
POST {OCR_LLM_BASE_URL}/chat/completions with image_url data parts
(base64) + an extraction prompt. Bounded timeout (OCR_TIMEOUT_S, default
120s), NO retries (RECON §5 CPU latency; a retry would double-bill
ocr_page). Tolerant structured parse: the model answer is located as JSON
(bare, or inside a ```json fence); receipt fields vendor/amount/date/items
fall back to nulls rather than failing the extraction.

Mock path (OCR_MOCK=true, DEV ONLY, CRITICAL-logged at startup): a fixed
deterministic fixture — no sidecar, no rasterizer, no network.
"""

from __future__ import annotations

import asyncio
import base64
import json
import re
import shutil
import subprocess
import tempfile
from dataclasses import dataclass, field
from typing import Any

import httpx

from .logging import get_logger
from .validate import pdf_page_count

log = get_logger(__name__)


class OcrUpstreamError(RuntimeError):
    """Sidecar/rasterizer failure — caller marks the row failed + 502."""


@dataclass
class Extraction:
    text: str
    fields: dict[str, Any]
    page_count: int
    model_version: str


# --- deterministic mock fixture (OCR_MOCK=true) ------------------------------
MOCK_TEXT = (
    "# Receipt\n\n"
    "ACME STORES LTD\n"
    "12 Marina Road, Lagos\n\n"
    "Date: 2024-01-15\n\n"
    "| Item | Qty | Price |\n"
    "| --- | --- | --- |\n"
    "| A4 Ream Paper | 2 | 4500.00 |\n"
    "| Ballpoint Pens (box) | 1 | 3500.00 |\n\n"
    "TOTAL: 12500.00 NGN\n"
)
MOCK_RECEIPT_FIELDS: dict[str, Any] = {
    "vendor": "ACME STORES LTD",
    "amount": 12500.00,
    "currency": "NGN",
    "date": "2024-01-15",
    "items": [
        {"name": "A4 Ream Paper", "quantity": 2, "price": 4500.00},
        {"name": "Ballpoint Pens (box)", "quantity": 1, "price": 3500.00},
    ],
}

_EXTRACTION_PROMPT = (
    "You are an OCR engine. Transcribe the document page image(s) into "
    "markdown. Then, on the LAST line, emit ONLY a JSON object with the "
    "structured fields. For receipts/invoices use "
    '{"vendor": str|null, "amount": number|null, "currency": str|null, '
    '"date": "YYYY-MM-DD"|null, "items": [{"name": str, "quantity": '
    "number|null, \"price\": number|null}]}. For other documents use "
    '{"title": str|null, "date": "YYYY-MM-DD"|null, "summary": str|null}. '
    "Use null for any field you cannot read. Never invent values."
)


def mock_extraction(kind: str, data: bytes, mime: str, model: str) -> Extraction:
    """Deterministic fixture (fixed vendor/amount/date) — e2e/dev only."""
    page_count = pdf_page_count(data) if mime == "application/pdf" else 1
    fields: dict[str, Any]
    if kind == "receipt":
        fields = dict(MOCK_RECEIPT_FIELDS)
    else:
        fields = {
            "title": "Mock Document",
            "date": "2024-01-15",
            "summary": "Deterministic OCR_MOCK fixture extraction.",
        }
    return Extraction(
        text=MOCK_TEXT,
        fields=fields,
        page_count=page_count,
        model_version=f"{model}+mock",
    )


# --- PDF rasterization (poppler-utils, real mode only) ------------------------
def rasterize_pdf(data: bytes, *, max_pages: int, dpi: int = 150) -> list[bytes]:
    """PDF -> list of PNG page images via pdftoppm (bounded pages)."""
    if shutil.which("pdftoppm") is None:
        raise OcrUpstreamError("pdftoppm (poppler-utils) not installed in image")
    with tempfile.TemporaryDirectory(prefix="ocr-") as tmp:
        src = f"{tmp}/in.pdf"
        with open(src, "wb") as fh:
            fh.write(data)
        try:
            proc = subprocess.run(
                [
                    "pdftoppm", "-png", "-r", str(dpi),
                    "-f", "1", "-l", str(max_pages),
                    src, f"{tmp}/page",
                ],
                capture_output=True,
                timeout=120,
                check=False,
            )
        except subprocess.TimeoutExpired as exc:
            raise OcrUpstreamError("pdftoppm timed out") from exc
        if proc.returncode != 0:
            raise OcrUpstreamError(
                f"pdftoppm failed rc={proc.returncode}: {proc.stderr[:300]!r}"
            )
        import glob
        import os

        pages = sorted(glob.glob(f"{tmp}/page-*.png"))
        if not pages:
            raise OcrUpstreamError("pdftoppm produced no pages")
        out = []
        for p in pages:
            with open(p, "rb") as fh:
                out.append(fh.read())
            os.unlink(p)
        return out


# --- OpenAI-compatible sidecar call -------------------------------------------
def _image_part(data: bytes, mime: str) -> dict[str, Any]:
    b64 = base64.b64encode(data).decode("ascii")
    return {
        "type": "image_url",
        "image_url": {"url": f"data:{mime};base64,{b64}"},
    }


def build_chat_request(model: str, pages: list[tuple[bytes, str]]) -> dict[str, Any]:
    """Request body for POST {base}/chat/completions (vision parts)."""
    content: list[dict[str, Any]] = [{"type": "text", "text": _EXTRACTION_PROMPT}]
    content.extend(_image_part(data, mime) for data, mime in pages)
    return {
        "model": model,
        "messages": [{"role": "user", "content": content}],
        "temperature": 0.0,
        "max_tokens": 4096,
    }


_JSON_RE = re.compile(r"\{.*\}", re.DOTALL)


def parse_model_answer(answer: str, kind: str) -> tuple[str, dict[str, Any]]:
    """Tolerant parse: split markdown text from the trailing JSON object.

    Accepts a bare JSON tail or a ```json fenced block; any parse problem
    degrades to null fields (never raises).
    """
    text = answer.strip()
    fields: dict[str, Any] | None = None
    fence = re.search(r"```(?:json)?\s*(\{.*?\})\s*```", text, re.DOTALL)
    candidate = fence.group(1) if fence else None
    if candidate is None:
        tail = _JSON_RE.search(text)
        candidate = tail.group(0) if tail else None
    if candidate is not None:
        try:
            parsed = json.loads(candidate)
            if isinstance(parsed, dict):
                fields = parsed
                # strip the JSON (fence or tail) from the markdown text
                if fence:
                    text = text[: fence.start()].strip()
                else:
                    m = _JSON_RE.search(text)
                    if m:
                        text = text[: m.start()].strip()
        except (ValueError, TypeError):
            fields = None
    if fields is None:
        fields = {}
    if kind == "receipt":
        items = fields.get("items")
        fields = {
            "vendor": fields.get("vendor") or None,
            "amount": _as_number(fields.get("amount")),
            "currency": fields.get("currency") or None,
            "date": fields.get("date") or None,
            "items": items if isinstance(items, list) else [],
        }
    else:
        fields = {
            "title": fields.get("title") or None,
            "date": fields.get("date") or None,
            "summary": fields.get("summary") or None,
        }
    return text, fields


def _as_number(value: Any) -> float | None:
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        return float(value)
    if isinstance(value, str):
        try:
            return float(value.replace(",", "").strip())
        except ValueError:
            return None
    return None


class OcrClient:
    """OpenAI-compatible llama-server client (one call, no retries)."""

    def __init__(self, *, base_url: str, model: str, timeout_s: float):
        self._base = base_url.rstrip("/")
        self._model = model
        self._client = httpx.AsyncClient(timeout=timeout_s)

    @property
    def model(self) -> str:
        return self._model

    async def aclose(self) -> None:
        await self._client.aclose()

    async def extract(
        self, *, kind: str, data: bytes, mime: str, max_pages: int
    ) -> Extraction:
        if mime == "application/pdf":
            pngs = await asyncio.to_thread(
                rasterize_pdf, data, max_pages=max_pages
            )
            pages: list[tuple[bytes, str]] = [(p, "image/png") for p in pngs]
        else:
            pages = [(data, mime)]
        body = build_chat_request(self._model, pages)
        try:
            resp = await self._client.post(
                f"{self._base}/chat/completions", json=body
            )
        except httpx.HTTPError as exc:
            raise OcrUpstreamError(f"llama-ocr request failed: {exc}") from exc
        if resp.status_code != 200:
            raise OcrUpstreamError(
                f"llama-ocr returned {resp.status_code}: {resp.text[:300]}"
            )
        try:
            payload = resp.json()
            answer = payload["choices"][0]["message"]["content"]
        except (ValueError, KeyError, IndexError, TypeError) as exc:
            raise OcrUpstreamError(f"llama-ocr malformed response: {exc}") from exc
        if not isinstance(answer, str) or not answer.strip():
            raise OcrUpstreamError("llama-ocr returned an empty completion")
        text, fields = parse_model_answer(answer, kind)
        return Extraction(
            text=text,
            fields=fields,
            page_count=len(pages),
            model_version=self._model,
        )
