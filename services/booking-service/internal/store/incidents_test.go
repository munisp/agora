package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// SPEC-W11 Part B store tests run against embedded Postgres (same harness
// as the waitlist/CRM tests; STORE_TEST=0 / -short skips).

// Ingest idempotency: inserting the same incident_id twice yields exactly
// one row; the second insert reports created=false.
func TestInsertIncidentIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	in := Incident{
		ID:              uuid.New(),
		TenantID:        tenantID,
		ReferenceNumber: "INC-2026-000123",
		IncidentType:    "fire",
		Severity:        "critical",
		Payload:         []byte(`{"incident_id":"x","severity":"critical"}`),
	}
	created, err := st.InsertIncident(ctx, &in)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if !created {
		t.Fatal("first insert must report created=true")
	}
	if in.Status != "new" {
		t.Fatalf("status = %q, want new", in.Status)
	}

	dup := in
	dup.Payload = []byte(`{"replay":true}`)
	created, err = st.InsertIncident(ctx, &dup)
	if err != nil {
		t.Fatalf("duplicate insert: %v", err)
	}
	if created {
		t.Fatal("duplicate incident_id must report created=false")
	}

	got, err := st.GetIncident(ctx, tenantID, in.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// JSONB normalizes key order/whitespace — compare semantically.
	var gotPayload map[string]any
	if err := json.Unmarshal(got.Payload, &gotPayload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if gotPayload["incident_id"] != "x" || gotPayload["severity"] != "critical" || gotPayload["replay"] != nil {
		t.Fatalf("payload overwritten by replay: %s", got.Payload)
	}

	// Cross-tenant isolation (RLS): another tenant cannot see the row.
	if _, err := st.GetIncident(ctx, uuid.New(), in.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant get = %v, want ErrNotFound", err)
	}
}

// List filters: status + from/to bounds.
func TestListIncidentsFilters(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	mk := func(sev string) Incident {
		in := Incident{ID: uuid.New(), TenantID: tenantID, IncidentType: "other", Severity: sev,
			Payload: []byte(`{}`)}
		if _, err := st.InsertIncident(ctx, &in); err != nil {
			t.Fatalf("insert: %v", err)
		}
		return in
	}
	a := mk("low")
	mk("high")

	all, err := st.ListIncidents(ctx, tenantID, "", nil, nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("list all = %d, %v", len(all), err)
	}
	newOnly, err := st.ListIncidents(ctx, tenantID, "new", nil, nil)
	if err != nil || len(newOnly) != 2 {
		t.Fatalf("list status=new = %d, %v", len(newOnly), err)
	}
	if err := st.MarkIncidentDispatched(ctx, tenantID, a.ID); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	dispatched, err := st.ListIncidents(ctx, tenantID, "dispatched", nil, nil)
	if err != nil || len(dispatched) != 1 || dispatched[0].ID != a.ID {
		t.Fatalf("list status=dispatched = %+v, %v", dispatched, err)
	}
	if dispatched[0].DispatchedAt == nil {
		t.Fatal("dispatched_at must be stamped")
	}
	// Time bounds: future 'from' excludes everything.
	future := time.Now().Add(24 * time.Hour)
	none, err := st.ListIncidents(ctx, tenantID, "", &future, nil)
	if err != nil || len(none) != 0 {
		t.Fatalf("list from=future = %d, %v", len(none), err)
	}
	past := time.Now().Add(-time.Hour)
	some, err := st.ListIncidents(ctx, tenantID, "", &past, nil)
	if err != nil || len(some) != 2 {
		t.Fatalf("list from=past = %d, %v", len(some), err)
	}
}

// Dispatch endpoint CRUD + delivery ledger transitions.
func TestDispatchEndpointsAndDeliveries(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	ep := DispatchEndpoint{TenantID: tenantID, URL: "https://psap.example/hook", Secret: "s3", Active: true}
	if err := st.UpsertDispatchEndpoint(ctx, &ep); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Re-upsert (PK tenant+url) deactivates without duplicating.
	ep.Active = false
	if err := st.UpsertDispatchEndpoint(ctx, &ep); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	eps, err := st.ListDispatchEndpoints(ctx, tenantID, false)
	if err != nil || len(eps) != 1 {
		t.Fatalf("list = %d, %v", len(eps), err)
	}
	active, err := st.ListDispatchEndpoints(ctx, tenantID, true)
	if err != nil || len(active) != 0 {
		t.Fatalf("active list = %d, %v", len(active), err)
	}
	ep.Active = true
	if err := st.UpsertDispatchEndpoint(ctx, &ep); err != nil {
		t.Fatalf("reactivate: %v", err)
	}

	// Ledger: incident + delivery row, then retrying → delivered stamps.
	inc := Incident{ID: uuid.New(), TenantID: tenantID, IncidentType: "medical", Severity: "high", Payload: []byte(`{}`)}
	if _, err := st.InsertIncident(ctx, &inc); err != nil {
		t.Fatalf("incident: %v", err)
	}
	d := IncidentDelivery{TenantID: tenantID, IncidentID: inc.ID, EndpointURL: ep.URL}
	if err := st.InsertIncidentDelivery(ctx, &d); err != nil {
		t.Fatalf("delivery insert: %v", err)
	}
	// Deterministic-id duplicate insert is a no-op.
	if err := st.InsertIncidentDelivery(ctx, &d); err != nil {
		t.Fatalf("delivery duplicate insert: %v", err)
	}
	code := 500
	if err := st.UpdateIncidentDelivery(ctx, d.ID, "retrying", 1, &code, "boom"); err != nil {
		t.Fatalf("update retrying: %v", err)
	}
	rows, err := st.ListIncidentDeliveries(ctx, tenantID, inc.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ledger = %d, %v", len(rows), err)
	}
	if rows[0].Status != "retrying" || rows[0].Attempts != 1 || rows[0].LastStatusCode == nil || *rows[0].LastStatusCode != 500 {
		t.Fatalf("ledger after retrying: %+v", rows[0])
	}
	if rows[0].DeliveredAt != nil {
		t.Fatal("delivered_at must stay NULL while retrying")
	}
	code = 200
	if err := st.UpdateIncidentDelivery(ctx, d.ID, "delivered", 2, &code, ""); err != nil {
		t.Fatalf("update delivered: %v", err)
	}
	rows, _ = st.ListIncidentDeliveries(ctx, tenantID, inc.ID)
	if rows[0].Status != "delivered" || rows[0].DeliveredAt == nil {
		t.Fatalf("ledger after delivered: %+v", rows[0])
	}

	if err := st.DeleteDispatchEndpoint(ctx, tenantID, ep.URL); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeleteDispatchEndpoint(ctx, tenantID, ep.URL); err != ErrNotFound {
		t.Fatalf("re-delete = %v, want ErrNotFound", err)
	}
}
