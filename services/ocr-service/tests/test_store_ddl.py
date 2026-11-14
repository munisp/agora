"""SPEC-W58 A2/A6: kind/status CHECK drop+re-add rebuild idiom present and
mirrored in BOTH DDL copies (app/store.py runtime bootstrap AND
infra/postgres/init-scripts/31-ocr-schema.sql), enum parity with
config.DOCUMENT_KINDS, additive expires_at, and sweep-escape policy.

Offline checks (no DB needed) + a DB-gated idempotency test that runs the
runtime bootstrap twice against a real Postgres when
OCR_TEST_DATABASE_URL is set (same gate as test_rls.py).
"""

from __future__ import annotations

import os
import re
from pathlib import Path

import pytest

from app.config import DOCUMENT_KINDS
from app.store import DDL

INIT_SQL = (
    Path(__file__).resolve().parents[3] / "infra/postgres/init-scripts/31-ocr-schema.sql"
).read_text()


def _kind_enum(sql: str) -> list[str]:
    m = re.search(r"kind IN \(([^)]*)\)", sql)
    assert m, "kind CHECK not found"
    return [t.strip().strip("'") for t in m.group(1).split(",")]


def test_kind_enum_parity_both_ddl_copies():
    for name, sql in (("store.DDL", DDL), ("31-ocr-schema.sql", INIT_SQL)):
        assert _kind_enum(sql) == list(DOCUMENT_KINDS), name
        # kyc_* kinds present
        for kind in (
            "kyc_nin_slip",
            "kyc_drivers_license",
            "kyc_passport",
            "kyc_voters_card",
            "kyc_cac_document",
        ):
            assert f"'{kind}'" in sql, name


def test_check_rebuild_idiom_both_copies():
    """W32 idiom: DROP CONSTRAINT IF EXISTS immediately followed by the
    re-ADD with the full enum (idempotent on re-run), for BOTH the kind and
    status enums, in BOTH DDL copies."""
    for name, sql in (("store.DDL", DDL), ("31-ocr-schema.sql", INIT_SQL)):
        assert (
            "ALTER TABLE ocr_documents DROP CONSTRAINT IF EXISTS "
            "ocr_documents_kind_check;" in sql
        ), name
        assert "ADD CONSTRAINT ocr_documents_kind_check" in sql, name
        assert (
            "ALTER TABLE ocr_documents DROP CONSTRAINT IF EXISTS "
            "ocr_documents_status_check;" in sql
        ), name
        assert "ADD CONSTRAINT ocr_documents_status_check" in sql, name
        # status enum gains the P3 erasure tombstone state
        assert "'erased'" in sql, name
        # additive TTL column, idempotent
        assert (
            "ALTER TABLE ocr_documents ADD COLUMN IF NOT EXISTS expires_at "
            "TIMESTAMPTZ;" in sql
        ), name


def test_subject_phone_column_parity_both_copies():
    """SPEC-W65 G2: additive nullable subject_phone + the partial kyc
    subject index, mirrored in BOTH DDL copies (store.py runtime bootstrap
    AND 31-ocr-schema.sql) — the change must land in the same commit."""
    for name, sql in (("store.DDL", DDL), ("31-ocr-schema.sql", INIT_SQL)):
        assert (
            "ALTER TABLE ocr_documents ADD COLUMN IF NOT EXISTS subject_phone "
            "TEXT;" in sql
        ), name
        assert "CREATE INDEX IF NOT EXISTS idx_ocr_documents_kyc_subject" in sql, name
        assert "(tenant_id, subject_phone)" in sql, name
        assert "WHERE subject_phone IS NOT NULL" in sql, name


def test_sweep_escape_policy_both_copies():
    for name, sql in (("store.DDL", DDL), ("31-ocr-schema.sql", INIT_SQL)):
        assert "DROP POLICY IF EXISTS tenant_isolation ON ocr_documents" in sql, name
        assert "app.ocr_internal_sweep" in sql, name
        assert "app.tenant_id" in sql, name


# ---------------------------------------------------------------------------
# DB-gated idempotency (skips cleanly without a test Postgres)
# ---------------------------------------------------------------------------
@pytest.mark.asyncio
@pytest.mark.skipif(
    not os.environ.get("OCR_TEST_DATABASE_URL"),
    reason="OCR_TEST_DATABASE_URL unset — no test Postgres available",
)
async def test_ddl_bootstrap_is_idempotent_and_accepts_kyc_kinds():
    import uuid
    from datetime import datetime, timedelta, timezone

    from app.store import Database

    db = Database(os.environ["OCR_TEST_DATABASE_URL"])
    try:
        await db.connect()
        await db.ensure_ocr_documents_table()
        await db.ensure_ocr_documents_table()  # second run must converge
        doc_id = uuid.uuid4()
        tenant = "11111111-2222-3333-4444-555555555555"
        async with db.tenant_conn(tenant) as conn:
            await db.insert_document(
                conn,
                {
                    "id": doc_id,
                    "tenant_id": tenant,
                    "kind": "kyc_nin_slip",
                    "storage_bucket": "documents",
                    "storage_key": f"{tenant}/{doc_id}.png",
                    "mime": "image/png",
                    "bytes": 10,
                    "sha256": "ab" * 32,
                    "phash": None,
                    "expires_at": datetime.now(timezone.utc) + timedelta(hours=72),
                    "subject_phone": "+2348012345678",
                },
            )
            row = await db.get_document(conn, doc_id)
            assert row is not None and row["expires_at"] is not None
            # SPEC-W65 G2: subject_phone column match is the primary
            # erasure path; mark_erased NULLs the locator (tombstone).
            rows = await db.list_kyc_rows_matching_subject(conn, "+2348012345678")
            assert [r["id"] for r in rows] == [doc_id]
            assert await db.list_kyc_rows_matching_subject(conn, "+2348099999999") == []
            await db.mark_erased(conn, doc_id)
            assert await db.list_kyc_rows_matching_subject(conn, "+2348012345678") == []
        async with db.sweep_conn() as conn:
            await db.delete_document(conn, doc_id)
    finally:
        await db.aclose()
