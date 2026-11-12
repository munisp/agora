"""Upload validation + content hashing (SPEC-W53 Scope A).

Validation posture (RECON §4, geo-import precedent): hard byte cap,
extension + magic-byte allow-list — the extension and the sniffed magic
MUST agree (a `.png` carrying JPEG magic is rejected). There is no AV
scanner anywhere in the platform; that gap is documented in README/docs.

phash: a dependency-free perceptual-ish hash (inline aHash/dHash over a
9x8 grayscale grid). PNG inputs are decoded for real (stdlib zlib +
un-filtering, 8-bit gray/RGB/RGBA non-interlaced); JPEG/WebP/PDF fall back
to the same grid computed over the raw byte stream (catches re-saves by
the same encoder; exact duplicates are already caught by sha256). This
deliberately avoids imagehash/Pillow in the slim image (I5) — it is a
duplicate-clustering signal for fraud D9 prep, not a forensic hash.
"""

from __future__ import annotations

import hashlib
import struct
import zlib

# --- extension <-> mime allow-list -----------------------------------------
EXT_TO_MIME = {
    ".jpg": "image/jpeg",
    ".jpeg": "image/jpeg",
    ".png": "image/png",
    ".webp": "image/webp",
    ".pdf": "application/pdf",
}

# --- magic-byte signatures --------------------------------------------------
_MAGIC = (
    (b"\xff\xd8\xff", "image/jpeg"),
    (b"\x89PNG\r\n\x1a\n", "image/png"),
    (b"%PDF-", "application/pdf"),
)


class ValidationError(ValueError):
    """Rejected upload (maps to HTTP 415/413 in the route layer)."""


def sniff_mime(data: bytes) -> str | None:
    """Magic-byte sniff; WebP needs RIFF<4 bytes>WEBP (12-byte check)."""
    if len(data) >= 12 and data[:4] == b"RIFF" and data[8:12] == b"WEBP":
        return "image/webp"
    for sig, mime in _MAGIC:
        if len(data) >= len(sig) and data[: len(sig)] == sig:
            return mime
    return None


def validate_upload(filename: str, data: bytes, *, max_bytes: int) -> tuple[str, str]:
    """Validate size + extension + magic bytes.

    Returns (extension, mime). Raises ValidationError otherwise.
    """
    if not data:
        raise ValidationError("empty upload")
    if len(data) > max_bytes:
        raise ValidationError(f"upload exceeds hard cap of {max_bytes} bytes")
    dot = filename.rfind(".")
    ext = filename[dot:].lower() if dot >= 0 else ""
    if ext not in EXT_TO_MIME:
        raise ValidationError(
            f"unsupported extension {ext!r}; allowed: {sorted(EXT_TO_MIME)}"
        )
    mime = sniff_mime(data)
    if mime is None:
        raise ValidationError("unrecognized or truncated content (magic-byte check failed)")
    if mime != EXT_TO_MIME[ext]:
        raise ValidationError(
            f"extension {ext!r} implies {EXT_TO_MIME[ext]} but content sniffs as {mime}"
        )
    return ext, mime


def sha256_hex(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


# --- PDF page count (pure-python heuristic; mock mode never rasterizes) ------
def pdf_page_count(data: bytes) -> int:
    """Count `/Type /Page` objects (excluding /Pages) — best-effort.

    Authoritative page_count in real mode comes from the rasterizer; this
    keeps mock mode free of any poppler dependency.
    """
    import re

    return max(1, len(re.findall(rb"/Type\s*/Page(?![s/\w])", data)))


# --- perceptual hash (inline dHash, no heavy deps) ---------------------------
_GRID_W, _GRID_H = 9, 8  # dHash compares horizontally adjacent pixels


def _paeth(a: int, b: int, c: int) -> int:
    p = a + b - c
    pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
    if pa <= pb and pa <= pc:
        return a
    return b if pb <= pc else c


def _decode_png_gray(data: bytes) -> tuple[int, int, list[int]] | None:
    """Decode a non-interlaced 8-bit PNG (gray/RGB/RGBA) to grayscale rows.

    Returns (width, height, gray_bytes) or None when the PNG uses features
    outside the minimal decoder (caller falls back to the byte grid).
    """
    if data[:8] != b"\x89PNG\r\n\x1a\n":
        return None
    pos = 8
    width = height = 0
    bit_depth = color_type = None
    interlace = 0
    idat = bytearray()
    while pos + 8 <= len(data):
        (length,) = struct.unpack(">I", data[pos : pos + 4])
        ctype = data[pos + 4 : pos + 8]
        chunk = data[pos + 8 : pos + 8 + length]
        if len(chunk) < length:
            return None  # truncated
        pos += 12 + length
        if ctype == b"IHDR":
            width, height, bit_depth, color_type, _, _, interlace = struct.unpack(
                ">IIBBBBB", chunk
            )
        elif ctype == b"IDAT":
            idat += chunk
        elif ctype == b"IEND":
            break
    if not width or not height or bit_depth != 8 or interlace != 0:
        return None
    channels = {0: 1, 2: 3, 4: 2, 6: 4}.get(color_type)
    if channels is None:
        return None
    try:
        raw = zlib.decompress(bytes(idat))
    except zlib.error:
        return None
    stride = width * channels
    if len(raw) < (stride + 1) * height:
        return None  # truncated image data
    gray: list[int] = []
    prev = bytearray(stride)
    off = 0
    for _y in range(height):
        ftype = raw[off]
        line = bytearray(raw[off + 1 : off + 1 + stride])
        off += stride + 1
        for i in range(stride):
            a = line[i - channels] if i >= channels else 0
            b = prev[i]
            c = prev[i - channels] if i >= channels else 0
            if ftype == 1:
                line[i] = (line[i] + a) & 0xFF
            elif ftype == 2:
                line[i] = (line[i] + b) & 0xFF
            elif ftype == 3:
                line[i] = (line[i] + (a + b) // 2) & 0xFF
            elif ftype == 4:
                line[i] = (line[i] + _paeth(a, b, c)) & 0xFF
            elif ftype != 0:
                return None
        prev = line
        for x in range(width):
            base = x * channels
            if channels == 1:
                gray.append(line[base])
            else:
                r, g, b = line[base], line[base + 1], line[base + 2]
                gray.append((r * 299 + g * 587 + b * 114) // 1000)
    return width, height, gray


def _byte_grid(data: bytes, width: int, height: int) -> list[int]:
    """Fallback grid: sample the raw byte stream into a width*height grid.

    Deterministic per content; near-identical encodes cluster together.
    """
    if len(data) < 2:
        data = data + b"\x00" * (2 - len(data))
    grid: list[int] = []
    n = len(data)
    for gy in range(height):
        for gx in range(width):
            start = (gy * width + gx) * n // (width * height)
            end = ((gy * width + gx + 1) * n) // (width * height)
            cell = data[start:end] or b"\x00"
            grid.append(sum(cell) // len(cell))
    return grid


def perceptual_hash(data: bytes, mime: str) -> str:
    """64-bit dHash over a 9x8 grayscale grid, hex-encoded.

    PNGs are decoded to pixels (real perceptual hash); other formats use
    the byte-grid fallback (documented in the module docstring).
    """
    grid: list[int]
    decoded = _decode_png_gray(data) if mime == "image/png" else None
    if decoded is not None:
        w, h, gray = decoded
        grid = []
        for gy in range(_GRID_H):
            y0 = gy * h // _GRID_H
            for gx in range(_GRID_W):
                x0 = gx * w // _GRID_W
                x1 = max(x0 + 1, (gx + 1) * w // _GRID_W)
                y1 = max(y0 + 1, (gy + 1) * h // _GRID_H)
                total = cnt = 0
                for yy in range(y0, min(y1, h)):
                    row = yy * w
                    for xx in range(x0, min(x1, w)):
                        total += gray[row + xx]
                        cnt += 1
                grid.append(total // max(cnt, 1))
    else:
        grid = _byte_grid(data, _GRID_W, _GRID_H)
    bits = 0
    for gy in range(_GRID_H):
        row = gy * _GRID_W
        for gx in range(_GRID_W - 1):
            bits = (bits << 1) | (1 if grid[row + gx] > grid[row + gx + 1] else 0)
    return f"{bits:016x}"
