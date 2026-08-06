"""C1/GC1: single-production invariant — serial flip and CONCURRENT promotes
(partial unique index guarantees exactly one production under races)."""

from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor

import psycopg

from conftest import TENANT_A
from model_registry.store import Conflict, RegistryStore


def _stages(super_dsn, family, tenant):
    with psycopg.connect(super_dsn) as conn:
        rows = conn.execute(
            "SELECT version, stage FROM model_version"
            " WHERE family=%s AND tenant_id=%s ORDER BY version",
            (family, tenant)).fetchall()
        return {v: s for v, s in rows}


def test_serial_promotes_keep_single_production(store, super_dsn):
    fam = "fraud-clf"
    for i in range(3):
        store.register_version(family=fam, tenant_id=TENANT_A,
                               artifact_uri=f"s3://x/v{i+1}", version=i + 1)
    store.promote(fam, TENANT_A, 1)
    assert list(_stages(super_dsn, fam, TENANT_A).values()).count("production") == 1
    store.promote(fam, TENANT_A, 2)
    stages = _stages(super_dsn, fam, TENANT_A)
    assert stages == {1: "archived", 2: "production", 3: "staging"}
    store.promote(fam, TENANT_A, 3)
    stages = _stages(super_dsn, fam, TENANT_A)
    assert list(stages.values()).count("production") == 1
    assert stages[3] == "production"


def test_concurrent_promotes_flip_atomically(app_dsn, super_dsn):
    fam = "fraud-clf"
    setup = RegistryStore(app_dsn)
    for i in range(3):
        setup.register_version(family=fam, tenant_id=TENANT_A,
                               artifact_uri=f"s3://x/v{i+1}", version=i + 1)
    setup.promote(fam, TENANT_A, 1)

    outcomes = {}

    def _promote(v):
        # separate connection per thread (psycopg connections are not shared)
        try:
            outcomes[v] = RegistryStore(app_dsn).promote(fam, TENANT_A, v)
        except Conflict as exc:
            outcomes[v] = exc

    with ThreadPoolExecutor(max_workers=2) as pool:
        list(pool.map(_promote, [2, 3]))

    stages = _stages(super_dsn, fam, TENANT_A)
    assert list(stages.values()).count("production") == 1
    assert stages[1] == "archived"
    assert stages[2] == "production" or stages[3] == "production"
    # the loser either hit the unique-index Conflict or archived the winner
    # and got re-archived — either way the invariant holds and no 500-class
    # error escaped as an unhandled exception.
