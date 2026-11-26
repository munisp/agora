"""Text from an uploaded policy document, for policy rules (2026-11-26; copied from knowledge-service app/extract.py,
which reads the same files for the receptionist's knowledge): PDF (text layer), Word (.docx), plain text / Markdown.
Scanned PDFs (no text layer) are refused.
"""

from __future__ import annotations

import io
import re

MAX_BYTES = 25 * 1024 * 1024
MAX_PAGES = 500
MAX_CHARS = 200_000

ACCEPTED = (".pdf", ".docx", ".txt", ".md")


class ExtractError(ValueError):
    """The file cannot be turned into text (message is safe to show the user)."""


def _clean(text: str) -> str:
    text = text.replace("\x00", "").replace("\r\n", "\n").replace("\r", "\n")
    text = re.sub(r"[ \t ]+", " ", text)
    text = re.sub(r"(\w)-\n(\w)", r"\1\2", text)  # words hyphenated across PDF lines
    text = re.sub(r"\n{3,}", "\n\n", text)
    return text.strip()


def _pdf(data: bytes) -> tuple[str, int]:
    from pypdf import PdfReader
    from pypdf.errors import PdfReadError

    try:
        reader = PdfReader(io.BytesIO(data))
        if reader.is_encrypted:
            try:
                reader.decrypt("")
            except Exception as exc:  # noqa: BLE001
                raise ExtractError("This PDF is password-protected. Remove the password and upload it again.") from exc
        pages = reader.pages
        if len(pages) > MAX_PAGES:
            raise ExtractError(f"This PDF has {len(pages)} pages; the limit is {MAX_PAGES}. Split it and upload the parts.")
        parts = []
        for n, page in enumerate(pages, 1):
            t = (page.extract_text() or "").strip()
            if t:
                parts.append(f"[Page {n}]\n{t}")
        return "\n\n".join(parts), len(pages)
    except PdfReadError as exc:
        raise ExtractError("This file is not a readable PDF.") from exc


def _docx(data: bytes) -> tuple[str, int]:
    import docx  # python-docx

    try:
        d = docx.Document(io.BytesIO(data))
    except Exception as exc:  # noqa: BLE001 - corrupt / not really a .docx
        raise ExtractError("This file is not a readable Word document (.docx).") from exc
    lines = [p.text for p in d.paragraphs if p.text.strip()]
    for table in d.tables:
        for row in table.rows:
            cells = [c.text.strip() for c in row.cells if c.text.strip()]
            if cells:
                lines.append(" | ".join(cells))
    return "\n".join(lines), 0


def extract_text(filename: str, data: bytes) -> tuple[str, int]:
    """(text, page count; 0 when not paged). Raises ExtractError with a user-facing message."""
    name = (filename or "").lower().strip()
    if not data:
        raise ExtractError("The file is empty.")
    if len(data) > MAX_BYTES:
        raise ExtractError("The file is larger than 25 MB. Compress or split it and upload again.")
    if name.endswith(".pdf"):
        text, pages = _pdf(data)
    elif name.endswith(".docx"):
        text, pages = _docx(data)
    elif name.endswith((".txt", ".md")):
        text, pages = data.decode("utf-8", errors="replace"), 0
    else:
        raise ExtractError("Upload a PDF, Word (.docx), .txt or .md file.")
    text = _clean(text)
    if len(text) < 20:
        if name.endswith(".pdf"):
            raise ExtractError("No text found in this PDF: it looks scanned. Upload a PDF with text, a Word file or a .txt.")
        raise ExtractError("No text found in this file.")
    if len(text) > MAX_CHARS:
        raise ExtractError("The document is too long (over 200,000 characters). Split it and upload the parts.")
    return text, pages
