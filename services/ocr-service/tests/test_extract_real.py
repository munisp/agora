"""Real-sidecar path against a LOCAL stub llama-server (loopback only):
OpenAI request shape asserted, tolerant response parse, failure -> row
status=failed + 502, no events emitted on failure."""

from __future__ import annotations

import base64

import pytest

from app.config import settings
from app.llm import OcrClient
from conftest import (
    INTERNAL_TOKEN,
    TENANT_A,
    make_chat_completion_stub,
    make_failing_stub,
    make_png,
)

ANSWER = (
    "# Receipt\n\nACME STORES LTD\nTOTAL: 12500.00\n\n"
    '```json\n{"vendor": "ACME STORES LTD", "amount": 12500.00, '
    '"currency": "NGN", "date": "2024-01-15", "items": '
    '[{"name": "A4 Ream Paper", "quantity": 2, "price": 4500.0}]}\n```'
)


@pytest.fixture
def real_client(client, deps, monkeypatch):
    """`client` fixture with OCR_MOCK off and OcrClient pointed at a stub."""
    stub, captured = make_chat_completion_stub(ANSWER)
    monkeypatch.setattr(settings, "ocr_mock", False)
    deps.ocr = OcrClient(base_url=f"{stub.url}/v1", model="minicpm-ocr", timeout_s=10.0)
    yield client, stub, captured
    stub.close()


def test_openai_request_shape_and_response(real_client):
    client, stub, captured = real_client
    png = make_png()
    resp = client.post(
        "/v1/ocr/extract",
        files={"file": ("receipt.png", png, "image/png")},
        data={"kind": "receipt", "tenant": TENANT_A},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["status"] == "extracted"
    assert body["page_count"] == 1
    assert body["extracted_fields"]["vendor"] == "ACME STORES LTD"
    assert body["extracted_fields"]["amount"] == 12500.00
    assert body["extracted_fields"]["items"][0]["name"] == "A4 Ream Paper"

    assert len(captured) == 1  # exactly ONE call — no retries
    req = captured[0]
    assert req["path"] == "/v1/chat/completions"
    payload = req["json"]
    assert payload["model"] == "minicpm-ocr"
    assert payload["temperature"] == 0.0
    [msg] = payload["messages"]
    assert msg["role"] == "user"
    kinds = [part["type"] for part in msg["content"]]
    assert kinds == ["text", "image_url"]
    url = msg["content"][1]["image_url"]["url"]
    assert url.startswith("data:image/png;base64,")
    assert base64.b64decode(url.split(",", 1)[1]) == png


def test_sidecar_failure_marks_failed_and_502(client, deps, monkeypatch):
    stub = make_failing_stub(500)
    try:
        monkeypatch.setattr(settings, "ocr_mock", False)
        deps.ocr = OcrClient(base_url=f"{stub.url}/v1", model="minicpm-ocr", timeout_s=5.0)
        resp = client.post(
            "/v1/ocr/extract",
            files={"file": ("receipt.png", make_png(), "image/png")},
            data={"kind": "receipt", "tenant": TENANT_A},
            headers={"X-Internal-Token": INTERNAL_TOKEN},
        )
    finally:
        stub.close()
    assert resp.status_code == 502
    detail = resp.json()["detail"]
    assert detail["error"] == "ocr_upstream_failed"
    assert detail["status"] == "failed"
    doc_id = detail["document_id"]
    assert deps.db.transitions == [(doc_id, "uploaded"), (doc_id, "failed")]
    row = deps.db.rows[doc_id]
    assert row["status"] == "failed"
    assert row["error"]
    # no usage/cloudevent emission on failure
    assert deps.dapr.published == []


def test_no_internal_token_rejected_even_in_real_mode(client, monkeypatch):
    monkeypatch.setattr(settings, "ocr_mock", False)
    resp = client.post(
        "/v1/ocr/extract",
        files={"file": ("receipt.png", make_png(), "image/png")},
        data={"kind": "receipt", "tenant": TENANT_A},
    )
    assert resp.status_code == 401
