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

# SPEC-W58 A3: per-kind KYC mock fixtures (synthetic values only — the
# passport MRZ is a fabricated specimen whose check digits VALIDATE, so the
# mock pipeline exercises the code-side ICAO 9303 path end to end).
MOCK_KYC_TEXT = (
    "# KYC Document\n\n"
    "FEDERAL REPUBLIC OF NIGERIA\n\n"
    "Synthetic OCR_MOCK fixture transcription (dev/e2e only).\n"
)
MOCK_KYC_PASSPORT_MRZ = (
    "P<NGADOE<<JANE<CHIDINMA<<<<<<<<<<<<<<<<<<<<<\n"
    "A012345690NGA9004126F3012316<<<<<<<<<<<<<<00"
)
MOCK_KYC_FIELDS: dict[str, dict[str, Any]] = {
    "kyc_nin_slip": {
        "full_name": "ADAEZE CHINWE OKAFOR",
        "dob": "1990-04-12",
        "nin": "12345678901",
        "address": "12 Marina Road, Lagos",
    },
    "kyc_drivers_license": {
        "full_name": "ADAEZE CHINWE OKAFOR",
        "dob": "1990-04-12",
        "license_no": "FKJ12345AB",
        "expiry": "2027-06-30",
    },
    "kyc_passport": {
        "full_name": "JANE CHIDINMA DOE",
        "dob": "1990-04-12",
        "passport_no": "A01234569",
        "nationality": "NGA",
        "expiry": "2030-12-31",
        "mrz": MOCK_KYC_PASSPORT_MRZ,
    },
    "kyc_voters_card": {
        "full_name": "ADAEZE CHINWE OKAFOR",
        "vin": "90F5B12345678901234",
        "dob": "1990-04-12",
    },
    "kyc_cac_document": {
        "company_name": "ACME STORES LTD",
        "rc_number": "RC1234567",
        "registration_date": "2015-03-20",
        "address": "12 Marina Road, Lagos",
    },
}

# Receipt prompt BYTE-PRESERVED from SPEC-W53 (P14: receipt/knowledge_scan
# behaviour is unchanged — all three legacy kinds use this exact prompt).
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

_KYC_PROMPT_HEADER = (
    "You are an OCR engine. Transcribe the Nigerian identity document page "
    "image(s) into markdown. Then, on the LAST line, emit ONLY a JSON object "
    "with the structured fields "
)

# SPEC-W58 A3/P9: kind -> prompt. The extraction model only transcribes; MRZ
# check-digit VALIDATION is deterministic code (parse layer, P7), never the
# model — the passport prompt asks for the raw MRZ lines, not a verdict.
EXTRACTION_PROMPTS: dict[str, str] = {
    "receipt": _EXTRACTION_PROMPT,
    "document": _EXTRACTION_PROMPT,
    "knowledge_scan": _EXTRACTION_PROMPT,
    "kyc_nin_slip": (
        _KYC_PROMPT_HEADER
        + '{"full_name": str|null, "dob": "YYYY-MM-DD"|null, "nin": str|null, '
        + '"address": str|null}. Use null for any field you cannot read. '
        + "Never invent values."
    ),
    "kyc_drivers_license": (
        _KYC_PROMPT_HEADER
        + '{"full_name": str|null, "dob": "YYYY-MM-DD"|null, '
        + '"license_no": str|null, "expiry": "YYYY-MM-DD"|null}. '
        + "Use null for any field you cannot read. Never invent values."
    ),
    "kyc_passport": (
        _KYC_PROMPT_HEADER
        + '{"full_name": str|null, "dob": "YYYY-MM-DD"|null, '
        + '"passport_no": str|null, "nationality": str|null, '
        + '"expiry": "YYYY-MM-DD"|null, "mrz": str|null}. For "mrz" '
        + "transcribe the two 44-character machine-readable zone lines "
        + "verbatim, joined by a newline. Use null for any field you cannot "
        + "read. Never invent values."
    ),
    "kyc_voters_card": (
        _KYC_PROMPT_HEADER
        + '{"full_name": str|null, "vin": str|null, "dob": "YYYY-MM-DD"|null}. '
        + "Use null for any field you cannot read. Never invent values."
    ),
    "kyc_cac_document": (
        _KYC_PROMPT_HEADER
        + '{"company_name": str|null, "rc_number": str|null, '
        + '"registration_date": "YYYY-MM-DD"|null, "address": str|null}. '
        + "Use null for any field you cannot read. Never invent values."
    ),
}


def prompt_for_kind(kind: str) -> str:
    return EXTRACTION_PROMPTS.get(kind, _EXTRACTION_PROMPT)


def mock_extraction(kind: str, data: bytes, mime: str, model: str) -> Extraction:
    """Deterministic fixture (fixed vendor/amount/date) — e2e/dev only."""
    page_count = pdf_page_count(data) if mime == "application/pdf" else 1
    fields: dict[str, Any]
    text = MOCK_TEXT
    if kind == "receipt":
        fields = dict(MOCK_RECEIPT_FIELDS)
    elif kind in MOCK_KYC_FIELDS:
        text = MOCK_KYC_TEXT
        fields = dict(MOCK_KYC_FIELDS[kind])
        if kind == "kyc_passport":
            # MRZ validity is CODE, never claimed from the fixture (P7).
            fields["mrz_valid"] = validate_mrz(fields.pop("mrz", None))
    else:
        fields = {
            "title": "Mock Document",
            "date": "2024-01-15",
            "summary": "Deterministic OCR_MOCK fixture extraction.",
        }
    return Extraction(
        text=text,
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


def build_chat_request(
    model: str, pages: list[tuple[bytes, str]], kind: str = "receipt"
) -> dict[str, Any]:
    """Request body for POST {base}/chat/completions (vision parts)."""
    content: list[dict[str, Any]] = [{"type": "text", "text": prompt_for_kind(kind)}]
    content.extend(_image_part(data, mime) for data, mime in pages)
    return {
        "model": model,
        "messages": [{"role": "user", "content": content}],
        "temperature": 0.0,
        "max_tokens": 4096,
    }


_JSON_RE = re.compile(r"\{.*\}", re.DOTALL)

# SPEC-W58 P9: per-kind KYC field whitelists. Tolerant parse, null-fallback;
# the whitelist DROPS everything else the model emits. `mrz_valid` for
# kyc_passport is code-computed (never taken from the model), so it is not
# listed here — it is attached after validation.
KYC_FIELD_WHITELISTS: dict[str, tuple[str, ...]] = {
    "kyc_nin_slip": ("full_name", "dob", "nin", "address"),
    "kyc_drivers_license": ("full_name", "dob", "license_no", "expiry"),
    "kyc_passport": ("full_name", "dob", "passport_no", "nationality", "expiry"),
    "kyc_voters_card": ("full_name", "vin", "dob"),
    "kyc_cac_document": ("company_name", "rc_number", "registration_date", "address"),
}


# --- ICAO 9303 MRZ check-digit validation (deterministic code, P7) ----------
_MRZ_WEIGHTS = (7, 3, 1)


def _mrz_char_value(ch: str) -> int:
    if "0" <= ch <= "9":
        return ord(ch) - ord("0")
    if "A" <= ch <= "Z":
        return ord(ch) - ord("A") + 10
    if ch == "<":
        return 0
    return -1


def _mrz_check_digits_ok(field: str, digit: str) -> bool:
    """One ICAO 9303 check digit: sum(char_value * 7/3/1 cycling) mod 10."""
    if len(digit) != 1 or not digit.isdigit():
        return False
    total = 0
    for i, ch in enumerate(field):
        value = _mrz_char_value(ch)
        if value < 0:
            return False
        total += value * _MRZ_WEIGHTS[i % 3]
    return total % 10 == int(digit)


def _mrz_check(field: str, digit: str) -> bool:
    if not field.strip("<"):
        # Optional field left entirely as filler: ICAO permits the check
        # position to be filler OR the computed digit — accept either.
        return digit == "<" or digit.isdigit()
    return _mrz_check_digits_ok(field, digit)


def validate_mrz(mrz: Any) -> bool | None:
    """ICAO 9303 TD3 (passport) check-digit validation, in CODE (P7).

    Returns None when no MRZ was transcribed (no claim either way), False
    when a supplied MRZ is malformed or any check digit fails, True only
    when every applicable check digit validates. Never raises.
    """
    if not isinstance(mrz, str) or not mrz.strip():
        return None
    try:
        lines = [
            "".join(ch for ch in ln.strip().upper() if ch != " ")
            for ln in mrz.strip().splitlines()
            if ln.strip()
        ]
        if len(lines) != 2 or any(len(ln) != 44 for ln in lines):
            return False
        line1, line2 = lines
        if not line1.startswith("P"):
            return False
        # passport number + check digit (chars 1-9 / 10)
        if not _mrz_check(line2[0:9], line2[9]):
            return False
        # date of birth + check digit (chars 14-19 / 20)
        if not _mrz_check(line2[13:19], line2[19]):
            return False
        # expiry + check digit (chars 22-27 / 28)
        if not _mrz_check(line2[21:27], line2[27]):
            return False
        # personal number + check digit (chars 29-42 / 43) when present
        if not _mrz_check(line2[28:42], line2[42]):
            return False
        # composite check digit (char 44) over 1-10, 14-20, 22-43
        composite = line2[0:10] + line2[13:20] + line2[21:43]
        if not _mrz_check_digits_ok(composite, line2[43]):
            return False
        return True
    except Exception:  # tolerant: a malformed MRZ is invalid, never a crash
        return False


def _as_str_field(value: Any) -> str | None:
    if isinstance(value, str):
        return value.strip() or None
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        return str(value)
    return None


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
    if kind in KYC_FIELD_WHITELISTS:
        # SPEC-W58 P9: tolerant whitelist parse — whitelisted string fields
        # with null fallback; EVERYTHING else the model emitted is dropped.
        whitelist = KYC_FIELD_WHITELISTS[kind]
        parsed_kyc = {name: _as_str_field(fields.get(name)) for name in whitelist}
        if kind == "kyc_passport":
            # MRZ check digits validated in CODE (ICAO 9303, P7); the raw
            # MRZ itself is NOT retained in the fields payload.
            parsed_kyc["mrz_valid"] = validate_mrz(fields.get("mrz"))
        fields = parsed_kyc
    elif kind == "receipt":
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
        body = build_chat_request(self._model, pages, kind)
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
