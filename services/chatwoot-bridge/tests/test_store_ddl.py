"""DDL parity (SPEC-W118 §4): the runtime bootstrap block in app/store.py
is byte-identical to infra/postgres/init-scripts/32-chatwoot-bridge-schema.sql
and carries FORCE ROW LEVEL SECURITY + tenant_isolation policies on all
three tables. Offline (no DB needed)."""

from __future__ import annotations

import re
from pathlib import Path

from app.store import DDL

INIT_SQL = (
    Path(__file__).resolve().parents[3]
    / "infra/postgres/init-scripts/32-chatwoot-bridge-schema.sql"
).read_text()

TABLES = ("chatwoot_tenants", "chatwoot_links", "chatwoot_sent_messages")


def test_ddl_block_byte_identical_in_init_script():
    assert DDL.strip() in INIT_SQL, (
        "store.DDL drifted from 32-chatwoot-bridge-schema.sql — "
        "update BOTH copies (init script wins on fresh clusters; the "
        "service converges existing ones)"
    )


def test_all_three_tables_present_with_expected_keys():
    for table in TABLES:
        assert f"CREATE TABLE IF NOT EXISTS {table}" in DDL, table
    assert "PRIMARY KEY (tenant_id, conversation_id)" in DDL
    assert "UNIQUE (tenant_id, chatwoot_conversation_id)" in DDL
    assert "PRIMARY KEY (tenant_id, source_id)" in DDL
    assert "webhook_token    TEXT NOT NULL UNIQUE" in DDL


def test_force_rls_and_policy_on_every_table():
    for table in TABLES:
        assert f"ALTER TABLE {table} ENABLE ROW LEVEL SECURITY" in DDL
        assert f"ALTER TABLE {table} FORCE ROW LEVEL SECURITY" in DDL
        assert re.search(
            rf"CREATE POLICY tenant_isolation ON {table}\s+"
            rf"USING \(tenant_id = NULLIF\(current_setting\("
            rf"'app.tenant_id', true\), ''\)::uuid\)",
            DDL,
        ), table


def test_no_cross_tenant_escape_guc():
    """The bridge has no sweeper: unlike ocr_documents there must be NO
    internal-sweep escape hatch in any policy."""
    assert "internal_sweep" not in DDL
