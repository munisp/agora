package store

// SPEC-W45 CODER-M store tests:
//   - GetWaitlistEntryByToken (K13 completion): the public token-resolution
//     path used by /v1/waitlist/claim-info and /v1/waitlist/claim;
//   - team_members.user_id (STK O14 completion): bootstrap column +
//     create/get/list roundtrip, NULL for unlinked members.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestGetWaitlistEntryByToken(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	// Offering FK-less in the test schema; the entry only needs the IDs.
	start := time.Now().UTC().Add(24 * time.Hour)
	entry := WaitlistEntry{
		TenantID:     tenantID,
		OfferingID:   uuid.New(),
		ContactName:  "Token Tess",
		ContactPhone: "+15550010",
		WindowStart:  start,
		WindowEnd:    start.Add(2 * time.Hour),
	}
	if err := st.CreateWaitlistEntry(ctx, &entry); err != nil {
		t.Fatalf("create entry: %v", err)
	}

	// Token resolves the entry without any tenant context (public path).
	got, err := st.GetWaitlistEntryByToken(ctx, entry.ClaimToken)
	if err != nil {
		t.Fatalf("by token: %v", err)
	}
	if got.ID != entry.ID || got.TenantID != tenantID || got.Status != WaitlistWaiting {
		t.Fatalf("resolved entry mismatch: %+v", got)
	}

	// Unknown token → ErrNotFound (the handler maps it to 404).
	if _, err := st.GetWaitlistEntryByToken(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token err = %v, want ErrNotFound", err)
	}
}

func TestTeamMemberUserIDRoundtrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// newTestStore ran store.New BEFORE the test schema existed, so the
	// bootstrap ALTER found no team_members table; re-run it now that the
	// fixture table is in place (idempotent either way).
	if err := st.ensureTeamMemberUserIDColumn(ctx); err != nil {
		t.Fatalf("ensure user_id column: %v", err)
	}
	// Idempotent re-run is a no-op.
	if err := st.ensureTeamMemberUserIDColumn(ctx); err != nil {
		t.Fatalf("ensure user_id column (2nd): %v", err)
	}

	tenantID := uuid.New()
	uid := uuid.New()
	linked := TeamMember{TenantID: tenantID, Name: "Linked", Email: "linked@x.test", Role: "staff", Active: true, UserID: &uid}
	if err := st.CreateTeamMember(ctx, &linked); err != nil {
		t.Fatalf("create linked: %v", err)
	}
	plain := TeamMember{TenantID: tenantID, Name: "Plain", Email: "plain@x.test", Role: "staff", Active: true}
	if err := st.CreateTeamMember(ctx, &plain); err != nil {
		t.Fatalf("create plain: %v", err)
	}

	got, err := st.GetTeamMember(ctx, tenantID, linked.ID)
	if err != nil {
		t.Fatalf("get linked: %v", err)
	}
	if got.UserID == nil || *got.UserID != uid {
		t.Fatalf("linked user_id = %v, want %s", got.UserID, uid)
	}
	got, err = st.GetTeamMember(ctx, tenantID, plain.ID)
	if err != nil {
		t.Fatalf("get plain: %v", err)
	}
	if got.UserID != nil {
		t.Fatalf("plain user_id = %v, want nil", got.UserID)
	}

	members, err := st.ListTeamMembers(ctx, tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("list len = %d, want 2", len(members))
	}
	for _, m := range members {
		switch m.Name {
		case "Linked":
			if m.UserID == nil || *m.UserID != uid {
				t.Fatalf("list linked user_id = %v", m.UserID)
			}
		case "Plain":
			if m.UserID != nil {
				t.Fatalf("list plain user_id = %v, want nil", m.UserID)
			}
		}
	}
}
