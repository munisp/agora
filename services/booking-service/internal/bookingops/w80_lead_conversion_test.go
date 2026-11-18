package bookingops

// SPEC-W80 B3: booking→lead conversion — the LeadConverter hook fires on
// the contact's FIRST completed booking with the contact's phone, never on
// an idempotent replay and never on a SECOND completion (a repeat customer
// is not a fresh conversion). Nil hook = no-op (covered by the existing
// completion tests, which wire no LeadConverter).

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// recordingLeadConverter is the OpenLeadConverter test double.
type recordingLeadConverter struct {
	mu     sync.Mutex
	phones []string
	slugs  []string
}

func (r *recordingLeadConverter) ConvertOpenLeadByPhone(_ context.Context, _ uuid.UUID, phone, tenantSlug string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phones = append(r.phones, phone)
	r.slugs = append(r.slugs, tenantSlug)
}

func (r *recordingLeadConverter) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.phones...)
}

// w80ConfirmedBooking seeds offering + contact + one confirmed booking and
// returns the pieces (completeFixture idiom with the contact exposed).
func w80ConfirmedBooking(t *testing.T, st *store.Store, ctx context.Context, tenantID uuid.UUID, phone string, offsetHours int) (store.Offering, store.Contact, store.Booking) {
	t.Helper()
	offering := store.Offering{TenantID: tenantID, Name: "Cut", DurationMin: 30, PriceCents: 5000, Currency: "NGN"}
	if err := st.CreateOffering(ctx, &offering); err != nil {
		t.Fatal(err)
	}
	contact := store.Contact{TenantID: tenantID, Name: "Ada", Phone: phone}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}
	b := addSweeperBooking(t, ctx, st, tenantID, offering, contact, store.StatusConfirmed, offsetHours)
	return offering, contact, b
}

func TestCompleteConvertsLeadOnFirstCompletionOnly(t *testing.T) {
	st, ctx := newSweeperFixture(t)
	tenantID := uuid.New()
	hook := &recordingLeadConverter{}
	svc := &Service{
		Store:         st,
		EventsTopic:   "booking.events",
		Logger:        zap.NewNop(),
		LeadConverter: hook,
	}

	_, _, b1 := w80ConfirmedBooking(t, st, ctx, tenantID, "+234800", 5)
	res, err := svc.Complete(ctx, tenantID, "acme-ng", b1.ID, nil, CallerIdentity{})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if res.AlreadyCompleted {
		t.Fatal("first complete must not be a replay")
	}
	if got := hook.calls(); len(got) != 1 || got[0] != "+234800" {
		t.Fatalf("hook calls after first completion = %v, want exactly [+234800]", got)
	}

	// Idempotent replay: no second conversion.
	if _, err := svc.Complete(ctx, tenantID, "acme-ng", b1.ID, nil, CallerIdentity{}); err != nil {
		t.Fatalf("replay complete: %v", err)
	}
	if got := hook.calls(); len(got) != 1 {
		t.Fatalf("hook calls after replay = %d, want still 1", len(got))
	}

	// A SECOND completed booking for the same contact: the customer is no
	// longer converting for the first time — the hook must stay silent.
	_, _, b2 := w80ConfirmedBooking2(t, st, ctx, tenantID, b1)
	if _, err := svc.Complete(ctx, tenantID, "acme-ng", b2.ID, nil, CallerIdentity{}); err != nil {
		t.Fatalf("second complete: %v", err)
	}
	if got := hook.calls(); len(got) != 1 {
		t.Fatalf("hook calls after second completion = %d, want still 1 (not a first conversion)", len(got))
	}
}

// w80ConfirmedBooking2 seeds a second confirmed booking reusing the first
// booking's offering + contact.
func w80ConfirmedBooking2(t *testing.T, st *store.Store, ctx context.Context, tenantID uuid.UUID, first store.Booking) (store.Offering, store.Contact, store.Booking) {
	t.Helper()
	offering, err := st.GetOffering(ctx, tenantID, first.OfferingID)
	if err != nil {
		t.Fatal(err)
	}
	contact, err := st.GetContact(ctx, tenantID, first.ContactID)
	if err != nil {
		t.Fatal(err)
	}
	b := addSweeperBooking(t, ctx, st, tenantID, offering, contact, store.StatusConfirmed, 48)
	return offering, contact, b
}
