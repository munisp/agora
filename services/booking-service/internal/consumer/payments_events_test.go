package consumer

// SPEC-W49 (W49-B items B1/B4) consumer tests — embedded Postgres (dedicated
// port 5591, -short skips): captured→paid happy path, idempotent replay,
// unknown tx_ref ack (no retry-loop on poison), amount mismatch path,
// tenant isolation, malformed/poison envelopes. Process is exercised
// directly (no Kafka broker needed — the reader is nil).
//
// SPEC-W50 (W50-A items A1/A2/A3): superseded-link fallback (capture on an
// OLD checkout ref resolved via booking_payment_attempts — booking paid,
// payment_ref advanced, attempt marked, exactly-once replay, mismatch and
// cross-tenant paths) and the enriched BookingPaymentConfirmed contract
// payload asserted field-by-field (amount_minor int64, customer/contact/
// booking_ref, paid_at).

import (
	"context"
	"encoding/json"
	"regexp"
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
CREATE TABLE IF NOT EXISTS contacts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    phone TEXT,
    email TEXT,
    notes TEXT NOT NULL DEFAULT ''
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
    public_ref TEXT NOT NULL DEFAULT '',
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

// seedContact are the customer fields the fixture's seeded contact carries
// (SPEC-W50 A2 enriched-payload assertions). seedTenantSlug is the seeded
// site's tenant_slug — the CloudEvent subject (V-INT-50).
const (
	seedContactName  = "Grace Hopper"
	seedContactPhone = "+14155552671"
	seedContactEmail = "grace@example.com"
	seedTenantSlug   = "acme-spa"
)

// seed inserts one offering (75.00 NGN) + one initialized-payment booking
// bound to a seeded contact, returning (tenantID, bookingID, txRef).
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
		`INSERT INTO contacts (id, tenant_id, name, phone, email)
		 VALUES ($1,$2,$3,$4,$5)`,
		uuid.New(), tenantID, seedContactName, seedContactPhone, seedContactEmail); err != nil {
		f.t.Fatalf("seed contact: %v", err)
	}
	// Site registry row: the consumer reverse-resolves the tenant slug from
	// it for the CloudEvent subject (V-INT-50).
	if _, err := f.raw.Exec(ctx,
		`INSERT INTO sites (tenant_id, tenant_slug, slug, display_name, published)
		 VALUES ($1,$2,$3,'Acme Spa',TRUE)`,
		tenantID, seedTenantSlug, seedTenantSlug); err != nil {
		f.t.Fatalf("seed site: %v", err)
	}
	// SPEC-W51 B1/B2: the seed carries the canonical public_ref so the
	// contract payload asserts the BK-XXXXXXXX booking_ref, not the UUID
	// fallback (raw inserts bypass the create path's assignment).
	if _, err := f.raw.Exec(ctx,
		`INSERT INTO bookings (id, tenant_id, offering_id, contact_id, starts_at, ends_at, status, source, public_ref)
		 VALUES ($1,$2,$3,(SELECT id FROM contacts WHERE tenant_id=$2 AND phone=$4),
		         now() + interval '1 hour', now() + interval '2 hours', 'pending', 'api', $5)`,
		bookingID, tenantID, off.ID, seedContactPhone, store.PublicRefForBooking(bookingID)); err != nil {
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

// w51PublicRefRe is the SPEC-W51 booking_ref contract shape.
var w51PublicRefRe = regexp.MustCompile(`^BK-[0-9A-Z]{8}$`)

// assertContractPayload checks the SPEC-W50/W51 sacred cross-scope
// BookingPaymentConfirmed/BookingPaymentAmountMismatch event: the CloudEvent
// SUBJECT carries the tenant slug (V-INT-50 — ChannelRouter provider-routing
// key) and the data payload matches field-by-field: booking_id, tenant_id,
// tx_ref, amount_minor (int64 minor units), currency, customer_name,
// contact_phone, contact_email, booking_ref (SPEC-W51: the public_ref
// BK-XXXXXXXX), paid_at (RFC3339). expectedAmountMinor >= 0 asserts the
// SPEC-W51 mismatch-only expected_amount_minor field; < 0 asserts the field
// is ABSENT (BookingPaymentConfirmed payload unchanged otherwise).
func assertContractPayload(t *testing.T, evt map[string]any, tenantID, bookingID uuid.UUID, txRef string, amountMinor int64, currency string, expectedAmountMinor int64) {
	t.Helper()
	if evt["subject"] != seedTenantSlug {
		t.Fatalf("subject = %v, want %s (tenant slug for ChannelRouter)", evt["subject"], seedTenantSlug)
	}
	data, _ := evt["data"].(map[string]any)
	if data["booking_id"] != bookingID.String() {
		t.Fatalf("booking_id = %v, want %s", data["booking_id"], bookingID)
	}
	if data["tenant_id"] != tenantID.String() {
		t.Fatalf("tenant_id = %v, want %s", data["tenant_id"], tenantID)
	}
	if data["tx_ref"] != txRef {
		t.Fatalf("tx_ref = %v, want %s", data["tx_ref"], txRef)
	}
	if data["amount_minor"] != float64(amountMinor) { // JSON numbers decode as float64
		t.Fatalf("amount_minor = %v (%T), want %d", data["amount_minor"], data["amount_minor"], amountMinor)
	}
	if data["currency"] != currency {
		t.Fatalf("currency = %v, want %s", data["currency"], currency)
	}
	if data["customer_name"] != seedContactName {
		t.Fatalf("customer_name = %v, want %s", data["customer_name"], seedContactName)
	}
	if data["contact_phone"] != seedContactPhone {
		t.Fatalf("contact_phone = %v, want %s", data["contact_phone"], seedContactPhone)
	}
	if data["contact_email"] != seedContactEmail {
		t.Fatalf("contact_email = %v, want %s", data["contact_email"], seedContactEmail)
	}
	if data["booking_ref"] != store.PublicRefForBooking(bookingID) {
		t.Fatalf("booking_ref = %v, want public_ref %s", data["booking_ref"], store.PublicRefForBooking(bookingID))
	}
	if ref, _ := data["booking_ref"].(string); !w51PublicRefRe.MatchString(ref) {
		t.Fatalf("booking_ref %q does not match ^BK-[0-9A-Z]{8}$", ref)
	}
	if expectedAmountMinor >= 0 {
		if data["expected_amount_minor"] != float64(expectedAmountMinor) {
			t.Fatalf("expected_amount_minor = %v (%T), want %d", data["expected_amount_minor"], data["expected_amount_minor"], expectedAmountMinor)
		}
	} else if _, present := data["expected_amount_minor"]; present {
		t.Fatalf("BookingPaymentConfirmed must NOT carry expected_amount_minor (W51 contract): %v", data["expected_amount_minor"])
	}
	paidAt, ok := data["paid_at"].(string)
	if !ok || paidAt == "" {
		t.Fatalf("paid_at = %v (%T), want RFC3339 string", data["paid_at"], data["paid_at"])
	}
	if _, err := time.Parse(time.RFC3339, paidAt); err != nil {
		t.Fatalf("paid_at %q not RFC3339: %v", paidAt, err)
	}
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
	assertContractPayload(t, evt, tenantID, bookingID, txRef, 7500, "NGN", -1)

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
	// SPEC-W51 contract delta: the fast-path mismatch alert carries the
	// public_ref as booking_ref and the expected offering price.
	data, _ := rows[0]["data"].(map[string]any)
	if ref, _ := data["booking_ref"].(string); !w51PublicRefRe.MatchString(ref) ||
		ref != store.PublicRefForBooking(bookingID) {
		t.Fatalf("mismatch booking_ref = %v, want public_ref %s", data["booking_ref"], store.PublicRefForBooking(bookingID))
	}
	if data["expected_amount_minor"] != 7500.0 {
		t.Fatalf("expected_amount_minor = %v, want 7500 (offering price)", data["expected_amount_minor"])
	}
	if data["amount_minor"] != 7000.0 {
		t.Fatalf("amount_minor = %v, want 7000 (captured)", data["amount_minor"])
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

// attemptStatus reads one booking_payment_attempts row by tx_ref.
func (f *w49ConsumerFixture) attemptStatus(txRef string) string {
	f.t.Helper()
	var status string
	if err := f.raw.QueryRow(context.Background(),
		`SELECT status FROM booking_payment_attempts WHERE tx_ref=$1`, txRef).Scan(&status); err != nil {
		f.t.Fatalf("attempt status for %s: %v", txRef, err)
	}
	return status
}

// supersede re-initializes the booking's payment with a NEW tx_ref,
// superseding the seeded one; returns the new ref.
func (f *w49ConsumerFixture) supersede(tenantID, bookingID uuid.UUID) string {
	f.t.Helper()
	newRef := "fw-" + uuid.NewString()
	if err := f.st.InitializeBookingPaymentTx(context.Background(), tenantID, bookingID, newRef, "https://pay.test/y"); err != nil {
		f.t.Fatalf("re-init payment: %v", err)
	}
	return newRef
}

// TestPaymentsEvents_SupersededLinkCapture (SPEC-W50 A1/A3): the customer
// paid through the OLD checkout link after re-initializing — the fast path
// misses, the attempt fallback reconciles: booking paid, payment_ref
// advanced to the captured ref, attempt marked paid, enriched outbox
// payload asserted field-by-field; replay is an exactly-once no-op.
func TestPaymentsEvents_SupersededLinkCapture(t *testing.T) {
	f := newW49ConsumerFixture(t)
	tenantID, bookingID, oldRef := f.seed()
	newRef := f.supersede(tenantID, bookingID)
	ctx := context.Background()

	// Fast path must miss the superseded ref (precondition).
	if err := f.c.Process(ctx, f.event(PaymentsCapturedEventType, tenantID, oldRef, 75.00, "NGN")); err != nil {
		t.Fatalf("fallback capture: %v", err)
	}
	status, ref, err := f.st.GetBookingPayment(ctx, tenantID, bookingID)
	if err != nil || status != store.PaymentStatusPaid || ref != oldRef {
		t.Fatalf("payment state = %q/%q, %v; want paid/%s (ref advanced)", status, ref, err, oldRef)
	}
	if s := f.attemptStatus(oldRef); s != store.AttemptStatusPaid {
		t.Fatalf("old attempt = %q, want paid", s)
	}
	rows := f.outboxRows(bookingID)
	if len(rows) != 1 || rows[0]["type"] != BookingPaymentConfirmedEventType {
		t.Fatalf("outbox = %v, want one %s", rows, BookingPaymentConfirmedEventType)
	}
	assertContractPayload(t, rows[0], tenantID, bookingID, oldRef, 7500, "NGN", -1)

	// Replay of the same capture → acked no-op (exactly-once), no churn.
	if err := f.c.Process(ctx, f.event(PaymentsCapturedEventType, tenantID, oldRef, 75.00, "NGN")); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if rows := f.outboxRows(bookingID); len(rows) != 1 {
		t.Fatalf("after replay outbox rows = %d, want 1", len(rows))
	}
	status, ref, err = f.st.GetBookingPayment(ctx, tenantID, bookingID)
	if err != nil || status != store.PaymentStatusPaid || ref != oldRef {
		t.Fatalf("after replay state = %q/%q, %v; want paid/%s", status, ref, err, oldRef)
	}

	// A capture for the NEW (current) ref afterwards hits the fast path on
	// an already-paid booking → idempotent no-op, still one outbox row.
	if err := f.c.Process(ctx, f.event(PaymentsCapturedEventType, tenantID, newRef, 75.00, "NGN")); err != nil {
		t.Fatalf("current-ref capture after paid: %v", err)
	}
	if rows := f.outboxRows(bookingID); len(rows) != 1 {
		t.Fatalf("after current-ref capture outbox rows = %d, want 1", len(rows))
	}
}

// TestPaymentsEvents_FallbackAmountMismatch (SPEC-W50 A1/A3): a capture on
// a superseded link with the WRONG amount takes the same mismatch path as
// the fast path — paid_amount_mismatch, attempt flagged, alert outbox with
// the enriched payload, ref advanced; replay is a no-op (one alert).
func TestPaymentsEvents_FallbackAmountMismatch(t *testing.T) {
	f := newW49ConsumerFixture(t)
	tenantID, bookingID, oldRef := f.seed()
	f.supersede(tenantID, bookingID)
	ctx := context.Background()

	if err := f.c.Process(ctx, f.event(PaymentsCapturedEventType, tenantID, oldRef, 70.00, "NGN")); err != nil {
		t.Fatalf("fallback mismatch: %v", err)
	}
	status, ref, err := f.st.GetBookingPayment(ctx, tenantID, bookingID)
	if err != nil || status != store.PaymentStatusPaidAmountMismatch || ref != oldRef {
		t.Fatalf("state = %q/%q, %v; want paid_amount_mismatch/%s", status, ref, err, oldRef)
	}
	if s := f.attemptStatus(oldRef); s != store.AttemptStatusAmountMismatch {
		t.Fatalf("attempt = %q, want amount_mismatch", s)
	}
	rows := f.outboxRows(bookingID)
	if len(rows) != 1 || rows[0]["type"] != BookingPaymentAmountMismatchEventType {
		t.Fatalf("outbox = %v, want one %s alert", rows, BookingPaymentAmountMismatchEventType)
	}
	assertContractPayload(t, rows[0], tenantID, bookingID, oldRef, 7000, "NGN", 7500)

	// Replay → no duplicate alert.
	if err := f.c.Process(ctx, f.event(PaymentsCapturedEventType, tenantID, oldRef, 70.00, "NGN")); err != nil {
		t.Fatalf("mismatch replay: %v", err)
	}
	if rows := f.outboxRows(bookingID); len(rows) != 1 {
		t.Fatalf("after replay outbox = %d, want 1", len(rows))
	}
}

// TestPaymentsEvents_CrossTenantAttemptAcked (SPEC-W50 A1/A3): a capture
// whose tx_ref exists ONLY in another tenant's attempts must not cross —
// the fallback lookup is strictly tenant-scoped, so it is not found and
// the event is acked with zero state churn.
func TestPaymentsEvents_CrossTenantAttemptAcked(t *testing.T) {
	f := newW49ConsumerFixture(t)
	tenantID, bookingID, oldRef := f.seed()
	newRef := f.supersede(tenantID, bookingID)
	ctx := context.Background()

	otherTenant := uuid.New()
	if err := f.c.Process(ctx, f.event(PaymentsCapturedEventType, otherTenant, oldRef, 75.00, "NGN")); err != nil {
		t.Fatalf("cross-tenant attempt capture must be acked (nil), got %v", err)
	}
	status, ref, err := f.st.GetBookingPayment(ctx, tenantID, bookingID)
	if err != nil || status != store.PaymentStatusInitialized || ref != newRef {
		t.Fatalf("state = %q/%q, %v; want untouched initialized/%s", status, ref, err, newRef)
	}
	if s := f.attemptStatus(oldRef); s != store.AttemptStatusSuperseded {
		t.Fatalf("old attempt = %q, want still superseded", s)
	}
	if rows := f.outboxRows(bookingID); len(rows) != 0 {
		t.Fatalf("outbox rows = %d, want 0", len(rows))
	}
}

// TestPaymentsEvents_NoSiteStillEmitsEmptySubject (V-INT-50 fail-closed):
// a tenant with NO site-registry row must not lose its receipt — the slug
// lookup failure degrades the CloudEvent subject to "" (omitted by
// omitempty) with a structured warning; the confirmation is still emitted
// with the full contract payload.
func TestPaymentsEvents_NoSiteStillEmitsEmptySubject(t *testing.T) {
	f := newW49ConsumerFixture(t)
	ctx := context.Background()
	tenantID := uuid.New() // deliberately NO sites row for this tenant
	off := store.Offering{TenantID: tenantID, Name: "Consult", DurationMin: 30,
		PriceCents: 7500, Currency: "NGN", Bookable: true}
	if err := f.st.CreateOffering(ctx, &off); err != nil {
		t.Fatalf("offering: %v", err)
	}
	bookingID := uuid.New()
	if _, err := f.raw.Exec(ctx,
		`INSERT INTO bookings (id, tenant_id, offering_id, starts_at, ends_at, status, source)
		 VALUES ($1,$2,$3,now() + interval '1 hour', now() + interval '2 hours', 'pending', 'api')`,
		bookingID, tenantID, off.ID); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	txRef := "fw-" + uuid.NewString()
	if err := f.st.InitializeBookingPaymentTx(ctx, tenantID, bookingID, txRef, "https://pay.test/x"); err != nil {
		t.Fatalf("init payment: %v", err)
	}

	if err := f.c.Process(ctx, f.event(PaymentsCapturedEventType, tenantID, txRef, 75.00, "NGN")); err != nil {
		t.Fatalf("process: %v", err)
	}
	rows := f.outboxRows(bookingID)
	if len(rows) != 1 || rows[0]["type"] != BookingPaymentConfirmedEventType {
		t.Fatalf("outbox = %v, want one %s (receipt must not be lost)", rows, BookingPaymentConfirmedEventType)
	}
	if subj, ok := rows[0]["subject"]; ok && subj != "" {
		t.Fatalf("subject = %v, want empty/omitted for a site-less tenant", subj)
	}
	data, _ := rows[0]["data"].(map[string]any)
	if data["booking_id"] != bookingID.String() || data["tx_ref"] != txRef ||
		data["amount_minor"] != 7500.0 || data["contact_phone"] != "" || data["contact_email"] != "" {
		t.Fatalf("event data = %v", data)
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
