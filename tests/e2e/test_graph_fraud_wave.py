"""SPEC-W30 e2e — fraud & trust intelligence.

Live-compose style, mirroring test_graph_wave.py (W28). Fraud fixtures are
seeded through graph-service's dev-only fixture seeder (E2E_FIXTURES=1);
detection runs through fraud-engine's manual trigger; alerts are read and
adjudicated through graph-service's tenant-scoped alerts API.

API shapes (verified at the W29/W30 gate):
  GET  /v1/graph/alerts            -> {"alerts": [...]}; evidence is a JSON
                                      STRING (parse it); D4 key is
                                      "implied_speed_kmh"
  GET  /v1/graph/persons/{id}      -> {"person": {...}, ...} (nested)
  POST /v1/graph/cypher            -> {"rows": [...]}
  referral_ring fixture: size >= 3 (one open alert per ring member)

Gate mapping (SPEC-W30 §5):
  G1 bound tenant_id · G2 quarantine matrix / no auto-erasure · G3 replayable
  evidence · G4 resolve audit trail · G5 dedup · G6 tenant isolation.

Env overrides: E2E_GRAPH_SERVICE (:7014), E2E_FRAUD_ENGINE (:7017),
E2E_INTERNAL_TOKEN (dev default opendesk-dev-internal-token).
"""
from __future__ import annotations

import json
import os
import uuid

import pytest
import requests

from conftest import poll, wait_http_ok

GRAPH = os.environ.get("E2E_GRAPH_SERVICE", "http://localhost:7014")
FRAUD = os.environ.get("E2E_FRAUD_ENGINE", "http://localhost:7017")
INTERNAL_TOKEN = os.environ.get("E2E_INTERNAL_TOKEN",
                                "opendesk-dev-internal-token")
DETECT_SLA_S = int(os.environ.get("E2E_DETECT_SLA", "120"))

pytestmark = pytest.mark.docker


@pytest.fixture(scope="session")
def fstack(stack):
    if not wait_http_ok(f"{GRAPH}/healthz", 15):
        pytest.skip("graph-service not running (compose without graph overlay?)")
    if not wait_http_ok(f"{FRAUD}/healthz", 15):
        pytest.skip("fraud-engine not running (compose without W30 overlay?)")
    return {"graph": GRAPH, "fraud": FRAUD}


@pytest.fixture(scope="session")
def fflow():
    return {}


def _slug() -> str:
    return f"e2e-w30-{uuid.uuid4().hex[:8]}"


def _h(slug: str) -> dict:
    return {"X-Tenant-Slug": slug, "X-Tenant-Id": slug,
            "content-type": "application/json"}


def _ih(slug: str) -> dict:
    return {**_h(slug), "X-Internal-Token": INTERNAL_TOKEN}


def _seed(slug: str, scenario: str, params: dict | None = None) -> dict:
    r = requests.post(f"{GRAPH}/v1/graph/internal/fixtures/seed",
                      headers=_ih(slug),
                      json={"tenant_id": slug, "scenario": scenario,
                            "params": params or {}},
                      timeout=30)
    assert r.status_code == 200, f"fixture seed failed: {r.status_code} {r.text}"
    return r.json()["ids"]


def _detect(slug: str, detector: str | None = None) -> None:
    body: dict = {"tenant_id": slug}
    if detector:
        body["detector"] = detector
    r = requests.post(f"{FRAUD}/v1/detect/run", json=body, timeout=60)
    assert r.status_code == 200, f"detect run failed: {r.status_code} {r.text}"


def _alerts(slug: str, **filt) -> list[dict]:
    r = requests.get(f"{GRAPH}/v1/graph/alerts", headers=_h(slug),
                     params={k: v for k, v in filt.items() if v}, timeout=20)
    assert r.status_code == 200, f"alerts list failed: {r.status_code} {r.text}"
    body = r.json()
    return body.get("alerts", body if isinstance(body, list) else [])


def _evidence(alert: dict) -> dict:
    """Alert evidence is a replayable JSON STRING — parse it (G3)."""
    ev = alert.get("evidence") or {}
    if isinstance(ev, str):
        ev = json.loads(ev)
    return ev


def _person(slug: str, pid: str) -> dict | None:
    r = requests.get(f"{GRAPH}/v1/graph/persons/{pid}", headers=_h(slug),
                     timeout=15)
    if r.status_code != 200:
        return None
    body = r.json()
    return body.get("person", body)


# --- detection + evidence (G3) ----------------------------------------------

def test_01_referral_ring_fires_alert_with_evidence(fstack, fflow):
    slug = _slug()
    fflow["slug"] = slug
    ids = _seed(slug, "referral_ring",
                {"size": 3, "with_conversion": True})
    fflow["ring"] = ids["person_ids"]
    _detect(slug, "referral_cycle")

    def _fired():
        got = _alerts(slug, type="referral_cycle")
        return got if got else None

    alerts = poll(_fired, DETECT_SLA_S, interval=5, desc="referral_cycle alert")
    a = alerts[0]
    fflow["alert_ids"] = [al["alert_id"] for al in alerts]
    assert a["status"] == "open"
    assert a["severity"] == "high"  # ring >= 3 with reward conversion
    ev = _evidence(a)
    assert set(ev.get("cycle", [])) >= set(fflow["ring"]), \
        f"evidence must replay the cycle path: {ev}"


def test_02_clean_tenant_zero_alerts(fstack, fflow):
    slug = _slug()
    _seed(slug, "small_tenant", {"persons": 8, "bookings": 12, "offerings": 3})
    _detect(slug)
    assert _alerts(slug) == []


def test_03_sweep_idempotent_no_duplicates(fstack, fflow):
    """G5: repeated sweeps over the same pattern never ADD alerts — one open
    alert per ring member, stable across sweeps."""
    slug = fflow["slug"]
    before = len(_alerts(slug, type="referral_cycle", status="open"))
    assert before >= 1
    for _ in range(2):
        _detect(slug, "referral_cycle")
    after = len(_alerts(slug, type="referral_cycle", status="open"))
    assert after == before, f"dedup broken: {before} -> {after} open alerts"


def test_04_consent_backdating_high_no_auto_quarantine(fstack, fflow):
    """D5: always high severity, but NEVER auto-quarantines (G2) — it routes
    to human compliance review."""
    slug = _slug()
    ids = _seed(slug, "backdated_consent", {})
    pid = ids["person_id"]
    _detect(slug, "consent_backdating")

    def _fired():
        got = _alerts(slug, type="consent_backdating")
        return got if got else None

    alerts = poll(_fired, DETECT_SLA_S, interval=5,
                  desc="consent_backdating alert")
    assert alerts[0]["severity"] == "high"
    person = _person(slug, pid)
    assert person and not person.get("quarantine"), \
        "D5 must never auto-quarantine"


# --- enforcement (G2): the money test ----------------------------------------

def test_05_fraud_quarantine_excludes_from_audience(fstack, fflow):
    """F1-high auto-quarantines ring members; audience-safe templates then
    exclude them (W28 quarantine gate reuse)."""
    slug = fflow["slug"]
    ring = fflow["ring"]
    for pid in ring:
        person = _person(slug, pid)
        assert person and person.get("quarantine"), \
            f"ring member not auto-quarantined: {person}"
    r = requests.post(f"{GRAPH}/v1/graph/cypher", headers=_h(slug),
                      json={"template": "churn_risk_band",
                            "params": {"min_score": 0.0}}, timeout=20)
    assert r.status_code == 200
    rows = r.json().get("rows", [])
    assert not {row.get("person_id") for row in rows} & set(ring)


# --- adjudication (G4) --------------------------------------------------------

def test_06_resolve_requires_reason_then_dismiss_unquarantines(fstack, fflow):
    slug = fflow["slug"]
    for alert_id in fflow["alert_ids"]:
        r_bad = requests.post(
            f"{GRAPH}/v1/graph/alerts/{alert_id}/resolve",
            headers=_h(slug),
            json={"decision": "dismissed", "reason": "short"},
            timeout=15)
        assert r_bad.status_code == 422
        r_ok = requests.post(
            f"{GRAPH}/v1/graph/alerts/{alert_id}/resolve",
            headers=_h(slug),
            json={"decision": "dismissed",
                  "reason": "verified legitimate family referrals"},
            timeout=15)
        assert r_ok.status_code == 200
    for pid in fflow["ring"]:
        person = _person(slug, pid)
        assert person and not person.get("quarantine")
        assert not person.get("erased"), "no auto-erasure, ever (G2)"


def test_07_confirm_keeps_quarantine(fstack, fflow):
    slug = _slug()
    ids = _seed(slug, "referral_ring",
                {"size": 3, "with_conversion": True})
    _detect(slug, "referral_cycle")

    def _fired():
        got = _alerts(slug, type="referral_cycle", status="open")
        return got if got else None

    alerts = poll(_fired, DETECT_SLA_S, interval=5, desc="second ring alert")
    for a in alerts:
        r = requests.post(
            f"{GRAPH}/v1/graph/alerts/{a['alert_id']}/resolve",
            headers=_h(slug),
            json={"decision": "confirmed",
                  "reason": "ring fabricated for referral rewards"},
            timeout=15)
        assert r.status_code == 200
    for pid in ids["person_ids"]:
        assert _person(slug, pid).get("quarantine")


# --- detectors D3/D4 + tenant isolation (G6) ----------------------------------

def test_08_geo_impossibility_and_velocity(fstack, fflow):
    slug = _slug()
    _seed(slug, "impossible_travel", {"agent": "ag-1"})
    _seed(slug, "capture_burst", {"agent": "ag-2", "count": 35})
    _detect(slug, "geo_impossibility")
    _detect(slug, "capture_velocity")

    def _geo():
        got = _alerts(slug, type="geo_impossibility")
        return got if got else None

    geo = poll(_geo, DETECT_SLA_S, interval=5, desc="geo_impossibility alert")
    ev = _evidence(geo[0])
    jump = ev.get("jump", ev)  # d4 nests speed under evidence["jump"]
    assert float(jump.get("implied_speed_kmh", 0)) > 120
    assert _alerts(slug, type="capture_velocity"), "velocity alert missing"


def test_09_alerts_never_cross_tenants(fstack, fflow):
    slug_a = fflow["slug"]  # has the (now dismissed) ring alerts
    slug_b = _slug()
    _seed(slug_b, "referral_ring", {"size": 3})
    _detect(slug_b)
    alerts_b = _alerts(slug_b)
    assert alerts_b
    assert all(a.get("tenant_id", slug_b) == slug_b for a in alerts_b)
    assert {a["alert_id"] for a in alerts_b}.isdisjoint(
        {a["alert_id"] for a in _alerts(slug_a)})
