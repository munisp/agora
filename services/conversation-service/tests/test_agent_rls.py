"""SQL-001: agent/capture tables must be created with FORCE RLS + tenant policy.

Regression guard for ensure_agent_tables(): every tenant-owned table it
creates must carry FORCE ROW LEVEL SECURITY plus the tenant_isolation
policy, so a fresh database is secure from the first migration.
"""

from __future__ import annotations

import pytest

from app import db as db_module

pgserver = pytest.importorskip("pgserver")

TENANT_A = "11111111-1111-1111-1111-111111111111"
TENANT_B = "22222222-2222-2222-2222-222222222222"
APP_ROLE = "app_conversation"
SUBJECT_ROLE = "rls_subject"


def _run(db_url: str, fn) -> None:
    orig = db_module.DATABASE_URL
    db_module.DATABASE_URL = db_url
    try:
        fn()
    finally:
        db_module.DATABASE_URL = orig


@pytest.fixture()
def pg_url():
    srv = pgserver.get_server("/tmp/agora-pg-rls")
    yield srv.psql()


@pytest.fixture()
def migrated(pg_url):
    import psycopg

    _run(pg_url, db_module.ensure_schema)
    _run(pg_url, db_module.ensure_agent_tables)
    with psycopg.connect(pg_url, autocommit=True) as conn, conn.cursor() as cur:
        cur.execute(
            f"DO $$ BEGIN CREATE ROLE {SUBJECT_ROLE} NOLOGIN; "
            "EXCEPTION WHEN duplicate_object THEN NULL; END $$"
        )
        cur.execute(f"GRANT USAGE ON SCHEMA public TO {SUBJECT_ROLE}")
        cur.execute(
            f"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO {SUBJECT_ROLE}"
        )
    return pg_url


def _table_flags(pg_url: str, table: str) -> tuple[bool, bool, list[str]]:
    import psycopg

    with psycopg.connect(pg_url) as conn, conn.cursor() as cur:
        cur.execute(
            "SELECT relrowsecurity, relforcerowsecurity FROM pg_class c "
            "JOIN pg_namespace n ON n.oid = c.relnamespace "
            "WHERE n.nspname = 'public' AND c.relname = %s",
            (table,),
        )
        row = cur.fetchone()
        assert row is not None, f"table {table} missing"
        cur.execute(
            "SELECT polname FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid "
            "JOIN pg_namespace n ON n.oid = c.relnamespace "
            "WHERE n.nspname = 'public' AND c.relname = %s",
            (table,),
        )
        policies = [r[0] for r in cur.fetchall()]
    return bool(row[0]), bool(row[1]), policies


def _rows_as(pg_url: str, table: str, tenant: str | None) -> int:
    import psycopg

    with psycopg.connect(pg_url) as conn, conn.cursor() as cur:
        cur.execute(f"SET LOCAL ROLE {SUBJECT_ROLE}") if False else None
        cur.execute(f"SET ROLE {SUBJECT_ROLE}")
        if tenant is None:
            cur.execute("RESET app.tenant_id")
        else:
            cur.execute("SELECT set_config('app.tenant_id', %s, false)", (tenant,))
        cur.execute(f"SELECT count(*) FROM {table}")
        n = int(cur.fetchone()[0])
        cur.execute("RESET ROLE")
    return n


def test_agent_tables_have_force_rls_and_tenant_policy(migrated):
    for table in ("agents", "capture_schemas", "capture_records"):
        rls, force, policies = _table_flags(migrated, table)
        assert rls, f"{table}: row security disabled"
        assert force, f"{table}: FORCE row security missing"
        assert "tenant_isolation" in policies, f"{table}: tenant_isolation policy missing"


def test_tenant_slugs_table_is_not_rls(migrated):
    # tenant_slugs is a global lookup table (slug -> tenant uuid); it must stay
    # readable regardless of tenant GUC so slug resolution works pre-auth.
    rls, force, _ = _table_flags(migrated, "tenant_slugs")
    assert not rls and not force


def test_cross_tenant_rows_invisible(migrated):
    import psycopg

    with psycopg.connect(migrated, autocommit=True) as conn, conn.cursor() as cur:
        cur.execute(
            "INSERT INTO tenant_slugs (tenant_id, slug) VALUES (%s, 'rls-a'), (%s, 'rls-b') "
            "ON CONFLICT (slug) DO NOTHING",
            (TENANT_A, TENANT_B),
        )
        cur.execute(
            "INSERT INTO agents (id, tenant_id, name, role, goal) VALUES "
            "('ag-a', %s, 'A', 'r', 'g'), ('ag-b', %s, 'B', 'r', 'g') ON CONFLICT DO NOTHING",
            (TENANT_A, TENANT_B),
        )
    assert _rows_as(migrated, "agents", TENANT_A) == 1
    assert _rows_as(migrated, "agents", TENANT_B) == 1


def test_empty_string_tenant_guc_denies_without_error(migrated):
    import psycopg

    with psycopg.connect(migrated, autocommit=True) as conn, conn.cursor() as cur:
        cur.execute(
            "INSERT INTO agents (id, tenant_id, name, role, goal) VALUES "
            "('ag-empty', %s, 'E', 'r', 'g') ON CONFLICT DO NOTHING",
            (TENANT_A,),
        )
    for table in ("agents", "capture_schemas", "capture_records"):
        assert _rows_as(migrated, table, "") == 0
