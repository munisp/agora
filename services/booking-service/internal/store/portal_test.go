package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Portal token lifecycle against embedded Postgres (Wave 5 #7).

func TestPortalTokenLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	contact := Contact{TenantID: tenantID, Name: "Pia", Phone: "+15550999"}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}

	tok := PortalToken{
		TenantID:  tenantID,
		ContactID: contact.ID,
		TokenHash: "hash-1",
		Channel:   PortalChannelSMS,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	if err := st.CreatePortalToken(ctx, &tok); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if tok.ID == uuid.Nil || tok.CreatedAt.IsZero() {
		t.Fatal("expected generated id + created_at")
	}

	got, err := st.GetActivePortalToken(ctx, tenantID, contact.ID)
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if got.ID != tok.ID || got.TokenHash != "hash-1" || got.Attempts != 0 {
		t.Fatalf("unexpected token %+v", got)
	}

	// Failed attempts count up.
	for i := 1; i <= 2; i++ {
		n, err := st.IncrementPortalTokenAttempts(ctx, tenantID, tok.ID)
		if err != nil {
			t.Fatalf("increment: %v", err)
		}
		if n != i {
			t.Fatalf("attempts = %d, want %d", n, i)
		}
	}

	// Consume; afterwards no active token remains.
	if err := st.ConsumePortalToken(ctx, tenantID, tok.ID); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if _, err := st.GetActivePortalToken(ctx, tenantID, contact.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("active after consume err = %v, want ErrNotFound", err)
	}
	// Consuming twice is not possible.
	if err := st.ConsumePortalToken(ctx, tenantID, tok.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-consume err = %v, want ErrNotFound", err)
	}
}

func TestPortalTokenExpiryAndTenantIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	contact := Contact{TenantID: tenantID, Name: "Eli", Phone: "+15550888"}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}

	// Expired tokens are never active.
	expired := PortalToken{
		TenantID: tenantID, ContactID: contact.ID, TokenHash: "old",
		Channel: PortalChannelSMS, ExpiresAt: time.Now().Add(-time.Minute),
	}
	if err := st.CreatePortalToken(ctx, &expired); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetActivePortalToken(ctx, tenantID, contact.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired token active, err = %v", err)
	}

	// A valid token is invisible to another tenant.
	valid := PortalToken{
		TenantID: tenantID, ContactID: contact.ID, TokenHash: "new",
		Channel: PortalChannelSMS, ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	if err := st.CreatePortalToken(ctx, &valid); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetActivePortalToken(ctx, uuid.New(), contact.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant token visible, err = %v", err)
	}
}

func TestGetContactByChannel(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	c := Contact{TenantID: tenantID, Name: "Nia", Phone: "+15550777", Email: "Nia@Example.com"}
	if err := st.CreateContact(ctx, &c); err != nil {
		t.Fatal(err)
	}
	byPhone, err := st.GetContactByChannel(ctx, tenantID, PortalChannelSMS, "+15550777")
	if err != nil || byPhone.ID != c.ID {
		t.Fatalf("phone lookup: %v %+v", err, byPhone)
	}
	// e-mail matches case-insensitively.
	byMail, err := st.GetContactByChannel(ctx, tenantID, PortalChannelEmail, "nia@example.COM")
	if err != nil || byMail.ID != c.ID {
		t.Fatalf("email lookup: %v %+v", err, byMail)
	}
	if _, err := st.GetContactByChannel(ctx, tenantID, PortalChannelSMS, "+100"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown phone err = %v, want ErrNotFound", err)
	}
	// Tenant isolation: same phone under another tenant does not resolve.
	if _, err := st.GetContactByChannel(ctx, uuid.New(), PortalChannelSMS, "+15550777"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant contact visible, err = %v", err)
	}
}

// SPEC-W81 B1: the portal_tokens channel CHECK gains 'whatsapp' (purely
// additive). A whatsapp token round-trips, contact resolution uses the
// phone column, and unknown channels remain rejected by the constraint.
func TestPortalTokenWhatsAppChannel(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	contact := Contact{TenantID: tenantID, Name: "Wale", Phone: "+2348012345678"}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}

	tok := PortalToken{
		TenantID: tenantID, ContactID: contact.ID, TokenHash: "hash-wa",
		Channel: PortalChannelWhatsApp, ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	if err := st.CreatePortalToken(ctx, &tok); err != nil {
		t.Fatalf("create whatsapp token: %v", err)
	}
	got, err := st.GetActivePortalToken(ctx, tenantID, contact.ID)
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if got.ID != tok.ID || got.Channel != PortalChannelWhatsApp {
		t.Fatalf("unexpected token %+v", got)
	}

	// WhatsApp is phone-addressed: contact resolution uses the phone column.
	byWA, err := st.GetContactByChannel(ctx, tenantID, PortalChannelWhatsApp, "+2348012345678")
	if err != nil || byWA.ID != contact.ID {
		t.Fatalf("whatsapp contact lookup: %v %+v", err, byWA)
	}

	// The enum is widened, not dropped: unknown channels still fail the CHECK.
	bad := PortalToken{
		TenantID: tenantID, ContactID: contact.ID, TokenHash: "hash-bad",
		Channel: "pigeon", ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	if err := st.CreatePortalToken(ctx, &bad); err == nil {
		t.Fatal("channel pigeon accepted — CHECK constraint must reject it")
	}
}

// SPEC-W81 B1: the bootstrap DDL is re-runnable (W32 DROP + re-ADD idiom)
// and the widened enum holds after re-runs; sms/email are unaffected.
func TestPortalTokensChannelConstraintIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := st.ensurePortalTokensTable(ctx); err != nil {
			t.Fatalf("bootstrap re-run %d: %v", i+1, err)
		}
	}

	// The named constraint exists once, with the full enum.
	var def string
	if err := st.pool.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conrelid = 'portal_tokens'::regclass AND conname = 'portal_tokens_channel_check'`).Scan(&def); err != nil {
		t.Fatalf("constraint lookup: %v", err)
	}
	for _, ch := range []string{"sms", "email", "whatsapp"} {
		if !strings.Contains(def, ch) {
			t.Fatalf("constraint %q missing channel %q", def, ch)
		}
	}

	// All three channels insert after the re-runs.
	for _, ch := range []string{PortalChannelSMS, PortalChannelEmail, PortalChannelWhatsApp} {
		tok := PortalToken{
			TenantID: uuid.New(), ContactID: uuid.New(), TokenHash: "h-" + ch,
			Channel: ch, ExpiresAt: time.Now().Add(10 * time.Minute),
		}
		if err := st.CreatePortalToken(ctx, &tok); err != nil {
			t.Fatalf("create %s token after re-run: %v", ch, err)
		}
	}
}
