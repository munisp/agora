"""SPEC-W34 GF10 / G3: registry input hardening — every input that used to
reach the database (or crash it) as a 500 now fails pydantic validation with
422: non-UUID tenant/experiment ids, int4-overflow version, NaN/Inf
predicted_score, oversized artifact_uri."""

from __future__ import annotations

import json
import uuid

from conftest import TENANT_A

INT4_MAX = 2**31 - 1


def _register_payload(**over):
    payload = {"family": "fraud-clf", "tenant_id": TENANT_A,
               "artifact_uri": "s3://lake/models/fraud-clf/v1"}
    payload.update(over)
    return payload


# ------------------------------------------------------------- UUID ids (422)
def test_register_non_uuid_tenant_422(client):
    r = client.post("/v1/registry/register",
                    json=_register_payload(tenant_id="not-a-uuid"))
    assert r.status_code == 422, r.text


def test_promote_non_uuid_tenant_422(client):
    r = client.post("/v1/registry/promote", json={
        "family": "fraud-clf", "tenant_id": "evil', DROP TABLE", "version": 1})
    assert r.status_code == 422, r.text


def test_rollback_non_uuid_tenant_422(client):
    r = client.post("/v1/registry/rollback", json={
        "family": "fraud-clf", "tenant_id": "nope"})
    assert r.status_code == 422, r.text


def test_experiment_non_uuid_tenant_422(client):
    r = client.post("/v1/registry/experiments", json={
        "family": "fraud-clf", "tenant_id": "nope",
        "champion_version": 1, "challenger_version": 2, "pct": 50})
    assert r.status_code == 422, r.text


def test_production_path_non_uuid_tenant_422(client):
    r = client.get("/v1/registry/fraud-clf/not-a-uuid/production")
    assert r.status_code == 422, r.text


def test_versions_path_non_uuid_tenant_422(client):
    r = client.get("/v1/registry/fraud-clf/not-a-uuid/versions")
    assert r.status_code == 422, r.text


def test_assignment_query_non_uuid_tenant_422(client):
    r = client.get("/v1/registry/experiments/assignment", params={
        "family": "fraud-clf", "tenant_id": "not-a-uuid", "person_id": "p1"})
    assert r.status_code == 422, r.text


def test_outcome_path_non_uuid_experiment_422(client):
    r = client.post("/v1/registry/experiments/not-a-uuid/outcomes", json={
        "tenant_id": TENANT_A, "person_id": "p1", "assigned_arm": "champion",
        "predicted_label": 1, "predicted_score": 0.5})
    assert r.status_code == 422, r.text


def test_report_path_non_uuid_experiment_422(client):
    r = client.get("/v1/registry/experiments/not-a-uuid/report")
    assert r.status_code == 422, r.text


# ------------------------------------------------------- version bounds (422)
def test_register_version_zero_422(client):
    r = client.post("/v1/registry/register",
                    json=_register_payload(version=0))
    assert r.status_code == 422, r.text


def test_register_version_negative_422(client):
    r = client.post("/v1/registry/register",
                    json=_register_payload(version=-3))
    assert r.status_code == 422, r.text


def test_register_version_int4_overflow_422(client):
    # previously: int4 overflow deep in Postgres → 500
    r = client.post("/v1/registry/register",
                    json=_register_payload(version=INT4_MAX + 1))
    assert r.status_code == 422, r.text


def test_promote_version_int4_overflow_422(client):
    r = client.post("/v1/registry/promote", json={
        "family": "fraud-clf", "tenant_id": TENANT_A,
        "version": INT4_MAX + 1})
    assert r.status_code == 422, r.text


def test_experiment_version_int4_overflow_422(client):
    r = client.post("/v1/registry/experiments", json={
        "family": "fraud-clf", "tenant_id": TENANT_A,
        "champion_version": INT4_MAX + 1, "challenger_version": 2, "pct": 50})
    assert r.status_code == 422, r.text


def test_register_version_int4_max_accepted(client):
    r = client.post("/v1/registry/register",
                    json=_register_payload(version=INT4_MAX))
    assert r.status_code == 201, r.text
    assert r.json()["version"] == INT4_MAX


# ------------------------------------------------------ predicted_score (422)
def _outcome_payload(**over):
    payload = {"tenant_id": TENANT_A, "person_id": "p1",
               "assigned_arm": "champion", "predicted_label": 1,
               "predicted_score": 0.5}
    payload.update(over)
    return payload


def _post_outcome(client, **over):
    # Raw content (not json=): stdlib json.dumps emits the NaN/Infinity
    # literals real-world clients send; httpx's strict json= refuses them.
    return client.post(
        f"/v1/registry/experiments/{uuid.uuid4()}/outcomes",
        content=json.dumps(_outcome_payload(**over)),
        headers={"content-type": "application/json"})


def test_outcome_nan_score_422(client):
    # httpx/json emits a bare NaN literal; pydantic allow_inf_nan=False → 422
    # (previously reached the DB CHECK / float8 path as a 500 or silent NaN).
    r = _post_outcome(client, predicted_score=float("nan"))
    assert r.status_code == 422, r.text


def test_outcome_inf_score_422(client):
    r = _post_outcome(client, predicted_score=float("inf"))
    assert r.status_code == 422, r.text


def test_outcome_negative_inf_score_422(client):
    r = _post_outcome(client, predicted_score=float("-inf"))
    assert r.status_code == 422, r.text


def test_outcome_out_of_range_score_422(client):
    r = _post_outcome(client, predicted_score=1.5)
    assert r.status_code == 422, r.text


# --------------------------------------------------------- artifact_uri (422)
def test_register_artifact_uri_over_cap_422(client):
    r = client.post("/v1/registry/register", json=_register_payload(
        artifact_uri="s3://x/" + "a" * 2048))
    assert r.status_code == 422, r.text


def test_register_artifact_uri_at_cap_accepted(client):
    uri = "s3://x/" + "a" * (2048 - len("s3://x/"))
    r = client.post("/v1/registry/register",
                    json=_register_payload(artifact_uri=uri))
    assert r.status_code == 201, r.text


# --------------------------------------------------------- leak-free failures
def test_validation_errors_do_not_leak_internals(client):
    r = client.post("/v1/registry/register",
                    json=_register_payload(tenant_id="not-a-uuid",
                                           version=INT4_MAX + 1))
    assert r.status_code == 422
    body = r.text.lower()
    for marker in ("traceback", "psycopg", "sql", "dsn", "password"):
        assert marker not in body
