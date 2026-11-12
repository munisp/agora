"""Auth matrix: X-Internal-Token fail-closed (503 unset, 401 mismatch) and
the SPEC-W43 C1 X-Tenant-Slugs gateway binding."""

from __future__ import annotations

from app.config import settings
from conftest import INTERNAL_TOKEN, TENANT_A, TENANT_B, make_png


def _post(client, headers, tenant=TENANT_A):
    return client.post(
        "/v1/ocr/extract",
        files={"file": ("receipt.png", make_png(), "image/png")},
        data={"kind": "receipt", "tenant": tenant},
        headers=headers,
    )


def test_internal_token_unset_fails_closed(client, monkeypatch):
    monkeypatch.setattr(settings, "internal_token", "")
    resp = _post(client, {"X-Internal-Token": "anything"})
    assert resp.status_code == 503


def test_internal_token_mismatch_401(client):
    resp = _post(client, {"X-Internal-Token": "wrong"})
    assert resp.status_code == 401


def test_internal_token_ok(client):
    resp = _post(client, {"X-Internal-Token": INTERNAL_TOKEN})
    assert resp.status_code == 200


def test_internal_caller_must_name_tenant(client):
    resp = client.post(
        "/v1/ocr/extract",
        files={"file": ("receipt.png", make_png(), "image/png")},
        data={"kind": "receipt"},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )
    assert resp.status_code == 400


def test_gateway_path_tenant_slugs_binding(client):
    # no internal token: X-Tenant-Slugs carries the tenant; explicit match ok
    resp = _post(client, {"X-Tenant-Slugs": TENANT_A})
    assert resp.status_code == 200
    # explicit selector not in the header -> 403
    resp = _post(client, {"X-Tenant-Slugs": TENANT_B}, tenant=TENANT_A)
    assert resp.status_code == 403
    # no header at all, no dev escape -> 401
    resp = _post(client, {})
    assert resp.status_code == 401


def test_gateway_path_multi_slug_requires_explicit(client):
    resp = client.post(
        "/v1/ocr/extract",
        files={"file": ("receipt.png", make_png(), "image/png")},
        data={"kind": "receipt"},
        headers={"X-Tenant-Slugs": f"{TENANT_A},{TENANT_B}"},
    )
    assert resp.status_code == 400
    resp = client.post(
        "/v1/ocr/extract",
        files={"file": ("receipt.png", make_png(), "image/png")},
        data={"kind": "receipt", "tenant": TENANT_B},
        headers={"X-Tenant-Slugs": f"{TENANT_A},{TENANT_B}"},
    )
    assert resp.status_code == 200


def test_get_document_requires_auth(client):
    import uuid

    resp = client.get(f"/v1/ocr/documents/{uuid.uuid4()}", params={"tenant": TENANT_A})
    assert resp.status_code == 401
