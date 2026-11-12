"""End-to-end OCR_MOCK extraction (SPEC-W53 Scope A item 6): deterministic
fixture, response schema (A<->B/A<->C contract), status transitions,
MinIO key shape, usage + CloudEvent emission (fake capture — TESTS ONLY),
document_url ingress, size cap."""

from __future__ import annotations

import json

from app.llm import MOCK_RECEIPT_FIELDS, MOCK_TEXT
from conftest import (
    INTERNAL_TOKEN,
    TENANT_A,
    make_download_stub,
    make_jpeg,
    make_pdf,
    make_png,
)

EXTRACT = "/v1/ocr/extract"


def _post_receipt(client, tenant=TENANT_A, data=None, filename="receipt.png"):
    return client.post(
        EXTRACT,
        files={"file": (filename, data if data is not None else make_png(), "image/png")},
        data={"kind": "receipt", "tenant": tenant},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )


def test_mock_extract_happy_path_schema_and_fixture(client, deps):
    resp = _post_receipt(client)
    assert resp.status_code == 200, resp.text
    body = resp.json()
    # A<->B / A<->C contract keys, exactly (extracted_text added by W53-D —
    # C's knowledge flow consumes it; B codes against the original four).
    assert set(body) == {
        "document_id",
        "status",
        "extracted_fields",
        "page_count",
        "extracted_text",
    }
    assert body["status"] == "extracted"
    assert body["page_count"] == 1
    # OCR_MOCK mode returns the deterministic fixture text verbatim.
    assert body["extracted_text"] == MOCK_TEXT
    # deterministic fixture: fixed vendor/amount/date
    assert body["extracted_fields"] == MOCK_RECEIPT_FIELDS
    assert body["extracted_fields"]["vendor"] == "ACME STORES LTD"
    assert body["extracted_fields"]["amount"] == 12500.00
    assert body["extracted_fields"]["date"] == "2024-01-15"
    assert len(body["extracted_fields"]["items"]) == 2


def test_status_transitions_and_row_contents(client, deps):
    resp = _post_receipt(client)
    doc_id = resp.json()["document_id"]
    assert deps.db.transitions == [(doc_id, "uploaded"), (doc_id, "extracted")]
    row = deps.db.rows[doc_id]
    assert row["tenant_id"] == TENANT_A
    assert row["kind"] == "receipt"
    assert row["mime"] == "image/png"
    assert row["sha256"] and len(row["sha256"]) == 64
    assert row["phash"] and len(row["phash"]) == 16
    assert row["extracted_text"] == MOCK_TEXT
    assert json.loads(row["extracted_fields"] if isinstance(row["extracted_fields"], str)
                    else json.dumps(row["extracted_fields"]))["vendor"] == "ACME STORES LTD"
    assert row["model_version"].endswith("+mock")


def test_minio_key_is_tenant_prefixed(client, deps):
    resp = _post_receipt(client, data=make_jpeg(), filename="receipt.jpg")
    doc_id = resp.json()["document_id"]
    (bucket, key), payload = next(iter(deps.documents.objects.items()))
    assert bucket == "documents"
    assert key == f"{TENANT_A}/{doc_id}.jpg"
    assert payload == make_jpeg()


def test_mock_pdf_page_count(client, deps):
    resp = client.post(
        EXTRACT,
        files={"file": ("scan.pdf", make_pdf(pages=3), "application/pdf")},
        data={"kind": "document", "tenant": TENANT_A},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )
    assert resp.status_code == 200
    body = resp.json()
    assert body["page_count"] == 3
    # non-receipt kinds use the document fixture shape
    assert body["extracted_fields"]["title"] == "Mock Document"


def test_usage_and_cloudevent_emitted(client, deps):
    resp = _post_receipt(client)
    doc_id = resp.json()["document_id"]
    topics = [t for _, t, _ in deps.dapr.published]
    assert topics == ["opendesk.usage.events", "opendesk.ocr.events"]

    usage = deps.dapr.published[0][2]
    assert usage["type"] == "com.opendesk.usage.UsageRecord"
    assert usage["specversion"] == "1.0"
    assert usage["tenantid"] == TENANT_A
    assert usage["data"]["metric"] == "ocr_page"
    assert usage["data"]["value"] == 1
    assert usage["data"]["meta"]["document_id"] == doc_id
    assert usage["data"]["meta"]["service"] == "ocr-service"
    assert usage["data"]["meta"]["model_version"].endswith("+mock")

    evt = deps.dapr.published[1][2]
    assert evt["type"] == "com.opendesk.ocr.DocumentExtracted"
    assert evt["subject"] == doc_id
    assert evt["data"]["status"] == "extracted"
    assert evt["data"]["storage_key"].startswith(f"{TENANT_A}/")
    assert "text" not in evt["data"]  # event never carries full text/bytes


def test_document_url_ingress(client, deps):
    stub = make_download_stub(make_png())
    try:
        resp = client.post(
            EXTRACT,
            json={
                "document_url": f"{stub.url}/receipt.png",
                "kind": "receipt",
                "tenant": TENANT_A,
            },
            headers={"X-Internal-Token": INTERNAL_TOKEN},
        )
    finally:
        stub.close()
    assert resp.status_code == 200, resp.text
    assert resp.json()["status"] == "extracted"


def test_document_url_bad_scheme(client):
    resp = client.post(
        EXTRACT,
        json={"document_url": "file:///etc/passwd", "kind": "receipt", "tenant": TENANT_A},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )
    assert resp.status_code == 400


def test_size_cap_enforced(client, monkeypatch):
    from app.config import settings

    monkeypatch.setattr(settings, "ocr_max_bytes", 64)
    resp = _post_receipt(client)  # tiny PNG is still > 64 bytes
    assert resp.status_code == 413


def test_kind_validation(client):
    resp = client.post(
        EXTRACT,
        files={"file": ("receipt.png", make_png(), "image/png")},
        data={"kind": "passport", "tenant": TENANT_A},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )
    assert resp.status_code == 400


def test_get_document_metadata_no_bytes(client, deps):
    doc_id = _post_receipt(client).json()["document_id"]
    resp = client.get(
        f"/v1/ocr/documents/{doc_id}",
        params={"tenant": TENANT_A},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )
    assert resp.status_code == 200
    body = resp.json()
    assert body["document_id"] == doc_id
    assert body["status"] == "extracted"
    assert body["sha256"] and body["phash"]
    assert body["extracted_fields"]["vendor"] == "ACME STORES LTD"
    assert body["storage_key"].startswith(f"{TENANT_A}/")
    assert "data" not in body and "content" not in body  # no bytes served


def test_get_document_missing_404(client):
    import uuid

    resp = client.get(
        f"/v1/ocr/documents/{uuid.uuid4()}",
        params={"tenant": TENANT_A},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )
    assert resp.status_code == 404
