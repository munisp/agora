package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestVerificationGrantLifecycle covers the SPEC-W87 grant store: bootstrap
// (ran inside store.New), upsert/refresh semantics (one active grant per
// contact), expiry, not-found, and tenant scoping of the queries.
func TestVerificationGrantLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()
	contact := uuid.New()

	// Not-found before any grant exists.
	if _, err := st.GetActiveVerificationGrant(ctx, tenantA, contact); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// Upsert creates an active grant.
	if err := st.UpsertVerificationGrant(ctx, tenantA, contact, "otp", time.Hour); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	g, err := st.GetActiveVerificationGrant(ctx, tenantA, contact)
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if g.Method != "otp" || g.TenantID != tenantA || g.ContactID != contact {
		t.Fatalf("unexpected grant: %+v", g)
	}
	if g.ExpiresAt.Before(time.Now().Add(59 * time.Minute)) {
		t.Fatalf("expires_at not ~1h out: %v", g.ExpiresAt)
	}

	// Refresh: same contact, longer TTL -> row count stays 1 (unique
	// (tenant_id, contact_id)), method + expiry move forward.
	if err := st.UpsertVerificationGrant(ctx, tenantA, contact, "otp", 48*time.Hour); err != nil {
		t.Fatalf("refresh upsert: %v", err)
	}
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM verification_grants WHERE tenant_id=$1 AND contact_id=$2`,
		tenantA, contact).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 grant row after refresh, got %d", n)
	}
	g2, err := st.GetActiveVerificationGrant(ctx, tenantA, contact)
	if err != nil {
		t.Fatalf("get after refresh: %v", err)
	}
	if !g2.ExpiresAt.After(g.ExpiresAt) {
		t.Fatalf("refresh did not extend expiry: %v -> %v", g.ExpiresAt, g2.ExpiresAt)
	}

	// Expired grant (negative TTL writes an already-past expiry) -> not found.
	if err := st.UpsertVerificationGrant(ctx, tenantA, contact, "otp", -time.Hour); err != nil {
		t.Fatalf("expiring upsert: %v", err)
	}
	if _, err := st.GetActiveVerificationGrant(ctx, tenantA, contact); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for expired grant, got %v", err)
	}

	// Tenant scoping: the grant is invisible to another tenant's query.
	if err := st.UpsertVerificationGrant(ctx, tenantA, contact, "otp", time.Hour); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if _, err := st.GetActiveVerificationGrant(ctx, tenantB, contact); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for foreign tenant, got %v", err)
	}
}
