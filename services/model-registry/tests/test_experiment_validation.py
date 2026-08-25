"""SPEC-W43 Y-04/Y-05: experiment creation validation (409s) and
server-side tenant derivation for outcomes (cross-tenant outcome
poisoning impossible — the FK bypasses RLS, so the tenant MUST come from
the experiment row).
"""

from __future__ import annotations

import uuid

import psycopg
import pytest

from conftest import TENANT_A, TENANT_B
from model_registry.store import Conflict, TenantMismatch

FAM = "fraud-clf"


def _register_two_versions(client, family=FAM, tenant=TENANT_A):
    for v in (1, 2):
        r = client.post("/v1/registry/register", json={
            "family": family, "tenant_id": tenant,
            "artifact_uri": f"s3://x/{family}/v{v}", "version": v})
        assert r.status_code == 201, r.text


def _create_experiment(client, family=FAM, tenant=TENANT_A):
    r = client.post("/v1/registry/experiments", json={
        "family": family, "tenant_id": tenant,
        "champion_version": 1, "challenger_version": 2, "pct": 50})
    assert r.status_code == 201, r.text
    return r.json()["id"]


def _outcome(tenant_id, **over):
    body = {
        "tenant_id": tenant_id,
        "person_id": "p-1",
        "assigned_arm": "champion",
        "predicted_label": 1,
        "predicted_score": 0.9,
        "true_label": 1,
    }
    body.update(over)
    return body


# ------------------------------------------------------------- Y-05 (409s)
def test_champion_equals_challenger_rejected_409(client):
    _register_two_versions(client)
    r = client.post("/v1/registry/experiments", json={
        "family": FAM, "tenant_id": TENANT_A,
        "champion_version": 1, "challenger_version": 1, "pct": 50})
    assert r.status_code == 409, r.text


def test_missing_versions_rejected_409(client):
    _register_two_versions(client)
    for champ, chall in ((1, 99), (77, 2), (88, 99)):
        r = client.post("/v1/registry/experiments", json={
            "family": FAM, "tenant_id": TENANT_A,
            "champion_version": champ, "challenger_version": chall, "pct": 50})
        assert r.status_code == 409, (champ, chall, r.text)


def test_versions_of_another_tenant_do_not_count(client):
    # versions exist only for TENANT_A; an experiment for TENANT_B must 409
    _register_two_versions(client, tenant=TENANT_A)
    r = client.post("/v1/registry/experiments", json={
        "family": FAM, "tenant_id": TENANT_B,
        "champion_version": 1, "challenger_version": 2, "pct": 50})
    assert r.status_code == 409, r.text


def test_store_level_validation_raises_conflict(store):
    with pytest.raises(Conflict):
        store.create_experiment(family=FAM, tenant_id=TENANT_A,
                                champion_version=1, challenger_version=1, pct=50)


# -------------------------------------------- Y-04 (server-side tenant)
def test_outcome_with_matching_tenant_accepted(client):
    _register_two_versions(client)
    exp_id = _create_experiment(client)
    r = client.post(f"/v1/registry/experiments/{exp_id}/outcomes",
                    json=_outcome(TENANT_A))
    assert r.status_code == 201, r.text
    assert r.json()["tenant_id"] == TENANT_A


def test_outcome_without_tenant_uses_experiment_tenant(client):
    _register_two_versions(client)
    exp_id = _create_experiment(client)
    body = _outcome(TENANT_A)
    del body["tenant_id"]  # omitted entirely
    r = client.post(f"/v1/registry/experiments/{exp_id}/outcomes", json=body)
    assert r.status_code == 201, r.text
    assert r.json()["tenant_id"] == TENANT_A  # derived server-side


def test_outcome_cross_tenant_poisoning_rejected_403(client, super_dsn):
    """The audit-proven hole: experiment belongs to TENANT_A, caller passes
    tenant_id=TENANT_B. FK bypasses RLS, so without server-side derivation
    this would insert a TENANT_B outcome row into TENANT_A's experiment."""
    _register_two_versions(client)
    exp_id = _create_experiment(client)
    r = client.post(f"/v1/registry/experiments/{exp_id}/outcomes",
                    json=_outcome(TENANT_B))
    assert r.status_code == 403, r.text
    # and NOTHING was written (no poisoning, no orphan row)
    with psycopg.connect(super_dsn) as conn:
        n = conn.execute(
            "SELECT count(*) FROM experiment_outcomes WHERE experiment_id=%s",
            (exp_id,)).fetchone()[0]
        assert n == 0


def test_outcome_unknown_experiment_404(client):
    r = client.post(f"/v1/registry/experiments/{uuid.uuid4()}/outcomes",
                    json=_outcome(TENANT_A))
    assert r.status_code == 404, r.text


def test_store_record_outcome_tenant_mismatch_raises(store):
    store.register_version(family=FAM, tenant_id=TENANT_A,
                           artifact_uri="s3://x/v1", version=1)
    store.register_version(family=FAM, tenant_id=TENANT_A,
                           artifact_uri="s3://x/v2", version=2)
    exp = store.create_experiment(family=FAM, tenant_id=TENANT_A,
                                  champion_version=1, challenger_version=2,
                                  pct=50)
    with pytest.raises(TenantMismatch):
        store.record_outcome(experiment_id=exp["id"], tenant_id=TENANT_B,
                             person_id="p", assigned_arm="champion",
                             predicted_label=1, predicted_score=0.5)
    # server-side derivation stores the experiment's tenant
    row = store.record_outcome(experiment_id=exp["id"], tenant_id=None,
                               person_id="p", assigned_arm="champion",
                               predicted_label=1, predicted_score=0.5)
    assert row["tenant_id"] == TENANT_A
