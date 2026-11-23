"""SPEC-W115: pay_stub kind end-to-end (mock OCR), validation verdict in
the extract response + GET round-trip, source_channel intake validation,
and erasure tombstone NULLing the verdict columns.
"""

from __future__ import annotations

import sys

sys.path.insert(0, ".")

from conftest import INTERNAL_TOKEN, TENANT_A, make_png  # noqa: E402


def _post(client, *, kind="pay_stub", extra=None, tenant=TENANT_A):
    data = {"kind": kind, "tenant": tenant}
    if extra:
        data.update(extra)
    return client.post(
        "/v1/ocr/extract",
        files={"file": ("stub.png", make_png(), "image/png")},
        data=data,
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )


def test_pay_stub_kind_accepted_and_validated(client):
    resp = _post(client)
    assert resp.status_code == 200, resp.text
    body = resp.json()
    # deterministic fixture is arithmetic-consistent => validated
    assert body["validation_verdict"] == "validated"
    assert body["validation_reasons"] == []
    assert body["extracted_fields"]["employer_name"] == "ACME STORES LTD"


def test_pay_stub_get_roundtrip(client, deps):
    resp = _post(client, extra={"source_channel": "web"})
    assert resp.status_code == 200, resp.text
    doc_id = resp.json()["document_id"]
    got = client.get(
        f"/v1/ocr/documents/{doc_id}",
        headers={"X-Internal-Token": INTERNAL_TOKEN},
        params={"tenant": TENANT_A},
    )
    assert got.status_code == 200, got.text
    row = got.json()
    assert row["kind"] == "pay_stub"
    assert row["validation_verdict"] == "validated"
    assert row["validation_reasons"] == []
    assert row["source_channel"] == "web"


def test_source_channel_invalid_400(client):
    resp = _post(client, extra={"source_channel": "fax"})
    assert resp.status_code == 400
    assert "source_channel" in resp.text


def test_source_channel_optional(client):
    resp = _post(client)
    assert resp.status_code == 200
    got = client.get(
        f"/v1/ocr/documents/{resp.json()['document_id']}",
        headers={"X-Internal-Token": INTERNAL_TOKEN},
        params={"tenant": TENANT_A},
    )
    assert got.json()["source_channel"] is None


def test_get_tenant_scoped_404(client):
    from conftest import TENANT_B

    resp = _post(client)
    doc_id = resp.json()["document_id"]
    got = client.get(
        f"/v1/ocr/documents/{doc_id}",
        headers={"X-Internal-Token": INTERNAL_TOKEN},
        params={"tenant": TENANT_B},
    )
    assert got.status_code == 404


def test_unwired_kinds_get_needs_review_verdict(client):
    # SPEC-W116 (sanctioned change, NOTES.md): receipt + document gained
    # dedicated validators; knowledge_scan remains the unwired kind that
    # must honestly report needs_review, never a blanket "validated".
    resp = _post(client, kind="knowledge_scan")
    assert resp.status_code == 200
    body = resp.json()
    assert body["validation_verdict"] == "needs_review"
    assert body["validation_reasons"] == ["no_validator_for_kind"]


def test_receipt_mock_fixture_now_validates(client):
    # SPEC-W116: the deterministic receipt fixture passes the receipt
    # validator (vendor + positive amount + ISO date + currency).
    resp = _post(client, kind="receipt")
    assert resp.status_code == 200
    body = resp.json()
    assert body["validation_verdict"] == "validated"
    assert body["validation_reasons"] == []


def test_erasure_nulls_verdict_columns(client, deps):
    resp = _post(client, extra={"source_channel": "voice"})
    doc_id = resp.json()["document_id"]
    erased = client.post(
        f"/v1/ocr/documents/{doc_id}/erase",
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )
    assert erased.status_code == 204
    assert str(doc_id) not in deps.db.rows  # internauth erase = hard delete


async def test_tombstone_nulls_verdict_columns(deps):
    """mark_erased (consent tombstone path) NULLs the verdict columns."""
    import uuid

    doc_id = uuid.uuid4()
    async with deps.db.tenant_conn(TENANT_A) as conn:
        await deps.db.insert_document(
            conn,
            {
                "id": doc_id,
                "tenant_id": TENANT_A,
                "kind": "pay_stub",
                "storage_bucket": "documents",
                "storage_key": f"{TENANT_A}/{doc_id}.png",
                "mime": "image/png",
                "bytes": 5,
                "sha256": "ef" * 32,
                "phash": None,
                "source_channel": "web",
            },
        )
        await deps.db.mark_extracted(
            conn,
            doc_id,
            page_count=1,
            model_version="mock",
            extracted_text="t",
            extracted_fields={"employer_name": "A"},
            validation_verdict="validated",
            validation_reasons=[],
        )
        await deps.db.mark_erased(conn, doc_id)
    row = deps.db.rows[str(doc_id)]
    assert row["status"] == "erased"
    assert row["validation_verdict"] is None
    assert row["validation_reasons"] is None
