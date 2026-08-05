"""Person 360 tests (tenant-scoped; 404 cross-tenant)."""

from __future__ import annotations

from conftest import HDR_A, HDR_B


def test_person_360_full(client):
    resp = client.get("/v1/graph/persons/pa1", headers=HDR_A)
    assert resp.status_code == 200, resp.text
    body = resp.json()
    person = body["person"]
    assert person["person_id"] == "pa1"
    assert person["name"] == "Ada"
    assert person["quarantine"] is False

    assert body["contacts"][0]["lead_id"] == "lead1"
    assert body["contacts"][0]["lga"] == "Alimosho"

    assert body["bookings"][0]["booking_id"] == "b1"
    assert body["bookings"][0]["offering_name"] == "Haircut"

    purposes = {c["purpose"] for c in body["consents"]}
    assert purposes == {"marketing"}
    assert body["consents"][0]["revoked_at"] is None

    refs = body["referrals"]
    assert refs[0]["direction"] == "outgoing"
    assert refs[0]["person_id"] == "pa2"

    assert body["messages"][0]["campaign_id"] == "camp1"


def test_person_360_incoming_referral(client):
    body = client.get("/v1/graph/persons/pa2", headers=HDR_A).json()
    assert body["referrals"][0]["direction"] == "incoming"
    assert body["referrals"][0]["person_id"] == "pa1"
    # pa2 has no consent edges -> empty consents list (still query-visible).
    assert body["consents"] == []


def test_person_360_quarantined_person_is_query_visible(client):
    # Gate 4: quarantined persons are query-visible (but audience-ineligible).
    resp = client.get("/v1/graph/persons/pa4", headers=HDR_A)
    assert resp.status_code == 200
    assert resp.json()["person"]["quarantine"] is True


def test_person_360_cross_tenant_404(client):
    # Gate 1 negative: tenant B cannot read tenant A's person.
    assert client.get("/v1/graph/persons/pa1", headers=HDR_B).status_code == 404
    # And tenant A cannot read tenant B's person.
    assert client.get("/v1/graph/persons/pb1", headers=HDR_A).status_code == 404


def test_person_360_unknown_404(client):
    assert client.get("/v1/graph/persons/nope", headers=HDR_A).status_code == 404


def test_person_360_requires_auth(client):
    assert client.get("/v1/graph/persons/pa1").status_code == 401
