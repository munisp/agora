package store

// SPEC-W94 tests: active verification-grant revocation complements the
// TTL — delete semantics, idempotency, tenant scoping.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRevokeVerificationGrant_W94(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()
	contact := uuid.New()

	// Revoke with no grant: idempotent false, no error.
	revoked, err := st.RevokeVerificationGrant(ctx, tenantA, contact)
	if err != nil {
		t.Fatalf("revoke missing: %v", err)
	}
	if revoked {
		t.Fatal("revoke with no grant reported true")
	}

	// Seed grants for the same contact id under TWO tenants; revoking one
	// must leave the other untouched (RLS + tenant predicate).
	if err := st.UpsertVerificationGrant(ctx, tenantA, contact, "otp", time.Hour); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if err := st.UpsertVerificationGrant(ctx, tenantB, contact, "otp", time.Hour); err != nil {
		t.Fatalf("upsert B: %v", err)
	}

	revoked, err = st.RevokeVerificationGrant(ctx, tenantA, contact)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !revoked {
		t.Fatal("revoke of existing grant reported false")
	}

	// The revoked grant is immediately inactive (not merely expired).
	if _, err := st.GetActiveVerificationGrant(ctx, tenantA, contact); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after revoke, got %v", err)
	}

	// Second revoke: idempotent false.
	revoked, err = st.RevokeVerificationGrant(ctx, tenantA, contact)
	if err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if revoked {
		t.Fatal("second revoke reported true")
	}

	// Tenant B's grant survives.
	if _, err := st.GetActiveVerificationGrant(ctx, tenantB, contact); err != nil {
		t.Fatalf("tenant B grant must survive tenant A revoke: %v", err)
	}
}
