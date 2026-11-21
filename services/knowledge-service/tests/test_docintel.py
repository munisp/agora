"""DocIntelClient contract tests (SPEC-W107).

The httpx.AsyncClient constructor is monkeypatched to a MockTransport-backed
client — a test seam only; production always builds the real client in the
service lifespan (no mock on any production path).
"""

from __future__ import annotations

import base64
import json
import sys

sys.path.insert(0, ".")

import httpx
import pytest

from app.docintel import (
    DocIntelClient,
    DocIntelEmpty,
    DocIntelRejected,
    DocIntelUnavailable,
)


_REAL_CLIENT = httpx.AsyncClient  # capture before any monkeypatching


def _patch_transport(monkeypatch, handler):
    transport = httpx.MockTransport(handler)
    monkeypatch.setattr(
        httpx, "AsyncClient", lambda **kw: _REAL_CLIENT(transport=transport, **kw)
    )


def _ok(md="# Title\n\nbody text", text="", status="success"):
    return {
        "document": {"md_content": md, "text_content": text},
        "status": status,
        "processing_time": 1.5,
        "errors": [],
    }


async def test_convert_happy_path_payload_contract(monkeypatch):
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["headers"] = dict(request.headers)
        seen["payload"] = json.loads(request.content)
        return httpx.Response(200, json=_ok())

    _patch_transport(monkeypatch, handler)
    client = DocIntelClient("http://doc-intel:5001/")
    result = await client.convert("contract.pdf", b"%PDF-fake")

    assert result.text == "# Title\n\nbody text"
    assert result.processing_time_s == 1.5
    assert seen["url"] == "http://doc-intel:5001/v1/convert/source"
    payload = seen["payload"]
    assert payload["target"] == {"kind": "inbody"}
    opts = payload["options"]
    assert opts["to_formats"] == ["md"]
    assert opts["do_ocr"] is True
    assert opts["ocr_engine"] == "rapidocr"  # PaddleOCR ONNX models
    assert opts["do_table_structure"] is True
    src = payload["sources"][0]
    assert src["kind"] == "file"
    assert src["filename"] == "contract.pdf"
    assert base64.b64decode(src["base64_string"]) == b"%PDF-fake"
    assert "x-api-key" not in seen["headers"]


async def test_convert_sends_api_key_when_configured(monkeypatch):
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["headers"] = dict(request.headers)
        return httpx.Response(200, json=_ok())

    _patch_transport(monkeypatch, handler)
    client = DocIntelClient("http://doc-intel:5001", api_key="sekrit")
    await client.convert("a.pdf", b"x")
    assert seen["headers"].get("x-api-key") == "sekrit"


async def test_convert_falls_back_to_text_content(monkeypatch):
    _patch_transport(
        monkeypatch,
        lambda req: httpx.Response(200, json=_ok(md="", text="plain text")),
    )
    client = DocIntelClient("http://doc-intel:5001")
    result = await client.convert("a.txt", b"x")
    assert result.text == "plain text"


async def test_convert_empty_extraction_raises_and_names_ocr(monkeypatch):
    _patch_transport(
        monkeypatch, lambda req: httpx.Response(200, json=_ok(md="", text=""))
    )
    client = DocIntelClient("http://doc-intel:5001")
    with pytest.raises(DocIntelEmpty) as excinfo:
        await client.convert("scan.pdf", b"x")
    assert "OCR" in str(excinfo.value)  # honest pointer, never silent


async def test_convert_failure_status_raises_rejected(monkeypatch):
    body = _ok(status="failure")
    body["errors"] = [{"error_message": "boom"}]
    _patch_transport(monkeypatch, lambda req: httpx.Response(200, json=body))
    client = DocIntelClient("http://doc-intel:5001")
    with pytest.raises(DocIntelRejected, match="boom"):
        await client.convert("a.pdf", b"x")


async def test_convert_non_200_raises_rejected(monkeypatch):
    _patch_transport(
        monkeypatch, lambda req: httpx.Response(500, text="server exploded")
    )
    client = DocIntelClient("http://doc-intel:5001")
    with pytest.raises(DocIntelRejected, match="HTTP 500"):
        await client.convert("a.pdf", b"x")


async def test_convert_connect_error_raises_unavailable(monkeypatch):
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("connection refused", request=request)

    _patch_transport(monkeypatch, handler)
    client = DocIntelClient("http://doc-intel:5001")
    with pytest.raises(DocIntelUnavailable):
        await client.convert("a.pdf", b"x")


async def test_convert_timeout_raises_unavailable(monkeypatch):
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ReadTimeout("slow", request=request)

    _patch_transport(monkeypatch, handler)
    client = DocIntelClient("http://doc-intel:5001")
    with pytest.raises(DocIntelUnavailable):
        await client.convert("a.pdf", b"x")


async def test_health_true_and_false(monkeypatch):
    _patch_transport(monkeypatch, lambda req: httpx.Response(200))
    assert await DocIntelClient("http://doc-intel:5001").health() is True

    def down(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("nope", request=request)

    _patch_transport(monkeypatch, down)
    assert await DocIntelClient("http://doc-intel:5001").health() is False
