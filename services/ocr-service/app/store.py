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
    kind             TEXT NOT NULL CHECK (kind IN ('receipt','document','knowledge_scan')),
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
    status           TEXT NOT NULL DEFAULT 'uploaded' CHECK (status IN ('uploaded','extracted','failed')),
    error            TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_ocr_documents_tenant_sha256
    ON ocr_documents (tenant_id, sha256);
CREATE INDEX IF NOT EXISTS idx_ocr_documents_tenant_created
    ON ocr_documents (tenant_id, created_at DESC);

ALTER TABLE ocr_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE ocr_documents FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE schemaname = 'public' AND tablename = 'ocr_documents'
          AND policyname = 'tenant_isolation'
    ) THEN
        CREATE POLICY tenant_isolation ON ocr_documents
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END
$$;
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

    # --- row helpers (caller supplies a tenant-scoped conn) -----------------
    @staticmethod
    async def insert_document(conn, doc: dict[str, Any]) -> None:
        await conn.execute(
            """
            INSERT INTO ocr_documents
                (id, tenant_id, kind, storage_bucket, storage_key, mime,
                 bytes, sha256, phash, page_count, status)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'uploaded')
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
        )

    @staticmethod
    async def mark_extracted(
        conn,
        document_id: uuid.UUID,
        *,
        page_count: int,
        model_version: str,
        extracted_text: str,
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
                   created_at, updated_at
              FROM ocr_documents
             WHERE id = $1
            """,
            document_id,
        )
        return dict(row) if row else None
