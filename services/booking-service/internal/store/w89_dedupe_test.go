package store

// SPEC-W89 tests: digits-aware contact dedupe migration + digits unique
// index. Cross-format duplicates ('+234…' vs '234…' — the SAME
// subscriber) fold to the deterministic keeper (MIN(id::text)::uuid),
// every hard/soft reference is re-pointed, TTL-ephemeral loser rows are
// deleted, and the swapped digits-expression unique index then BLOCKS a
// fresh cross-format insert.
// REDIRECTION PROOF: fails on the pristine tree (no digits fold, raw
// unique index permits the duplicate insert).

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestW89DigitsDedupeFold(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	// Seed cross-format duplicates + references via raw SQL (the
	// migration under test must do the fold — app paths would resolve
	// canonically since W88).
	idA, idB := uuid.New(), uuid.New()
	keeper, loser := idA, idB
	if strings.Compare(idB.String(), idA.String()) < 0 {
		keeper, loser = idB, idA
	}
	execTenant(t, st, tenantID,
		`INSERT INTO contacts (id, tenant_id, name, phone) VALUES ($2,$1,'Plus Form','+234 801 111 2222')`, idA)
	execTenant(t, st, tenantID,
		`INSERT INTO contacts (id, tenant_id, name, phone) VALUES ($2,$1,'Digits Form','2348011112222')`, idB)

	// Hard-FK booking on the LOSER.
	execTenant(t, st, tenantID,
		`INSERT INTO bookings (id, tenant_id, offering_id, contact_id, team_member_id, starts_at, ends_at, status)
		 VALUES ($2,$1,$3,$4,$5, now() + interval '1 day', now() + interval '1 day 30 minutes', 'pending')`,
		uuid.New(), uuid.New(), loser, uuid.New())

	// TTL-ephemeral rows on BOTH (grants UNIQUE per contact): loser
	// rows must be deleted, keeper rows survive.
	if err := st.ensureVerificationGrantsTable(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.ensurePortalTokensTable(ctx); err != nil {
		t.Fatal(err)
	}
	execTenant(t, st, tenantID,
		`INSERT INTO verification_grants (tenant_id, contact_id, method, verified_at, expires_at)
		 VALUES ($1,$2,'otp', now(), now() + interval '1 day')`, keeper)
	execTenant(t, st, tenantID,
		`INSERT INTO verification_grants (tenant_id, contact_id, method, verified_at, expires_at)
		 VALUES ($1,$2,'otp', now(), now() + interval '1 day')`, loser)
	execTenant(t, st, tenantID,
		`INSERT INTO portal_tokens (tenant_id, contact_id, token_hash, channel, expires_at)
		 VALUES ($1,$2,'h','sms', now() + interval '10 minutes')`, loser)

	// Run the migration (New() ran it before the fixture tables existed).
	if err := st.ensureContactDedupe(ctx); err != nil {
		t.Fatalf("dedupe migration: %v", err)
	}

	// Exactly ONE contact left — the deterministic keeper.
	if got := queryTenant(t, st, tenantID,
		`SELECT count(*)::text FROM contacts WHERE tenant_id=$1`); got != "1" {
		t.Fatalf("contacts after fold = %s, want 1", got)
	}
	if got := queryTenant(t, st, tenantID,
		`SELECT id::text FROM contacts WHERE tenant_id=$1`); got != keeper.String() {
		t.Fatalf("keeper = %s, want %s", got, keeper)
	}
	// Booking re-pointed to the keeper.
	if got := queryTenant(t, st, tenantID,
		`SELECT contact_id::text FROM bookings WHERE tenant_id=$1`); got != keeper.String() {
		t.Fatalf("booking contact_id = %s, want keeper %s", got, keeper)
	}
	// Loser grant/token deleted, keeper grant survives.
	if got := queryTenant(t, st, tenantID,
		`SELECT count(*)::text FROM verification_grants WHERE tenant_id=$1`); got != "1" {
		t.Fatalf("grants after fold = %s, want 1 (loser deleted)", got)
	}
	if got := queryTenant(t, st, tenantID,
		`SELECT contact_id::text FROM verification_grants WHERE tenant_id=$1`); got != keeper.String() {
		t.Fatalf("surviving grant contact = %s, want keeper %s", got, keeper)
	}
	if got := queryTenant(t, st, tenantID,
		`SELECT count(*)::text FROM portal_tokens WHERE tenant_id=$1`); got != "0" {
		t.Fatalf("portal tokens after fold = %s, want 0", got)
	}

	// The swapped digits unique index now BLOCKS a cross-format insert
	// (and the domain error is ErrConflict for the 409 mapping).
	dup := Contact{TenantID: tenantID, Name: "Dup", Phone: "+2348011112222"}
	if err := st.CreateContact(ctx, &dup); err != ErrConflict {
		t.Fatalf("cross-format duplicate insert err = %v, want ErrConflict", err)
	}
	// Idempotent: a second run folds nothing, errors nothing.
	if err := st.ensureContactDedupe(ctx); err != nil {
		t.Fatalf("dedupe rerun: %v", err)
	}
	if got := queryTenant(t, st, tenantID,
		`SELECT count(*)::text FROM contacts WHERE tenant_id=$1`); got != "1" {
		t.Fatalf("contacts after rerun = %s, want 1", got)
	}
}

func TestW89DedupeKeepsDistinctNumbers(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	// Genuinely different subscribers and a phone-less row are NOT
	// folded; national vs international forms never fold (W61 ruling).
	for _, c := range [][2]string{
		{"A", "+2348011112222"}, {"B", "2348099998888"},
		{"C", "08011112222"}, // national form of A's number — NOT folded
		{"D", ""},            // phone-less — exempt
	} {
		execTenant(t, st, tenantID,
			`INSERT INTO contacts (id, tenant_id, name, phone) VALUES ($2,$1,$3,$4)`,
			uuid.New(), c[0], c[1])
	}
	if err := st.ensureContactDedupe(ctx); err != nil {
		t.Fatal(err)
	}
	if got := queryTenant(t, st, tenantID,
		`SELECT count(*)::text FROM contacts WHERE tenant_id=$1`); got != "4" {
		t.Fatalf("distinct rows folded: count = %s, want 4", got)
	}
}
