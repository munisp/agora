"""/v1/graph/cypher tests — compliance gate 5: template allowlist ONLY,
arbitrary Cypher rejected; tenant isolation on every template."""

from __future__ import annotations

from conftest import HDR_A, HDR_B


def test_persons_by_consent_template(client):
    resp = client.post(
        "/v1/graph/cypher",
        json={"template": "persons_by_consent", "params": {"purpose": "marketing"}},
        headers=HDR_A,
    )
    assert resp.status_code == 200, resp.text
    ids = {r["person_id"] for r in resp.json()["rows"]}
    # pa2 (no consent), pa3 (revoked), pa7 (wrong purpose) excluded;
    # pa4 (quarantined) is query-visible here (gate 4: query-visible).
    assert ids == {"pa1", "pa4", "pa5", "pa6"}
    assert "$tenant_id" in resp.json()["cypher"]


def test_template_results_tenant_isolated(client):
    ids_b = {
        r["person_id"]
        for r in client.post(
            "/v1/graph/cypher",
            json={"template": "persons_by_consent", "params": {"purpose": "marketing"}},
            headers=HDR_B,
        ).json()["rows"]
    }
    assert ids_b == {"pb1"}  # never any tenant-A person


def test_raw_cypher_key_rejected(client):
    resp = client.post(
        "/v1/graph/cypher",
        json={"cypher": "MATCH (p:Person) DETACH DELETE p"},
        headers=HDR_A,
    )
    assert resp.status_code == 400
    assert "raw Cypher" in resp.json()["detail"]


def test_unknown_template_rejected(client):
    resp = client.post(
        "/v1/graph/cypher",
        json={"template": "drop_everything", "params": {}},
        headers=HDR_A,
    )
    assert resp.status_code == 400


def test_injection_via_params_rejected(client):
    resp = client.post(
        "/v1/graph/cypher",
        json={
            "template": "persons_by_consent",
            "params": {"purpose": "marketing' OR 1=1 //"},
        },
        headers=HDR_A,
    )
    assert resp.status_code == 400


def test_consent_counts_template(client):
    rows = client.post(
        "/v1/graph/cypher", json={"template": "consent_counts"}, headers=HDR_A
    ).json()["rows"]
    by_purpose = {r["purpose"]: r["persons"] for r in rows}
    assert by_purpose == {"marketing": 4, "service": 1}  # pa3 revoked not counted


def test_bookings_per_offering_template(client):
    rows = client.post(
        "/v1/graph/cypher", json={"template": "bookings_per_offering"}, headers=HDR_A
    ).json()["rows"]
    assert rows == [{"offering_id": "o1", "name": "Haircut", "bookings": 4}]
    # Tenant B has no offering rows.
    assert client.post(
        "/v1/graph/cypher", json={"template": "bookings_per_offering"}, headers=HDR_B
    ).json()["rows"] == []


def test_persons_lapsed_template(client):
    rows = client.post(
        "/v1/graph/cypher",
        json={"template": "persons_lapsed", "params": {"before": "2025-06-01"}},
        headers=HDR_A,
    ).json()["rows"]
    assert {r["person_id"] for r in rows} == {"pa1", "pa2", "pa6"}


def test_persons_not_messaged_since_template(client):
    rows = client.post(
        "/v1/graph/cypher",
        json={"template": "persons_not_messaged_since", "params": {"days": 30}},
        headers=HDR_A,
    ).json()["rows"]
    ids = {r["person_id"] for r in rows}
    assert "pa6" not in ids  # messaged 2 days ago
    assert "pa1" in ids  # messaged 40 days ago


def test_cypher_endpoint_requires_auth(client):
    assert client.post("/v1/graph/cypher", json={"template": "consent_counts"}).status_code == 401
