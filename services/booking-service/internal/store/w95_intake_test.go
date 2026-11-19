package store

// SPEC-W95 tests: intake_submissions — upsert semantics (resub detection),
// payload replacement, transactional outbox row, tenant isolation.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestSubmitIntakeTxRoundTrip_W95(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	bookingID := uuid.New()

	// Get with no submission: ErrNotFound.
	if _, err := st.GetIntakeSubmission(ctx, tenantID, bookingID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound before submit, got %v", err)
	}

	// First submit: resub=false, outbox row written in the same tx.
	payload := json.RawMessage(`{"allergies":"penicillin","notes":"first visit"}`)
	resub, err := st.SubmitIntakeTx(ctx, tenantID, bookingID, payload, "opendesk.booking.events", []byte(`{"type":"com.opendesk.booking.IntakeCompleted"}`))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if resub {
		t.Fatal("first submit reported resub=true")
	}

	// Round-trip.
	got, err := st.GetIntakeSubmission(ctx, tenantID, bookingID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BookingID != bookingID || got.TenantID != tenantID {
		t.Fatalf("round-trip ids = %v/%v, want %v/%v", got.TenantID, got.BookingID, tenantID, bookingID)
	}
	var fields map[string]string
	if err := json.Unmarshal(got.Payload, &fields); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if fields["allergies"] != "penicillin" {
		t.Fatalf("payload = %v", fields)
	}
	if got.SubmittedAt.IsZero() {
		t.Fatal("submitted_at is zero")
	}

	// Outbox row present (aggregate = booking).
	rows, err := st.FetchUnsentOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("FetchUnsentOutbox: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %d, want 1", len(rows))
	}
	if rows[0].AggregateID != bookingID || rows[0].Topic != "opendesk.booking.events" {
		t.Fatalf("outbox row = %+v", rows[0])
	}

	// Resubmit: resub=true, payload replaced, second outbox row appended.
	firstSubmittedAt := got.SubmittedAt
	payload2 := json.RawMessage(`{"allergies":"none","notes":"updated"}`)
	resub, err = st.SubmitIntakeTx(ctx, tenantID, bookingID, payload2, "opendesk.booking.events", []byte(`{"type":"com.opendesk.booking.IntakeCompleted"}`))
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if !resub {
		t.Fatal("resubmit reported resub=false")
	}
	got, err = st.GetIntakeSubmission(ctx, tenantID, bookingID)
	if err != nil {
		t.Fatalf("get after resubmit: %v", err)
	}
	if err := json.Unmarshal(got.Payload, &fields); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if fields["allergies"] != "none" {
		t.Fatalf("payload not replaced: %v", fields)
	}
	if got.SubmittedAt.Before(firstSubmittedAt) {
		t.Fatal("submitted_at moved backwards on resubmit")
	}
	rows, err = st.FetchUnsentOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("FetchUnsentOutbox: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("outbox rows after resubmit = %d, want 2", len(rows))
	}
}

func TestIntakeSubmissionTenantIsolation_W95(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()
	bookingID := uuid.New()

	// Same booking id under TWO tenants; B must not read A's submission.
	if _, err := st.SubmitIntakeTx(ctx, tenantA, bookingID, json.RawMessage(`{"a":"1"}`), "opendesk.booking.events", []byte(`{}`)); err != nil {
		t.Fatalf("submit A: %v", err)
	}
	if _, err := st.GetIntakeSubmission(ctx, tenantB, bookingID); err != ErrNotFound {
		t.Fatalf("tenant B read tenant A submission, err = %v", err)
	}

	// B can still submit under the same booking id (the UNIQUE key is
	// (tenant_id, booking_id), not booking_id alone).
	resub, err := st.SubmitIntakeTx(ctx, tenantB, bookingID, json.RawMessage(`{"b":"2"}`), "opendesk.booking.events", []byte(`{}`))
	if err != nil {
		t.Fatalf("submit B: %v", err)
	}
	if resub {
		t.Fatal("tenant B first submit reported resub=true")
	}
	got, err := st.GetIntakeSubmission(ctx, tenantA, bookingID)
	if err != nil {
		t.Fatalf("tenant A get: %v", err)
	}
	var fields map[string]string
	if err := json.Unmarshal(got.Payload, &fields); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if fields["a"] != "1" {
		t.Fatalf("tenant A payload clobbered: %v", fields)
	}
}
