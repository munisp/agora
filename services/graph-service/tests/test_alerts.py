"""SPEC-W30 §4 WS-C: fraud alerts router.

Covers: tenant isolation of list/detail, status/type/severity filters,
resolve validation (decision enum, mandatory reason min 10 chars),
resolve bookkeeping (status/resolved_at/resolved_by/resolve_reason),
unquarantine ONLY when no other open high-severity alert flags the person,
confirmed keeps quarantine, audit CloudEvent emission, and 409 on re-resolve.
"""

from __future__ import annotations

import pytest
from fastapi.testclient import TestClient

from app.backend import InMemoryBackend, InMemoryGraph
from app.config import Settings
from app.main import create_app
from app.store import SegmentStore
from conftest import HDR_A, HDR_B, StubLLM, build_graph


class FakePublisher:
    def __init__(self):
        self.events: list[tuple[str, dict]] = []

    async def publish(self, topic, event):
        self.events.append((topic, event))


def _add_alert(
    g: InMemoryGraph,
    alert_id: str,
    tenant: str,
    *,
    type="referral_cycle",
    severity="high",
    status="open",
    person_id=None,
    flag_node=None,
    created_at="2026-08-05T10:00:00+00:00",
):
    props = {
        "alert_id": alert_id,
        "tenant_id": tenant,
        "type": type,
        "severity": severity,
        "status": status,
        "evidence": '{"cycle": ["pa1", "pa2"]}',
        "created_at": created_at,
    }
    if person_id is not None:
        props["person_id"] = person_id
    node = g.add_node(f"{tenant}:{alert_id}", {"Alert"}, **props)
    if flag_node is not None:
        g.add_edge(node.node_id, flag_node, "FLAGGED")
    return node


@pytest.fixture()
def alerts_env(tmp_path):
    g = build_graph()
    a, b = "tenant-a", "tenant-b"
    # Quarantined person pa4 flagged by a high-severity open alert.
    _add_alert(g, "al-high-1", a, person_id="pa4", flag_node=f"{a}:pa4")
    # A medium-severity open alert on pa1.
    _add_alert(g, "al-med-1", a, type="ghost_booking", severity="medium",
               person_id="pa1", flag_node=f"{a}:pa1",
               created_at="2026-08-05T11:00:00+00:00")
    # An already-dismissed alert.
    _add_alert(g, "al-old-1", a, type="gnn_anomaly", severity="low",
               status="dismissed", created_at="2026-08-04T09:00:00+00:00")
    # Tenant B alert (must never leak into tenant A).
    _add_alert(g, "al-b-1", b, person_id="pb1", flag_node=f"{b}:pb1")

    settings = Settings(
        graph_backend="memory",
        segment_store_dir=str(tmp_path / "store"),
        jwt_public_key="",
        internal_token="tok",
    )
    events = FakePublisher()
    app = create_app(
        settings,
        backend=InMemoryBackend(g),
        llm=StubLLM(),
        store=SegmentStore(str(tmp_path / "seg")),
        events=events,
    )
    return TestClient(app), g, events


# ------------------------------------------------------------------ list
def test_list_alerts_tenant_isolated(alerts_env):
    client, _, _ = alerts_env
    body = client.get("/v1/graph/alerts", headers=HDR_A).json()
    ids = {a["alert_id"] for a in body["alerts"]}
    assert ids == {"al-high-1", "al-med-1", "al-old-1"}
    body_b = client.get("/v1/graph/alerts", headers=HDR_B).json()
    assert {a["alert_id"] for a in body_b["alerts"]} == {"al-b-1"}


def test_list_alerts_filters(alerts_env):
    client, _, _ = alerts_env
    open_only = client.get("/v1/graph/alerts?status=open", headers=HDR_A).json()
    assert {a["alert_id"] for a in open_only["alerts"]} == {"al-high-1", "al-med-1"}
    high = client.get("/v1/graph/alerts?severity=high", headers=HDR_A).json()
    assert {a["alert_id"] for a in high["alerts"]} == {"al-high-1"}
    ghost = client.get("/v1/graph/alerts?type=ghost_booking", headers=HDR_A).json()
    assert {a["alert_id"] for a in ghost["alerts"]} == {"al-med-1"}


def test_list_alerts_invalid_filter_422(alerts_env):
    client, _, _ = alerts_env
    assert client.get("/v1/graph/alerts?status=bogus", headers=HDR_A).status_code == 422
    assert client.get("/v1/graph/alerts?severity=extreme", headers=HDR_A).status_code == 422


def test_list_alerts_requires_auth(alerts_env):
    client, _, _ = alerts_env
    assert client.get("/v1/graph/alerts").status_code == 401


# ----------------------------------------------------------------- detail
def test_get_alert_detail(alerts_env):
    client, _, _ = alerts_env
    body = client.get("/v1/graph/alerts/al-med-1", headers=HDR_A).json()
    assert body["alert_id"] == "al-med-1"
    assert body["type"] == "ghost_booking"
    assert body["evidence"] == '{"cycle": ["pa1", "pa2"]}'


def test_get_alert_cross_tenant_404(alerts_env):
    client, _, _ = alerts_env
    assert client.get("/v1/graph/alerts/al-b-1", headers=HDR_A).status_code == 404
    assert client.get("/v1/graph/alerts/al-high-1", headers=HDR_B).status_code == 404


# ---------------------------------------------------------------- resolve
def test_resolve_requires_reason_min_10(alerts_env):
    client, _, _ = alerts_env
    resp = client.post(
        "/v1/graph/alerts/al-med-1/resolve",
        json={"decision": "dismissed", "reason": "short"},
        headers=HDR_A,
    )
    assert resp.status_code == 422
    resp = client.post(
        "/v1/graph/alerts/al-med-1/resolve",
        json={"decision": "dismissed"},
        headers=HDR_A,
    )
    assert resp.status_code == 422


def test_resolve_invalid_decision_422(alerts_env):
    client, _, _ = alerts_env
    resp = client.post(
        "/v1/graph/alerts/al-med-1/resolve",
        json={"decision": "maybe", "reason": "not a valid decision"},
        headers=HDR_A,
    )
    assert resp.status_code == 422


def test_resolve_dismissed_clears_quarantine_when_no_other_open_high(alerts_env):
    client, g, events = alerts_env
    resp = client.post(
        "/v1/graph/alerts/al-high-1/resolve",
        json={"decision": "dismissed", "reason": "false positive, reviewed by hand"},
        headers=HDR_A,
    )
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["status"] == "dismissed"
    assert body["quarantine_cleared"] is True
    alert = g.nodes["tenant-a:al-high-1"].props
    assert alert["status"] == "dismissed"
    assert alert["resolved_by"] == "tenant-a"  # dev seam: JWT sub equivalent
    assert alert["resolve_reason"] == "false positive, reviewed by hand"
    assert alert["resolved_at"]
    assert g.nodes["tenant-a:pa4"].props["quarantine"] is False
    # Audit CloudEvent emitted to the fraud alerts topic.
    assert len(events.events) == 1
    topic, event = events.events[0]
    assert topic == "opendesk.fraud.alerts.v1"
    assert event["type"] == "com.opendesk.fraud.AlertResolved"
    assert event["specversion"] == "1.0"
    assert event["tenantid"] == "tenant-a"
    assert event["data"]["alert_id"] == "al-high-1"
    assert event["data"]["decision"] == "dismissed"
    assert event["data"]["resolved_by"] == "tenant-a"


def test_resolve_dismissed_keeps_quarantine_with_other_open_high(alerts_env):
    client, g, _ = alerts_env
    # Second open high-severity alert on pa4.
    _add_alert(g, "al-high-2", "tenant-a", type="sybil_cluster",
               person_id="pa4", flag_node="tenant-a:pa4")
    resp = client.post(
        "/v1/graph/alerts/al-high-1/resolve",
        json={"decision": "dismissed", "reason": "duplicate of al-high-2"},
        headers=HDR_A,
    )
    assert resp.status_code == 200
    assert resp.json()["quarantine_cleared"] is False
    assert g.nodes["tenant-a:pa4"].props["quarantine"] is True


def test_resolve_confirmed_keeps_quarantine(alerts_env):
    client, g, events = alerts_env
    resp = client.post(
        "/v1/graph/alerts/al-high-1/resolve",
        json={"decision": "confirmed", "reason": "verified referral ring of 3"},
        headers=HDR_A,
    )
    assert resp.status_code == 200
    assert resp.json()["quarantine_cleared"] is False
    assert g.nodes["tenant-a:pa4"].props["quarantine"] is True
    assert g.nodes["tenant-a:al-high-1"].props["status"] == "confirmed"
    assert events.events[0][1]["data"]["decision"] == "confirmed"


def test_resolve_already_resolved_409(alerts_env):
    client, _, _ = alerts_env
    resp = client.post(
        "/v1/graph/alerts/al-old-1/resolve",
        json={"decision": "dismissed", "reason": "re-resolving an old alert"},
        headers=HDR_A,
    )
    assert resp.status_code == 409


def test_resolve_cross_tenant_404(alerts_env):
    client, _, _ = alerts_env
    resp = client.post(
        "/v1/graph/alerts/al-b-1/resolve",
        json={"decision": "dismissed", "reason": "cross tenant attempt here"},
        headers=HDR_A,
    )
    assert resp.status_code == 404


def test_resolve_requires_auth(alerts_env):
    client, _, _ = alerts_env
    resp = client.post(
        "/v1/graph/alerts/al-med-1/resolve",
        json={"decision": "confirmed", "reason": "no auth header present"},
    )
    assert resp.status_code == 401
