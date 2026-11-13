"""SPEC-W61 G2: optional `expires_in_hours` form field on POST /v1/ocr/extract.

Covers: present => expires_at = now()+hours for ANY kind (incl. the kyc
override); absent => byte-identical legacy behavior (receipt NULL, kyc
OCR_KYC_TTL_HOURS path untouched); invalid values => 400.
"""

from __future__ import annotations

from datetime import datetime, timezone

from conftest import INTERNAL_TOKEN, TENANT_A, make_png

EXTRACT = "/v1/ocr/extract"


def _post(client, kind, tenant=TENANT_A, extra=None):
    data = {"kind": kind, "tenant": tenant}
    if extra:
        data.update(extra)
    return client.post(
        EXTRACT,
        files={"file": ("doc.png", make_png(), "image/png")},
        data=data,
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )


def test_expires_in_hours_sets_expires_at_any_kind(client, deps):
    """whatsapp-media flow: receipt kind with expires_in_hours=168."""
    before = datetime.now(timezone.utc)
    resp = _post(client, "receipt", extra={"expires_in_hours": "168"})
    assert resp.status_code == 200, resp.text
    row = deps.db.rows[resp.json()["document_id"]]
    assert row["expires_at"] is not None
    delta = (row["expires_at"] - before).total_seconds()
    assert 167 * 3600 < delta <= 168 * 3600 + 5


def test_expires_in_hours_boundary_values(client, deps):
    """1 and 8760 are both valid (inclusive range)."""
    for hours in ("1", "8760"):
        resp = _post(client, "document", extra={"expires_in_hours": hours})
        assert resp.status_code == 200, (hours, resp.text)
        assert deps.db.rows[resp.json()["document_id"]]["expires_at"] is not None


def test_expires_in_hours_invalid_400(client, deps):
    for bad in ("abc", "0", "8761", "-3", "1.5", "  "):
        resp = _post(client, "receipt", extra={"expires_in_hours": bad})
        assert resp.status_code == 400, (bad, resp.status_code, resp.text)
    assert deps.db.rows == {}, "invalid expires_in_hours must not store anything"


def test_absent_field_legacy_behavior_unchanged(client, deps):
    """Absent => byte-identical legacy behavior: receipt expires_at NULL."""
    resp = _post(client, "receipt")
    assert resp.status_code == 200
    assert deps.db.rows[resp.json()["document_id"]]["expires_at"] is None


def test_kyc_ttl_path_regression(client, deps, monkeypatch):
    """kyc_* WITHOUT the field keeps the OCR_KYC_TTL_HOURS path (untouched);
    WITH the field the caller-pinned value wins (any-kind override)."""
    from app.config import settings

    monkeypatch.setattr(settings, "kyc_ttl_hours", 72)

    before = datetime.now(timezone.utc)
    resp = _post(client, "kyc_nin_slip")
    assert resp.status_code == 200
    row = deps.db.rows[resp.json()["document_id"]]
    delta = (row["expires_at"] - before).total_seconds()
    assert 71 * 3600 < delta <= 72 * 3600 + 5

    before = datetime.now(timezone.utc)
    resp = _post(client, "kyc_nin_slip", extra={"expires_in_hours": "24"})
    assert resp.status_code == 200
    row = deps.db.rows[resp.json()["document_id"]]
    delta = (row["expires_at"] - before).total_seconds()
    assert 23 * 3600 < delta <= 24 * 3600 + 5
