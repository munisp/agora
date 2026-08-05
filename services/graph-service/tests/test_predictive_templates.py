"""SPEC-W29 §3 WS-B: the four predictive read-only templates.

All executed through the allowlisted /v1/graph/cypher seam against the
seeded in-memory graph (conftest) plus predictive-layer props/edges added
per test. Tenant isolation is asserted for every template.
"""

from __future__ import annotations

import pytest
from fastapi.testclient import TestClient

from app.backend import InMemoryBackend
from app.config import Settings
from app.main import create_app
from app.store import SegmentStore
from conftest import HDR_A, HDR_B, StubLLM, build_graph


@pytest.fixture()
def predictive_client(tmp_path):
    """Graph seeded with scores, RECOMMENDED_FOR edges, and embeddings."""
    g = build_graph()
    a, b = "tenant-a", "tenant-b"

    # Predictive scores (tenant A): pa1 high churn, pa5 low churn,
    # pa4 high churn BUT quarantined; pb1 high churn in tenant B.
    g.nodes[f"{a}:pa1"].props.update(
        propensity_churn=0.9, scored_at="2026-08-05T00:00:00+00:00",
        model_version="heuristic-v1",
    )
    g.nodes[f"{a}:pa5"].props.update(propensity_churn=0.3)
    g.nodes[f"{a}:pa4"].props.update(propensity_churn=0.95)  # quarantined
    g.nodes[f"{b}:pb1"].props.update(propensity_churn=0.99)

    # RECOMMENDED_FOR edges for pa1 -> o2 (rank 1), o3 (rank 2).
    o2 = g.add_node(f"{a}:o2", {"Offering"}, offering_id="o2", tenant_id=a, name="Pedicure")
    o3 = g.add_node(f"{a}:o3", {"Offering"}, offering_id="o3", tenant_id=a, name="Massage")
    g.add_edge(f"{a}:pa1", o2.node_id, "RECOMMENDED_FOR",
               score=0.91, rank=1, reason="booked_cleaning_2x",
               model_version="heuristic-v1", scored_at="2026-08-05T00:00:00+00:00")
    g.add_edge(f"{a}:pa1", o3.node_id, "RECOMMENDED_FOR",
               score=0.7, rank=2, reason="clients_like_them_booked",
               model_version="heuristic-v1", scored_at="2026-08-05T00:00:00+00:00")

    # pa2 has NO recommendation edges but shares Alimosho + offering o1 with
    # pa1 -> co-occurrence fallback should propose o2/o3 if booked by peers.
    # Give pa6 (Alimosho? no — pa6 has no contact) a booking on o2 via pa5's
    # peer set: pa5 shares offering o1 with pa1 and booked o2.
    b7 = g.add_node(f"{a}:b7", {"Booking"}, booking_id="b7", tenant_id=a,
                    status="completed", created_at="2026-07-01T00:00:00+00:00")
    g.add_edge(f"{a}:pa5", b7.node_id, "BOOKED", at="2026-07-01T00:00:00+00:00")
    g.add_edge(b7.node_id, o2.node_id, "FOR")

    # Embeddings for similar_persons (name_embedding, written by graph-sync).
    g.nodes[f"{a}:pa1"].props["name_embedding"] = [1.0, 0.0, 0.0]
    g.nodes[f"{a}:pa2"].props["name_embedding"] = [0.9, 0.1, 0.0]   # near pa1
    g.nodes[f"{a}:pa3"].props["name_embedding"] = [0.0, 1.0, 0.0]   # orthogonal
    g.nodes[f"{b}:pb1"].props["name_embedding"] = [1.0, 0.0, 0.0]   # identical, WRONG TENANT

    settings = Settings(
        graph_backend="memory",
        segment_store_dir=str(tmp_path / "store"),
        jwt_public_key="",
        internal_token="tok",
    )
    app = create_app(
        settings,
        backend=InMemoryBackend(g),
        llm=StubLLM(),
        store=SegmentStore(str(tmp_path / "seg")),
    )
    return TestClient(app)


def _query(client, template, params=None, headers=HDR_A):
    resp = client.post(
        "/v1/graph/cypher",
        json={"template": template, "params": params or {}},
        headers=headers,
    )
    assert resp.status_code == 200, resp.text
    return resp.json()["rows"]


# ------------------------------------------------------- next_best_services
def test_next_best_services_edges_ordered_by_rank(predictive_client):
    rows = _query(predictive_client, "next_best_services", {"person_id": "pa1"})
    assert [r["offering_id"] for r in rows] == ["o2", "o3"]
    assert rows[0]["rank"] == 1 and rows[0]["reason"] == "booked_cleaning_2x"
    assert all(r["source"] == "edges" for r in rows)


def test_next_best_services_cooccurrence_fallback(predictive_client):
    """pa2 has no RECOMMENDED_FOR edges; fallback proposes offerings booked
    by similar (same-location / overlapping-offering) same-tenant persons."""
    rows = _query(predictive_client, "next_best_services", {"person_id": "pa2"})
    assert rows, "expected co-occurrence fallback rows"
    assert all(r["source"] == "cooccurrence" for r in rows)
    assert all(r["reason"] == "clients_like_them_booked" for r in rows)
    ids = {r["offering_id"] for r in rows}
    assert "o1" not in ids  # pa2 already booked o1 — excluded
    assert "o2" in ids      # booked by peers pa1/pa5


def test_next_best_services_tenant_scoped(predictive_client):
    rows_b = _query(predictive_client, "next_best_services", {"person_id": "pb1"}, HDR_B)
    assert all(r["offering_id"] not in {"o1", "o2", "o3"} for r in rows_b)
    # Cross-tenant person id resolves to nothing under the other tenant.
    rows = _query(predictive_client, "next_best_services", {"person_id": "pa1"}, HDR_B)
    assert rows == []


# ----------------------------------------------------------- churn_risk_band
def test_churn_risk_band_filters_and_excludes_quarantined(predictive_client):
    rows = _query(predictive_client, "churn_risk_band", {"min_score": 0.7})
    ids = {r["person_id"] for r in rows}
    assert ids == {"pa1"}  # pa5 below min; pa4 quarantined -> excluded
    row = rows[0]
    assert row["propensity_churn"] == 0.9
    assert row["model_version"] == "heuristic-v1"
    assert row["consent_purposes"] == ["marketing"]


def test_churn_risk_band_default_min_and_tenant_scope(predictive_client):
    rows_a = _query(predictive_client, "churn_risk_band")  # default 0.7
    assert {r["person_id"] for r in rows_a} == {"pa1"}
    rows_b = _query(predictive_client, "churn_risk_band", headers=HDR_B)
    assert {r["person_id"] for r in rows_b} == {"pb1"}  # never any tenant-A id


def test_churn_risk_band_invalid_min_score_400(predictive_client):
    resp = predictive_client.post(
        "/v1/graph/cypher",
        json={"template": "churn_risk_band", "params": {"min_score": 1.5}},
        headers=HDR_A,
    )
    assert resp.status_code == 400


# ----------------------------------------------------------- referral_value
def test_referral_value_single_person(predictive_client):
    rows = _query(predictive_client, "referral_value", {"person_id": "pa1"})
    assert len(rows) == 1
    # pa1 -> pa2 (REFERRED); pa2 has a booking -> converted.
    assert rows[0]["referral_out_degree"] == 1
    assert rows[0]["converted_referees"] == 1


def test_referral_value_leaderboard_when_person_omitted(predictive_client):
    rows = _query(predictive_client, "referral_value")
    assert len(rows) >= 7  # every tenant-A person listed
    assert rows[0]["person_id"] == "pa1"  # only referrer ranks first
    assert all(r["referral_out_degree"] == 0 for r in rows[1:])


def test_referral_value_tenant_scoped(predictive_client):
    rows_b = _query(predictive_client, "referral_value", headers=HDR_B)
    assert {r["person_id"] for r in rows_b} == {"pb1"}
    assert rows_b[0]["referral_out_degree"] == 0


# ----------------------------------------------------------- similar_persons
def test_similar_persons_ranked_and_excludes_self(predictive_client):
    rows = _query(predictive_client, "similar_persons", {"person_id": "pa1"})
    ids = [r["person_id"] for r in rows]
    assert "pa1" not in ids  # self excluded
    assert ids[0] == "pa2"   # nearest embedding
    assert rows[0]["similarity"] > 0.99


def test_similar_persons_never_crosses_tenant(predictive_client):
    rows = _query(predictive_client, "similar_persons", {"person_id": "pa1"})
    assert "pb1" not in {r["person_id"] for r in rows}  # identical embedding, other tenant


def test_similar_persons_k_param(predictive_client):
    rows = _query(
        predictive_client, "similar_persons", {"person_id": "pa1", "k": 1}
    )
    assert len(rows) == 1 and rows[0]["person_id"] == "pa2"


def test_similar_persons_without_embedding_empty(predictive_client):
    rows = _query(predictive_client, "similar_persons", {"person_id": "pa6"})
    assert rows == []
