"""Postgres access (asyncpg) for the `ocr` database (SPEC-W53 Scope A).

RLS: every request path runs inside tenant_conn() — `SET LOCAL
app.tenant_id` inside a transaction so the tenant_isolation policy on
ocr_documents binds (ENABLE + FORCE ROW LEVEL SECURITY, NULLIF idiom —
mirrors infra/postgres/init-scripts/31-ocr-schema.sql). The runtime
ensure_* bootstrap below is idempotent and byte-equivalent to that init
script (the init script wins on fresh clusters; the ensure* path covers
standalone/existing deployments — geo/payments bootstrap idiom).
"""

from __future__ import annotations

import uuid
from contextlib import asynccontextmanager
from typing import Any

import asyncpg

from .logging import get_logger

log = get_logger(__name__)

DDL = """
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS ocr_documents (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('receipt','document','knowledge_scan','kyc_nin_slip','kyc_drivers_license','kyc_passport','kyc_voters_card','kyc_cac_document')),
    storage_bucket   TEXT NOT NULL,
    storage_key      TEXT NOT NULL,
    mime             TEXT NOT NULL CHECK (mime IN ('image/jpeg','image/png','image/webp','application/pdf')),
    bytes            INTEGER NOT NULL CHECK (bytes >= 0),
    sha256           TEXT NOT NULL,
    phash            TEXT,
    page_count       INTEGER NOT NULL DEFAULT 1,
    model_version    TEXT NOT NULL DEFAULT '',
    extracted_text   TEXT,
    extracted_fields JSONB,
    status           TEXT NOT NULL DEFAULT 'uploaded' CHECK (status IN ('uploaded','extracted','failed','erased')),
    error            TEXT,
    expires_at       TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- SPEC-W58 (A2, W32 drop+re-add idiom — fieldcapture/store.go:94-99):
-- CREATE TABLE IF NOT EXISTS never updates an existing CHECK constraint,
-- so the kind enum (+5 kyc_* kinds) and the status enum (+'erased' for the
-- consent-erasure tombstone state) are rebuilt idempotently. Both are
-- metadata-only widenings — existing rows cannot violate them.
ALTER TABLE ocr_documents DROP CONSTRAINT IF EXISTS ocr_documents_kind_check;
ALTER TABLE ocr_documents ADD CONSTRAINT ocr_documents_kind_check
    CHECK (kind IN ('receipt','document','knowledge_scan','kyc_nin_slip','kyc_drivers_license','kyc_passport','kyc_voters_card','kyc_cac_document'));
ALTER TABLE ocr_documents DROP CONSTRAINT IF EXISTS ocr_documents_status_check;
ALTER TABLE ocr_documents ADD CONSTRAINT ocr_documents_status_check
    CHECK (status IN ('uploaded','extracted','failed','erased'));

-- SPEC-W58 P4 (GATE R3): TTL expiry, set ONLY for kyc_* rows
-- (OCR_KYC_TTL_HOURS, default 72h). Additive nullable column.
ALTER TABLE ocr_documents ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_ocr_documents_tenant_sha256
    ON ocr_documents (tenant_id, sha256);
CREATE INDEX IF NOT EXISTS idx_ocr_documents_tenant_created
    ON ocr_documents (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ocr_documents_expires
    ON ocr_documents (expires_at) WHERE expires_at IS NOT NULL;

ALTER TABLE ocr_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE ocr_documents FORCE ROW LEVEL SECURITY;
-- DROP + CREATE (identity consents idiom): converges the policy on
-- deployments that already have the pre-W58 version. The
-- app.ocr_internal_sweep GUC escape exists ONLY for the retention sweeper
-- (GATE R3), which must enumerate expired rows cross-tenant; it carries
-- the same trust level as app.tenant_id itself (the app role chooses the
-- value), and request paths never set it.
DROP POLICY IF EXISTS tenant_isolation ON ocr_documents;
CREATE POLICY tenant_isolation ON ocr_documents
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR current_setting('app.ocr_internal_sweep', true) = 'on');
"""


class Database:
    def __init__(self, dsn: str):
        self._dsn = dsn
        self._pool: asyncpg.Pool | None = None

    async def connect(self) -> None:
        self._pool = await asyncpg.create_pool(self._dsn, min_size=1, max_size=8)
        log.info("postgres.pool_created")

    async def aclose(self) -> None:
        if self._pool:
            await self._pool.close()
            self._pool = None

    async def ping(self) -> bool:
        if not self._pool:
            return False
        try:
            async with self._pool.acquire() as conn:
                await conn.fetchval("SELECT 1")
            return True
        except Exception:
            return False

    async def ensure_ocr_documents_table(self) -> None:
        """Idempotent bootstrap DDL (ensure* idiom; see module docstring)."""
        assert self._pool is not None, "database not connected"
        async with self._pool.acquire() as conn:
            await conn.execute(DDL)
        log.info("ocr_documents.table_ensured")

    @asynccontextmanager
    async def tenant_conn(self, tenant_id: str):
        """Yield a connection with `app.tenant_id` SET LOCAL for RLS."""
        assert self._pool is not None, "database not connected"
        async with self._pool.acquire() as conn:
            async with conn.transaction():
                await conn.execute(
                    "SELECT set_config('app.tenant_id', $1, true)", tenant_id
                )
                yield conn

    @asynccontextmanager
    async def sweep_conn(self):
        """Yield a connection for the retention sweeper (SPEC-W58 P4).

        Sets the app.ocr_internal_sweep GUC escape in the tenant_isolation
        policy so expired rows can be enumerated cross-tenant. INTERNAL
        ONLY — request paths never use this.
        """
        assert self._pool is not None, "database not connected"
        async with self._pool.acquire() as conn:
            async with conn.transaction():
                await conn.execute(
                    "SELECT set_config('app.ocr_internal_sweep', 'on', true)"
                )
                yield conn

    # --- row helpers (caller supplies a tenant-scoped conn) -----------------
    @staticmethod
    async def insert_document(conn, doc: dict[str, Any]) -> None:
        await conn.execute(
            """
            INSERT INTO ocr_documents
                (id, tenant_id, kind, storage_bucket, storage_key, mime,
                 bytes, sha256, phash, page_count, status, expires_at)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'uploaded', $11)
            """,
            doc["id"],
            uuid.UUID(doc["tenant_id"]),
            doc["kind"],
            doc["storage_bucket"],
            doc["storage_key"],
            doc["mime"],
            doc["bytes"],
            doc["sha256"],
            doc.get("phash"),
            doc.get("page_count", 1),
            doc.get("expires_at"),  # SPEC-W58 P4: set ONLY for kyc_* kinds
        )

    @staticmethod
    async def mark_extracted(
        conn,
        document_id: uuid.UUID,
        *,
        page_count: int,
        model_version: str,
        extracted_text: str | None,
        extracted_fields: dict[str, Any],
    ) -> None:
        import json as _json

        await conn.execute(
            """
            UPDATE ocr_documents
               SET status = 'extracted', page_count = $2, model_version = $3,
                   extracted_text = $4, extracted_fields = $5::jsonb,
                   error = NULL, updated_at = now()
             WHERE id = $1
            """,
            document_id,
            page_count,
            model_version,
            extracted_text,
            _json.dumps(extracted_fields),
        )

    @staticmethod
    async def mark_failed(conn, document_id: uuid.UUID, *, error: str) -> None:
        await conn.execute(
            """
            UPDATE ocr_documents
               SET status = 'failed', error = $2, updated_at = now()
             WHERE id = $1
            """,
            document_id,
            error[:1000],
        )

    @staticmethod
    async def get_document(conn, document_id: uuid.UUID) -> dict[str, Any] | None:
        row = await conn.fetchrow(
            """
            SELECT id, tenant_id, kind, storage_bucket, storage_key, mime,
                   bytes, sha256, phash, page_count, model_version,
                   extracted_text, extracted_fields, status, error,
                   expires_at, created_at, updated_at
              FROM ocr_documents
             WHERE id = $1
            """,
            document_id,
        )
        return dict(row) if row else None

    # --- SPEC-W58 retention/erasure helpers (P3/P4) -------------------------
    @staticmethod
    async def list_expired(conn, *, limit: int = 500) -> list[dict[str, Any]]:
        """Expired rows (expires_at reached), any kind, cross-tenant — call
        on a sweep_conn(). 'erased' rows are EXCLUDED: their MinIO object is
        already deleted and the blanked row is the hash-only audit stub
        retained per P3.
        """
        rows = await conn.fetch(
            """
            SELECT id, tenant_id, storage_bucket, storage_key
              FROM ocr_documents
             WHERE expires_at IS NOT NULL
               AND expires_at <= now()
               AND status <> 'erased'
             ORDER BY expires_at
             LIMIT $1
            """,
            limit,
        )
        return [dict(r) for r in rows]

    @staticmethod
    async def delete_document(conn, document_id: uuid.UUID) -> None:
        """Hard-delete one row (TTL sweeper, TenantDeleted cascade)."""
        await conn.execute("DELETE FROM ocr_documents WHERE id = $1", document_id)

    # --- SPEC-W61 G2: per-document internauth erase ----------------------
    @staticmethod
    async def get_document_for_erase(conn, document_id: uuid.UUID) -> dict[str, Any] | None:
        """Cross-tenant row fetch for the internauth per-document erase
        (POST /v1/ocr/documents/{id}/erase). Call on sweep_conn() ONLY —
        the caller authenticates by machine token and addresses the
        document by uuid, not by tenant."""
        row = await conn.fetchrow(
            """
            SELECT id, tenant_id, storage_bucket, storage_key
              FROM ocr_documents
             WHERE id = $1
            """,
            document_id,
        )
        return dict(row) if row else None

    @staticmethod
    async def erase_document(conn, document_id: uuid.UUID) -> bool:
        """Hard-delete one row by id (per-document internauth erase).
        Returns True when a row was removed. Call on sweep_conn() only."""
        result = await conn.execute(
            "DELETE FROM ocr_documents WHERE id = $1", document_id
        )
        return result.split()[-1] == "1"

    @staticmethod
    async def list_kyc_rows_matching_subject(
        conn, subject: str
    ) -> list[dict[str, Any]]:
        """kyc_* rows whose extracted_fields carry the erasure subject as an
        exact top-level string value (consent ErasureRequested, P3).

        CAVEAT: ocr_documents has no subject column — the authoritative
        subject_phone <-> ocr_document_id binding lives in kyc-service
        (P8); this match covers subjects that ARE document field values
        (e.g. the NIN itself) and is a best-effort belt-and-braces sweep.
        """
        rows = await conn.fetch(
            r"""
            SELECT id, storage_bucket, storage_key
              FROM ocr_documents
             WHERE kind LIKE 'kyc\_%' ESCAPE '\'
               AND status <> 'erased'
               AND extracted_fields IS NOT NULL
               AND EXISTS (
                   SELECT 1 FROM jsonb_each_text(extracted_fields) AS f(val)
                    WHERE f.val = $1
               )
            """,
            subject,
        )
        return [dict(r) for r in rows]

    @staticmethod
    async def mark_erased(conn, document_id: uuid.UUID) -> None:
        """P3 erasure tombstone: blank PII, keep sha256/phash for audit."""
        await conn.execute(
            """
            UPDATE ocr_documents
               SET extracted_text = NULL, extracted_fields = NULL,
                   status = 'erased', error = NULL, updated_at = now()
             WHERE id = $1
            """,
            document_id,
        )

    @staticmethod
    async def list_tenant_documents(conn) -> list[dict[str, Any]]:
        """ALL rows for the tenant (TenantDeleted cascade, P3)."""
        rows = await conn.fetch(
            "SELECT id, storage_bucket, storage_key FROM ocr_documents"
        )
        return [dict(r) for r in rows]

    @staticmethod
    async def delete_tenant_documents(conn) -> int:
        """Hard-delete ALL rows for the tenant (TenantDeleted, P3)."""
        result = await conn.execute("DELETE FROM ocr_documents")
        return int(result.split()[-1]) if result else 0
