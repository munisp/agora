"""SPEC-W30 §4 WS-C: fraud alert triage/resolve API.

Covers: tenant isolation of list/detail (cross-tenant id -> 404), resolve
validation (reason mandatory >=10 chars, decision enum, 409 on re-resolve),
dismissed unquarantines ONLY when no other open high alert remains,
confirmed keeps quarantine, and the audit CloudEvent emission.
"""

from __future__ import annotations

import pytest
from fastapi.testclient import TestClient

from app.backend import InMemoryBackend
from app.config import Settings
from app.main import create_app
from app.store import SegmentStore
from conftest import HDR_A, HDR_B, StubLLM, build_graph


def _alert(graph, tenant, alert_id, *, status="open", severity="high",
           person_id="pa1", type="referral_cycle", agent_id=None):
    graph.add_node(
        alert_id,
        {"Alert"},
        tenant_id=tenant,
        alert_id=alert_id,
        type=type,
        severity=severity,
        status=status,
        person_id=person_id,
        agent_id=agent_id,
        evidence='{"cycle": ["pa1", "pa2"]}',
        created_at="2026-08-05T00:00:00+00:00",
    )


@pytest.fixture()
def alerts_client(tmp_path):
    settings = Settings(
        graph_backend="memory",
        segment_store_dir=str(tmp_path / "store"),
        jwt_public_key="",
    )
    backend = InMemoryBackend(build_graph())
    g = backend.graph
    _alert(g, "tenant-a", "ra1", person_id="pa1")
    _alert(g, "tenant-a", "ra2", severity="medium", type="ghost_booking", person_id=None,
           agent_id="staff-1")
    _alert(g, "tenant-b", "rb1", person_id="pb1")
    app = create_app(
        settings,
        backend=backend,
        llm=StubLLM(),
        store=SegmentStore(str(tmp_path / "seg")),
    )
    return TestClient(app), backend


# ------------------------------------------------------------------ list

def test_list_alerts_tenant_scoped(alerts_client):
    client, _ = alerts_client
    rows = client.get("/v1/graph/alerts", headers=HDR_A).json()["alerts"]
    assert {r["alert_id"] for r in rows} == {"ra1", "ra2"}
    rows_b = client.get("/v1/graph/alerts", headers=HDR_B).json()["alerts"]
    assert {r["alert_id"] for r in rows_b} == {"rb1"}


def test_list_alerts_filters(alerts_client):
    client, _ = alerts_client
    open_rows = client.get("/v1/graph/alerts?status=open", headers=HDR_A).json()["alerts"]
    assert {r["alert_id"] for r in open_rows} == {"ra1", "ra2"}
    high = client.get("/v1/graph/alerts?severity=high", headers=HDR_A).json()["alerts"]
    assert {r["alert_id"] for r in high} == {"ra1"}
    ghost = client.get("/v1/graph/alerts?type=ghost_booking", headers=HDR_A).json()["alerts"]
    assert {r["alert_id"] for r in ghost} == {"ra2"}


def test_list_alerts_bad_enum_422(alerts_client):
    client, _ = alerts_client
    assert client.get("/v1/graph/alerts?status=bogus", headers=HDR_A).status_code == 422


# ------------------------------------------------------------------ detail

def test_alert_detail_tenant_scoped(alerts_client):
    client, _ = alerts_client
    resp = client.get("/v1/graph/alerts/ra1", headers=HDR_A)
    assert resp.status_code == 200
    assert resp.json()["alert_id"] == "ra1"
    assert resp.json()["evidence"] == '{"cycle": ["pa1", "pa2"]}'
    # Cross-tenant detail answers 404 (no existence leak).
    assert client.get("/v1/graph/alerts/rb1", headers=HDR_A).status_code == 404
    assert client.get("/v1/graph/alerts/nope", headers=HDR_A).status_code == 404


# ------------------------------------------------------------------ resolve

def _resolve(client, alert_id, *, decision="dismissed", reason="false positive confirmed",
             headers=HDR_A):
    return client.post(
        f"/v1/graph/alerts/{alert_id}/resolve",
        json={"decision": decision, "reason": reason},
        headers=headers,
    )


def test_resolve_dismissed_clears_quarantine_and_emits_event(alerts_client):
    client, backend = alerts_client
    backend.graph.nodes["tenant-a:pa1"].props["quarantine"] = True
    resp = _resolve(client, "ra1")
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["status"] == "dismissed"
    assert body["quarantine_cleared"] is True
    assert body["event_type"] == "com.opendesk.fraud.AlertResolved"
    assert body["topic"] == "opendesk.fraud.alerts.v1"
    alert = backend.graph.nodes["ra1"].props
    assert alert["status"] == "dismissed"
    assert alert["resolve_reason"] == "false positive confirmed"
    assert alert["resolved_at"]
    assert backend.graph.nodes["tenant-a:pa1"].props["quarantine"] is False
    events = client.app.state.deps.events.published
    assert len(events) == 1
    topic, event = events[0]
    assert topic == "opendesk.fraud.alerts.v1"
    assert event["type"] == "com.opendesk.fraud.AlertResolved"
    assert event["tenantid"] == "tenant-a"
    assert event["data"]["decision"] == "dismissed"
    assert event["data"]["alert_id"] == "ra1"


def test_resolve_confirmed_keeps_quarantine(alerts_client):
    client, backend = alerts_client
    backend.graph.nodes["tenant-a:pa1"].props["quarantine"] = True
    resp = _resolve(client, "ra1", decision="confirmed", reason="verified referral ring")
    assert resp.status_code == 200
    assert resp.json()["quarantine_cleared"] is False
    assert backend.graph.nodes["tenant-a:pa1"].props["quarantine"] is True


def test_resolve_dismissed_keeps_quarantine_with_other_open_high(alerts_client):
    client, backend = alerts_client
    backend.graph.nodes["tenant-a:pa1"].props["quarantine"] = True
    # A second open HIGH alert flags the same person.
    _alert(backend.graph, "tenant-a", "ra9", person_id="pa1", type="sybil_cluster")
    resp = _resolve(client, "ra1")
    assert resp.status_code == 200
    assert resp.json()["quarantine_cleared"] is False
    assert backend.graph.nodes["tenant-a:pa1"].props["quarantine"] is True


def test_resolve_requires_reason_min_length(alerts_client):
    client, _ = alerts_client
    resp = _resolve(client, "ra1", reason="short")
    assert resp.status_code == 422


def test_resolve_rejects_bad_decision(alerts_client):
    client, _ = alerts_client
    resp = _resolve(client, "ra1", decision="banish")
    assert resp.status_code == 422


def test_resolve_cross_tenant_404(alerts_client):
    client, _ = alerts_client
    assert _resolve(client, "rb1").status_code == 404


def test_resolve_already_resolved_409(alerts_client):
    client, _ = alerts_client
    assert _resolve(client, "ra1").status_code == 200
    assert _resolve(client, "ra1").status_code == 409
