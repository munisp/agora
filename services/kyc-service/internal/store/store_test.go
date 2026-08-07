package store

import (
	"context"
	"strings"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
)

// Store-level kyc_audit tests run against a real (embedded) Postgres so the
// RLS bootstrap and insert path are exercised for real (booking-service
// waitlist_test.go pattern). Run with -short to skip in constrained
// environments.

func newTestStore(t *testing.T) *Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres store test in -short mode")
	}
	// Dedicated port + data dir so parallel packages don't race on the
	// default 5432/data-dir (booking-service newTestStore note).
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("kyc_test").
		Port(5435).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	st, err := New(ctx, "postgres://postgres:postgres@localhost:5435/kyc_test?sslmode=disable")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestStoreInsertAudit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	a := Audit{
		TenantID:     uuid.New(),
		Actor:        "kyc-service-test",
		SubjectPhone: "+2348012345678",
		IDType:       "bvn",
		IDValueHash:  strings.Repeat("ab", 32),
		Status:       "verified",
		Reference:    "kyc_" + uuid.NewString(),
		LatencyMS:    42,
	}
	if err := st.InsertAudit(ctx, &a); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if a.AuditID == uuid.Nil || a.CreatedAt.IsZero() {
		t.Errorf("audit not populated: %+v", a)
	}

	// CHECK constraints enforce the contract enums.
	bad := a
	bad.IDType = "passport"
	if err := st.InsertAudit(ctx, &bad); err == nil {
		t.Errorf("id_type CHECK must reject 'passport'")
	}
	bad = a
	bad.Status = "banana"
	if err := st.InsertAudit(ctx, &bad); err == nil {
		t.Errorf("status CHECK must reject 'banana'")
	}
}

func TestStoreRLSPolicyPresent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	var enabled, forced bool
	if err := st.pool.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'kyc_audit'`).
		Scan(&enabled, &forced); err != nil {
		t.Fatalf("rls flags: %v", err)
	}
	if !enabled || !forced {
		t.Errorf("RLS enabled=%v forced=%v, want true/true", enabled, forced)
	}
	var policy string
	if err := st.pool.QueryRow(ctx,
		`SELECT policyname FROM pg_policies WHERE tablename = 'kyc_audit' AND policyname = 'tenant_isolation'`).
		Scan(&policy); err != nil {
		t.Errorf("tenant_isolation policy missing: %v", err)
	}
}

// SPEC-W34 GF7: kyc_audit is an append-only forensic trail — UPDATE and
// DELETE must both fail (BEFORE trigger raises), INSERT must still work.
func TestAuditAppendOnly(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	a := Audit{
		TenantID:     uuid.New(),
		Actor:        "kyc-service-test",
		SubjectPhone: "+2348012345678",
		IDType:       "nin",
		IDValueHash:  strings.Repeat("cd", 32),
		Status:       "verified",
		Reference:    "kyc_" + uuid.NewString(),
	}
	if err := st.InsertAudit(ctx, &a); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// UPDATE must fail (even as the owner/superuser — the trigger binds
	// every role, including ones REVOKE cannot touch).
	if _, err := st.pool.Exec(ctx,
		`UPDATE kyc_audit SET status = 'mismatch' WHERE audit_id = $1`, a.AuditID); err == nil {
		t.Errorf("UPDATE on kyc_audit must fail (append-only)")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("UPDATE error should cite append-only rule, got: %v", err)
	}

	// DELETE must fail.
	if _, err := st.pool.Exec(ctx,
		`DELETE FROM kyc_audit WHERE audit_id = $1`, a.AuditID); err == nil {
		t.Errorf("DELETE on kyc_audit must fail (append-only)")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("DELETE error should cite append-only rule, got: %v", err)
	}

	// Row untouched.
	var status string
	if err := st.pool.QueryRow(ctx,
		`SELECT status FROM kyc_audit WHERE audit_id = $1`, a.AuditID).Scan(&status); err != nil {
		t.Fatalf("select after blocked mutations: %v", err)
	}
	if status != "verified" {
		t.Errorf("row mutated despite append-only rule: status=%q", status)
	}

	// INSERT still works — the service's only write path.
	b := a
	b.AuditID = uuid.Nil
	b.Reference = "kyc_" + uuid.NewString()
	if err := st.InsertAudit(ctx, &b); err != nil {
		t.Errorf("INSERT must still work on append-only kyc_audit: %v", err)
	}

	// Trigger + revokes present.
	var triggerName string
	if err := st.pool.QueryRow(ctx,
		`SELECT tgname FROM pg_trigger WHERE tgname = 'kyc_audit_append_only'`).Scan(&triggerName); err != nil {
		t.Errorf("kyc_audit_append_only trigger missing: %v", err)
	}
}
