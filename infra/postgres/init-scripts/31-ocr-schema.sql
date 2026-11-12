-- 31-ocr-schema.sql — ocr DB schema (SPEC-W53 Scope A) with tenant RLS.
-- Mirrors the runtime idempotent bootstrap in services/ocr-service/app/store.py
-- (ensure* idiom): the init script wins on fresh clusters; the service's
-- IF NOT EXISTS DDL converges existing/standalone deployments.
\c ocr

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- OCR document metadata. Uploaded BYTES live in the MinIO `documents`
-- bucket under tenant-prefixed keys; this table is metadata + extraction
-- results only (PII — see services/ocr-service/README.md).
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

-- ---------------- Row Level Security (SPEC §7) ----------------
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
