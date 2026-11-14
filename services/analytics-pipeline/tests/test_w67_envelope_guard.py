"""SPEC-W67 (gate 9): analytics-pipeline envelope-type guard.

map_ocr_document filtered only on data.kind == "receipt" and never checked
the CloudEvent envelope type, so a com.opendesk.ocr.CrossTenantDocumentMatch
event for a receipt-kind duplicate would have been mis-mapped into
bronze.receipt_documents. The guard rejects any PRESENT-but-foreign
envelope type; DocumentExtracted events and bare payloads are byte-
identical to legacy behavior. (Coder C carries an independent gate test —
this duplication is intentional, belt and suspenders.)
"""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from analytics_pipeline.mapping import (  # noqa: E402
    OCR_DOCUMENT_EXTRACTED_TYPE,
    RECEIPT_DOCUMENT_COLUMNS,
    map_ocr_document,
)

T1 = "11111111-1111-1111-1111-111111111111"
T2 = "99999999-8888-7777-6666-555555555555"
SHA = "ab" * 32


def _match_event():
    """The PINNED W67 A<->B contract (SPEC-W67): type
    com.opendesk.ocr.CrossTenantDocumentMatch on opendesk.ocr.events,
    tenantid/subject = the LATER tenant, data EXACTLY
    {document_id, sha256, kind, tenant_count}."""
    return {
        "specversion": "1.0",
        "id": "evt-match-1",
        "source": "ocr-service",
        "type": "com.opendesk.ocr.CrossTenantDocumentMatch",
        "subject": T2,
        "time": "2026-02-02T10:00:00Z",
        "tenantid": T2,
        "data": {
            "document_id": "doc-later",
            "sha256": SHA,
            "kind": "receipt",  # receipt-kind duplicate: the trap
            "tenant_count": 2,
        },
    }


def _document_extracted_event():
    return {
        "specversion": "1.0",
        "id": "evt-ocr-1",
        "source": "ocr-service",
        "type": "com.opendesk.ocr.DocumentExtracted",
        "subject": "doc-1",
        "time": "2026-02-01T10:00:00Z",
        "tenantid": T1,
        "data": {
            "document_id": "doc-1",
            "tenant_id": T1,
            "kind": "receipt",
            "status": "extracted",
            "page_count": 2,
            "sha256": SHA,
            "phash": "f" * 16,
            "model_version": "ocr-v3",
            "extracted_fields": {
                "amount_minor": 123400,
                "currency": "NGN",
                "receipt_date": "2026-01-31",
            },
        },
    }


def test_cross_tenant_match_receipt_kind_maps_to_none():
    """The exact W67 failure mode: kind='receipt' on a match envelope must
    NOT land in bronze.receipt_documents."""
    assert map_ocr_document(_match_event()) is None


def test_document_extracted_receipt_still_maps():
    """No regression: a normal DocumentExtracted receipt maps exactly as
    before the guard."""
    row = map_ocr_document(_document_extracted_event())
    assert row is not None
    assert list(row.keys()) == list(RECEIPT_DOCUMENT_COLUMNS)
    assert row["event_id"] == "evt-ocr-1"
    assert row["tenant_id"] == T1
    assert row["document_id"] == "doc-1"
    assert row["kind"] == "receipt"
    assert row["sha256"] == SHA
    assert row["amount_minor"] == 123400
    assert row["currency"] == "NGN"
    assert row["receipt_date"] == "2026-01-31"


def test_guard_constant_and_other_foreign_types():
    """The guard keys on the short type name; ANY foreign envelope type on
    the topic is ignored (unknown-type tolerance, gate 9)."""
    assert OCR_DOCUMENT_EXTRACTED_TYPE == "DocumentExtracted"
    for foreign in (
        "com.opendesk.ocr.AvScanAlert",
        "com.opendesk.ocr.SomethingFuture",
        "CrossTenantDocumentMatch",  # short form, same ruling
    ):
        msg = _match_event()
        msg["type"] = foreign
        assert map_ocr_document(msg) is None, foreign


def test_bare_payload_still_maps():
    """Bare payloads (no envelope) keep legacy behavior — the guard only
    rejects a PRESENT-but-foreign type."""
    row = map_ocr_document(
        {
            "document_id": "doc-9",
            "tenant_id": T1,
            "kind": "receipt",
            "sha256": SHA,
            "extracted_fields": {"amount_minor": 100},
        }
    )
    assert row is not None
    assert row["document_id"] == "doc-9"
    assert row["amount_minor"] == 100
