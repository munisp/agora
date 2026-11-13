"""SPEC-W58 Scope A: kyc_* kinds end-to-end under OCR_MOCK.

Covers: all 5 kinds accepted (A1), per-kind mock fixture shapes (A3/P9),
passport mrz_valid computed in CODE (P7), GATE R1 event redaction (P2 —
no PII on the bus), extracted_text suppression (A5) and expires_at set on
insert (P4, GATE R3).
"""

from __future__ import annotations

import json

from app.config import DOCUMENT_KINDS, is_kyc_kind
from app.llm import MOCK_KYC_FIELDS
from conftest import INTERNAL_TOKEN, TENANT_A, make_png

EXTRACT = "/v1/ocr/extract"

# Canary PII strings from the fixtures — must NEVER appear on the bus.
CANARIES = ["12345678901", "FKJ12345AB", "A01234569", "90F5B12345678901234",
            "RC1234567", "ADAEZE CHINWE OKAFOR", "JANE CHIDINMA DOE"]


def _post_kyc(client, kind, tenant=TENANT_A):
    return client.post(
        EXTRACT,
        files={"file": ("doc.png", make_png(), "image/png")},
        data={"kind": kind, "tenant": tenant},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )


def test_all_kyc_kinds_registered():
    for kind in MOCK_KYC_FIELDS:
        assert kind in DOCUMENT_KINDS
        assert is_kyc_kind(kind)
    assert not is_kyc_kind("receipt")
    assert not is_kyc_kind("knowledge_scan")


def test_kyc_mock_e2e_shapes(client, deps):
    """Per-kind mock e2e: response carries the whitelisted fixture fields."""
    expected_keys = {
        "kyc_nin_slip": {"full_name", "dob", "nin", "address"},
        "kyc_drivers_license": {"full_name", "dob", "license_no", "expiry"},
        "kyc_passport": {"full_name", "dob", "passport_no", "nationality",
                         "expiry", "mrz_valid"},
        "kyc_voters_card": {"full_name", "vin", "dob"},
        "kyc_cac_document": {"company_name", "rc_number", "registration_date",
                             "address"},
    }
    for kind, keys in expected_keys.items():
        resp = _post_kyc(client, kind)
        assert resp.status_code == 200, (kind, resp.text)
        body = resp.json()
        assert body["status"] == "extracted"
        assert set(body["extracted_fields"]) == keys, kind


def test_kyc_passport_mrz_valid_is_code_computed(client, deps):
    resp = _post_kyc(client, "kyc_passport")
    assert resp.status_code == 200
    fields = resp.json()["extracted_fields"]
    # Fixture MRZ has valid ICAO 9303 check digits; verdict comes from the
    # deterministic validator, and the raw MRZ is NOT retained (P7/P9).
    assert fields["mrz_valid"] is True
    assert "mrz" not in fields


def test_kyc_extracted_text_suppressed(client, deps):
    """A5: kyc_* transcript neither persisted nor echoed."""
    resp = _post_kyc(client, "kyc_nin_slip")
    body = resp.json()
    assert body["extracted_text"] == ""
    row = deps.db.rows[body["document_id"]]
    assert row["extracted_text"] is None
    # fields ARE persisted (TTL-bound review window)
    assert row["extracted_fields"]["nin"] == "12345678901"
    # receipt path unchanged (P14)
    resp2 = client.post(
        EXTRACT,
        files={"file": ("r.png", make_png(), "image/png")},
        data={"kind": "receipt", "tenant": TENANT_A},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )
    assert resp2.json()["extracted_text"] != ""


def test_kyc_expires_at_set_on_insert(client, deps, monkeypatch):
    """P4: expires_at = insert time + OCR_KYC_TTL_HOURS; receipts get NULL."""
    from datetime import datetime, timezone

    from app.config import settings

    monkeypatch.setattr(settings, "kyc_ttl_hours", 72)
    before = datetime.now(timezone.utc)
    body = _post_kyc(client, "kyc_nin_slip").json()
    row = deps.db.rows[body["document_id"]]
    assert row["expires_at"] is not None
    delta = (row["expires_at"] - before).total_seconds()
    assert 71 * 3600 < delta <= 72 * 3600 + 5

    body2 = client.post(
        EXTRACT,
        files={"file": ("r.png", make_png(), "image/png")},
        data={"kind": "receipt", "tenant": TENANT_A},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    ).json()
    assert deps.db.rows[body2["document_id"]]["expires_at"] is None


def test_kyc_event_redaction_gate_r1(client, deps):
    """GATE R1 (P2): kyc_* DocumentExtracted carries extracted_fields == {}
    and NO fixture PII anywhere in the event envelope."""
    resp = _post_kyc(client, "kyc_nin_slip")
    assert resp.status_code == 200
    doc_events = [
        p for _, topic, p in deps.dapr.published if topic == "opendesk.ocr.events"
    ]
    assert len(doc_events) == 1
    evt = doc_events[0]
    assert evt["type"] == "com.opendesk.ocr.DocumentExtracted"
    assert evt["data"]["kind"] == "kyc_nin_slip"
    assert evt["data"]["extracted_fields"] == {}
    blob = json.dumps(evt)
    for canary in CANARIES:
        assert canary not in blob, f"PII canary {canary!r} leaked onto the bus"
    # usage event meta must not leak fields either
    usage = deps.dapr.published[0][2]
    for canary in CANARIES:
        assert canary not in json.dumps(usage)


def test_receipt_event_still_carries_fields(client, deps):
    """P14: redaction is kyc-scoped; receipts keep verbatim fields."""
    client.post(
        EXTRACT,
        files={"file": ("r.png", make_png(), "image/png")},
        data={"kind": "receipt", "tenant": TENANT_A},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )
    evt = [
        p for _, topic, p in deps.dapr.published if topic == "opendesk.ocr.events"
    ][0]
    assert evt["data"]["extracted_fields"]["vendor"] == "ACME STORES LTD"


def test_kyc_document_get_reflects_suppressed_text(client, deps):
    doc_id = _post_kyc(client, "kyc_voters_card").json()["document_id"]
    resp = client.get(
        f"/v1/ocr/documents/{doc_id}",
        params={"tenant": TENANT_A},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )
    assert resp.status_code == 200
    body = resp.json()
    assert body["kind"] == "kyc_voters_card"
    assert body["extracted_text"] is None
    assert body["extracted_fields"]["vin"] == "90F5B12345678901234"
    assert body["expires_at"] is not None


def test_kyc_env_parse_invalid_falls_back(monkeypatch):
    """A1: envStrExplicit-style — invalid OCR_KYC_TTL_HOURS warns + defaults."""
    from app.config import Settings

    monkeypatch.setenv("OCR_KYC_TTL_HOURS", "banana")
    assert Settings().kyc_ttl_hours == 72
    monkeypatch.setenv("OCR_KYC_TTL_HOURS", "24")
    assert Settings().kyc_ttl_hours == 24
    monkeypatch.delenv("OCR_KYC_TTL_HOURS")
    assert Settings().kyc_ttl_hours == 72
    monkeypatch.setenv("OCR_KYC_SWEEP_MINUTES", "5")
    assert Settings().kyc_sweep_minutes == 5
    monkeypatch.setenv("OCR_KYC_SWEEP_MINUTES", "never")
    assert Settings().kyc_sweep_minutes == 15
