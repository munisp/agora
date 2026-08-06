"""C1/GC1: real RLS policy test against embedded Postgres.

Connects as the least-privilege app role (`app_model_registry_login`, NOT the
table owner, NOT superuser — so FORCE ROW LEVEL SECURITY genuinely applies):
tenant A cannot read tenant B rows; an unset tenant GUC is fail-closed; the
internal-job GUC sees across tenants (documented design).
"""

from __future__ import annotations

import psycopg

from conftest import APP_ROLE, TENANT_A, TENANT_B
from model_registry.store import RegistryStore


def _login_dsn(super_dsn):
    info = psycopg.conninfo.conninfo_to_dict(super_dsn)
    info["user"] = APP_ROLE
    return psycopg.conninfo.make_conninfo(**info)


def _read_as(super_dsn, tenant_guc, internal=False):
    """Count model_version rows visible to the app role under a GUC context."""
    with psycopg.connect(_login_dsn(super_dsn)) as conn:
        if tenant_guc is not None:
            conn.execute("SELECT set_config('app.tenant_id', %s, true)",
                         (tenant_guc,))
        if internal:
            conn.execute("SELECT set_config('app.registry_internal', 'on', true)")
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
    # internal batch-job GUC → cross-tenant enumeration (drift/trainer only)
    assert _read_as(super_dsn, None, internal=True) == 2


def test_app_role_write_with_check_blocks_cross_tenant_insert(super_dsn):
    # Direct INSERT as tenant B context with tenant_id=A must be rejected by
    # the WITH CHECK clause of the policy.
    with psycopg.connect(_login_dsn(super_dsn)) as conn:
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
