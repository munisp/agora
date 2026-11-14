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

-- SPEC-W65 (G2): the consent registry's data_subject_id is the E.164
-- subject phone; kyc-service now forwards it at extract time so consent
-- ErasureRequested can match on a real column instead of the legacy
-- extracted_fields JSONB value match (kept as fallback for pre-column
-- rows). Additive nullable column; set ONLY for kyc_* kinds; NULLed by
-- mark_erased (the tombstone must not retain the PII locator).
ALTER TABLE ocr_documents ADD COLUMN IF NOT EXISTS subject_phone TEXT;

CREATE INDEX IF NOT EXISTS idx_ocr_documents_tenant_sha256
    ON ocr_documents (tenant_id, sha256);
CREATE INDEX IF NOT EXISTS idx_ocr_documents_tenant_created
    ON ocr_documents (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ocr_documents_expires
    ON ocr_documents (expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ocr_documents_kyc_subject
    ON ocr_documents (tenant_id, subject_phone) WHERE subject_phone IS NOT NULL;

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

-- SPEC-W67 (G1/G4): cross-tenant document-fraud registry. Platform-scoped
-- tables keyed by registry_key = HMAC-SHA256(OCR_REGISTRY_HMAC_SECRET,
-- sha256) — the raw sha256 NEVER enters the registry store (G3), and there
-- is NO document_id, storage_key, perceptual hash, kind, fields, or text
-- anywhere in these tables (G2 minimization; a perceptual hash is an
-- invertible-thumbnail disclosure cross-tenant). FORCE RLS both; the
-- single policy passes ONLY
-- via the app.ocr_registry GUC escape, which is set exclusively inside
-- app/registry.py's dedicated connection helper (sweep_conn idiom, kept
-- SEPARATE from app.ocr_internal_sweep for auditability) — request paths
-- can never read or write the registry. Membership rows are the minimum
-- needed for the distinct-tenant count and the at-most-one alert per
-- (tenant, hash); TenantDelete removes only the tenant's own memberships
-- and the parent row is dropped when the last membership leaves (G7).
CREATE TABLE IF NOT EXISTS ocr_doc_registry (
    registry_key TEXT PRIMARY KEY,
    first_seen   TIMESTAMPTZ NOT NULL,
    last_seen    TIMESTAMPTZ NOT NULL,
    tenant_count INT NOT NULL,
    doc_count    INT NOT NULL
);

CREATE TABLE IF NOT EXISTS ocr_doc_registry_tenants (
    registry_key TEXT NOT NULL REFERENCES ocr_doc_registry ON DELETE CASCADE,
    tenant_id    UUID NOT NULL,
    first_seen   TIMESTAMPTZ NOT NULL,
    alerted      BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (registry_key, tenant_id)
);

ALTER TABLE ocr_doc_registry ENABLE ROW LEVEL SECURITY;
ALTER TABLE ocr_doc_registry FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS registry_internal_access ON ocr_doc_registry;
CREATE POLICY registry_internal_access ON ocr_doc_registry
    USING (current_setting('app.ocr_registry', true) = 'on');

ALTER TABLE ocr_doc_registry_tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE ocr_doc_registry_tenants FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS registry_internal_access ON ocr_doc_registry_tenants;
CREATE POLICY registry_internal_access ON ocr_doc_registry_tenants
    USING (current_setting('app.ocr_registry', true) = 'on');
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
                 bytes, sha256, phash, page_count, status, expires_at,
                 subject_phone)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'uploaded', $11,
                    $12)
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
            # SPEC-W65 G2: set ONLY for kyc_* kinds (caller-enforced);
            # the erasure tombstone NULLs it again.
            doc.get("subject_phone"),
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
        """kyc_* rows matching the erasure subject (consent
        ErasureRequested, P3). SPEC-W65 G2 DUAL-MATCH:

          1. subject_phone column match — the consent registry's
             data_subject_id IS the E.164 phone (identity consent keys on
             (tenant, subject, purpose); the kyc gate checks
             subject_phone), and kyc-service now forwards it at extract
             time. This is the primary, indexed match
             (idx_ocr_documents_kyc_subject).
          2. LEGACY fallback, ONLY for pre-column rows
             (subject_phone IS NULL): exact top-level extracted_fields
             string value match (jsonb_each_text). Keeps rows ingested
             before the column existed erasable; the 72h TTL sweeper
             bounds that fallback window (no cross-DB backfill is
             possible — the authoritative binding lives in kyc-service).

        A row WITH a subject_phone is matched on the column alone — the
        JSONB value match must not shadow it.
        """
        rows = await conn.fetch(
            r"""
            SELECT id, storage_bucket, storage_key
              FROM ocr_documents
             WHERE kind LIKE 'kyc\_%' ESCAPE '\'
               AND status <> 'erased'
               AND (
                   subject_phone = $1
                   OR (
                       subject_phone IS NULL
                       AND extracted_fields IS NOT NULL
                       AND EXISTS (
                           SELECT 1
                             FROM jsonb_each_text(extracted_fields) AS f(val)
                            WHERE f.val = $1
                       )
                   )
               )
            """,
            subject,
        )
        return [dict(r) for r in rows]

    @staticmethod
    async def mark_erased(conn, document_id: uuid.UUID) -> None:
        """P3 erasure tombstone: blank PII, keep sha256/phash for audit.
        SPEC-W65 G2: subject_phone is NULLed too — the tombstone must not
        retain the very PII locator it erased."""
        await conn.execute(
            """
            UPDATE ocr_documents
               SET extracted_text = NULL, extracted_fields = NULL,
                   subject_phone = NULL,
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
        """Hard-delete ALL rows for the tenant (TenantDeleted, P3).
        SPEC-W67 G7: the tenant's registry MEMBERSHIPS are removed by the
        caller via the registry module (app/registry.py) — they live behind
        the app.ocr_registry GUC escape and can never be reached from this
        tenant-scoped connection."""
        result = await conn.execute("DELETE FROM ocr_documents")
        return int(result.split()[-1]) if result else 0

    # --- SPEC-W66 WS1-A: D9 backfill reconciler helpers ---------------------
    @staticmethod
    async def list_backfill_tenant_ids(
        conn, since: Any = None, until: Any = None
    ) -> list[str]:
        """DISTINCT tenant_ids holding status='extracted' rows in the
        [since, until] window. Call on sweep_conn() ONLY — cross-tenant
        enumeration rides the EXISTING app.ocr_internal_sweep GUC escape
        (retention-sweeper precedent); NO new GUC, and the per-tenant row
        reads themselves happen under tenant_conn() below.
        """
        rows = await conn.fetch(
            """
            SELECT DISTINCT tenant_id
              FROM ocr_documents
             WHERE status = 'extracted'
               AND ($1::timestamptz IS NULL OR created_at >= $1)
               AND ($2::timestamptz IS NULL OR created_at <= $2)
             ORDER BY tenant_id
            """,
            since,
            until,
        )
        return [str(r["tenant_id"]) for r in rows]

    @staticmethod
    async def list_documents_for_backfill(
        conn,
        since: Any = None,
        until: Any = None,
        cursor: tuple[Any, Any] | None = None,
        limit: int = 500,
    ) -> list[dict[str, Any]]:
        """One keyset page of status='extracted' rows for the D9 backfill
        reconciler (SPEC-W66 WS1-A). Call on tenant_conn() — every row read
        stays RLS-scoped to one tenant.

        Keyset (created_at, id) ASC; `cursor` is the (created_at, id) tuple
        of the last row of the previous page (None = first page) — stateless
        resumability. 'erased'/'failed'/'uploaded' rows are EXCLUDED
        (graph-sync's own status filter, syncer.go:648-653, is mirrored).
        extracted_fields is selected ONLY so the caller can apply the
        receipt {amount_minor, currency, receipt_date} whitelist before
        emission — raw fields never leave the service.
        """
        cursor_created, cursor_id = cursor if cursor else (None, None)
        rows = await conn.fetch(
            """
            SELECT id, tenant_id, kind, status, page_count, sha256, phash,
                   model_version, created_at, extracted_fields
              FROM ocr_documents
             WHERE status = 'extracted'
               AND ($1::timestamptz IS NULL OR created_at >= $1)
               AND ($2::timestamptz IS NULL OR created_at <= $2)
               AND (
                   $3::timestamptz IS NULL
                   OR (created_at, id) > ($3::timestamptz, $4::uuid)
               )
             ORDER BY created_at, id
             LIMIT $5
            """,
            since,
            until,
            cursor_created,
            cursor_id,
            limit,
        )
        return [dict(r) for r in rows]
