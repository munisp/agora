package consent

import (
	"context"
	"strings"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store-level consent tests run against a real (embedded) Postgres so the
// upsert idempotency, tombstone semantics and RLS policy are exercised for
// real (booking-service waitlist_test.go pattern). Set STORE_TEST=0 or run
// with -short to skip in constrained environments.

// testSchema is the minimal slice of 02-identity-schema.sql the consent
// tests need (the init script itself contains \c meta-commands and cannot
// be replayed through pgx).
const testSchema = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE IF NOT EXISTS tenants (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`

func newTestStore(t *testing.T) (*Store, uuid.UUID) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres store test in -short mode")
	}
	// Dedicated port + data dir so parallel packages don't race on the
	// default 5432/data-dir (booking-service newTestStore note).
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("identity_test").
		Port(5434).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5434/identity_test?sslmode=disable"
	// The tenants table must exist before consent.New bootstraps consents
	// (FK reference).
	raw, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("raw pool: %v", err)
	}
	if _, err := raw.Exec(ctx, testSchema); err != nil {
		t.Fatalf("test schema: %v", err)
	}
	raw.Close()

	st, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("consent.New: %v", err)
	}
	t.Cleanup(st.Close)

	tenantID := uuid.New()
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO tenants (id, slug, name) VALUES ($1, $2, $3)`,
		tenantID, "t-"+tenantID.String()[:8], "Test Tenant"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return st, tenantID
}

func TestStoreCaptureIdempotentAndErase(t *testing.T) {
	st, tenantID := newTestStore(t)
	ctx := context.Background()

	rec := Record{TenantID: tenantID, DataSubjectID: "+234809990001", Purpose: "kyc",
		CapturedChannel: "web", CapturedLocale: "en-NG"}
	if err := st.Capture(ctx, &rec); err != nil {
		t.Fatalf("capture: %v", err)
	}
	if rec.ConsentID == uuid.Nil || rec.CapturedTS.IsZero() {
		t.Fatalf("record not populated: %+v", rec)
	}

	// Replay: same consent_id, original captured_ts.
	replay := Record{TenantID: tenantID, DataSubjectID: rec.DataSubjectID, Purpose: "kyc",
		CapturedChannel: "ussd"}
	if err := st.Capture(ctx, &replay); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.ConsentID != rec.ConsentID {
		t.Errorf("replay changed consent_id: %s -> %s", rec.ConsentID, replay.ConsentID)
	}
	if !replay.CapturedTS.Equal(rec.CapturedTS) {
		t.Errorf("replay changed captured_ts")
	}
	if replay.CapturedChannel != "ussd" {
		t.Errorf("replay must refresh channel, got %q", replay.CapturedChannel)
	}

	// Active + List.
	if _, err := st.Active(ctx, tenantID, rec.DataSubjectID, "kyc"); err != nil {
		t.Errorf("active: %v", err)
	}
	recs, err := st.List(ctx, tenantID, rec.DataSubjectID)
	if err != nil || len(recs) != 1 {
		t.Errorf("list: %v, n=%d", err, len(recs))
	}

	// Erase tombstones; Active then reports not found; second erase is a no-op.
	n, err := st.Erase(ctx, tenantID, rec.DataSubjectID, "kyc")
	if err != nil || n != 1 {
		t.Fatalf("erase: %v, n=%d", err, n)
	}
	if _, err := st.Active(ctx, tenantID, rec.DataSubjectID, "kyc"); err != ErrNotFound {
		t.Errorf("post-erase active: %v, want ErrNotFound", err)
	}
	if n, _ = st.Erase(ctx, tenantID, rec.DataSubjectID, "kyc"); n != 0 {
		t.Errorf("second erase n=%d, want 0", n)
	}

	// Tombstone retained for audit.
	recs, _ = st.List(ctx, tenantID, rec.DataSubjectID)
	if len(recs) != 1 || recs[0].ErasureTS == nil {
		t.Errorf("tombstone not retained: %+v", recs)
	}

	// Re-capture clears the tombstone (re-consent).
	if err := st.Capture(ctx, &replay); err != nil {
		t.Fatalf("re-capture: %v", err)
	}
	if _, err := st.Active(ctx, tenantID, rec.DataSubjectID, "kyc"); err != nil {
		t.Errorf("re-consent must reactivate: %v", err)
	}
}

// TestStoreEraseWritesOutbox (SPEC-W43 I-04): Erase persists the tombstone
// AND the durable outbox row in one transaction; the relay-facing
// fetch/mark lifecycle drains it.
func TestStoreEraseWritesOutbox(t *testing.T) {
	st, tenantID := newTestStore(t)
	ctx := context.Background()

	rec := Record{TenantID: tenantID, DataSubjectID: "+234809990077", Purpose: "kyc"}
	if err := st.Capture(ctx, &rec); err != nil {
		t.Fatalf("capture: %v", err)
	}
	n, err := st.Erase(ctx, tenantID, rec.DataSubjectID, "kyc")
	if err != nil || n != 1 {
		t.Fatalf("erase: %v, n=%d", err, n)
	}

	rows, err := st.FetchUnsentOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("fetch unsent: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("unsent rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.TenantID != tenantID || row.DataSubjectID != rec.DataSubjectID ||
		row.Purpose != "kyc" || row.ErasedRecords != 1 || row.CreatedAt.IsZero() {
		t.Errorf("outbox row = %+v", row)
	}
	if err := st.MarkOutboxSent(ctx, row.ID); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	rows, err = st.FetchUnsentOutbox(ctx, 10)
	if err != nil || len(rows) != 0 {
		t.Errorf("post-mark unsent = %v, %d", err, len(rows))
	}

	// A no-op erase (already tombstoned) must NOT enqueue another row.
	if n, _ := st.Erase(ctx, tenantID, rec.DataSubjectID, "kyc"); n != 0 {
		t.Fatalf("re-erase n=%d, want 0", n)
	}
	rows, _ = st.FetchUnsentOutbox(ctx, 10)
	if len(rows) != 0 {
		t.Errorf("no-op erase enqueued %d rows, want 0", len(rows))
	}
}

func TestStoreRLSPolicyPresent(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	var enabled, forced bool
	if err := st.pool.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'consents'`).
		Scan(&enabled, &forced); err != nil {
		t.Fatalf("rls flags: %v", err)
	}
	if !enabled || !forced {
		t.Errorf("RLS enabled=%v forced=%v, want true/true", enabled, forced)
	}
	var policy string
	err := st.pool.QueryRow(ctx,
		`SELECT policyname FROM pg_policies WHERE tablename = 'consents' AND policyname = 'tenant_isolation'`).
		Scan(&policy)
	if err != nil {
		t.Errorf("tenant_isolation policy missing: %v", err)
	}
	// SPEC-W43 I-03: fail-closed NULLIF idiom + app_identity_internal escape.
	var qual string
	if err := st.pool.QueryRow(ctx,
		`SELECT pg_get_expr(polqual, polrelid) FROM pg_policy
		 WHERE polrelid = 'consents'::regclass AND polname = 'tenant_isolation'`).Scan(&qual); err != nil {
		t.Fatalf("policy expr: %v", err)
	}
	if !strings.Contains(qual, "NULLIF") || !strings.Contains(qual, "app_identity_internal") {
		t.Errorf("policy must be fail-closed with internal escape, got: %s", qual)
	}
	// Outbox table carries the same fail-closed + escape policy.
	if err := st.pool.QueryRow(ctx,
		`SELECT pg_get_expr(polqual, polrelid) FROM pg_policy
		 WHERE polrelid = 'consent_events_outbox'::regclass AND polname = 'tenant_isolation'`).Scan(&qual); err != nil {
		t.Fatalf("outbox policy expr: %v", err)
	}
	if !strings.Contains(qual, "NULLIF") || !strings.Contains(qual, "app_identity_internal") {
		t.Errorf("outbox policy must be fail-closed with internal escape, got: %s", qual)
	}
}
