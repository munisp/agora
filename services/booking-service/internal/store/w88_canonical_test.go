package store

// SPEC-W88: E.164 pin-canonicalization — the phone compare paths match on
// the pinned W61 G3 digits canonical form BOTH SIDES, so the WhatsApp
// digits-only wa_id form and the portal/OTP '+'-form of the SAME
// subscriber resolve to one contact. Storage stays RAW (no migration);
// only the compare changes. Redirection: removing mediaInboxDigitsSQL
// from GetContactByChannel/FindContactByPhoneOrEmail fails these tests.

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestW88GetContactByChannelCrossFormat(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	// Stored digits-only (the WhatsApp wa_id write form).
	stored := Contact{TenantID: tenantID, Name: "Ada", Phone: "2348012345678"}
	if err := st.CreateContact(ctx, &stored); err != nil {
		t.Fatal(err)
	}

	// '+'-form needle (SIP claim / OTP verify form) MUST resolve.
	got, err := st.GetContactByChannel(ctx, tenantID, PortalChannelSMS, "+234 801 234 5678")
	if err != nil {
		t.Fatalf("cross-format needle missed: %v", err)
	}
	if got.ID != stored.ID {
		t.Fatalf("resolved wrong contact: got %s want %s", got.ID, stored.ID)
	}
	// whatsapp channel is phone-addressed — same canonical match.
	got, err = st.GetContactByChannel(ctx, tenantID, PortalChannelWhatsApp, "+2348012345678")
	if err != nil || got.ID != stored.ID {
		t.Fatalf("whatsapp channel cross-format miss: %v", err)
	}

	// Reverse direction: '+'-form stored, digits-only needle (wa_id pin).
	stored2 := Contact{TenantID: tenantID, Name: "Bala", Phone: "+1 (415) 555-0132"}
	if err := st.CreateContact(ctx, &stored2); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetContactByChannel(ctx, tenantID, PortalChannelSMS, "14155550132")
	if err != nil || got.ID != stored2.ID {
		t.Fatalf("reverse cross-format miss: %v", err)
	}

	// Genuinely different numbers NEVER resolve (fail-closed compare).
	if _, err = st.GetContactByChannel(ctx, tenantID, PortalChannelSMS, "+2348099999999"); err != ErrNotFound {
		t.Fatalf("different number resolved: %v", err)
	}
	// National vs international forms do NOT fold (W61 ruling: trunk-0
	// kept, no country inference).
	if _, err = st.GetContactByChannel(ctx, tenantID, PortalChannelSMS, "08012345678"); err != ErrNotFound {
		t.Fatalf("national form folded: %v", err)
	}
	// Tenant isolation survives the canonical match.
	if _, err = st.GetContactByChannel(ctx, uuid.New(), PortalChannelSMS, "+2348012345678"); err != ErrNotFound {
		t.Fatalf("cross-tenant hit: %v", err)
	}
}

func TestW88GetContactByChannelGuards(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	// A phone-less contact row and a no-digit needle must NOT match
	// ('' = '' would otherwise be true).
	noPhone := Contact{TenantID: tenantID, Name: "NoPhone", Email: "nophone@example.com"}
	if err := st.CreateContact(ctx, &noPhone); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetContactByChannel(ctx, tenantID, PortalChannelSMS, "+++ ---"); err != ErrNotFound {
		t.Fatalf("no-digit needle matched: %v", err)
	}

	// E-mail branch is byte-identical behavior (case-insensitive exact).
	got, err := st.GetContactByChannel(ctx, tenantID, PortalChannelEmail, "NoPhone@Example.com")
	if err != nil || got.ID != noPhone.ID {
		t.Fatalf("email lookup regressed: %v", err)
	}
	if _, err = st.GetContactByChannel(ctx, tenantID, PortalChannelEmail, "other@example.com"); err != ErrNotFound {
		t.Fatalf("email miss regressed: %v", err)
	}
}

func TestW88FindContactByPhoneOrEmailCrossFormat(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	stored := Contact{TenantID: tenantID, Name: "Chidi", Phone: "+2348012345678", Email: "chidi@example.com"}
	if err := st.CreateContact(ctx, &stored); err != nil {
		t.Fatal(err)
	}

	// Digits-only needle (CRM payload in the wa_id form) matches the
	// '+'-form row — the dedupe path must NOT create a parallel contact.
	got, err := st.FindContactByPhoneOrEmail(ctx, tenantID, "2348012345678", "")
	if err != nil || got.ID != stored.ID {
		t.Fatalf("cross-format find missed: %v", err)
	}
	created, err := st.UpsertExternalContact(ctx, tenantID, &Contact{
		Name: "Chidi Updated", Phone: "2348012345678", Source: "twenty", ExternalID: "p1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("cross-format upsert created a duplicate contact")
	}
	// GetContact does not project source/external_id — re-read via the
	// CRM lookup which does (COALESCE(source,'')).
	merged, err := st.FindContactByPhoneOrEmail(ctx, tenantID, "+2348012345678", "")
	if err != nil {
		t.Fatal(err)
	}
	if merged.ID != stored.ID || merged.Name != "Chidi Updated" || merged.Source != "twenty" {
		t.Fatalf("merge did not land on the existing row: %+v", merged)
	}

	// No-digit phone needle never matches a phone-less row.
	if _, err = st.FindContactByPhoneOrEmail(ctx, tenantID, "(---)", ""); err != ErrNotFound {
		t.Fatalf("no-digit needle matched: %v", err)
	}
	// E-mail match unchanged.
	got, err = st.FindContactByPhoneOrEmail(ctx, tenantID, "", "chidi@example.com")
	if err != nil || got.ID != stored.ID {
		t.Fatalf("email find regressed: %v", err)
	}
}
