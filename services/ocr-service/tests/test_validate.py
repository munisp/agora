"""Upload validation matrix (SPEC-W53 Scope A item 6): magic bytes vs
extension, truncation, size cap."""

from __future__ import annotations

import pytest

from app.validate import (
    ValidationError,
    pdf_page_count,
    sha256_hex,
    sniff_mime,
    validate_upload,
)
from conftest import make_jpeg, make_pdf, make_png, make_webp

CAP = 25 * 1024 * 1024


@pytest.mark.parametrize(
    "filename,data,mime",
    [
        ("receipt.jpg", make_jpeg(), "image/jpeg"),
        ("receipt.jpeg", make_jpeg(), "image/jpeg"),
        ("scan.png", make_png(), "image/png"),
        ("photo.webp", make_webp(), "image/webp"),
        ("doc.pdf", make_pdf(), "application/pdf"),
        ("UPPER.PNG", make_png(), "image/png"),  # extension case-insensitive
    ],
)
def test_valid_magic_matrix(filename, data, mime):
    ext, got = validate_upload(filename, data, max_bytes=CAP)
    assert got == mime
    assert filename.lower().endswith(ext)


@pytest.mark.parametrize(
    "filename,data",
    [
        ("fake.png", make_jpeg()),   # wrong extension for the magic
        ("fake.jpg", make_png()),
        ("fake.pdf", make_webp()),
        ("fake.webp", make_pdf()),
    ],
)
def test_wrong_extension_rejected(filename, data):
    with pytest.raises(ValidationError, match="sniffs as"):
        validate_upload(filename, data, max_bytes=CAP)


@pytest.mark.parametrize(
    "filename,data",
    [
        ("t.png", b"\x89PNG"),          # truncated: shorter than signature
        ("t.jpg", b"\xff\xd8"),          # truncated JPEG magic
        ("t.webp", b"RIFF\x00\x00"),     # RIFF without WEBP fourcc
        ("t.pdf", b"%PDF"),              # truncated PDF magic
        ("t.png", b""),                  # empty
        ("noext", make_png()),           # no extension
        ("evil.gif", make_png()),        # disallowed extension
        ("evil.png.exe", make_png()),    # disguised double extension
    ],
)
def test_truncated_or_disallowed_rejected(filename, data):
    with pytest.raises(ValidationError):
        validate_upload(filename, data, max_bytes=CAP)


def test_size_cap():
    over = make_png() + b"\x00" * CAP  # valid magic, over the cap
    with pytest.raises(ValidationError, match="hard cap"):
        validate_upload("big.png", over, max_bytes=CAP)
    at_cap = make_jpeg() + b"\x00" * (CAP - len(make_jpeg()))
    ext, _ = validate_upload("ok.jpg", at_cap, max_bytes=CAP)
    assert ext == ".jpg"


def test_sniff_webp_requires_riff_and_fourcc():
    assert sniff_mime(make_webp()) == "image/webp"
    assert sniff_mime(b"RIFF\x20\x00\x00\x00WAVE") is None  # WAV, not WEBP


def test_pdf_page_count_heuristic():
    assert pdf_page_count(make_pdf(pages=3)) == 3
    assert pdf_page_count(b"%PDF-1.4\nno pages here\n%%EOF") == 1


def test_sha256_known_vector():
    assert (
        sha256_hex(b"abc")
        == "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
    )
