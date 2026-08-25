package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// CreateBookingTx writes the booking event AND any extra outbox rows (usage
// metering, Wave 5 #9) atomically in one transaction.
func TestCreateBookingTxWritesExtraOutboxRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	booking := Booking{
		ID:           uuid.New(),
		TenantID:     tenantID,
		OfferingID:   uuid.New(),
		TeamMemberID: uuid.New(),
		ContactID:    uuid.New(),
		StartsAt:     time.Now().Add(24 * time.Hour),
		EndsAt:       time.Now().Add(25 * time.Hour),
		Status:       StatusPending,
		Source:       "api",
	}
	extra := []ExtraOutbox{
		{Topic: "opendesk.usage.events", Payload: []byte(`{"type":"com.opendesk.usage.UsageRecord"}`)},
		{Topic: "", Payload: []byte(`skipped`)}, // empty topic skipped
		{Topic: "also-skipped", Payload: nil},   // nil payload skipped
	}
	err := st.CreateBookingTx(ctx, &booking, SlotGuard{}, "opendesk.booking.events", []byte(`{"type":"com.opendesk.booking.BookingCreated"}`), extra...)
	if err != nil {
		t.Fatalf("CreateBookingTx: %v", err)
	}

	rows, err := st.FetchUnsentOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("FetchUnsentOutbox: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("outbox rows = %d, want 2 (booking event + usage record)", len(rows))
	}
	topics := map[string]bool{}
	for _, r := range rows {
		if r.AggregateID != booking.ID {
			t.Fatalf("aggregate_id = %v, want booking id %v", r.AggregateID, booking.ID)
		}
		topics[r.Topic] = true
	}
	if !topics["opendesk.booking.events"] || !topics["opendesk.usage.events"] {
		t.Fatalf("topics = %v", topics)
	}
}

// SetBookingStatus joins extra outbox rows to the status update atomically.
func TestSetBookingStatusWritesExtraOutboxRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	booking := Booking{
		ID:           uuid.New(),
		TenantID:     tenantID,
		OfferingID:   uuid.New(),
		TeamMemberID: uuid.New(),
		StartsAt:     time.Now().Add(24 * time.Hour),
		EndsAt:       time.Now().Add(25 * time.Hour),
		Status:       StatusPending,
		Source:       "api",
	}
	if err := st.CreateBookingTx(ctx, &booking, SlotGuard{}, "opendesk.booking.events", []byte(`{}`)); err != nil {
		t.Fatalf("CreateBookingTx: %v", err)
	}

	extra := []ExtraOutbox{{Topic: "opendesk.usage.events", Payload: []byte(`{"type":"com.opendesk.usage.UsageRecord"}`)}}
	if err := st.SetBookingStatus(ctx, tenantID, booking.ID, StatusConfirmed, "opendesk.booking.events", []byte(`{}`), extra...); err != nil {
		t.Fatalf("SetBookingStatus: %v", err)
	}

	got, err := st.GetBooking(ctx, tenantID, booking.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusConfirmed {
		t.Fatalf("status = %q, want confirmed", got.Status)
	}
	rows, err := st.FetchUnsentOutbox(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 { // created + confirmed + usage
		t.Fatalf("outbox rows = %d, want 3", len(rows))
	}
	// Order-independent: outbox ids are random UUIDs, so FetchUnsentOutbox's
	// ORDER BY id is NOT insertion order.
	topics := map[string]int{}
	for _, r := range rows {
		topics[r.Topic]++
	}
	if topics["opendesk.usage.events"] != 1 || topics["opendesk.booking.events"] != 2 {
		t.Fatalf("outbox topics = %v, want 2 booking events + 1 usage record", topics)
	}
}
