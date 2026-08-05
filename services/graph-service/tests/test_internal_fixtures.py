"""SPEC-W30 WS-C addendum: dev/e2e fixture seeder.

Covers: router absent (404) unless E2E_FIXTURES=1, X-Internal-Token auth
(401 without/with wrong token; JWT never accepted), and every scenario
building a tenant-scoped fixture verified through the existing read
templates (bound-tenant evaluation).
"""

from __future__ import annotations

import pytest
from fastapi.testclient import TestClient

from app.backend import InMemoryBackend
from app.config import Settings
from app.main import create_app
from app.store import SegmentStore
from conftest import HDR_A, HDR_B, StubLLM, build_graph

TOKEN = "fixture-token"


def _client(tmp_path, e2e: bool, graph=None):
    settings = Settings(
        graph_backend="memory",
        segment_store_dir=str(tmp_path / "store"),
        jwt_public_key="",
        internal_token=TOKEN,
        e2e_fixtures=e2e,
    )
    backend = InMemoryBackend(graph if graph is not None else build_graph())
    app = create_app(
        settings,
        backend=backend,
        llm=StubLLM(),
        store=SegmentStore(str(tmp_path / "seg")),
    )
    return TestClient(app), backend


def _seed(client, scenario, params=None, tenant="tenant-e2e", token=TOKEN):
    headers = {"X-Internal-Token": token} if token else {}
    return client.post(
        "/v1/graph/internal/fixtures/seed",
        json={"tenant_id": tenant, "scenario": scenario, "params": params or {}},
        headers=headers,
    )


def test_route_absent_without_e2e_fixtures(tmp_path):
    client, _ = _client(tmp_path, e2e=False)
    resp = _seed(client, "small_tenant")
    assert resp.status_code == 404  # router not mounted at all


def test_seed_requires_internal_token(tmp_path):
    client, _ = _client(tmp_path, e2e=True)
    assert _seed(client, "small_tenant", token=None).status_code == 401
    assert _seed(client, "small_tenant", token="wrong").status_code == 401


def test_seed_jwt_seam_not_accepted(tmp_path):
    client, _ = _client(tmp_path, e2e=True)
    resp = client.post(
        "/v1/graph/internal/fixtures/seed",
        json={"tenant_id": "tenant-e2e", "scenario": "small_tenant", "params": {}},
        headers=HDR_A,  # dev tenant header must not authenticate internal routes
    )
    assert resp.status_code == 401


def test_seed_unknown_scenario_422(tmp_path):
    client, _ = _client(tmp_path, e2e=True)
    assert _seed(client, "drop_graph").status_code == 422


def test_seed_param_bounds_enforced(tmp_path):
    client, _ = _client(tmp_path, e2e=True)
    resp = _seed(client, "small_tenant", {"persons": 5000})
    assert resp.status_code == 422
    resp = _seed(client, "referral_ring", {"size": 2})
    assert resp.status_code == 422


def test_small_tenant_scenario(tmp_path):
    client, _ = _client(tmp_path, e2e=True)
    resp = _seed(client, "small_tenant", {"persons": 4, "bookings": 6, "offerings": 2})
    assert resp.status_code == 200, resp.text
    ids = resp.json()["ids"]
    assert ids["person_ids"] == ["fx-p1", "fx-p2", "fx-p3", "fx-p4"]
    assert ids["offering_ids"] == ["fx-o1", "fx-o2"]
    # Verify via existing read templates (tenant-scoped evaluation).
    rows = client.post(
        "/v1/graph/cypher",
        json={"template": "persons_by_consent", "params": {"purpose": "marketing"}},
        headers={"X-Tenant-Id": "tenant-e2e"},
    ).json()["rows"]
    assert {r["person_id"] for r in rows} == set(ids["person_ids"])
    offerings = client.post(
        "/v1/graph/cypher",
        json={"template": "bookings_per_offering"},
        headers={"X-Tenant-Id": "tenant-e2e"},
    ).json()["rows"]
    assert sum(r["bookings"] for r in offerings) == 6


def test_small_tenant_is_tenant_scoped(tmp_path):
    client, _ = _client(tmp_path, e2e=True)
    _seed(client, "small_tenant", tenant="tenant-e2e")
    # The seeded fixture persons are invisible to other tenants.
    rows = client.post(
        "/v1/graph/cypher",
        json={"template": "persons_by_consent", "params": {"purpose": "marketing"}},
        headers=HDR_A,
    ).json()["rows"]
    assert "fx-p1" not in {r["person_id"] for r in rows}


def test_referral_ring_scenario(tmp_path):
    client, backend = _client(tmp_path, e2e=True)
    resp = _seed(client, "referral_ring", {"size": 4, "with_conversion": True})
    assert resp.status_code == 200, resp.text
    ids = resp.json()["ids"]["person_ids"]
    assert len(ids) == 4
    g = backend.graph
    # Cycle: p1 -> p2 -> p3 -> p4 -> p1.
    for i, pid in enumerate(ids):
        out = g.edges_from(f"tenant-e2e:{pid}", "REFERRED")
        assert len(out) == 1
        assert out[0].dst == f"tenant-e2e:{ids[(i + 1) % 4]}"
        # with_conversion -> every ring member booked.
        assert g.edges_from(f"tenant-e2e:{pid}", "BOOKED")


def test_backdated_consent_scenario(tmp_path):
    client, backend = _client(tmp_path, e2e=True)
    resp = _seed(client, "backdated_consent")
    assert resp.status_code == 200, resp.text
    pid = resp.json()["ids"]["person_id"]
    g = backend.graph
    consent_edge = g.edges_from(f"tenant-e2e:{pid}", "CONSENTED")[0]
    msg_edge = g.edges_from(f"tenant-e2e:{pid}", "MESSAGED")[0]
    # granted_at AFTER first message for the same purpose (F4 tripwire).
    assert consent_edge.props["granted_at"] > msg_edge.props["at"]
    assert consent_edge.props["purpose"] == msg_edge.props["purpose"]


def test_impossible_travel_scenario(tmp_path):
    client, backend = _client(tmp_path, e2e=True)
    resp = _seed(client, "impossible_travel", {"agent": "agent-x"})
    assert resp.status_code == 200, resp.text
    ids = resp.json()["ids"]
    assert ids["agent"] == "agent-x"
    g = backend.graph
    loc1 = g.nodes["tenant-e2e:fx-geo-loc1"].props
    loc2 = g.nodes["tenant-e2e:fx-geo-loc2"].props
    # ~150km apart (Lagos vs Ibadan), captures 10 min apart.
    assert abs(loc1["lat"] - loc2["lat"]) > 0.5
    c1 = g.nodes["tenant-e2e:fx-geo-lead1"].props
    c2 = g.nodes["tenant-e2e:fx-geo-lead2"].props
    assert c1["captured_by"] == c2["captured_by"] == "agent-x"
    assert c1["captured_at"] != c2["captured_at"]


def test_capture_burst_scenario(tmp_path):
    client, backend = _client(tmp_path, e2e=True)
    resp = _seed(client, "capture_burst", {"agent": "agent-b", "count": 40})
    assert resp.status_code == 200, resp.text
    assert resp.json()["ids"]["agent"] == "agent-b"
    g = backend.graph
    captures = [
        n for n in g.nodes.values()
        if "Contact" in n.labels
        and n.props.get("tenant_id") == "tenant-e2e"
        and n.props.get("captured_by") == "agent-b"
    ]
    assert len(captures) == 40
    stamps = sorted(c.props["captured_at"] for c in captures)
    # All captures within a 30-minute window.
    from app.plans import parse_instant

    span = parse_instant(stamps[-1]) - parse_instant(stamps[0])
    assert span.total_seconds() <= 1800
