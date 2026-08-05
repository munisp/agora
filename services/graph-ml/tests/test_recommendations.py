"""Recommendation tests: co-occurrence math, top-K, already-booked exclusion,
reason strings (SPEC-W29 §3 WS-A)."""

from __future__ import annotations

import pytest

from graph_ml.heuristic import CooccurrenceModel, score_tenant, slug


def test_slug():
    assert slug("Cleaning") == "cleaning"
    assert slug("Deep Clean!") == "deep_clean"
    assert slug("") == "service"


def test_cooccurrence_conditional_math(tenant_graph):
    model = CooccurrenceModel(tenant_graph)
    # bookers: o1:2 (p1,p2), o2:2 (p1,p3), o3:1 (p2); cooc o1->o3: 1
    assert model.bookers == {"o1": 2, "o2": 2, "o3": 1}
    assert model.conditional("o1", "o3") == pytest.approx(0.5)
    assert model.conditional("o1", "o2") == pytest.approx(0.5)
    assert model.conditional("o3", "o2") == pytest.approx(0.0)
    assert model.conditional("nope", "o1") == 0.0


def test_already_booked_excluded(tenant_graph):
    model = CooccurrenceModel(tenant_graph)
    recs = model.recommend("p1", top_k=5)
    offered = {oid for oid, _, _ in recs}
    assert offered.isdisjoint({"o1", "o2"})  # p1 already booked o1, o2
    assert recs[0][0] == "o3"
    assert recs[0][1] == pytest.approx(0.5)


def test_top_k_respected(tenant_graph, now):
    scores, recs = score_tenant(tenant_graph, now=now, top_k=1)
    by_person: dict[str, list] = {}
    for r in recs:
        by_person.setdefault(r.person_id, []).append(r)
    assert all(len(rs) <= 1 for rs in by_person.values())

    _, recs_k5 = score_tenant(tenant_graph, now=now, top_k=5)
    by_person5: dict[str, list] = {}
    for r in recs_k5:
        by_person5.setdefault(r.person_id, []).append(r)
    # only 3 offerings exist; p1 booked 2 -> at most 1 recommendation
    assert len(by_person5["p1"]) == 1
    assert len(by_person5["p4"]) == 3  # cold-start popularity: o1, o2, o3


def test_ranks_sequential(tenant_graph, now):
    _, recs = score_tenant(tenant_graph, now=now, top_k=5)
    by_person: dict[str, list[int]] = {}
    for r in recs:
        by_person.setdefault(r.person_id, []).append(r.rank)
    for ranks in by_person.values():
        assert sorted(ranks) == list(range(1, len(ranks) + 1))


def test_reason_string_cooccurrence(tenant_graph, now):
    """SPEC §2 example shape: 'booked_cleaning_2x' (p1 booked Cleaning 2x)."""
    _, recs = score_tenant(tenant_graph, now=now, top_k=5)
    p1_rec = [r for r in recs if r.person_id == "p1"]
    assert len(p1_rec) == 1
    assert p1_rec[0].offering_id == "o3"
    assert p1_rec[0].reason == "booked_cleaning_2x"


def test_reason_string_cold_start(tenant_graph, now):
    """Never-booked persons get popular offerings with 'clients_like_them_booked'."""
    _, recs = score_tenant(tenant_graph, now=now, top_k=5)
    p4_recs = [r for r in recs if r.person_id == "p4"]
    assert p4_recs  # non-empty on cold start (SPEC §4 gate 4)
    assert all(r.reason == "clients_like_them_booked" for r in p4_recs)
    # popularity order: o1/o2 (2 bookers) before o3 (1 booker)
    assert [r.offering_id for r in p4_recs] == ["o1", "o2", "o3"]


def test_recommendation_payload_shape(tenant_graph, now):
    _, recs = score_tenant(tenant_graph, now=now, top_k=5)
    payload = recs[0].as_payload()
    assert set(payload) == {
        "tenant_id",
        "person_id",
        "offering_id",
        "score",
        "rank",
        "reason",
        "model_version",
        "scored_at",
    }
    assert payload["tenant_id"] == "t1"
    assert 0.0 <= payload["score"] <= 1.0
