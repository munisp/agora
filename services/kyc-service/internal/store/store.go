// Package store provides Postgres persistence for the kyc DB (SPEC-W12 §5):
// the kyc_audit table (who/what/when/result for every resolution attempt).
//
// RLS: every tenant-scoped query runs inside a transaction that first sets
// `SET LOCAL app.tenant_id` (booking-service withTenant idiom) so the FORCE
// ROW LEVEL SECURITY policy enforces tenant isolation at the database layer.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Audit mirrors the kyc.kyc_audit table. Raw BVN/NIN values are NEVER
// stored — IDValueHash is a SHA-256 hex digest (NDPA data minimization,
// docs/kyc.md).
type Audit struct {
	AuditID      uuid.UUID `json:"audit_id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	Actor        string    `json:"actor"` // who: calling service / api client id
	SubjectPhone string    `json:"subject_phone"`
	IDType       string    `json:"id_type"` // bvn|nin|<kyc doc_type> (SPEC-W58 widened CHECK)
	IDValueHash  string    `json:"id_value_hash"`
	Status       string    `json:"status"` // verified|mismatch|pending|<document status>|<review decision>
	Reference    string    `json:"reference"`
	// ConsentID records the consent receipt id presented at capture time
	// (SPEC-W58 P6: consent_id/subject recorded in kyc_audit). "" for
	// legacy resolve rows.
	ConsentID string `json:"consent_id"`
	// Detail carries free-form action context (e.g. the review reason).
	// Never raw PII.
	Detail    string    `json:"detail"`
	LatencyMS int64     `json:"latency_ms"`
	CreatedAt time.Time `json:"created_at"`
}

// Document mirrors the kyc.kyc_documents table (SPEC-W58 B1, P8): the
// HASH-ONLY durable record of a consent-gated KYC document extraction.
// Raw extracted field values are NEVER stored here — FieldHashes holds
// per-field HMAC-SHA256 digests and IDValueHash the HMAC of the extracted
// document number; cleartext fields live only in the ocr DB under TTL.
type Document struct {
	DocumentID    uuid.UUID         `json:"document_id"`
	TenantID      uuid.UUID         `json:"tenant_id"`
	SubjectPhone  string            `json:"subject_phone"`
	DocType       string            `json:"doc_type"` // kyc_nin_slip|kyc_drivers_license|kyc_passport|kyc_voters_card|kyc_cac_document
	OCRDocumentID uuid.UUID         `json:"ocr_document_id"`
	SHA256        string            `json:"sha256"`
	PHash         string            `json:"phash"`
	IDValueHash   string            `json:"id_value_hash"` // "" when extraction found no document number
	FieldHashes   map[string]string `json:"field_hashes"`
	Status        string            `json:"status"`       // uploaded|extracted|failed
	ReviewState   string            `json:"review_state"` // pending_review|approved|rejected
	ReviewerActor string            `json:"reviewer_actor"`
	ReviewedAt    *time.Time        `json:"reviewed_at,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
}

// Repository is the persistence contract the HTTP handlers depend on.
// *Store is the Postgres implementation; tests substitute an in-memory fake.
type Repository interface {
	InsertAudit(ctx context.Context, a *Audit) error
	// IDHashOwner returns the subject_phone an (id_type, id_value_hash) pair
	// was FIRST resolved under for this tenant ("" when never resolved) —
	// the consent↔ID binding lookup (SPEC-W45 K22).
	IDHashOwner(ctx context.Context, tenantID uuid.UUID, idType, idValueHash string) (string, error)
	// InsertDocument commits the hash-only kyc_documents row (SPEC-W58 B1).
	InsertDocument(ctx context.Context, d *Document) error
	// DocumentIDHashOwner returns the subject_phone the
	// (doc_type, id_value_hash) pair was FIRST committed under for this
	// tenant ("" when unseen) — the document K22 binding lookup
	// (SPEC-W58 P8). Callers skip the lookup when idValueHash is "".
	DocumentIDHashOwner(ctx context.Context, tenantID uuid.UUID, docType, idValueHash string) (string, error)
	// GetDocument returns one kyc_documents row (nil, nil when absent).
	GetDocument(ctx context.Context, tenantID, documentID uuid.UUID) (*Document, error)
	// ReviewDocument performs the ONLY mutation kyc_documents allows:
	// review_state + reviewer_actor + reviewed_at (enforced at the
	// database layer by the kyc_documents_review_only trigger). Returns
	// the updated row, (nil, nil) when the document is absent.
	ReviewDocument(ctx context.Context, tenantID, documentID uuid.UUID, decision, actor string) (*Document, error)
	Ping(ctx context.Context) error
}

// Store wraps a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New connects to Postgres, verifies connectivity and bootstraps the
// kyc_audit table + RLS policy (idempotent; covers fresh and existing
// installs — booking-service ensureSitesTable role).
func New(ctx context.Context, databaseURL string) (*Store, error) {
	// SPEC-W46 PERF-08: budgeted pool (see pool.go); URL pool_* params win.
	pool, err := newPool(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.bootstrap(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("bootstrap kyc schema: %w", err)
	}
	return s, nil
}

// bootstrap creates the kyc_audit table with the tenant_isolation RLS
// policy (02-identity-schema.sql pattern).
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) bootstrap(ctx context.Context) error {
	const ddl = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE IF NOT EXISTS kyc_audit (
    audit_id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL,
    actor         TEXT NOT NULL DEFAULT '',
    subject_phone TEXT NOT NULL,
    id_type       TEXT NOT NULL CHECK (id_type IN ('bvn','nin','kyc_nin_slip','kyc_drivers_license','kyc_passport','kyc_voters_card','kyc_cac_document')),
    id_value_hash TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('verified','mismatch','pending','uploaded','extracted','failed','approved','rejected')),
    reference     TEXT NOT NULL,
    consent_id    TEXT NOT NULL DEFAULT '',
    detail        TEXT NOT NULL DEFAULT '',
    latency_ms    BIGINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- SPEC-W58 upgrade path for pre-W58 installs: CREATE TABLE IF NOT EXISTS
-- never alters an existing table, so the W12-era CHECKs (id_type bvn|nin,
-- status verified|mismatch|pending) are widened with the W32 drop+re-add
-- idiom, and consent_id/detail are added additively. All idempotent.
ALTER TABLE kyc_audit ADD COLUMN IF NOT EXISTS consent_id TEXT NOT NULL DEFAULT '';
ALTER TABLE kyc_audit ADD COLUMN IF NOT EXISTS detail TEXT NOT NULL DEFAULT '';
ALTER TABLE kyc_audit DROP CONSTRAINT IF EXISTS kyc_audit_id_type_check;
ALTER TABLE kyc_audit ADD CONSTRAINT kyc_audit_id_type_check
    CHECK (id_type IN ('bvn','nin','kyc_nin_slip','kyc_drivers_license','kyc_passport','kyc_voters_card','kyc_cac_document'));
ALTER TABLE kyc_audit DROP CONSTRAINT IF EXISTS kyc_audit_status_check;
ALTER TABLE kyc_audit ADD CONSTRAINT kyc_audit_status_check
    CHECK (status IN ('verified','mismatch','pending','uploaded','extracted','failed','approved','rejected'));
CREATE INDEX IF NOT EXISTS idx_kyc_audit_tenant ON kyc_audit (tenant_id, created_at DESC);
-- SPEC-W46 PERF-09: IDHashOwner (consent↔ID binding, SPEC-W45 K22) filters
-- (tenant_id, id_type, id_value_hash) on EVERY /v1/kyc/resolve; without a
-- matching index that scans + sorts the tenant's ever-growing audit
-- partition. The query orders created_at ASC — served by a backward scan
-- of the DESC index (the audit_id tiebreak only matters for same-instant
-- rows and is resolved inside the handful of matching rows).
CREATE INDEX IF NOT EXISTS idx_kyc_audit_idhash
    ON kyc_audit (tenant_id, id_type, id_value_hash, created_at DESC);
ALTER TABLE kyc_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_audit FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_policies
                   WHERE schemaname = current_schema()
                     AND tablename = 'kyc_audit'
                     AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON kyc_audit
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END
$$;
-- SPEC-W34 GF7: kyc_audit is an append-only forensic trail. Defense in
-- depth: (1) REVOKE UPDATE/DELETE from PUBLIC and the application role
-- (the service only ever INSERTs, so nothing legitimate loses rights);
-- (2) a BEFORE UPDATE OR DELETE trigger that raises — this also binds the
-- table owner / superuser paths that REVOKE cannot touch.
REVOKE UPDATE, DELETE ON kyc_audit FROM PUBLIC;
DO $$
BEGIN
    EXECUTE format('REVOKE UPDATE, DELETE ON kyc_audit FROM %I', current_user);
END
$$;
CREATE OR REPLACE FUNCTION kyc_audit_append_only() RETURNS trigger AS $fn$
BEGIN
    RAISE EXCEPTION 'kyc_audit is append-only: % is forbidden (SPEC-W34 GF7)', TG_OP;
END;
$fn$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS kyc_audit_append_only ON kyc_audit;
CREATE TRIGGER kyc_audit_append_only
    BEFORE UPDATE OR DELETE ON kyc_audit
    FOR EACH ROW EXECUTE FUNCTION kyc_audit_append_only();
-- ---------------------------------------------------------------------
-- SPEC-W58 B1: kyc_documents — the HASH-ONLY durable record (P8) for
-- consent-gated KYC document extraction. Raw document field values are
-- NEVER stored here: field_hashes holds per-field HMAC-SHA256 digests and
-- id_value_hash the HMAC of the extracted document number; cleartext
-- fields live only in the ocr DB under TTL (OCR_KYC_TTL_HOURS).
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS kyc_documents (
    document_id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL,
    subject_phone   TEXT NOT NULL,
    doc_type        TEXT NOT NULL CHECK (doc_type IN ('kyc_nin_slip','kyc_drivers_license','kyc_passport','kyc_voters_card','kyc_cac_document')),
    ocr_document_id UUID NOT NULL,
    sha256          TEXT NOT NULL,
    phash           TEXT NOT NULL DEFAULT '',
    id_value_hash   TEXT NOT NULL DEFAULT '',
    field_hashes    JSONB NOT NULL DEFAULT '{}'::jsonb,
    status          TEXT NOT NULL CHECK (status IN ('uploaded','extracted','failed')),
    review_state    TEXT NOT NULL DEFAULT 'pending_review'
                    CHECK (review_state IN ('pending_review','approved','rejected')),
    reviewer_actor  TEXT NOT NULL DEFAULT '',
    reviewed_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_kyc_documents_tenant ON kyc_documents (tenant_id, created_at DESC);
-- Document K22 binding lookup (P8): earliest (doc_type, id_value_hash)
-- commit per tenant. Served by a backward scan like idx_kyc_audit_idhash.
CREATE INDEX IF NOT EXISTS idx_kyc_documents_binding
    ON kyc_documents (tenant_id, doc_type, id_value_hash, created_at DESC);
ALTER TABLE kyc_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_documents FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_policies
                   WHERE schemaname = current_schema()
                     AND tablename = 'kyc_documents'
                     AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON kyc_documents
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END
$$;
-- SPEC-W58 B1 mutability rule: a committed row is immutable EXCEPT the
-- human-review columns review_state / reviewer_actor / reviewed_at (P7:
-- human review is the terminal state; OCR never auto-verifies). The
-- trigger binds every role including the table owner (kyc_audit
-- append-only precedent), and DELETE is forbidden outright — tenant
-- erasure wipes the ocr row + MinIO object, leaving the hash tombstone.
REVOKE DELETE ON kyc_documents FROM PUBLIC;
DO $$
BEGIN
    EXECUTE format('REVOKE DELETE ON kyc_documents FROM %I', current_user);
END
$$;
CREATE OR REPLACE FUNCTION kyc_documents_review_only() RETURNS trigger AS $fn$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'kyc_documents is never deleted (SPEC-W58 B1 hash-only durable record)';
    END IF;
    IF NEW.document_id     IS DISTINCT FROM OLD.document_id
       OR NEW.tenant_id       IS DISTINCT FROM OLD.tenant_id
       OR NEW.subject_phone   IS DISTINCT FROM OLD.subject_phone
       OR NEW.doc_type        IS DISTINCT FROM OLD.doc_type
       OR NEW.ocr_document_id IS DISTINCT FROM OLD.ocr_document_id
       OR NEW.sha256          IS DISTINCT FROM OLD.sha256
       OR NEW.phash           IS DISTINCT FROM OLD.phash
       OR NEW.id_value_hash   IS DISTINCT FROM OLD.id_value_hash
       OR NEW.field_hashes    IS DISTINCT FROM OLD.field_hashes
       OR NEW.status          IS DISTINCT FROM OLD.status
       OR NEW.created_at      IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'kyc_documents is mutable only on review_state/reviewer_actor/reviewed_at (SPEC-W58 B1)';
    END IF;
    RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS kyc_documents_review_only ON kyc_documents;
CREATE TRIGGER kyc_documents_review_only
    BEFORE UPDATE OR DELETE ON kyc_documents
    FOR EACH ROW EXECUTE FUNCTION kyc_documents_review_only();`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure kyc_audit table: %w", err)
	}
	return nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks database liveness (used by /healthz).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// withTenant runs fn inside a transaction with `SET LOCAL app.tenant_id`
// applied (set_config(..., true): parameter binding keeps this
// injection-safe), so the RLS policy scopes every statement of fn to the
// given tenant.
func (s *Store) withTenant(ctx context.Context, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// IDHashOwner implements the consent↔ID binding lookup (SPEC-W45 K22,
// OOS-17): the subject_phone of the EARLIEST audit row for
// (tenant, id_type, id_value_hash). "" + nil when the ID was never resolved
// for this tenant — the first resolution establishes the binding. Runs
// inside withTenant so RLS scopes the read.
func (s *Store) IDHashOwner(ctx context.Context, tenantID uuid.UUID, idType, idValueHash string) (string, error) {
	var owner string
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT subject_phone FROM kyc_audit
		                         WHERE tenant_id = $1 AND id_type = $2 AND id_value_hash = $3
		                         ORDER BY created_at ASC, audit_id ASC LIMIT 1`,
			tenantID, idType, idValueHash).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			owner = ""
			return nil
		}
		if err != nil {
			return fmt.Errorf("lookup id hash owner: %w", err)
		}
		return nil
	})
	return owner, err
}

// InsertAudit records one resolution attempt. Every /v1/kyc/resolve call
// (any outcome) must produce exactly one audit row — the request fails if
// the audit write fails (no audit, no resolution).
func (s *Store) InsertAudit(ctx context.Context, a *Audit) error {
	if a.AuditID == uuid.Nil {
		a.AuditID = uuid.New()
	}
	const q = `INSERT INTO kyc_audit
	           (audit_id, tenant_id, actor, subject_phone, id_type, id_value_hash, status, reference, consent_id, detail, latency_ms)
	           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING created_at`
	return s.withTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, q,
			a.AuditID, a.TenantID, a.Actor, a.SubjectPhone, a.IDType,
			a.IDValueHash, a.Status, a.Reference, a.ConsentID, a.Detail, a.LatencyMS).Scan(&a.CreatedAt); err != nil {
			return fmt.Errorf("insert kyc audit: %w", err)
		}
		return nil
	})
}

// documentColumns is the canonical kyc_documents projection (INSERT
// RETURNING and SELECT share it).
const documentColumns = `document_id, tenant_id, subject_phone, doc_type,
    ocr_document_id, sha256, phash, id_value_hash, field_hashes, status,
    review_state, reviewer_actor, reviewed_at, created_at`

// scanDocument scans one documentColumns row. field_hashes transits as
// raw JSONB bytes (map[string]string of HMAC digests — never raw values).
func scanDocument(row pgx.Row, d *Document) error {
	var fieldHashes []byte
	err := row.Scan(&d.DocumentID, &d.TenantID, &d.SubjectPhone, &d.DocType,
		&d.OCRDocumentID, &d.SHA256, &d.PHash, &d.IDValueHash, &fieldHashes,
		&d.Status, &d.ReviewState, &d.ReviewerActor, &d.ReviewedAt, &d.CreatedAt)
	if err != nil {
		return err
	}
	d.FieldHashes = map[string]string{}
	if len(fieldHashes) > 0 {
		if err := json.Unmarshal(fieldHashes, &d.FieldHashes); err != nil {
			return fmt.Errorf("decode field_hashes: %w", err)
		}
	}
	return nil
}

// InsertDocument commits the hash-only durable record (SPEC-W58 B1, P8).
// Called exactly once per successful OCR extraction, AFTER the document
// K22 binding check — a row here is what establishes the binding.
func (s *Store) InsertDocument(ctx context.Context, d *Document) error {
	if d.DocumentID == uuid.Nil {
		d.DocumentID = uuid.New()
	}
	fieldHashes, err := json.Marshal(d.FieldHashes)
	if err != nil {
		return fmt.Errorf("encode field_hashes: %w", err)
	}
	const q = `INSERT INTO kyc_documents
	           (document_id, tenant_id, subject_phone, doc_type, ocr_document_id,
	            sha256, phash, id_value_hash, field_hashes, status)
	           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING created_at, review_state`
	return s.withTenant(ctx, d.TenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, q,
			d.DocumentID, d.TenantID, d.SubjectPhone, d.DocType, d.OCRDocumentID,
			d.SHA256, d.PHash, d.IDValueHash, fieldHashes, d.Status).
			Scan(&d.CreatedAt, &d.ReviewState); err != nil {
			return fmt.Errorf("insert kyc document: %w", err)
		}
		return nil
	})
}

// DocumentIDHashOwner implements the document K22 binding lookup
// (SPEC-W58 P8): the subject_phone of the EARLIEST kyc_documents row for
// (tenant, doc_type, id_value_hash). "" + nil when the document number was
// never committed for this tenant — the first commit establishes the
// binding. Runs inside withTenant so RLS scopes the read.
func (s *Store) DocumentIDHashOwner(ctx context.Context, tenantID uuid.UUID, docType, idValueHash string) (string, error) {
	var owner string
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT subject_phone FROM kyc_documents
		                         WHERE tenant_id = $1 AND doc_type = $2 AND id_value_hash = $3
		                         ORDER BY created_at ASC, document_id ASC LIMIT 1`,
			tenantID, docType, idValueHash).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			owner = ""
			return nil
		}
		if err != nil {
			return fmt.Errorf("lookup document id hash owner: %w", err)
		}
		return nil
	})
	return owner, err
}

// GetDocument returns one kyc_documents row by primary key (nil, nil when
// absent), RLS-scoped to the tenant.
func (s *Store) GetDocument(ctx context.Context, tenantID, documentID uuid.UUID) (*Document, error) {
	d := &Document{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		err := scanDocument(tx.QueryRow(ctx,
			`SELECT `+documentColumns+` FROM kyc_documents
			 WHERE tenant_id = $1 AND document_id = $2`, tenantID, documentID), d)
		if errors.Is(err, pgx.ErrNoRows) {
			d = nil
			return nil
		}
		if err != nil {
			return fmt.Errorf("get kyc document: %w", err)
		}
		return nil
	})
	return d, err
}

// ReviewDocument performs the only mutation kyc_documents allows
// (SPEC-W58 B1): review_state + reviewer_actor + reviewed_at. The
// kyc_documents_review_only trigger rejects any wider write at the
// database layer. (nil, nil) when the document is absent.
func (s *Store) ReviewDocument(ctx context.Context, tenantID, documentID uuid.UUID, decision, actor string) (*Document, error) {
	d := &Document{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		err := scanDocument(tx.QueryRow(ctx,
			`UPDATE kyc_documents
			 SET review_state = $3, reviewer_actor = $4, reviewed_at = now()
			 WHERE tenant_id = $1 AND document_id = $2
			 RETURNING `+documentColumns, tenantID, documentID, decision, actor), d)
		if errors.Is(err, pgx.ErrNoRows) {
			d = nil
			return nil
		}
		if err != nil {
			return fmt.Errorf("review kyc document: %w", err)
		}
		return nil
	})
	return d, err
}
