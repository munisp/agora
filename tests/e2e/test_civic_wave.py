"""SPEC-W32 e2e — civic reporting: citizen intake → MDA resolution.

Live-compose style, mirroring test_graph_wave.py: ordered tests against the
stack, session guard skips when booking-service is down. Public endpoints are
hit through booking-service directly (dev) — the APISIX public route
(/api/civic/public/*) is config-verified separately.

Gate mapping (SPEC-W32 §4):
  G1 public routes never leak other citizens' data · G2 SLA math ·
  G3 merge semantics · G4 anonymous masking by role · G5 tenant-scoped
  events/projection · G6 throttling + honeypot, no auto-quarantine.

Env override: E2E_BOOKING (default http://localhost:7002).
"""
from __future__ import annotations

import os
import re
import uuid

import pytest
import requests

from conftest import poll, wait_http_ok

BOOKING = os.environ.get("E2E_BOOKING", "http://localhost:7002")

pytestmark = pytest.mark.docker


@pytest.fixture(scope="session")
def cstack(stack):
    if not wait_http_ok(f"{BOOKING}/healthz", 15):
        pytest.skip("booking-service not running")
    return {"booking": BOOKING}


@pytest.fixture(scope="session")
def cflow():
    return {}


def _pub(slug: str) -> str:
    return f"{BOOKING}/v1/civic/public/tenants/{slug}"


def _report(slug: str, **over) -> requests.Response:
    body = {
        "category_slug": "roads",
        "description": "Large pothole blocking the left lane near the market "
                       "junction; two kekes have burst tyres this week.",
        "ward": "IKD",
        "location_text": "Market junction, Ikoyi",
        "lat": 6.4541,
        "lon": 3.4316,
        "reporter_phone_e164": f"+23480{uuid.uuid4().int % 10**8:08d}",
        "reporter_name": "E2E Citizen",
        "wants_updates": True,
        "website": "",  # honeypot must stay empty
    }
    body.update(over)
    return requests.post(f"{_pub(slug)}/reports", json=body, timeout=20)


# --- intake + tracking -------------------------------------------------------

def test_01_public_report_returns_ref(cstack, tenant, cflow):
    slug = tenant["slug"]
    cflow["slug"] = slug
    cflow["headers"] = tenant["headers"]
    r = _report(slug)
    assert r.status_code in (200, 201), f"report failed: {r.status_code} {r.text}"
    body = r.json()
    ref = body.get("ref", "")
    assert re.fullmatch(r"GOV-[A-Z0-9]+-[A-Z0-9]+-\d{4}-\d{6}", ref), \
        f"bad ref format: {ref}"
    assert body.get("ack_due_at"), "ack SLA deadline missing"
    cflow["ref"] = ref
    cflow["phone"] = None  # phone captured inside _report per call


def test_02_case_appears_in_operator_list(cstack, cflow):
    slug, ref = cflow["slug"], cflow["ref"]

    def _listed():
        r = requests.get(f"{BOOKING}/v1/civic/cases",
                         headers=cflow["headers"],
                         params={"q": ref}, timeout=15)
        if r.status_code != 200:
            return None
        cases = r.json().get("cases", r.json() if isinstance(r.json(), list)
                             else [])
        return cases if any(c.get("ref") == ref for c in cases) else None

    cases = poll(_listed, 60, interval=5, desc="case in operator list")
    case = next(c for c in cases if c["ref"] == ref)
    cflow["case_id"] = case.get("id") or case.get("case_id")
    assert case["status"] == "new"


def test_03_triage_assign_sets_sla_and_status(cstack, cflow):
    cid = cflow["case_id"]
    h = cflow["headers"]
    # TriageInput takes category_id (uuid), not category_slug — resolve a
    # seeded category DIFFERENT from the report's "roads" via the operator
    # categories endpoint so the re-categorization axis is real.
    cats = requests.get(f"{BOOKING}/v1/civic/categories", headers=h,
                        timeout=15).json()
    cat_list = cats.get("categories", cats if isinstance(cats, list) else [])
    other = next((c for c in cat_list
                  if c.get("slug") not in (None, "roads")
                  and c.get("active", True)), None)
    triage_body = {"category_id": other["id"]} if other else {"ward": "IKD"}
    r = requests.post(f"{BOOKING}/v1/civic/cases/{cid}/triage", headers=h,
                      json=triage_body, timeout=15)
    assert r.status_code == 200, f"triage failed: {r.status_code} {r.text}"
    if other:
        after = requests.get(f"{BOOKING}/v1/civic/cases/{cid}", headers=h,
                             timeout=15).json()
        assert after.get("category_slug") == other["slug"], \
            f"triage re-categorization not applied: {after.get('category_slug')}"
    r = requests.post(f"{BOOKING}/v1/civic/cases/{cid}/assign", headers=h,
                      json={"assignee": "roads-desk-1"}, timeout=15)
    assert r.status_code == 200
    r = requests.get(f"{BOOKING}/v1/civic/cases/{cid}", headers=h, timeout=15)
    case = r.json()
    assert case["status"] == "assigned"
    assert case.get("ack_due_at") and case.get("resolve_due_at"), \
        "G2: SLA deadlines must be set by triage"


def test_04_citizen_track_requires_matching_phone(cstack, cflow):
    """G1: ref + wrong phone → 404/403; the public surface never confirms a
    case exists without possession of the reporter phone."""
    slug, ref = cflow["slug"], cflow["ref"]
    r_bad = requests.get(f"{_pub(slug)}/reports/{ref}",
                         params={"phone": "+23400000000000"}, timeout=15)
    assert r_bad.status_code in (403, 404)
    # The good path needs the phone used at report time; pull it from the
    # operator detail (owner sees unmasked) — the point under test is the
    # BINDING, not operator visibility.
    r = requests.get(f"{BOOKING}/v1/civic/cases/{cflow['case_id']}",
                     headers=cflow["headers"], timeout=15)
    phone = (r.json().get("reporter_phone_e164")
             or r.json().get("reporter", {}).get("phone_e164"))
    if not phone:
        pytest.skip("operator detail masks reporter phone for this role")
    r_ok = requests.get(f"{_pub(slug)}/reports/{ref}",
                        params={"phone": phone}, timeout=15)
    assert r_ok.status_code == 200
    body = r_ok.json()
    assert body["ref"] == ref and body["status"] in (
        "new", "triaged", "assigned", "in_progress", "resolved", "closed")
    assert isinstance(body, dict)
    assert not any("note" in k.lower() for k in body), \
        f"G1: public track leaks operator notes: {sorted(body)}"


def test_05_resolve_flows_to_citizen_status(cstack, cflow):
    cid = cflow["case_id"]
    r = requests.post(f"{BOOKING}/v1/civic/cases/{cid}/status",
                      headers=cflow["headers"],
                      json={"status": "resolved",
                            "note": "Patched and resurfaced."}, timeout=15)
    assert r.status_code == 200
    r = requests.get(f"{BOOKING}/v1/civic/cases/{cid}",
                     headers=cflow["headers"], timeout=15)
    assert r.json()["status"] == "resolved"
    assert r.json().get("resolved_at"), "resolved_at must be stamped"


# --- public stats: aggregate-only (G1) ----------------------------------------

def test_06_public_stats_aggregate_only(cstack, cflow):
    slug = cflow["slug"]
    r = requests.get(f"{_pub(slug)}/stats", timeout=15)
    assert r.status_code == 200
    text = r.text
    assert "+234" not in text, "stats leaks a phone number"
    assert cflow["ref"] not in text, "stats leaks a case reference"
    # Counts sanity: the suite's own reports must be reflected in the
    # aggregates (open/resolved counters + category/ward rows exist).
    stats = r.json()
    total = int(stats.get("open", 0)) + int(stats.get("resolved", 0))
    assert total >= 1, f"stats does not reflect submitted cases: {stats}"
    assert isinstance(stats.get("by_category", []), list)
    assert isinstance(stats.get("by_ward", []), list)


# --- abuse protections (G6) ----------------------------------------------------

def test_07_honeypot_and_validation(cstack, cflow):
    slug = cflow["slug"]
    r_bot = _report(slug, website="http://spam.example")
    assert r_bot.status_code in (400, 422), \
        f"honeypot submission accepted: {r_bot.status_code}"
    r_short = _report(slug, description="too short")
    assert r_short.status_code in (400, 422)


def test_08_throttle_triggers(cstack, cflow):
    """Per-phone/IP throttle: 11 rapid reports from one phone → 429."""
    slug = cflow["slug"]
    phone = f"+23481{uuid.uuid4().int % 10**8:08d}"
    codes = []
    for _ in range(11):
        r = _report(slug, reporter_phone_e164=phone)
        codes.append(r.status_code)
    assert 429 in codes, f"throttle never fired: {codes}"


# --- merge semantics (G3) -------------------------------------------------------

def test_09_merge_duplicate_points_at_canonical(cstack, cflow):
    slug, h = cflow["slug"], cflow["headers"]
    r2 = _report(slug, description="Same pothole at the market junction, "
                                   "reporting again — still not fixed.")
    assert r2.status_code in (200, 201)
    ref2 = r2.json()["ref"]
    listed = requests.get(f"{BOOKING}/v1/civic/cases", headers=h,
                          params={"q": ref2}, timeout=15)
    cases = listed.json().get("cases", [])
    cid2 = next(c for c in cases if c["ref"] == ref2)["id"]
    r = requests.post(f"{BOOKING}/v1/civic/cases/{cid2}/merge", headers=h,
                      json={"canonical_id": cflow["case_id"]}, timeout=15)
    assert r.status_code == 200
    detail = requests.get(f"{BOOKING}/v1/civic/cases/{cid2}", headers=h,
                          timeout=15).json()
    assert detail.get("merged_into") in (cflow["case_id"], cflow["ref"]), \
        f"merge pointer missing: {detail.get('merged_into')}"
