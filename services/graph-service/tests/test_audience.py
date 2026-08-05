"""Audience materialization tests — compliance gates 2 (consent) and 4
(quarantine), plus tenant-isolation negatives."""

from __future__ import annotations

from conftest import HDR_A, HDR_B, LAPSED_SEGMENT, MARKETING_SEGMENT


def _create_segment(client, payload=LAPSED_SEGMENT, headers=HDR_A):
    resp = client.post("/v1/graph/segments", json=payload, headers=headers)
    assert resp.status_code == 201, resp.text
    return resp.json()


def test_audience_contains_only_consent_passing_members(client):
    seg = _create_segment(client, MARKETING_SEGMENT)
    resp = client.post(f"/v1/graph/segments/{seg['id']}/audience", json={}, headers=HDR_A)
    assert resp.status_code == 201, resp.text
    body = resp.json()
    member_ids = {m["person_id"] for m in body["members"]}
    assert member_ids == {"pa1", "pa5", "pa6"}
    assert body["member_count"] == 3
    # Gate 2: persons without a purpose-matching unrevoked CONSENTED edge
    # never appear; gate 4: quarantined persons never appear.
    for excluded in ("pa2", "pa3", "pa4", "pa7"):
        assert excluded not in member_ids
    # Member refs carry no plaintext PII: ids + phone hash + lead ref only
    # (orchestrator contract member shape).
    for member in body["members"]:
        assert set(member.keys()) == {"person_id", "phone_hash", "lead_id"}


def test_audience_member_with_lead_resolves_lead_id(client):
    seg = _create_segment(client, MARKETING_SEGMENT)
    body = client.post(f"/v1/graph/segments/{seg['id']}/audience", json={}, headers=HDR_A).json()
    by_id = {m["person_id"]: m for m in body["members"]}
    assert by_id["pa5"]["lead_id"] == "lead5"
    assert by_id["pa5"]["phone_hash"] == "hash-pa5"


def test_audience_member_without_contact_has_null_lead_id(client):
    seg = _create_segment(client, MARKETING_SEGMENT)
    body = client.post(f"/v1/graph/segments/{seg['id']}/audience", json={}, headers=HDR_A).json()
    by_id = {m["person_id"]: m for m in body["members"]}
    assert "pa6" in by_id
    assert by_id["pa6"]["lead_id"] is None


def test_audience_multiple_contacts_most_recent_lead_id_wins(client):
    seg = _create_segment(client, MARKETING_SEGMENT)
    body = client.post(f"/v1/graph/segments/{seg['id']}/audience", json={}, headers=HDR_A).json()
    by_id = {m["person_id"]: m for m in body["members"]}
    # pa1 has lead1 (120d old) and lead1b (10d old): most recent wins.
    assert by_id["pa1"]["lead_id"] == "lead1b"


def test_audience_fetch_returns_same_member_shape(client):
    seg = _create_segment(client, MARKETING_SEGMENT)
    aud = client.post(f"/v1/graph/segments/{seg['id']}/audience", json={}, headers=HDR_A).json()
    fetched = client.get(f"/v1/graph/audiences/{aud['audience_id']}", headers=HDR_A).json()
    assert fetched["members"] == aud["members"]
    for member in fetched["members"]:
        assert set(member.keys()) == {"person_id", "phone_hash", "lead_id"}


def test_audience_full_filter_matches_only_pa1(client):
    seg = _create_segment(client, LAPSED_SEGMENT)
    resp = client.post(f"/v1/graph/segments/{seg['id']}/audience", json={"campaign_id": "camp9"}, headers=HDR_A)
    body = resp.json()
    assert [m["person_id"] for m in body["members"]] == ["pa1"]
    assert body["members"][0]["lead_id"] == "lead1b"  # most recent contact
    assert body["campaign_id"] == "camp9"


def test_audience_cross_tenant_404(client):
    seg = _create_segment(client, MARKETING_SEGMENT)
    resp = client.post(f"/v1/graph/segments/{seg['id']}/audience", json={}, headers=HDR_B)
    assert resp.status_code == 404


def test_audience_unknown_segment_404(client):
    resp = client.post("/v1/graph/segments/nope/audience", json={}, headers=HDR_A)
    assert resp.status_code == 404


def test_audience_fetch_handoff_and_isolation(client):
    seg = _create_segment(client, MARKETING_SEGMENT)
    aud = client.post(f"/v1/graph/segments/{seg['id']}/audience", json={}, headers=HDR_A).json()
    resp = client.get(f"/v1/graph/audiences/{aud['audience_id']}", headers=HDR_A)
    assert resp.status_code == 200
    assert resp.json()["member_count"] == 3
    # Cross-tenant fetch of the materialized audience -> 404.
    assert client.get(f"/v1/graph/audiences/{aud['audience_id']}", headers=HDR_B).status_code == 404


def test_audience_requires_auth(client):
    seg = _create_segment(client, MARKETING_SEGMENT)
    assert client.post(f"/v1/graph/segments/{seg['id']}/audience", json={}).status_code == 401
