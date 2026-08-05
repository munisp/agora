"""Segment DSL + count preview tests (incl. tenant-isolation negatives)."""

from __future__ import annotations

from conftest import HDR_A, HDR_B, LAPSED_SEGMENT, MARKETING_SEGMENT


def test_create_segment_compiles_mandatory_filters(client):
    resp = client.post("/v1/graph/segments", json=MARKETING_SEGMENT, headers=HDR_A)
    assert resp.status_code == 201, resp.text
    record = resp.json()
    assert record["tenant_id"] == "tenant-a"
    assert record["consent_purpose"] == "marketing"
    cypher = record["compiled_cypher"]
    # Compliance gates 1+2+4 are visible in the compiled query.
    assert "$tenant_id" in cypher
    assert "CONSENTED" in cypher
    assert "revoked_at IS NULL" in cypher
    assert "quarantine" in cypher


def test_consent_purpose_defaults_to_segment_purpose(client):
    resp = client.post("/v1/graph/segments", json=MARKETING_SEGMENT, headers=HDR_A)
    assert resp.json()["consent_purpose"] == "marketing"
    resp2 = client.post(
        "/v1/graph/segments",
        json={"name": "svc", "purpose": "service", "filter": {"has_consent": "marketing"}},
        headers=HDR_A,
    )
    assert resp2.json()["consent_purpose"] == "marketing"


def test_list_segments_tenant_isolated(client):
    client.post("/v1/graph/segments", json=MARKETING_SEGMENT, headers=HDR_A)
    assert len(client.get("/v1/graph/segments", headers=HDR_A).json()["segments"]) == 1
    # Tenant B cannot see tenant A's segments (gate 1, negative test).
    assert client.get("/v1/graph/segments", headers=HDR_B).json()["segments"] == []


def test_count_marketing_segment(client):
    seg = client.post("/v1/graph/segments", json=MARKETING_SEGMENT, headers=HDR_A).json()
    resp = client.get(f"/v1/graph/segments/{seg['id']}/count", headers=HDR_A)
    assert resp.status_code == 200
    # pa1, pa5, pa6 pass consent+quarantine; pa2 (no consent), pa3 (revoked),
    # pa4 (quarantined), pa7 (wrong purpose) excluded.
    assert resp.json()["count"] == 3


def test_count_lapsed_lga_not_messaged_segment(client):
    seg = client.post("/v1/graph/segments", json=LAPSED_SEGMENT, headers=HDR_A).json()
    resp = client.get(f"/v1/graph/segments/{seg['id']}/count", headers=HDR_A)
    assert resp.json()["count"] == 1  # only pa1


def test_count_cross_tenant_404(client):
    seg = client.post("/v1/graph/segments", json=MARKETING_SEGMENT, headers=HDR_A).json()
    resp = client.get(f"/v1/graph/segments/{seg['id']}/count", headers=HDR_B)
    assert resp.status_code == 404


def test_count_unknown_segment_404(client):
    assert client.get("/v1/graph/segments/nope/count", headers=HDR_A).status_code == 404


def test_quarantine_opt_in_rejected(client):
    payload = {
        "name": "bad",
        "purpose": "marketing",
        "filter": {"include_quarantined": True},
    }
    resp = client.post("/v1/graph/segments", json=payload, headers=HDR_A)
    assert resp.status_code == 422


def test_invalid_purpose_rejected(client):
    payload = {"name": "bad", "purpose": "marketing'; DETACH DELETE p //", "filter": {}}
    resp = client.post("/v1/graph/segments", json=payload, headers=HDR_A)
    assert resp.status_code == 422


def test_segments_require_auth(client):
    assert client.post("/v1/graph/segments", json=MARKETING_SEGMENT).status_code == 401
    assert client.get("/v1/graph/segments").status_code == 401
