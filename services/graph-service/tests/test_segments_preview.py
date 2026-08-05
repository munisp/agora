"""POST /v1/graph/segments/count tests — live count preview for UNSAVED DSL
(admin-web Segment Builder). Same mandatory gates as saved segments; nothing
is persisted."""

from __future__ import annotations

from conftest import HDR_A, HDR_B, LAPSED_SEGMENT, MARKETING_SEGMENT

PREVIEW_URL = "/v1/graph/segments/count"


def test_preview_marketing_count_consent_gated(client):
    resp = client.post(PREVIEW_URL, json=MARKETING_SEGMENT, headers=HDR_A)
    assert resp.status_code == 200, resp.text
    # pa1, pa5, pa6 pass; pa2 (no consent), pa3 (revoked), pa4 (quarantined),
    # pa7 (wrong purpose) excluded — same gates as the saved-segment count.
    assert resp.json() == {"count": 3}


def test_preview_full_filter_matches_only_pa1(client):
    resp = client.post(PREVIEW_URL, json=LAPSED_SEGMENT, headers=HDR_A)
    assert resp.json()["count"] == 1  # only pa1


def test_preview_quarantined_excluded(client):
    # A segment whose only extra filter matches everyone must still exclude
    # the quarantined pa4 (which holds a valid marketing consent).
    payload = {"name": "all marketing", "purpose": "marketing", "filter": {}}
    count = client.post(PREVIEW_URL, json=payload, headers=HDR_A).json()["count"]
    assert count == 3  # NOT 4 — quarantined pa4 excluded (gate 4)


def test_preview_wrong_purpose_counts_service_consents_only(client):
    payload = {"name": "svc", "purpose": "service", "filter": {}}
    resp = client.post(PREVIEW_URL, json=payload, headers=HDR_A)
    assert resp.json()["count"] == 1  # only pa7


def test_preview_has_consent_override(client):
    payload = {
        "name": "svc purpose, marketing consent",
        "purpose": "service",
        "filter": {"has_consent": "marketing"},
    }
    assert client.post(PREVIEW_URL, json=payload, headers=HDR_A).json()["count"] == 3


def test_preview_tenant_isolated(client):
    # Tenant B's preview counts only tenant B's persons (pb1), never A's.
    resp = client.post(PREVIEW_URL, json=MARKETING_SEGMENT, headers=HDR_B)
    assert resp.json()["count"] == 1


def test_preview_persists_nothing(client):
    client.post(PREVIEW_URL, json=MARKETING_SEGMENT, headers=HDR_A)
    client.post(PREVIEW_URL, json=LAPSED_SEGMENT, headers=HDR_A)
    assert client.get("/v1/graph/segments", headers=HDR_A).json()["segments"] == []


def test_preview_invalid_dsl_rejected(client):
    # Injection-shaped purpose -> 422.
    bad_purpose = {"name": "bad", "purpose": "marketing'; DETACH DELETE p //", "filter": {}}
    assert client.post(PREVIEW_URL, json=bad_purpose, headers=HDR_A).status_code == 422
    # Quarantine opt-in -> 422 (gate 4).
    quarantine_opt_in = {
        "name": "bad",
        "purpose": "marketing",
        "filter": {"include_quarantined": True},
    }
    assert client.post(PREVIEW_URL, json=quarantine_opt_in, headers=HDR_A).status_code == 422
    # Missing purpose -> 422.
    assert client.post(PREVIEW_URL, json={"name": "x", "filter": {}}, headers=HDR_A).status_code == 422


def test_preview_requires_auth(client):
    assert client.post(PREVIEW_URL, json=MARKETING_SEGMENT).status_code == 401
