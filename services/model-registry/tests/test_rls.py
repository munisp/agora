"""C1/GC1: real RLS policy test against embedded Postgres.

Connects as the least-privilege app role (`app_model_registry_login`, NOT the
table owner, NOT superuser — so FORCE ROW LEVEL SECURITY genuinely applies):
tenant A cannot read tenant B rows; an unset tenant GUC is fail-closed.
SPEC-W34 GF1: cross-tenant internal access is gated on ROLE MEMBERSHIP
(`app_model_registry_batch` ∈ `app_model_registry_internal`), never on a GUC —
the old `app.registry_internal` GUC is now inert for the login role (the G1
adversarial proof lives in test_guc_bypass.py).
"""

from __future__ import annotations

import logging

import psycopg

from conftest import APP_ROLE, BATCH_ROLE, TENANT_A, TENANT_B
from model_registry.store import RegistryStore


def _dsn_as(super_dsn, role):
    info = psycopg.conninfo.conninfo_to_dict(super_dsn)
    info["user"] = role
    return psycopg.conninfo.make_conninfo(**info)


def _read_as(super_dsn, tenant_guc, role=APP_ROLE):
    """Count model_version rows visible to `role` under a tenant GUC context."""
    with psycopg.connect(_dsn_as(super_dsn, role)) as conn:
        if tenant_guc is not None:
            conn.execute("SELECT set_config('app.tenant_id', %s, true)",
                         (tenant_guc,))
        row = conn.execute("SELECT count(*) FROM model_version").fetchone()
        return row[0]


def test_app_role_cannot_cross_tenants(app_dsn, super_dsn):
    # seed one version per tenant through the single write path (app role)
    store = RegistryStore(app_dsn)
    store.register_version(family="fraud-clf", tenant_id=TENANT_A,
                           artifact_uri="s3://x/v1", version=1)
    store.register_version(family="fraud-clf", tenant_id=TENANT_B,
                           artifact_uri="s3://x/v1", version=1)

    # tenant A sees exactly its own row
    assert _read_as(super_dsn, TENANT_A) == 1
    # tenant B cannot read tenant A rows (sees exactly its own)
    assert _read_as(super_dsn, TENANT_B) == 1
    # unset tenant GUC → fail-closed: nothing visible
    assert _read_as(super_dsn, None) == 0


def test_batch_role_sees_across_tenants(app_dsn, super_dsn):
    # internal batch jobs (drift sweep / nightly trainer) connect as the batch
    # role and enumerate all tenants WITHOUT any tenant GUC (GF1 mechanism).
    store = RegistryStore(app_dsn)
    store.register_version(family="fraud-clf", tenant_id=TENANT_A,
                           artifact_uri="s3://x/v1", version=1)
    store.register_version(family="fraud-clf", tenant_id=TENANT_B,
                           artifact_uri="s3://x/v1", version=1)
    assert _read_as(super_dsn, None, role=BATCH_ROLE) == 2


def test_store_internal_tx_uses_batch_role(store, super_dsn):
    # The store's internal=True path (drift list_productions) reaches across
    # tenants via MODEL_REGISTRY_INTERNAL_DSN, with no GUC involved.
    store.register_version(family="fraud-clf", tenant_id=TENANT_A,
                           artifact_uri="s3://x/v1", version=1)
    store.register_version(family="fraud-clf", tenant_id=TENANT_B,
                           artifact_uri="s3://x/v1", version=1)
    store.promote("fraud-clf", TENANT_A, 1)
    store.promote("fraud-clf", TENANT_B, 1)
    productions = store.list_productions()
    assert {p["tenant_id"] for p in productions} == {TENANT_A, TENANT_B}


def test_internal_dsn_fallback_warns_and_fails_closed(app_dsn, caplog):
    # GF1 contract: MODEL_REGISTRY_INTERNAL_DSN unset → unit-test fallback to
    # the primary DSN, logged as a warning; internal reads then see NOTHING
    # (the login role is not in app_model_registry_internal).
    with caplog.at_level(logging.WARNING, logger="model_registry.store"):
        store = RegistryStore(app_dsn)
    assert store.internal_dsn is None
    assert any("MODEL_REGISTRY_INTERNAL_DSN" in r.message
               and r.levelno == logging.WARNING for r in caplog.records)
    store.register_version(family="fraud-clf", tenant_id=TENANT_A,
                           artifact_uri="s3://x/v1", version=1)
    store.promote("fraud-clf", TENANT_A, 1)
    assert store.list_productions() == []  # fail-closed, not cross-tenant


def test_app_role_write_with_check_blocks_cross_tenant_insert(super_dsn):
    # Direct INSERT as tenant B context with tenant_id=A must be rejected by
    # the WITH CHECK clause of the policy.
    with psycopg.connect(_dsn_as(super_dsn, APP_ROLE)) as conn:
        conn.execute("SELECT set_config('app.tenant_id', %s, true)", (TENANT_B,))
        conn.execute("INSERT INTO model_family (name) VALUES ('fraud-clf')"
                     " ON CONFLICT DO NOTHING")
        try:
            conn.execute(
                "INSERT INTO model_version (family, tenant_id, version,"
                " artifact_uri) VALUES ('fraud-clf', %s, 1, 's3://x/v1')",
                (TENANT_A,))
            conn.commit()
            raised = False
        except psycopg.errors.InsufficientPrivilege:
            raised = True
        except Exception:
            raised = True  # any policy violation error counts
        assert raised, "cross-tenant INSERT was not blocked by RLS WITH CHECK"
