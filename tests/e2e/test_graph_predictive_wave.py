"""SPEC-W29 e2e — predictive layer: propensity scores & recommendations.

Live-compose style, mirroring test_graph_wave.py (W28): ordered tests against
the graph overlay, session guards skip the module when services are down.

Fixtures are seeded through graph-service's dev-only internal fixture seeder
(E2E_FIXTURES=1 in the dev overlay; fixed server-side scenario allowlist,
never raw client Cypher). Scoring runs through graph-ml's manual trigger.

API shapes (verified at the W29/W30 gate):
  POST /v1/graph/cypher            -> {"rows": [...]}
  GET  /v1/graph/persons/{id}      -> {"person": {...}, "contacts": [...]}
  POST /v1/graph/segments/count    -> {"count": N}   (requires name + purpose)
  POST /v1/graph/internal/scores   -> 200 {"written": N, ...} (non-empty body)

Gate mapping (SPEC-W29 §4):
  G1 tenant isolation · G2 consent/quarantine supremacy · G3 single write
  path · G4 cold start · G5 degraded GNN · G6 internal auth.

Env overrides: E2E_GRAPH_SERVICE (:7014), E2E_GRAPH_ML (:7016),
E2E_INTERNAL_TOKEN (dev default opendesk-dev-internal-token).
"""
from __future__ import annotations

import os
import uuid

import pytest
import requests

from conftest import poll, wait_http_ok

GRAPH = os.environ.get("E2E_GRAPH_SERVICE", "http://localhost:7014")
GRAPH_ML = os.environ.get("E2E_GRAPH_ML", "http://localhost:7016")
INTERNAL_TOKEN = os.environ.get("E2E_INTERNAL_TOKEN",
                                "opendesk-dev-internal-token")
SCORE_SLA_S = int(os.environ.get("E2E_SCORE_SLA", "120"))

pytestmark = pytest.mark.docker


@pytest.fixture(scope="session")
def pstack(stack):
    """Session guard: graph-service AND graph-ml must be healthy."""
    if not wait_http_ok(f"{GRAPH}/healthz", 15):
        pytest.skip("graph-service not running (compose without graph overlay?)")
    if not wait_http_ok(f"{GRAPH_ML}/healthz", 15):
        pytest.skip("graph-ml not running (compose without W29 overlay?)")
    return {"graph": GRAPH, "ml": GRAPH_ML}


@pytest.fixture(scope="session")
def pflow():
    return {}


def _slug() -> str:
    return f"e2e-w29-{uuid.uuid4().hex[:8]}"


def _h(slug: str) -> dict:
    return {"X-Tenant-Slug": slug, "X-Tenant-Id": slug,
            "content-type": "application/json"}


def _ih(slug: str, token: str = INTERNAL_TOKEN) -> dict:
    return {**_h(slug), "X-Internal-Token": token}


def _seed(slug: str, scenario: str, params: dict | None = None) -> dict:
    r = requests.post(f"{GRAPH}/v1/graph/internal/fixtures/seed",
                      headers=_ih(slug),
                      json={"tenant_id": slug, "scenario": scenario,
                            "params": params or {}},
                      timeout=30)
    assert r.status_code == 200, f"fixture seed failed: {r.status_code} {r.text}"
    return r.json()["ids"]


def _run_scorer(slug: str) -> dict:
    r = requests.post(f"{GRAPH_ML}/v1/score/run", json={"tenant_id": slug},
                      timeout=60)
    assert r.status_code == 200, f"score run failed: {r.status_code} {r.text}"
    return r.json()


def _person(slug: str, pid: str) -> dict | None:
    """Person-360: response is {"person": {...}, "contacts": [...]} — unwrap."""
    r = requests.get(f"{GRAPH}/v1/graph/persons/{pid}", headers=_h(slug),
                     timeout=15)
    if r.status_code != 200:
        return None
    body = r.json()
    return body.get("person", body)


def _rows(slug: str, template: str, params: dict) -> list[dict]:
    """Allowlisted template query: response is {"rows": [...]}."""
    r = requests.post(f"{GRAPH}/v1/graph/cypher", headers=_h(slug),
                      json={"template": template, "params": params},
                      timeout=20)
    assert r.status_code == 200, \
        f"template {template} failed: {r.status_code} {r.text}"
    body = r.json()
    return body.get("rows", [])


def _segment_count(slug: str, dsl: dict) -> requests.Response:
    """Unsaved-DSL count preview. SegmentCreate requires name + purpose, and
    the DSL (consent_purpose, score_filters, ...) nests under `filter` —
    top-level extras are silently ignored by the pydantic model."""
    payload = {"name": "e2e-count-preview", "purpose": "marketing",
               "filter": dsl}
    return requests.post(f"{GRAPH}/v1/graph/segments/count",
                         headers=_h(slug), json=payload, timeout=20)


# --- G4: cold-start heuristic sweep ----------------------------------------

def test_01_heuristic_sweep_scores_small_graph(pstack, pflow):
    """5-person tenant: sweep completes; every person scored with
    model_version + scored_at (cold start never crashes, never empty)."""
    slug = _slug()
    pflow["slug"] = slug
    ids = _seed(slug, "small_tenant",
                {"persons": 5, "bookings": 7, "offerings": 3})
    pflow["persons"] = ids["person_ids"]
    result = _run_scorer(slug)
    pflow["run"] = result
    # ScoreRunResponse = {run_id, backend, tenants[], ok}; per-tenant status
    # lives inside tenants[].
    assert result.get("ok") is True, f"score run not ok: {result}"

    def _scored():
        got = [_person(slug, p) for p in pflow["persons"]]
        return got if all(g and g.get("scored_at") for g in got) else None

    people = poll(_scored, SCORE_SLA_S, interval=5, desc="heuristic scores")
    for p in people:
        assert 0.0 <= float(p["propensity_churn"]) <= 1.0
        assert str(p["model_version"]).startswith("heuristic")


def test_02_recommendations_topk_with_reasons(pstack, pflow):
    """RECOMMENDED_FOR edges: ≤ top-K per person, human reason,
    model_version present; consumed via next_best_services template."""
    slug = pflow["slug"]
    pid = pflow["persons"][0]
    rows = _rows(slug, "next_best_services", {"person_id": pid})
    assert rows, "expected recommendations after sweep"
    assert len(rows) <= 5  # GRAPH_ML_TOP_K default
    for s in rows:
        assert s.get("reason"), f"recommendation without reason: {s}"
        assert s.get("model_version")


def test_03_churn_band_excludes_quarantined(pstack, pflow):
    """G2: quarantined persons never appear in churn_risk_band output.
    Uses the seeder's real quarantine knob (quarantine_last) on a fresh
    tenant so the exclusion is actually exercised, never skipped."""
    slug = _slug()
    ids = _seed(slug, "small_tenant",
                {"persons": 4, "bookings": 6, "offerings": 2,
                 "quarantine_last": True})
    q = ids["person_ids"][-1]
    person = _person(slug, q)
    assert person is not None and person.get("quarantine"), \
        f"quarantine_last knob did not quarantine the last person: {person}"
    _run_scorer(slug)
    rows = _rows(slug, "churn_risk_band", {"min_score": 0.0})
    assert q not in {row.get("person_id") for row in rows}, \
        "quarantined person leaked into churn_risk_band"


def test_04_segment_score_filters_compile_and_filter(pstack, pflow):
    """Score-filter DSL: valid filter narrows a segment; unknown field 422."""
    slug = pflow["slug"]
    base = {"consent_purpose": "marketing"}
    filtered = {**base, "score_filters": [
        {"field": "propensity_churn", "op": ">=", "value": 0.5}]}
    r_all = _segment_count(slug, base)
    r_f = _segment_count(slug, filtered)
    assert r_all.status_code == 200 and r_f.status_code == 200, (
        f"count failed: {r_all.status_code} {r_all.text} / "
        f"{r_f.status_code} {r_f.text}")
    assert 0 <= r_f.json()["count"] <= r_all.json()["count"]
    bad = {"score_filters": [{"field": "password", "op": ">=", "value": 0}]}
    r_bad = _segment_count(slug, bad)
    assert r_bad.status_code == 422


# --- G1/G3: tenant isolation + single write path ---------------------------

def test_05_scores_and_cross_tenant_writeback_rejected(pstack, pflow):
    """Tenant B's persons stay unscored by A's sweep; a write-back naming
    tenant A's person under tenant B is rejected (G1/G3)."""
    slug_a = pflow["slug"]
    slug_b = _slug()
    ids_b = _seed(slug_b, "small_tenant",
                  {"persons": 3, "bookings": 0, "offerings": 2})
    for pid in ids_b["person_ids"]:
        p = _person(slug_b, pid)
        assert p is not None and not p.get("model_version"), \
            f"tenant B person scored by tenant A's sweep: {p}"
    r = requests.post(f"{GRAPH}/v1/graph/internal/scores",
                      headers=_ih(slug_b),
                      json={"tenant_id": slug_b, "scores": [
                          {"tenant_id": slug_a,
                           "person_id": pflow["persons"][0],
                           "propensity_churn": 0.9}]},
                      timeout=20)
    assert r.status_code in (400, 403, 422), \
        f"cross-tenant write-back accepted: {r.status_code} {r.text}"


# --- G5/G6 ------------------------------------------------------------------

def test_06_gnn_backend_falls_back_without_torch(pstack, pflow):
    """Requesting the gnn backend without torch/PyG installed must degrade
    to heuristic — exit ok, never a crash (G5). With PyG installed but
    training unimplemented (W31), the per-tenant degrade also yields
    heuristic model_versions."""
    slug = pflow["slug"]
    r = requests.post(f"{GRAPH_ML}/v1/score/run",
                      json={"tenant_id": slug, "backend": "gnn"}, timeout=60)
    if r.status_code == 422:  # backend override not supported per-run
        pytest.skip("per-run backend override not exposed")
    assert r.status_code == 200
    people = [_person(slug, p) for p in pflow["persons"]]
    for p in people:
        if p and p.get("model_version"):
            assert str(p["model_version"]).startswith("heuristic")


def test_07_internal_routes_auth(pstack, pflow):
    """G6: internal routes reject wrong token and JWT; accept the configured
    X-Internal-Token (one valid score item is a writable payload; unknown
    persons are skipped, not stub-created)."""
    slug = pflow["slug"]
    url = f"{GRAPH}/v1/graph/internal/scores"
    payload = {"tenant_id": slug, "scores": [
        {"tenant_id": slug, "person_id": pflow["persons"][0],
         "propensity_churn": 0.42, "model_version": "e2e-probe",
         "scored_at": "2026-08-05T00:00:00Z"}]}
    r_wrong = requests.post(url, headers=_ih(slug, "wrong-token"),
                            json=payload, timeout=15)
    assert r_wrong.status_code == 401
    r_jwt = requests.post(url, headers={**_h(slug),
                                        "Authorization": "Bearer fake.jwt.here"},
                          json=payload, timeout=15)
    assert r_jwt.status_code == 401
    r_ok = requests.post(url, headers=_ih(slug), json=payload, timeout=15)
    assert r_ok.status_code == 200
    # Unknown persons must be skipped, never stub-created (gate WARN #4).
    ghost = "person-does-not-exist"
    r_ghost = requests.post(url, headers=_ih(slug),
                            json={"tenant_id": slug, "scores": [
                                {"tenant_id": slug, "person_id": ghost,
                                 "propensity_churn": 0.9}]},
                            timeout=15)
    assert r_ghost.status_code == 200
    assert _person(slug, ghost) is None, \
        "internal scores must not MERGE-create stub persons"


def test_08_similar_persons_tenant_scoped(pstack, pflow):
    """similar_persons excludes self and never returns other tenants'
    persons (G1 read path)."""
    slug = pflow["slug"]
    pid = pflow["persons"][0]
    rows = _rows(slug, "similar_persons", {"person_id": pid, "k": 10})
    assert pid not in {row.get("person_id") for row in rows}
    for row in rows:
        assert row.get("tenant_id", slug) == slug


def test_09_train_endpoint_honest_in_heuristic_mode(pstack):
    """W31: POST /v1/score/train must honestly refuse on the heuristic base
    image — 409, never a silent no-op or a 500 (SPEC-W31 G6). GNN training
    requires the `gnn` compose profile (Dockerfile.gnn); the live stack runs
    the heuristic base image, so 409 is the correct live assertion."""
    r = requests.post(f"{GRAPH_ML}/v1/score/train", json={}, timeout=60)
    assert r.status_code == 409
    assert "gnn" in r.json().get("error", "")
