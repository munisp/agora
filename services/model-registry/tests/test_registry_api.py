"""C1: registry REST round-trip register→promote→rollback against REAL
embedded Postgres; provenance fields; honest 404s."""

from __future__ import annotations

import psycopg

from conftest import TENANT_A


def _production_count(super_dsn, family, tenant):
    with psycopg.connect(super_dsn) as conn:
        row = conn.execute(
            "SELECT count(*) FROM model_version WHERE family=%s AND tenant_id=%s"
            " AND stage='production'", (family, tenant)).fetchone()
        return row[0]


def test_register_promote_rollback_roundtrip(client, super_dsn):
    fam = "fraud-clf"
    # register two versions (auto-assigned 1, 2), with full provenance (I2)
    r1 = client.post("/v1/registry/register", json={
        "family": fam, "tenant_id": TENANT_A,
        "artifact_uri": "s3://lake/models/fraud-clf/v1",
        "metrics": {"auc_pr": 0.91, "brier": 0.12, "data_basis": "synthetic"},
        "seed": 42, "dataset_hash": "sha256:aaa", "git_sha": "abc123"})
    assert r1.status_code == 201, r1.text
    assert r1.json()["version"] == 1
    assert r1.json()["stage"] == "staging"

    r2 = client.post("/v1/registry/register", json={
        "family": fam, "tenant_id": TENANT_A,
        "artifact_uri": "s3://lake/models/fraud-clf/v2",
        "metrics": {"auc_pr": 0.93, "brier": 0.10, "data_basis": "synthetic"},
        "seed": 43, "dataset_hash": "sha256:bbb", "git_sha": "def456"})
    assert r2.status_code == 201
    assert r2.json()["version"] == 2

    # promote v1 → production
    p1 = client.post("/v1/registry/promote", json={
        "family": fam, "tenant_id": TENANT_A, "version": 1})
    assert p1.status_code == 200, p1.text
    assert p1.json()["stage"] == "production"
    assert _production_count(super_dsn, fam, TENANT_A) == 1

    prod = client.get(f"/v1/registry/{fam}/{TENANT_A}/production")
    assert prod.status_code == 200
    assert prod.json()["version"] == 1
    # provenance round-trips (I2)
    assert prod.json()["seed"] == 42
    assert prod.json()["dataset_hash"] == "sha256:aaa"
    assert prod.json()["git_sha"] == "abc123"

    # promote v2 → flips atomically: v1 archived, v2 production, still ONE
    p2 = client.post("/v1/registry/promote", json={
        "family": fam, "tenant_id": TENANT_A, "version": 2})
    assert p2.status_code == 200
    assert _production_count(super_dsn, fam, TENANT_A) == 1
    prod = client.get(f"/v1/registry/{fam}/{TENANT_A}/production")
    assert prod.json()["version"] == 2

    # rollback → most recent archived (v1) re-promoted
    rb = client.post("/v1/registry/rollback", json={
        "family": fam, "tenant_id": TENANT_A})
    assert rb.status_code == 200, rb.text
    assert rb.json()["version"] == 1
    assert _production_count(super_dsn, fam, TENANT_A) == 1
    prod = client.get(f"/v1/registry/{fam}/{TENANT_A}/production")
    assert prod.json()["version"] == 1

    versions = client.get(f"/v1/registry/{fam}/{TENANT_A}/versions").json()
    stages = {v["version"]: v["stage"] for v in versions["versions"]}
    assert stages == {1: "production", 2: "archived"}


def test_promote_unknown_version_404(client):
    client.post("/v1/registry/register", json={
        "family": "credit-clf", "tenant_id": TENANT_A,
        "artifact_uri": "s3://x/v1"})
    r = client.post("/v1/registry/promote", json={
        "family": "credit-clf", "tenant_id": TENANT_A, "version": 99})
    assert r.status_code == 404


def test_rollback_without_archived_404(client):
    client.post("/v1/registry/register", json={
        "family": "graphsage", "tenant_id": TENANT_A,
        "artifact_uri": "s3://x/v1"})
    r = client.post("/v1/registry/rollback", json={
        "family": "graphsage", "tenant_id": TENANT_A})
    assert r.status_code == 404


def test_tenant_version_counters_are_independent(client):
    from conftest import TENANT_B
    for tenant in (TENANT_A, TENANT_B):
        r = client.post("/v1/registry/register", json={
            "family": "fraud-clf", "tenant_id": tenant,
            "artifact_uri": "s3://x/v1"})
        assert r.json()["version"] == 1
