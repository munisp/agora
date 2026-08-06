"""C3/GC4: deterministic bucketing, 50/50 convergence, fail-closed
assignment, assignment consistency with the hash."""

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

from conftest import TENANT_A

_SVC_DIR = Path(__file__).resolve().parents[1]
from model_registry import ab


def test_bucket_deterministic_same_process():
    b1 = ab.bucket(TENANT_A, "person-1", "exp-1")
    b2 = ab.bucket(TENANT_A, "person-1", "exp-1")
    assert b1 == b2 and 0 <= b1 < 100


def test_bucket_deterministic_across_processes():
    """Same (tenant, person, experiment) → same arm in two SEPARATE process
    invocations (GC4)."""
    code = ("from model_registry import ab; "
            f"print(ab.assign_arm({{'id': 'exp-xyz', 'status': 'active', 'pct': 50}},"
            f" tenant_id='{TENANT_A}', person_id='person-42'))")
    env = dict(os.environ, PYTHONPATH=str(_SVC_DIR))
    arm1 = subprocess.run([sys.executable, "-c", code], env=env,
                          capture_output=True, text=True, check=True).stdout.strip()
    arm2 = subprocess.run([sys.executable, "-c", code], env=env,
                          capture_output=True, text=True, check=True).stdout.strip()
    assert arm1 == arm2
    assert arm1 in ("champion", "challenger")


def test_fifty_fifty_converges_over_10k():
    exp = {"id": "exp-conv", "status": "active", "pct": 50}
    challenger = sum(
        1 for i in range(10_000)
        if ab.assign_arm(exp, tenant_id=TENANT_A, person_id=f"person-{i}")
        == "challenger")
    frac = challenger / 10_000
    assert 0.45 <= frac <= 0.55, f"50/50 bucketing converged to {frac:.3f}"


def test_missing_experiment_fails_closed_to_champion(client, store):
    # no experiment row exists for this family/tenant → champion + prod version
    store.register_version(family="fraud-clf", tenant_id=TENANT_A,
                           artifact_uri="s3://x/v1", version=1)
    store.promote("fraud-clf", TENANT_A, 1)
    r = client.get("/v1/registry/experiments/assignment", params={
        "family": "fraud-clf", "tenant_id": TENANT_A, "person_id": "p-1"})
    assert r.status_code == 200
    body = r.json()
    assert body["arm"] == "champion"
    assert body["experiment_id"] is None
    assert body["version"] == 1  # current production fallback


def test_assignment_matches_hash(client, store):
    store.register_version(family="fraud-clf", tenant_id=TENANT_A,
                           artifact_uri="s3://x/v1", version=1)
    store.register_version(family="fraud-clf", tenant_id=TENANT_A,
                           artifact_uri="s3://x/v2", version=2)
    r = client.post("/v1/registry/experiments", json={
        "family": "fraud-clf", "tenant_id": TENANT_A,
        "champion_version": 1, "challenger_version": 2, "pct": 50})
    assert r.status_code == 201, r.text
    exp_id = r.json()["id"]

    for person in ["p-1", "p-2", "p-3", "p-99"]:
        resp = client.get("/v1/registry/experiments/assignment", params={
            "family": "fraud-clf", "tenant_id": TENANT_A, "person_id": person})
        body = resp.json()
        expected = ("challenger"
                    if ab.bucket(TENANT_A, person, exp_id) < 50 else "champion")
        assert body["arm"] == expected
        assert body["experiment_id"] == exp_id
        assert body["version"] == (2 if expected == "challenger" else 1)
        # same person → same arm on a second request
        again = client.get("/v1/registry/experiments/assignment", params={
            "family": "fraud-clf", "tenant_id": TENANT_A, "person_id": person})
        assert again.json()["arm"] == expected


def test_stopped_experiment_fails_closed(client, store):
    store.register_version(family="fraud-clf", tenant_id=TENANT_A,
                           artifact_uri="s3://x/v1", version=1)
    exp = store.create_experiment(family="fraud-clf", tenant_id=TENANT_A,
                                  champion_version=1, challenger_version=1,
                                  pct=100)
    store.stop_experiment(exp["id"], TENANT_A)
    r = client.get("/v1/registry/experiments/assignment", params={
        "family": "fraud-clf", "tenant_id": TENANT_A, "person_id": "p-1"})
    assert r.json()["arm"] == "champion"
