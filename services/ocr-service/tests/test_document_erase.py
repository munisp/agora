"""SPEC-W61 G2: internauth per-document erase endpoint
POST /v1/ocr/documents/{id}/erase.

Covers: happy path removes the row AND the MinIO bytes; idempotent 204 on
re-erase / missing document; internauth enforcement (401 missing/wrong
token, 503 fail-closed when OCR_INTERNAL_TOKEN unset); 400 on a bad uuid.
"""

from __future__ import annotations

import uuid

from conftest import INTERNAL_TOKEN, TENANT_A, make_png

from app.config import settings

EXTRACT = "/v1/ocr/extract"


def _extract(client, tenant=TENANT_A):
    resp = client.post(
        EXTRACT,
        files={"file": ("doc.png", make_png(), "image/png")},
        data={"kind": "document", "tenant": tenant},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )
    assert resp.status_code == 200, resp.text
    return resp.json()["document_id"]


def _erase(client, doc_id, token=INTERNAL_TOKEN):
    headers = {"X-Internal-Token": token} if token is not None else {}
    return client.post(f"/v1/ocr/documents/{doc_id}/erase", headers=headers)


def test_erase_removes_row_and_bytes(client, deps):
    doc_id = _extract(client)
    row = deps.db.rows[doc_id]
    key = ("documents", row["storage_key"])
    assert key in deps.documents.objects  # bytes exist pre-erase

    resp = _erase(client, doc_id)
    assert resp.status_code == 204, resp.text
    assert resp.content == b""
    assert doc_id not in deps.db.rows, "row must be hard-deleted"
    assert key not in deps.documents.objects, "MinIO bytes must be deleted"


def test_erase_is_idempotent_204(client, deps):
    doc_id = _extract(client)
    assert _erase(client, doc_id).status_code == 204
    assert _erase(client, doc_id).status_code == 204  # already gone
    assert _erase(client, str(uuid.uuid4())).status_code == 204  # never existed


def test_erase_auth_matrix(client, deps, monkeypatch):
    doc_id = _extract(client)

    # Missing + wrong token => 401; the row survives.
    assert _erase(client, doc_id, token=None).status_code == 401
    assert _erase(client, doc_id, token="wrong").status_code == 401
    assert doc_id in deps.db.rows

    # Fail-closed: OCR_INTERNAL_TOKEN unset => 503 (even with a token sent).
    monkeypatch.setattr(settings, "internal_token", "")
    assert _erase(client, doc_id).status_code == 503
    assert doc_id in deps.db.rows


def test_erase_bad_uuid_400(client, deps):
    assert _erase(client, "not-a-uuid").status_code == 400
