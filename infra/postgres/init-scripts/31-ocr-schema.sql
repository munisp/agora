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

-- SPEC-W58 (A2, W32 drop+re-add idiom — booking fieldcapture/store.go:94-99):
-- CREATE TABLE IF NOT EXISTS never updates an existing CHECK constraint, so
-- the kind enum (+5 kyc_* kinds) and the status enum (+'erased' for the
-- consent-erasure tombstone state) are rebuilt idempotently. Metadata-only
-- widenings — existing rows cannot violate them. Safe to re-run.
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

-- ---------------- Row Level Security (SPEC §7) ----------------
ALTER TABLE ocr_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE ocr_documents FORCE ROW LEVEL SECURITY;
-- DROP + CREATE (identity consents idiom): converges the policy on
-- deployments that already have the pre-W58 version. The
-- app.ocr_internal_sweep GUC escape exists ONLY for the retention sweeper
-- (GATE R3), which must enumerate expired rows cross-tenant; it carries the
-- same trust level as app.tenant_id itself (the app role chooses the value)
-- and request paths never set it.
DROP POLICY IF EXISTS tenant_isolation ON ocr_documents;
CREATE POLICY tenant_isolation ON ocr_documents
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           OR current_setting('app.ocr_internal_sweep', true) = 'on');
