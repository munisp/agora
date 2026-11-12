"""W53-D tenant-resolution precedence (A<->B integration fix):

  1. X-Tenant-Slug header (geo-enrich precedent — the booking-service
     fieldcapture client sends the tenant uuid this way),
  2. tenant_id multipart form field (the client sends BOTH),
  3. gateway C1 claims (X-Tenant-Slugs binding — browser calls via the
     BFF, preserved exactly).

Header + form field must agree when both are present (400 on conflict);
an internal-token call with no resolvable tenant is a 400 — never a
silent global/no-tenant bucket prefix.
"""

from __future__ import annotations

from app.llm import MOCK_TEXT
from conftest import INTERNAL_TOKEN, TENANT_A, TENANT_B, make_png

EXTRACT = "/v1/ocr/extract"


def _post(client, *, headers, data):
    return client.post(
        EXTRACT,
        files={"file": ("receipt.png", make_png(), "image/png")},
        data={"kind": "receipt", **data},
        headers={"X-Internal-Token": INTERNAL_TOKEN, **headers},
    )


def test_tenant_from_x_tenant_slug_header(client, deps):
    """Header-only internal call resolves the tenant (booking client's
    X-Tenant-Slug: <uuid> path) and tenant-prefixes the MinIO key."""
    resp = _post(client, headers={"X-Tenant-Slug": TENANT_A}, data={})
    assert resp.status_code == 200, resp.text
    doc_id = resp.json()["document_id"]
    (bucket, key), _ = next(iter(deps.documents.objects.items()))
    assert bucket == "documents"
    assert key == f"{TENANT_A}/{doc_id}.png"
    assert deps.db.rows[doc_id]["tenant_id"] == TENANT_A


def test_tenant_from_tenant_id_form_field(client, deps):
    """Form-field-only internal call (tenant_id, the A<->B contract key)."""
    resp = _post(client, headers={}, data={"tenant_id": TENANT_B})
    assert resp.status_code == 200, resp.text
    doc_id = resp.json()["document_id"]
    assert deps.db.rows[doc_id]["tenant_id"] == TENANT_B
    (bucket, key), _ = next(iter(deps.documents.objects.items()))
    assert key == f"{TENANT_B}/{doc_id}.png"


def test_header_and_form_field_agreement(client, deps):
    """B sends BOTH — agreeing values resolve cleanly..."""
    resp = _post(
        client,
        headers={"X-Tenant-Slug": TENANT_A},
        data={"tenant_id": TENANT_A},
    )
    assert resp.status_code == 200, resp.text
    doc_id = resp.json()["document_id"]
    assert deps.db.rows[doc_id]["tenant_id"] == TENANT_A
    # ...and conflicting values are rejected (400), never silently picked.
    stored_rows, stored_objects = len(deps.db.rows), len(deps.documents.objects)
    resp = _post(
        client,
        headers={"X-Tenant-Slug": TENANT_A},
        data={"tenant_id": TENANT_B},
    )
    assert resp.status_code == 400
    # nothing persisted for an ambiguous tenant
    assert len(deps.db.rows) == stored_rows
    assert len(deps.documents.objects) == stored_objects


def test_internal_call_without_any_tenant_is_400(client, deps):
    """No header, no form field: the internal caller gets a 400 — the
    service NEVER falls back to a global/no-tenant bucket prefix."""
    resp = _post(client, headers={}, data={})
    assert resp.status_code == 400
    assert deps.documents.objects == {}
    assert deps.db.rows == {}


def test_gateway_header_selector_still_bound_to_c1_slugs(client, deps):
    """Browser calls (C via the BFF) send x-tenant-slug; on the gateway
    path it remains an explicit selector validated against the C1
    X-Tenant-Slugs claims — the header alone never bypasses binding."""
    # granted: header selector within the C1 claims -> 200
    resp = client.post(
        EXTRACT,
        files={"file": ("receipt.png", make_png(), "image/png")},
        data={"kind": "knowledge_scan"},
        headers={"X-Tenant-Slugs": TENANT_A, "X-Tenant-Slug": TENANT_A},
    )
    assert resp.status_code == 200, resp.text
    assert resp.json()["extracted_text"] == MOCK_TEXT
    # not granted: header selector outside the C1 claims -> 403
    resp = client.post(
        EXTRACT,
        files={"file": ("receipt.png", make_png(), "image/png")},
        data={"kind": "knowledge_scan"},
        headers={"X-Tenant-Slugs": TENANT_A, "X-Tenant-Slug": TENANT_B},
    )
    assert resp.status_code == 403
