"""SPEC-W29 §3 WS-B: segment DSL numeric score filters.

Covers: compilation to bound $sfN params (never interpolation), between ->
two bounds, unknown field -> 422, invalid op/value shapes -> 422, end-to-end
segment counts over stored scores, and the schema introspection endpoint the
segment-builder UI consumes.
"""

from __future__ import annotations

import pytest
from fastapi.testclient import TestClient
from pydantic import ValidationError

from app.backend import InMemoryBackend
from app.config import Settings
from app.dsl import SegmentCreate
from app.main import create_app
from app.segment.compiler import compile_segment_query
from app.store import SegmentStore
from conftest import HDR_A, StubLLM, build_graph


def _segment(score_filters):
    return SegmentCreate(
        name="score seg",
        purpose="marketing",
        filter={"score_filters": score_filters},
    )


# ------------------------------------------------------------ compilation
def test_score_filter_gte_compiles_to_bound_param():
    compiled = compile_segment_query(_segment(
        [{"field": "propensity_churn", "op": ">=", "value": 0.7}]
    ), projection="count")
    assert "p.propensity_churn >= $sf0" in compiled.cypher
    assert compiled.params["sf0"] == 0.7
    assert "0.7" not in compiled.cypher  # value never interpolated


def test_score_filter_lte_compiles_to_bound_param():
    compiled = compile_segment_query(_segment(
        [{"field": "propensity_convert", "op": "<=", "value": 0.25}]
    ), projection="count")
    assert "p.propensity_convert <= $sf0" in compiled.cypher
    assert compiled.params["sf0"] == 0.25


def test_score_filter_between_compiles_two_bounds():
    compiled = compile_segment_query(_segment(
        [{"field": "risk_score", "op": "between", "value": [0.2, 0.8]}]
    ), projection="count")
    assert "p.risk_score >= $sf0 AND p.risk_score <= $sf1" in compiled.cypher
    assert compiled.params["sf0"] == 0.2
    assert compiled.params["sf1"] == 0.8


def test_multiple_score_filters_number_params_sequentially():
    compiled = compile_segment_query(_segment([
        {"field": "propensity_churn", "op": ">=", "value": 0.7},
        {"field": "propensity_turnout", "op": "between", "value": [0.1, 0.5]},
    ]), projection="count")
    assert "p.propensity_churn >= $sf0" in compiled.cypher
    assert "p.propensity_turnout >= $sf1 AND p.propensity_turnout <= $sf2" in compiled.cypher
    assert compiled.params["sf0"] == 0.7
    assert compiled.params["sf1"] == 0.1
    assert compiled.params["sf2"] == 0.5


def test_unknown_score_field_rejected_422():
    with pytest.raises(ValidationError):
        _segment([{"field": "p.drop_graph", "op": ">=", "value": 0.5}])


def test_unknown_score_op_rejected_422():
    with pytest.raises(ValidationError):
        _segment([{"field": "propensity_churn", "op": "=", "value": 0.5}])


def test_between_requires_two_element_list():
    with pytest.raises(ValidationError):
        _segment([{"field": "risk_score", "op": "between", "value": 0.5}])
    with pytest.raises(ValidationError):
        _segment([{"field": "risk_score", "op": "between", "value": [0.1]}])
    with pytest.raises(ValidationError):
        _segment([{"field": "risk_score", "op": "between", "value": [0.9, 0.1]}])


def test_scalar_ops_reject_list_value():
    with pytest.raises(ValidationError):
        _segment([{"field": "propensity_churn", "op": ">=", "value": [0.1, 0.2]}])


# ------------------------------------------------------------ end-to-end
@pytest.fixture()
def scored_client(tmp_path):
    g = build_graph()
    a = "tenant-a"
    g.nodes[f"{a}:pa1"].props["propensity_churn"] = 0.9
    g.nodes[f"{a}:pa5"].props["propensity_churn"] = 0.4
    g.nodes[f"{a}:pa6"].props["propensity_churn"] = 0.75
    # pa4 (quarantined) has a high score but must never be segment-eligible.
    g.nodes[f"{a}:pa4"].props["propensity_churn"] = 0.99
    settings = Settings(
        graph_backend="memory",
        segment_store_dir=str(tmp_path / "store"),
        jwt_public_key="",
    )
    app = create_app(
        settings,
        backend=InMemoryBackend(g),
        llm=StubLLM(),
        store=SegmentStore(str(tmp_path / "seg")),
    )
    return TestClient(app)


def test_segment_count_with_score_filter(scored_client):
    resp = scored_client.post(
        "/v1/graph/segments/count",
        json={
            "name": "high churn",
            "purpose": "marketing",
            "filter": {
                "score_filters": [
                    {"field": "propensity_churn", "op": ">=", "value": 0.7}
                ]
            },
        },
        headers=HDR_A,
    )
    assert resp.status_code == 200, resp.text
    # pa1 (0.9) + pa6 (0.75); pa5 (0.4) below; pa4 quarantined; unscored
    # persons never match. Consent gate still applies by construction.
    assert resp.json()["count"] == 2


def test_segment_score_filter_between_end_to_end(scored_client):
    resp = scored_client.post(
        "/v1/graph/segments/count",
        json={
            "name": "mid churn",
            "purpose": "marketing",
            "filter": {
                "score_filters": [
                    {"field": "propensity_churn", "op": "between", "value": [0.3, 0.8]}
                ]
            },
        },
        headers=HDR_A,
    )
    assert resp.status_code == 200
    assert resp.json()["count"] == 2  # pa5 (0.4) + pa6 (0.75)


def test_segment_unknown_score_field_is_422(scored_client):
    resp = scored_client.post(
        "/v1/graph/segments/count",
        json={
            "name": "bad",
            "purpose": "marketing",
            "filter": {
                "score_filters": [
                    {"field": "unknown_score", "op": ">=", "value": 0.5}
                ]
            },
        },
        headers=HDR_A,
    )
    assert resp.status_code == 422


def test_saved_segment_with_score_filter_compiles_and_persists(scored_client):
    resp = scored_client.post(
        "/v1/graph/segments",
        json={
            "name": "saved churn band",
            "purpose": "marketing",
            "filter": {
                "score_filters": [
                    {"field": "propensity_churn", "op": ">=", "value": 0.7}
                ]
            },
        },
        headers=HDR_A,
    )
    assert resp.status_code == 201, resp.text
    segment_id = resp.json()["id"]
    assert "$sf0" in resp.json()["compiled_cypher"]
    count = scored_client.get(
        f"/v1/graph/segments/{segment_id}/count", headers=HDR_A
    )
    assert count.status_code == 200
    assert count.json()["count"] == 2


# ---------------------------------------------------- schema introspection
def test_segments_schema_surfaces_score_fields(scored_client):
    resp = scored_client.get("/v1/graph/segments/schema", headers=HDR_A)
    assert resp.status_code == 200, resp.text
    body = resp.json()
    fields = {f["field"] for f in body["score_filter_fields"]}
    assert fields == {
        "propensity_churn",
        "propensity_convert",
        "propensity_turnout",
        "risk_score",
    }
    for entry in body["score_filter_fields"]:
        assert entry["ops"] == [">=", "<=", "between"]
    assert "score_filters" in body["score_filters"]["dsl_example"]


def test_segments_schema_requires_auth(scored_client):
    assert scored_client.get("/v1/graph/segments/schema").status_code == 401
