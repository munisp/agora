"""SPEC-W34 GF1 / G1: adversarial re-probe of the killed GUC bypass.

The W34 attack: any session as `app_model_registry_login` could run
    set_config('app.registry_internal', 'on', false)   -- session-level!
and then read/UPDATE/DELETE EVERY tenant's rows, because the RLS policies
trusted a plain custom GUC that any role may set. GF1 rewired the policies to
`pg_has_role(current_user, 'app_model_registry_internal', 'USAGE')` — role membership
cannot be forged by a GUC. This file proves on REAL embedded Postgres that:

  1. the old attack now yields 0 rows / 0-row writes for the login role,
     even with the legacy GUC set session-wide (is_local = false);
  2. the batch role (member of app_model_registry_internal) still sees across
     tenants — internal batch jobs keep working;
  3. FORCE ROW LEVEL SECURITY is enabled on every tenant table and the app
     login role is NOT the table owner (owner-bypass is impossible);
  4. the legacy GUC no longer appears in any policy definition.
"""

from __future__ import annotations

import psycopg

from conftest import APP_ROLE, BATCH_ROLE, TENANT_A, TENANT_B
from model_registry.store import RegistryStore

TENANT_TABLES = ("model_version", "experiments", "experiment_outcomes",
                 "feature_observations", "score_observations")


def _dsn_as(super_dsn, role):
    info = psycopg.conninfo.conninfo_to_dict(super_dsn)
    info["user"] = role
    return psycopg.conninfo.make_conninfo(**info)


def _seed_two_tenants(app_dsn):
    store = RegistryStore(app_dsn)
    store.register_version(family="fraud-clf", tenant_id=TENANT_A,
                           artifact_uri="s3://x/v1", version=1)
    store.register_version(family="fraud-clf", tenant_id=TENANT_B,
                           artifact_uri="s3://x/v1", version=1)


def test_legacy_guc_bypass_is_dead_for_reads(app_dsn, super_dsn):
    """The exact W34 attack: session-level legacy GUC, then cross-tenant
    SELECT — must return 0 rows for the login role."""
    _seed_two_tenants(app_dsn)
    with psycopg.connect(_dsn_as(super_dsn, APP_ROLE),
                         autocommit=True) as conn:
        conn.execute(
            "SELECT set_config('app.registry_internal', 'on', false)")
        # sanity: the GUC really is set on this session
        row = conn.execute(
            "SELECT current_setting('app.registry_internal', true)").fetchone()
        assert row[0] == "on"
        for table in TENANT_TABLES:
            row = conn.execute(f"SELECT count(*) FROM {table}").fetchone()
            assert row[0] == 0, (f"{table}: login role saw rows with the "
                                 "legacy GUC set — GF1 bypass still live")


def test_legacy_guc_bypass_is_dead_for_writes(app_dsn, super_dsn):
    """Session-level legacy GUC, then cross-tenant UPDATE/DELETE — both must
    affect 0 rows (RLS filters them out, no error needed)."""
    _seed_two_tenants(app_dsn)
    with psycopg.connect(_dsn_as(super_dsn, APP_ROLE),
                         autocommit=True) as conn:
        conn.execute(
            "SELECT set_config('app.registry_internal', 'on', false)")
        cur = conn.execute("UPDATE model_version SET stage = 'archived'")
        assert cur.rowcount == 0
        cur = conn.execute("DELETE FROM model_version")
        assert cur.rowcount == 0
    # and the rows are genuinely untouched (checked as batch role)
    with psycopg.connect(_dsn_as(super_dsn, BATCH_ROLE)) as conn:
        row = conn.execute(
            "SELECT count(*) FROM model_version WHERE stage = 'staging'"
        ).fetchone()
        assert row[0] == 2


def test_login_role_cannot_escalate_to_internal_role(super_dsn):
    """SET ROLE / membership is not self-grantable by the login role."""
    with psycopg.connect(_dsn_as(super_dsn, APP_ROLE),
                         autocommit=True) as conn:
        row = conn.execute(
            "SELECT pg_has_role(current_user,"
            " 'app_model_registry_internal', 'USAGE')").fetchone()
        assert row[0] is False
        try:
            conn.execute("SET ROLE app_model_registry_batch")
            escalated = True
        except psycopg.errors.InsufficientPrivilege:
            escalated = False
        assert not escalated, "login role could SET ROLE to the batch role"


def test_batch_role_cross_tenant_read_still_works(app_dsn, super_dsn):
    """Internal batch jobs (drift sweep, nightly trainer) keep their
    cross-tenant read via role membership, no GUC involved."""
    _seed_two_tenants(app_dsn)
    with psycopg.connect(_dsn_as(super_dsn, BATCH_ROLE),
                         autocommit=True) as conn:
        row = conn.execute(
            "SELECT count(*) FROM model_version").fetchone()
        assert row[0] == 2
        # tenant GUC scoping still applies to the batch role when set
        conn.execute("SELECT set_config('app.tenant_id', %s, false)",
                     (TENANT_A,))
        row = conn.execute(
            "SELECT count(*) FROM model_version").fetchone()
        assert row[0] == 2  # internal membership ORs with tenant match


def test_force_rls_and_ownership_hold(super_dsn):
    """FORCE ROW LEVEL SECURITY is on for every tenant table and the app
    login role is not the table owner (owner bypass impossible)."""
    with psycopg.connect(super_dsn, autocommit=True) as conn:
        for table in TENANT_TABLES:
            row = conn.execute(
                "SELECT c.relforcerowsecurity, c.relrowsecurity,"
                "       pg_get_userbyid(c.relowner)"
                " FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace"
                " WHERE n.nspname = 'public' AND c.relname = %s",
                (table,)).fetchone()
            assert row is not None, f"{table} missing"
            force, enabled, owner = row
            assert enabled and force, f"{table}: RLS/FORCE not both on"
            assert owner != APP_ROLE, f"{table} owned by the app role"


def test_no_policy_references_legacy_guc(super_dsn):
    """Belt-and-braces: pg_policies must not mention app.registry_internal."""
    with psycopg.connect(super_dsn, autocommit=True) as conn:
        rows = conn.execute(
            "SELECT tablename, policyname, qual, with_check FROM pg_policies"
            " WHERE schemaname = 'public'").fetchall()
        assert rows, "no RLS policies found"
        for tablename, policyname, qual, with_check in rows:
            for expr in (qual or "", with_check or ""):
                # the legacy *GUC* (app.registry_internal) must be gone; the
                # role name app_model_registry_internal is expected to appear
                assert "app.registry_internal" not in expr, (
                    f"{tablename}/{policyname} still references the legacy GUC")
