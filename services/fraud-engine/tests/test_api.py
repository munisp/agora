"""FastAPI surface: /healthz, POST /v1/detect/run, GET /v1/detect/status.
Uses fakes only — no live FalkorDB/Kafka."""

import pytest
from fastapi.testclient import TestClient

from fraud_engine.config import Settings
from fraud_engine.main import create_app

from conftest import TENANT, make_cycle
from fakes import FakeGraphClient, PropertyGraph


@pytest.fixture()
def api(client, publisher):
    # sweep/kafka off: background loops must not run under TestClient
    import dataclasses

    settings = dataclasses.replace(Settings(), sweep_enabled=False, kafka_enabled=False)
    app = create_app(client=client, publisher=publisher, settings=settings)
    with TestClient(app) as tc:
        yield tc


def test_healthz(api):
    resp = api.get("/healthz")
    assert resp.status_code == 200
    body = resp.json()
    assert body["status"] == "ok" and body["graph"] == "up"


def test_detect_run_manual(api, client, graph, publisher):
    make_cycle(graph, TENANT, ["p1", "p2", "p3"])
    resp = api.post("/v1/detect/run", json={"tenant_id": TENANT, "detector": "d1_referral_cycle"})
    assert resp.status_code == 200
    body = resp.json()
    assert body["alerts_created"] == 3
    assert body["detectors"] == ["d1_referral_cycle"]
    assert len(publisher.published) == 3


def test_detect_run_full_sweep_for_tenant(api, client, graph):
    make_cycle(graph, TENANT, ["p1", "p2", "p3"])
    resp = api.post("/v1/detect/run", json={"tenant_id": TENANT})
    assert resp.status_code == 200
    body = resp.json()
    assert body["alerts_created"] == 3  # only D1 trips on this fixture
    assert set(body["detectors"]) == {
        "d1_referral_cycle", "d2_sybil_cluster", "d3_capture_velocity",
        "d4_geo_impossibility", "d5_consent_backdating", "d6_ghost_booking",
        "d7_gnn_anomaly", "d8_report_spam",
    }


def test_detect_run_unknown_detector_400(api):
    resp = api.post("/v1/detect/run", json={"detector": "d99_nope"})
    assert resp.status_code == 400
    assert "known_detectors" in resp.json()["detail"]


def test_detect_status(api):
    resp = api.get("/v1/detect/status")
    assert resp.status_code == 200
    body = resp.json()
    assert body["sweep_minutes"] == 15
    assert body["alerts_topic"] == "opendesk.fraud.alerts.v1"
    assert len(body["detectors"]) == 8
    assert body["thresholds"]["CAPTURE_VELOCITY_MAX"] == 30
    assert body["thresholds"]["ANOMALY_ALERT_THRESHOLD"] == 0.9
    assert body["thresholds"]["CIVIC_REPORT_MAX_PER_DAY"] == 5
    assert body["thresholds"]["CIVIC_COORD_RADIUS_M"] == 500.0
