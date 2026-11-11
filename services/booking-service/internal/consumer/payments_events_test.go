package consumer

// SPEC-W49 (W49-B items B1/B4) consumer tests — embedded Postgres (dedicated
// port 5591, -short skips): captured→paid happy path, idempotent replay,
// unknown tx_ref ack (no retry-loop on poison), amount mismatch path,
// tenant isolation, malformed/poison envelopes. Process is exercised
// directly (no Kafka broker needed — the reader is nil).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

const w49ConsumerTestPort = 5591

// w49ConsumerSchema is the minimal slice the payments-events consumer tests
// need (bookings carries the W48 payment columns directly; the W49 attempts
// table is created by store.New's ensure* bootstrap).
const w49ConsumerSchema = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE IF NOT EXISTS offerings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    duration_min INTEGER NOT NULL,
    buffer_min INTEGER NOT NULL DEFAULT 0,
    price_cents INTEGER NOT NULL DEFAULT 0,
    currency CHAR(3) NOT NULL DEFAULT 'USD',
    capacity INTEGER NOT NULL DEFAULT 1,
    bookable BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS bookings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    offering_id UUID NOT NULL,
    team_member_id UUID,
    contact_id UUID,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    source TEXT NOT NULL DEFAULT 'api',
    idempotency_key TEXT,
    payment_ref TEXT NOT NULL DEFAULT '',
    payment_status TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    sent_at TIMESTAMPTZ
);`

type w49ConsumerFixture struct {
	t   *testing.T
	st  *store.Store
	raw *pgx.Conn
	c   *PaymentsEventsConsumer
}

func newW49ConsumerFixture(t *testing.T) *w49ConsumerFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres consumer test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_w49_consumer_test").
		Port(w49ConsumerTestPort).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5591/booking_w49_consumer_test?sslmode=disable"
	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) }) //nolint:errcheck
	if _, err := conn.Exec(ctx, w49ConsumerSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	c := &PaymentsEventsConsumer{store: st, eventsTopic: "test.booking.events", log: zap.NewNop()}
	return &w49ConsumerFixture{t: t, st: st, raw: conn, c: c}
}

// seed inserts one offering (75.00 NGN) + one initialized-payment booking,
// returning (tenantID, bookingID, txRef).
func (f *w49ConsumerFixture) seed() (uuid.UUID, uuid.UUID, string) {
	f.t.Helper()
	ctx := context.Background()
	tenantID := uuid.New()
	off := store.Offering{TenantID: tenantID, Name: "Consult", DurationMin: 30,
		PriceCents: 7500, Currency: "NGN", Bookable: true}
	if err := f.st.CreateOffering(ctx, &off); err != nil {
		f.t.Fatalf("offering: %v", err)
	}
	bookingID := uuid.New()
	if _, err := f.raw.Exec(ctx,
		`INSERT INTO bookings (id, tenant_id, offering_id, starts_at, ends_at, status, source)
		 VALUES ($1,$2,$3,now() + interval '1 hour', now() + interval '2 hours', 'pending', 'api')`,
		bookingID, tenantID, off.ID); err != nil {
		f.t.Fatalf("seed booking: %v", err)
	}
	txRef := "fw-" + uuid.NewString()
	if err := f.st.InitializeBookingPaymentTx(ctx, tenantID, bookingID, txRef, "https://pay.test/x"); err != nil {
		f.t.Fatalf("init payment: %v", err)
	}
	return tenantID, bookingID, txRef
}

func (f *w49ConsumerFixture) event(eventType string, tenantID uuid.UUID, txRef string, amount float64, currency string) []byte {
	f.t.Helper()
	raw, err := json.Marshal(map[string]any{
		"specversion": "1.0",
		"id":          uuid.NewString(),
		"source":      "payments-service",
		"type":        eventType,
		"tenantid":    tenantID.String(),
		"time":        time.Now().UTC().Format(time.RFC3339),
		"data": map[string]any{
			"tx_ref":      txRef,
			"amount":      amount,
			"currency":    currency,
			"tenant_id":   tenantID.String(),
			"captured_at": time.Now().UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return raw
}

func (f *w49ConsumerFixture) outboxRows(bookingID uuid.UUID) []map[string]any {
	f.t.Helper()
	rows, err := f.st.FetchUnsentOutbox(context.Background(), 100)
	if err != nil {
		f.t.Fatalf("fetch outbox: %v", err)
	}
	var out []map[string]any
	for _, e := range rows {
		if e.AggregateID != bookingID {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			f.t.Fatalf("outbox payload: %v", err)
		}
		if e.Topic != "test.booking.events" {
			f.t.Fatalf("outbox topic = %q, want test.booking.events", e.Topic)
		}
		out = append(out, payload)
	}
	return out
}

func TestPaymentsEvents_CapturedMarksPaid(t *testing.T) {
	f := newW49ConsumerFixture(t)
	tenantID, bookingID, txRef := f.seed()
	ctx := context.Background()

	// Happy path (prefixed CloudEvent type form).
	if err := f.c.Process(ctx, f.event("com.opendesk.payments.FlutterwavePaymentCaptured", tenantID, txRef, 75.00, "NGN")); err != nil {
		t.Fatalf("process: %v", err)
	}
	status, ref, err := f.st.GetBookingPayment(ctx, tenantID, bookingID)
	if err != nil || status != store.PaymentStatusPaid || ref != txRef {
		t.Fatalf("payment state = %q/%q, %v; want paid/%s", status, ref, err, txRef)
	}
	// BookingPaymentConfirmed outbox row with the contract payload.
	rows := f.outboxRows(bookingID)
	if len(rows) != 1 {
		t.Fatalf("outbox rows = %d, want 1", len(rows))
	}
	evt := rows[0]
	if evt["type"] != BookingPaymentConfirmedEventType {
		t.Fatalf("event type = %v, want %s", evt["type"], BookingPaymentConfirmedEventType)
	}
	data, _ := evt["data"].(map[string]any)
	if data["booking_id"] != bookingID.String() || data["tenant_id"] != tenantID.String() ||
		data["tx_ref"] != txRef || data["amount"] != 75.00 {
		t.Fatalf("event data = %v", data)
	}

	// Idempotent replay (unprefixed type form): acked, no state churn, no
	// second outbox row.
	if err := f.c.Process(ctx, f.event(PaymentsCapturedEventType, tenantID, txRef, 75.00, "NGN")); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if rows := f.outboxRows(bookingID); len(rows) != 1 {
		t.Fatalf("after replay outbox rows = %d, want 1", len(rows))
	}
}

func TestPaymentsEvents_UnknownTxRefAcked(t *testing.T) {
	f := newW49ConsumerFixture(t)
	tenantID, bookingID, txRef := f.seed()
	ctx := context.Background()

	// Unknown tx_ref → nil error (ack), no state change, no outbox.
	if err := f.c.Process(ctx, f.event(PaymentsCapturedEventType, tenantID, "fw-unknown", 75.00, "NGN")); err != nil {
		t.Fatalf("unknown tx_ref must be acked (nil), got %v", err)
	}
	status, ref, err := f.st.GetBookingPayment(ctx, tenantID, bookingID)
	if err != nil || status != store.PaymentStatusInitialized || ref != txRef {
		t.Fatalf("payment state = %q/%q, %v; want untouched initialized", status, ref, err)
	}
	if rows := f.outboxRows(bookingID); len(rows) != 0 {
		t.Fatalf("outbox rows = %d, want 0", len(rows))
	}

	// Tenant isolation: the real tx_ref under a WRONG tenant is equally
	// unknown — acked, state untouched.
	if err := f.c.Process(ctx, f.event(PaymentsCapturedEventType, uuid.New(), txRef, 75.00, "NGN")); err != nil {
		t.Fatalf("cross-tenant capture must be acked (nil), got %v", err)
	}
	status, _, err = f.st.GetBookingPayment(ctx, tenantID, bookingID)
	if err != nil || status != store.PaymentStatusInitialized {
		t.Fatalf("payment state = %q, %v; want still initialized", status, err)
	}
}

func TestPaymentsEvents_AmountMismatch(t *testing.T) {
	f := newW49ConsumerFixture(t)
	tenantID, bookingID, txRef := f.seed()
	ctx := context.Background()

	// Captured 70.00 vs the 75.00 offering price → paid_amount_mismatch +
	// alert event, still acked.
	if err := f.c.Process(ctx, f.event(PaymentsCapturedEventType, tenantID, txRef, 70.00, "NGN")); err != nil {
		t.Fatalf("mismatch process: %v", err)
	}
	status, _, err := f.st.GetBookingPayment(ctx, tenantID, bookingID)
	if err != nil || status != store.PaymentStatusPaidAmountMismatch {
		t.Fatalf("state = %q, %v; want paid_amount_mismatch", status, err)
	}
	rows := f.outboxRows(bookingID)
	if len(rows) != 1 || rows[0]["type"] != BookingPaymentAmountMismatchEventType {
		t.Fatalf("outbox = %v, want one %s alert", rows, BookingPaymentAmountMismatchEventType)
	}

	// Replay: acked no-op, no duplicate alert.
	if err := f.c.Process(ctx, f.event(PaymentsCapturedEventType, tenantID, txRef, 70.00, "NGN")); err != nil {
		t.Fatalf("mismatch replay: %v", err)
	}
	if rows := f.outboxRows(bookingID); len(rows) != 1 {
		t.Fatalf("after replay outbox = %d, want 1", len(rows))
	}
}

func TestPaymentsEvents_PoisonAcked(t *testing.T) {
	f := newW49ConsumerFixture(t)
	ctx := context.Background()

	// Malformed JSON, missing fields, wrong event type: all acked (nil).
	for name, raw := range map[string][]byte{
		"malformed":     []byte("{not json"),
		"missing_txref": []byte(`{"type":"FlutterwavePaymentCaptured","data":{"amount":1,"currency":"NGN","tenant_id":"` + uuid.NewString() + `"}}`),
		"bad_tenant":    f.event(PaymentsCapturedEventType, uuid.Nil, "fw-x", 1, "NGN"), // tenant_id "00000000-..." parses but matches nothing → ack
		"other_type":    []byte(`{"type":"com.opendesk.payments.FlutterwavePaymentInitialized","data":{"tx_ref":"fw-x"}}`),
	} {
		if err := f.c.Process(ctx, raw); err != nil {
			t.Fatalf("%s: poison must be acked (nil), got %v", name, err)
		}
	}
}

// TestPaymentsEvents_ProcessWithRetryAcksPoison guards the loop discipline:
// poison (unknown tx_ref) returns nil from processWithRetry on the FIRST
// pass — no retries burned on undeliverable events.
func TestPaymentsEvents_ProcessWithRetryAcksPoison(t *testing.T) {
	f := newW49ConsumerFixture(t)
	tenantID, _, _ := f.seed()
	raw := f.event(PaymentsCapturedEventType, tenantID, "fw-poison", 75.00, "NGN")
	start := time.Now()
	if err := f.c.processWithRetry(context.Background(), raw); err != nil {
		t.Fatalf("processWithRetry: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("poison event burned retries: %v elapsed", elapsed)
	}
}
