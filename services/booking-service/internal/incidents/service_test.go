package incidents

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// fakeStarter captures workflow starts (delivery + alert).
type fakeStarter struct {
	mu         sync.Mutex
	deliveries []DeliveryStart
	alerts     []AlertStart
	err        error
}

func (f *fakeStarter) StartIncidentDelivery(_ context.Context, in DeliveryStart) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliveries = append(f.deliveries, in)
	return "wf-" + in.DeliveryID, f.err
}

func (f *fakeStarter) StartIncidentAlert(_ context.Context, in AlertStart) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alerts = append(f.alerts, in)
	return "wf-alert-" + in.IncidentID, f.err
}

// newServiceTestStore boots embedded Postgres like internal/store tests
// (STORE_TEST=0 / -short skips).
func newServiceTestStore(t *testing.T) *store.Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres incidents service test in -short mode")
	}
	// Dedicated port + data dir avoids colliding with the internal/store and
	// httpapi portal fixtures when `go test ./...` runs packages in parallel
	// (DefaultConfig shares one port and one data dir — postmaster.pid race).
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_incidents_test").
		Port(5544).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })
	dsn := "postgres://postgres:postgres@localhost:5544/booking_incidents_test?sslmode=disable"
	// The outbox table is infra-managed (01-booking-schema.sql), not
	// bootstrapped by store.New — create it for the usage-metering assert.
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	defer conn.Close(ctx) //nolint:errcheck
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS outbox (
	    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	    aggregate_id UUID NOT NULL,
	    topic TEXT NOT NULL,
	    payload JSONB NOT NULL,
	    sent_at TIMESTAMPTZ
	)`); err != nil {
		t.Fatalf("outbox ddl: %v", err)
	}
	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// Dispatch happy path (SPEC-W11 Part B §3/§4): one ledger row + one
// WebhookDeliveryWorkflow start per ACTIVE endpoint, incident flipped to
// dispatched, deterministic delivery ids (re-dispatch idempotent).
func TestDispatchHappyPath(t *testing.T) {
	st := newServiceTestStore(t)
	fs := &fakeStarter{}
	svc := &Service{Store: st, Starter: fs, AutoDispatch: false, Log: zap.NewNop()}
	ctx := context.Background()
	tenantID := uuid.New()

	inactive := store.DispatchEndpoint{TenantID: tenantID, URL: "https://off.example/hook", Active: false}
	if err := st.UpsertDispatchEndpoint(ctx, &inactive); err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	ep := store.DispatchEndpoint{TenantID: tenantID, URL: "https://psap.example/hook", Secret: "s3", Active: true}
	if err := st.UpsertDispatchEndpoint(ctx, &ep); err != nil {
		t.Fatalf("endpoint: %v", err)
	}

	idp := (&IDP{TenantID: tenantID, IncidentType: "crime", Severity: "high"}).Complete()
	row, created, err := svc.Ingest(ctx, *idp, "acme-ng")
	if err != nil || !created {
		t.Fatalf("ingest: created=%v err=%v", created, err)
	}

	deliveries, err := svc.Dispatch(ctx, tenantID, row.ID)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %d, want 1 (inactive endpoint skipped)", len(deliveries))
	}
	d := deliveries[0]
	if d.EndpointURL != ep.URL || d.Status != "pending" {
		t.Fatalf("delivery row: %+v", d)
	}
	if d.ID != DeliveryID(row.ID, ep.URL) {
		t.Fatalf("delivery id not deterministic: %s", d.ID)
	}

	// Workflow start carries the incident payload contract.
	if len(fs.deliveries) != 1 {
		t.Fatalf("workflow starts = %d", len(fs.deliveries))
	}
	start := fs.deliveries[0]
	if start.PayloadType != PayloadTypeIncident || start.IncidentID != row.ID.String() ||
		start.Secret != "s3" || start.URL != ep.URL || start.DeliveryID != d.ID.String() {
		t.Fatalf("delivery start: %+v", start)
	}
	var roundtrip IDP
	if err := json.Unmarshal(start.Body, &roundtrip); err != nil {
		t.Fatalf("body is not the IDP: %v", err)
	}
	if roundtrip.IncidentID != idp.IncidentID || roundtrip.ReferenceNumber != idp.ReferenceNumber {
		t.Fatalf("IDP roundtrip mismatch: %+v", roundtrip)
	}

	// Incident flipped to dispatched.
	got, err := st.GetIncident(ctx, tenantID, row.ID)
	if err != nil || got.Status != "dispatched" || got.DispatchedAt == nil {
		t.Fatalf("incident after dispatch: %+v (%v)", got, err)
	}

	// Re-dispatch is idempotent: same ledger row id, no duplicate rows.
	if _, err := svc.Dispatch(ctx, tenantID, row.ID); err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	rows, err := st.ListIncidentDeliveries(ctx, tenantID, row.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ledger after re-dispatch = %d, %v", len(rows), err)
	}
}

// Ingest idempotency at the service level: the same IDP twice persists one
// row and triggers auto-dispatch + outreach exactly once.
func TestIngestIdempotentSideEffects(t *testing.T) {
	st := newServiceTestStore(t)
	fs := &fakeStarter{}
	svc := &Service{Store: st, Starter: fs, AutoDispatch: true, UsageTopic: "opendesk.usage.events", Log: zap.NewNop()}
	ctx := context.Background()
	tenantID := uuid.New()

	ep := store.DispatchEndpoint{TenantID: tenantID, URL: "https://psap.example/hook", Secret: "s3", Active: true}
	if err := st.UpsertDispatchEndpoint(ctx, &ep); err != nil {
		t.Fatalf("endpoint: %v", err)
	}

	phone := "+2348012345678"
	idp := (&IDP{
		TenantID:       tenantID,
		IncidentType:   "medical",
		Severity:       "critical",
		CallbackNumber: &phone,
		Channel:        ChannelVoice,
	}).Complete()

	_, created1, err := svc.Ingest(ctx, *idp, "acme-ng")
	if err != nil || !created1 {
		t.Fatalf("first ingest: created=%v err=%v", created1, err)
	}
	_, created2, err := svc.Ingest(ctx, *idp, "acme-ng")
	if err != nil {
		t.Fatalf("replay ingest: %v", err)
	}
	if created2 {
		t.Fatal("replay must report created=false")
	}
	if len(fs.deliveries) != 1 {
		t.Fatalf("auto-dispatch runs = %d, want 1", len(fs.deliveries))
	}
	if len(fs.alerts) != 1 {
		t.Fatalf("outreach runs = %d, want 1", len(fs.alerts))
	}

	// Outreach carries the priority-lane contract: sms (voice channel → sms
	// fallback), callback phone, rendered template.
	alert := fs.alerts[0]
	if alert.Channel != ChannelSMS || alert.Phone != phone || alert.IncidentID != idp.IncidentID.String() {
		t.Fatalf("alert: %+v", alert)
	}
	if alert.Text == "" || !strings.Contains(alert.Text, idp.ReferenceNumber) || !strings.Contains(alert.Text, "medical") {
		t.Fatalf("alert text must carry reference + type: %q", alert.Text)
	}

	// Metering: one usage outbox row for the priority send.
	outbox, err := st.FetchUnsentOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("fetch outbox: %v", err)
	}
	if len(outbox) != 1 || outbox[0].Topic != "opendesk.usage.events" {
		t.Fatalf("usage outbox rows = %+v", outbox)
	}
}
