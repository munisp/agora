package store

import (
	"context"
	"strings"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// ---------------------------------------------------------------------------
// SPEC-W58 B1: kyc_documents — hash-only durable record, review-only mutable
// ---------------------------------------------------------------------------

func newTestDocument(tenantID uuid.UUID) *Document {
	return &Document{
		TenantID:      tenantID,
		SubjectPhone:  "+2348012345678",
		DocType:       "kyc_nin_slip",
		OCRDocumentID: uuid.New(),
		SHA256:        strings.Repeat("ab", 32),
		PHash:         "0123456789abcdef",
		IDValueHash:   strings.Repeat("cd", 32),
		FieldHashes:   map[string]string{"nin": strings.Repeat("ef", 32), "full_name": strings.Repeat("01", 32)},
		Status:        "extracted",
	}
}

func TestStoreDocumentRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tid := uuid.New()
	d := newTestDocument(tid)
	if err := st.InsertDocument(ctx, d); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if d.DocumentID == uuid.Nil || d.CreatedAt.IsZero() {
		t.Errorf("document not populated: %+v", d)
	}
	// P7: human review is the terminal state — default pending_review.
	if d.ReviewState != "pending_review" {
		t.Errorf("review_state default = %q, want pending_review", d.ReviewState)
	}

	got, err := st.GetDocument(ctx, tid, d.DocumentID)
	if err != nil || got == nil {
		t.Fatalf("get: %v nil=%v", err, got == nil)
	}
	if got.SubjectPhone != d.SubjectPhone || got.IDValueHash != d.IDValueHash ||
		got.FieldHashes["nin"] != d.FieldHashes["nin"] || got.Status != "extracted" {
		t.Errorf("round trip mismatch: %+v", got)
	}
	// Unknown id -> nil, nil.
	if miss, err := st.GetDocument(ctx, tid, uuid.New()); err != nil || miss != nil {
		t.Errorf("missing: %v %v", miss, err)
	}
	// CHECK constraints enforce the enums.
	bad := newTestDocument(tid)
	bad.DocType = "passport"
	if err := st.InsertDocument(ctx, bad); err == nil {
		t.Errorf("doc_type CHECK must reject 'passport'")
	}
	bad = newTestDocument(tid)
	bad.Status = "banana"
	if err := st.InsertDocument(ctx, bad); err == nil {
		t.Errorf("status CHECK must reject 'banana'")
	}
}

func TestStoreDocumentBindingOwner(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tid := uuid.New()
	d := newTestDocument(tid)
	if err := st.InsertDocument(ctx, d); err != nil {
		t.Fatalf("insert: %v", err)
	}
	owner, err := st.DocumentIDHashOwner(ctx, tid, "kyc_nin_slip", d.IDValueHash)
	if err != nil || owner != "+2348012345678" {
		t.Errorf("owner = %q err %v", owner, err)
	}
	// Unseen hash / different doc_type -> "".
	if owner, _ := st.DocumentIDHashOwner(ctx, tid, "kyc_nin_slip", strings.Repeat("ff", 32)); owner != "" {
		t.Errorf("unseen hash owner = %q, want \"\"", owner)
	}
	if owner, _ := st.DocumentIDHashOwner(ctx, tid, "kyc_passport", d.IDValueHash); owner != "" {
		t.Errorf("different doc_type owner = %q, want \"\"", owner)
	}
}

func TestStoreDocumentRLSAndTenantIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	// RLS flags + policy present.
	var enabled, forced bool
	if err := st.pool.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'kyc_documents'`).
		Scan(&enabled, &forced); err != nil {
		t.Fatalf("rls flags: %v", err)
	}
	if !enabled || !forced {
		t.Errorf("RLS enabled=%v forced=%v, want true/true", enabled, forced)
	}
	var policy string
	if err := st.pool.QueryRow(ctx,
		`SELECT policyname FROM pg_policies WHERE tablename = 'kyc_documents' AND policyname = 'tenant_isolation'`).
		Scan(&policy); err != nil {
		t.Errorf("tenant_isolation policy missing: %v", err)
	}

	// Cross-tenant read is invisible (RLS at the DB layer).
	tidA := uuid.New()
	d := newTestDocument(tidA)
	if err := st.InsertDocument(ctx, d); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got, err := st.GetDocument(ctx, uuid.New(), d.DocumentID); err != nil || got != nil {
		t.Errorf("cross-tenant get must be invisible: %v %v", got, err)
	}
}

// SPEC-W58 B1: mutable ONLY on review_state/reviewer_actor/reviewed_at —
// enforced by the kyc_documents_review_only trigger (binds every role);
// DELETE is forbidden outright.
func TestStoreDocumentReviewOnlyMutable(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tid := uuid.New()
	d := newTestDocument(tid)
	if err := st.InsertDocument(ctx, d); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The review path mutates the row.
	got, err := st.ReviewDocument(ctx, tid, d.DocumentID, "approved", "reviewer-1")
	if err != nil || got == nil {
		t.Fatalf("review: %v nil=%v", err, got == nil)
	}
	if got.ReviewState != "approved" || got.ReviewerActor != "reviewer-1" || got.ReviewedAt == nil {
		t.Errorf("reviewed row: %+v", got)
	}
	// Unknown id -> nil, nil.
	if miss, err := st.ReviewDocument(ctx, tid, uuid.New(), "approved", "x"); err != nil || miss != nil {
		t.Errorf("review missing: %v %v", miss, err)
	}

	// Any OTHER column write must fail at the database layer.
	if _, err := st.pool.Exec(ctx,
		`UPDATE kyc_documents SET subject_phone = '+2348099999999' WHERE document_id = $1`, d.DocumentID); err == nil {
		t.Errorf("UPDATE of subject_phone must fail (review-only mutable)")
	} else if !strings.Contains(err.Error(), "review_state") {
		t.Errorf("UPDATE error should cite the review-only rule, got: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE kyc_documents SET id_value_hash = 'zz' WHERE document_id = $1`, d.DocumentID); err == nil {
		t.Errorf("UPDATE of id_value_hash must fail (review-only mutable)")
	}
	// Even a no-op-shaped review write combined with an immutable change
	// must fail.
	if _, err := st.pool.Exec(ctx,
		`UPDATE kyc_documents SET review_state = 'rejected', sha256 = 'zz' WHERE document_id = $1`, d.DocumentID); err == nil {
		t.Errorf("mixed UPDATE must fail (review-only mutable)")
	}
	// DELETE is forbidden.
	if _, err := st.pool.Exec(ctx,
		`DELETE FROM kyc_documents WHERE document_id = $1`, d.DocumentID); err == nil {
		t.Errorf("DELETE on kyc_documents must fail")
	}

	// Trigger present.
	var triggerName string
	if err := st.pool.QueryRow(ctx,
		`SELECT tgname FROM pg_trigger WHERE tgname = 'kyc_documents_review_only'`).Scan(&triggerName); err != nil {
		t.Errorf("kyc_documents_review_only trigger missing: %v", err)
	}
}

// SPEC-W58: the kyc_audit CHECKs are widened for document actions and the
// consent_id/detail columns exist (P6: consent_id recorded in kyc_audit).
func TestStoreAuditDocumentStatusesAndConsentID(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	a := Audit{
		TenantID:     uuid.New(),
		Actor:        "kyc-service-test",
		SubjectPhone: "+2348012345678",
		IDType:       "kyc_nin_slip",
		IDValueHash:  strings.Repeat("ab", 32),
		Status:       "extracted",
		Reference:    "kyc_doc_" + uuid.NewString(),
		ConsentID:    "consent-123",
		Detail:       "",
	}
	if err := st.InsertAudit(ctx, &a); err != nil {
		t.Fatalf("document audit insert: %v", err)
	}
	var consentID string
	if err := st.withTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT consent_id FROM kyc_audit WHERE audit_id = $1`, a.AuditID).Scan(&consentID)
	}); err != nil || consentID != "consent-123" {
		t.Errorf("consent_id = %q err %v, want consent-123", consentID, err)
	}
	// Review decisions audit too.
	b := a
	b.AuditID = uuid.Nil
	b.Status = "approved"
	b.Detail = "matches the physical card"
	if err := st.InsertAudit(ctx, &b); err != nil {
		t.Errorf("review audit insert: %v", err)
	}
	// Legacy resolve rows (no consent_id) still insert.
	c := Audit{
		TenantID: uuid.New(), Actor: "t", SubjectPhone: "+2348", IDType: "bvn",
		IDValueHash: strings.Repeat("ab", 32), Status: "verified", Reference: "kyc_" + uuid.NewString(),
	}
	if err := st.InsertAudit(ctx, &c); err != nil {
		t.Errorf("legacy resolve audit insert: %v", err)
	}
}

// Bootstrap must be safely re-runnable (upgrade path on existing tables).
func TestStoreBootstrapIdempotent(t *testing.T) {
	st := newTestStore(t)
	if err := st.bootstrap(context.Background()); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
}

// SPEC-W65: ListDocuments — newest-first on idx_kyc_documents_tenant,
// review_state filter, limit/offset windowing, RLS tenant isolation.
func TestStoreListDocuments(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tid := uuid.New()

	// Three rows for tid + one decoy in another tenant.
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		d := newTestDocument(tid)
		d.IDValueHash = strings.Repeat(string(rune('a'+i)), 2) // distinct binding hashes
		if err := st.InsertDocument(ctx, d); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		ids = append(ids, d.DocumentID)
	}
	other := newTestDocument(uuid.New())
	if err := st.InsertDocument(ctx, other); err != nil {
		t.Fatalf("insert decoy: %v", err)
	}
	// Review the oldest row so the filter has a non-default state.
	if _, err := st.ReviewDocument(ctx, tid, ids[0], "approved", "reviewer-1"); err != nil {
		t.Fatalf("review: %v", err)
	}

	// Newest-first, unfiltered.
	all, err := st.ListDocuments(ctx, tid, "", 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("list len = %d, want 3", len(all))
	}
	if all[0].DocumentID != ids[2] || all[2].DocumentID != ids[0] {
		t.Errorf("order = %v, want newest-first", all)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].CreatedAt.Before(all[i].CreatedAt) {
			t.Errorf("not newest-first at %d", i)
		}
	}

	// review_state filter.
	approved, err := st.ListDocuments(ctx, tid, "approved", 50, 0)
	if err != nil || len(approved) != 1 || approved[0].DocumentID != ids[0] {
		t.Errorf("approved filter: %v %v", approved, err)
	}
	pending, err := st.ListDocuments(ctx, tid, "pending_review", 50, 0)
	if err != nil || len(pending) != 2 {
		t.Errorf("pending filter: len=%d err=%v, want 2", len(pending), err)
	}

	// Windowing: pages stitch to the full queue without repeats.
	p1, err := st.ListDocuments(ctx, tid, "", 2, 0)
	if err != nil || len(p1) != 2 || p1[0].DocumentID != ids[2] {
		t.Errorf("page1: %v %v", p1, err)
	}
	p2, err := st.ListDocuments(ctx, tid, "", 2, 2)
	if err != nil || len(p2) != 1 || p2[0].DocumentID != ids[0] {
		t.Errorf("page2: %v %v", p2, err)
	}
	past, err := st.ListDocuments(ctx, tid, "", 2, 10)
	if err != nil || len(past) != 0 {
		t.Errorf("past-end page: %v %v, want empty", past, err)
	}

	// Cross-tenant reads are invisible (RLS enforced).
	cross, err := st.ListDocuments(ctx, uuid.New(), "", 50, 0)
	if err != nil || len(cross) != 0 {
		t.Errorf("cross-tenant list: %v %v, want empty", cross, err)
	}
}
