package bookingops

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// SPEC-W44 W-B (K5): SagaInput.TenantSlug rides the `tenant_slug` JSON
// field and is populated at BOTH saga start sites — Service.startSaga
// (ops.go, in.TenantSlug) and the sweeper's redrive (sweeper.go, via
// ResolveSlug). This file pins that wiring so a refactor cannot silently
// drop the slug (events use it as the CloudEvent subject).

// Payload field-name assertion: the struct tag must stay `tenant_slug`
// (the workflow payload contract — Temporal serializes SagaInput as JSON).
func TestSagaInputTenantSlugJSONField(t *testing.T) {
	raw, err := json.Marshal(SagaInput{
		BookingID:  uuid.NewString(),
		TenantID:   uuid.NewString(),
		TenantSlug: "acme-ng",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["tenant_slug"] != "acme-ng" {
		t.Fatalf("SagaInput JSON must carry tenant_slug (K5): %s", raw)
	}
}

// Sweeper ResolveSlug path: a redriven stale-pending booking must start its
// saga with TenantSlug resolved via ResolveSlug (the sites registry).
func TestSweeperRedriveCarriesTenantSlug(t *testing.T) {
	st, ctx := newSweeperFixture(t)
	tenantID := uuid.New()
	offering := store.Offering{TenantID: tenantID, Name: "Cut", DurationMin: 30, PriceCents: 5000, Currency: "NGN"}
	if err := st.CreateOffering(ctx, &offering); err != nil {
		t.Fatal(err)
	}
	contact := store.Contact{TenantID: tenantID, Name: "Ada", Phone: "+234800"}
	if err := st.CreateContact(ctx, &contact); err != nil {
		t.Fatal(err)
	}
	pending := addSweeperBooking(t, ctx, st, tenantID, offering, contact, store.StatusPending, 1)

	saga := &recordingSaga{}
	var resolvedFor uuid.UUID
	sw := &Sweeper{
		Store: st, Saga: saga, Log: zap.NewNop(), MinAge: time.Millisecond,
		ResolveSlug: func(_ context.Context, tid uuid.UUID) string {
			resolvedFor = tid
			return "acme-ng"
		},
	}
	time.Sleep(5 * time.Millisecond) // let the seeded row age past MinAge

	n, err := sw.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("redriven = %d, want 1", n)
	}
	calls := saga.started()
	if len(calls) != 1 {
		t.Fatalf("saga starts = %d, want 1", len(calls))
	}
	if resolvedFor != tenantID {
		t.Fatalf("ResolveSlug called for %s, want %s", resolvedFor, tenantID)
	}
	if calls[0].TenantSlug != "acme-ng" {
		t.Fatalf("sweeper saga TenantSlug = %q, want acme-ng (K5)", calls[0].TenantSlug)
	}
	if calls[0].BookingID != pending.ID.String() {
		t.Fatalf("redriven booking = %s, want %s", calls[0].BookingID, pending.ID)
	}
}
