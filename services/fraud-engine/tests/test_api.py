"""FastAPI surface: /healthz, POST /v1/detect/run, GET /v1/detect/status.
Uses fakes only — no live FalkorDB/Kafka."""

import pytest
from fastapi.testclient import TestClient

from fraud_engine.config import Settings
from fraud_engine.main import create_app

from conftest import TENANT, make_cycle
from fakes import FakeGraphClient, PropertyGraph


# SPEC-W45 K23: /v1/detect/* is gated by X-Internal-Token; the shared
# fixture configures one and sends it (the auth matrix lives below).
TEST_INTERNAL_TOKEN = "test-fraud-internal-token"


@pytest.fixture()
def api_settings():
    import dataclasses

    # sweep/kafka off: background loops must not run under TestClient
    return dataclasses.replace(
        Settings(), sweep_enabled=False, kafka_enabled=False,
        internal_token=TEST_INTERNAL_TOKEN)


@pytest.fixture()
def api(client, publisher, api_settings):
    app = create_app(client=client, publisher=publisher, settings=api_settings)
    with TestClient(app, headers={"X-Internal-Token": TEST_INTERNAL_TOKEN}) as tc:
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
    assert body["sweep_enabled"] is False  # fixture disables the sweep loop
    assert body["alerts_topic"] == "opendesk.fraud.alerts.v1"
    assert len(body["detectors"]) == 8
    assert body["detector_count"] == 8
    # SPEC-W45 K23 (OOS-20): status must NOT leak evasion thresholds.
    assert "thresholds" not in body
    assert "sweep_minutes" not in body


# ---------------------------------------------------------------------------
# SPEC-W45 K23 (OOS-20): X-Internal-Token gate on /v1/detect/*
# ---------------------------------------------------------------------------


def test_detect_run_missing_token_401(client, publisher, api_settings):
    app = create_app(client=client, publisher=publisher, settings=api_settings)
    with TestClient(app) as tc:  # no token header
        resp = tc.post("/v1/detect/run", json={})
        assert resp.status_code == 401
        resp = tc.get("/v1/detect/status")
        assert resp.status_code == 401


def test_detect_run_wrong_token_401(client, publisher, api_settings):
    app = create_app(client=client, publisher=publisher, settings=api_settings)
    with TestClient(app, headers={"X-Internal-Token": "wrong"}) as tc:
        assert tc.post("/v1/detect/run", json={}).status_code == 401
        assert tc.get("/v1/detect/status").status_code == 401


def test_detect_unset_server_token_503_fail_closed(client, publisher):
    import dataclasses

    settings = dataclasses.replace(
        Settings(), sweep_enabled=False, kafka_enabled=False, internal_token="")
    app = create_app(client=client, publisher=publisher, settings=settings)
    with TestClient(app, headers={"X-Internal-Token": "anything"}) as tc:
        assert tc.post("/v1/detect/run", json={}).status_code == 503
        assert tc.get("/v1/detect/status").status_code == 503
        # healthz stays open for probes even when the gate is unconfigured
        assert tc.get("/healthz").status_code == 200


def test_kafka_trigger_topics_default_matches_producers():
    """ORPH O11: the default FRAUD_KAFKA_TOPICS must name topics producers
    actually write — `cac.events` (unprefixed; booking-service leads) plus
    the booking/identity/civic topics — never the dead `opendesk.cac.events`."""
    topics = Settings().kafka_trigger_topics.split(",")
    assert "cac.events" in topics
    assert "opendesk.booking.events" in topics
    assert "opendesk.identity.events" in topics
    assert "opendesk.civic.events.v1" in topics
    assert "opendesk.cac.events" not in topics
