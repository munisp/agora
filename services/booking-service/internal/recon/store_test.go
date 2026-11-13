package recon

// SPEC-W56 Scope A item 8: store + sweep tests against embedded Postgres
// (same harness idiom as fieldcapture/service_test.go; dedicated port 5580
// avoids the postmaster.pid race under `go test ./...`; -short skips).
// Covers: schema bootstrap idempotence + forced RLS, finding insert +
// outbox in one tx (outbox ONLY for newly inserted findings, NULL-keyed
// dedupe), the full three-pass sweep (exact/heuristic/ambiguous/no-
// candidate outcomes, amount_mismatch, currency_unknown gating,
// payment_without_receipt pinned-4 tenant gate, re-sweep idempotence) and
// the ListFindings read path (receipt_fields join, status filter).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

const reconTestPort = 5580

// reconTestSchema is the minimal slice of the booking DB the reconciler
// touches (the init script contains \c meta-commands and cannot be replayed
// through pgx). booking_payment_attempts carries the W56 additive columns
// in their post-bootstrap shape.
const reconTestSchema = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE IF NOT EXISTS offerings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    price_cents INTEGER NOT NULL DEFAULT 0,
    currency CHAR(3) NOT NULL DEFAULT 'NGN'
);
CREATE TABLE IF NOT EXISTS contacts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    phone TEXT,
    email TEXT
);
CREATE TABLE IF NOT EXISTS leads (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    phone_e164 TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS bookings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    offering_id UUID NOT NULL,
    contact_id UUID,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'confirmed',
    source TEXT NOT NULL DEFAULT 'api',
    payment_ref TEXT NOT NULL DEFAULT '',
    payment_status TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS booking_payment_attempts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    booking_id UUID NOT NULL,
    tx_ref TEXT NOT NULL UNIQUE,
    link TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    amount_minor BIGINT,
    currency TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS field_captures (
    tenant_id UUID NOT NULL,
    client_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}',
    captured_at TIMESTAMPTZ,
    status TEXT NOT NULL DEFAULT 'applied',
    result JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    server_received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    receipt_document_id UUID,
    receipt_fields JSONB,
    PRIMARY KEY (tenant_id, client_id)
);
CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    sent_at TIMESTAMPTZ
);`

type reconRig struct {
	st  *Store
	svc *Service
	raw *pgx.Conn
}

func newReconRig(t *testing.T) reconRig {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres recon test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_recon_test").
		Port(reconTestPort).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/booking_recon_test?sslmode=disable", reconTestPort)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) }) //nolint:errcheck
	if _, err := conn.Exec(ctx, reconTestSchema); err != nil {
		t.Fatalf("test schema: %v", err)
	}
	st, err := DialStore(ctx, dsn)
	if err != nil {
		t.Fatalf("DialStore: %v", err)
	}
	t.Cleanup(st.Close)
	svc := &Service{
		Store:       st,
		Log:         zap.NewNop(),
		EventsTopic: "opendesk.booking.events",
	}
	return reconRig{st: st, svc: svc, raw: conn}
}

func TestSchemaBootstrapIdempotentForcedRLS(t *testing.T) {
	rig := newReconRig(t)
	ctx := context.Background()

	// Forced RLS + the tenant_isolation NULLIF idiom.
	var rls, force bool
	if err := rig.raw.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname='receipt_recon_findings'`).
		Scan(&rls, &force); err != nil {
		t.Fatalf("pg_class: %v", err)
	}
	if !rls || !force {
		t.Fatalf("RLS must be ENABLE+FORCE (rls=%v force=%v)", rls, force)
	}
	var policy string
	if err := rig.raw.QueryRow(ctx,
		`SELECT pg_get_expr(p.polqual, p.polrelid) FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid
		  WHERE c.relname='receipt_recon_findings' AND p.polname='tenant_isolation'`).
		Scan(&policy); err != nil {
		t.Fatalf("policy: %v", err)
	}
	// The house NULLIF idiom (pg_get_expr parenthesization varies by PG
	// version — match the substance, not the string).
	for _, frag := range []string{"tenant_id", "NULLIF", "current_setting('app.tenant_id'::text, true)", "::uuid"} {
		if !strings.Contains(policy, frag) {
			t.Fatalf("tenant_isolation qual %q missing %q", policy, frag)
		}
	}

	// Idempotent: a second bootstrap over the same pool succeeds.
	pool, err := pgxpool.New(ctx, fmt.Sprintf("postgres://postgres:postgres@localhost:%d/booking_recon_test?sslmode=disable", reconTestPort))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	s2, err := NewStore(ctx, pool)
	if err != nil {
		t.Fatalf("second NewStore (idempotence): %v", err)
	}
	_ = s2
}

func TestInsertFindingDedupeOutbox(t *testing.T) {
	rig := newReconRig(t)
	ctx := context.Background()
	tenantID := uuid.New()

	mk := func(kind string, capture *string, booking *uuid.UUID) *Finding {
		return &Finding{
			TenantID: tenantID, RunID: uuid.New(), Kind: kind,
			CaptureClientID: capture, BookingID: booking,
			Confidence: ConfidenceNone,
			Detail:     json.RawMessage(`{"currency_unknown":true}`),
		}
	}

	// New finding → inserted + exactly one outbox row carrying finding_id.
	captureID := "cap-1"
	f := mk(KindReceiptWithoutPayment, &captureID, nil)
	payload, err := rig.svc.findingEventPayload(ctx, f)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	inserted, err := rig.st.InsertFindingWithOutbox(ctx, f, rig.svc.EventsTopic, payload)
	if err != nil || !inserted {
		t.Fatalf("first insert: inserted=%v err=%v", inserted, err)
	}
	// Replay (same dedupe key, new run/id) → deduped, NO second outbox row.
	replay := mk(KindReceiptWithoutPayment, &captureID, nil)
	inserted, err = rig.st.InsertFindingWithOutbox(ctx, replay, rig.svc.EventsTopic, payload)
	if err != nil || inserted {
		t.Fatalf("replay must dedupe: inserted=%v err=%v", inserted, err)
	}

	// NULL capture_client_id (payment_without_receipt shape) must ALSO
	// dedupe — the COALESCE expression index covers the NULL halves the
	// plain UNIQUE constraint cannot.
	bookingID := uuid.New()
	g := mk(KindPaymentWithoutReceipt, nil, &bookingID)
	inserted, err = rig.st.InsertFindingWithOutbox(ctx, g, rig.svc.EventsTopic, payload)
	if err != nil || !inserted {
		t.Fatalf("null-keyed first insert: inserted=%v err=%v", inserted, err)
	}
	inserted, err = rig.st.InsertFindingWithOutbox(ctx, mk(KindPaymentWithoutReceipt, nil, &bookingID), rig.svc.EventsTopic, payload)
	if err != nil || inserted {
		t.Fatalf("null-keyed replay must dedupe: inserted=%v err=%v", inserted, err)
	}

	var findings, outboxRows int
	if err := rig.raw.QueryRow(ctx, `SELECT count(*) FROM receipt_recon_findings WHERE tenant_id=$1`, tenantID).Scan(&findings); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if err := rig.raw.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&outboxRows); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if findings != 2 || outboxRows != 2 {
		t.Fatalf("findings=%d outbox=%d, want 2/2 (outbox only for NEW findings)", findings, outboxRows)
	}
	// The outbox payload is the ReceiptReconFinding CloudEvent with
	// finding_id + kind + confidence (+ currency_unknown detail).
	var rawPayload []byte
	if err := rig.raw.QueryRow(ctx,
		`SELECT payload FROM outbox WHERE topic='opendesk.booking.events' ORDER BY id LIMIT 1`).Scan(&rawPayload); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	var env struct {
		Type     string         `json:"type"`
		TenantID string         `json:"tenantid"`
		Data     map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rawPayload, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.Type != "com.opendesk.booking.ReceiptReconFinding" || env.TenantID != tenantID.String() {
		t.Fatalf("envelope type/tenant: %+v", env)
	}
	if env.Data["finding_id"] == "" || env.Data["kind"] != KindReceiptWithoutPayment || env.Data["confidence"] != ConfidenceNone {
		t.Fatalf("payload fields: %+v", env.Data)
	}
	detail, _ := env.Data["detail"].(map[string]any)
	if cu, _ := detail["currency_unknown"].(bool); !cu {
		t.Fatalf("detail must carry currency_unknown:true: %+v", detail)
	}
}

// seedPaidBooking inserts contact+booking+paid attempt (post-W56 shape:
// amount_minor/currency filled) and returns their ids.
func seedPaidBooking(t *testing.T, rig reconRig, tenantID, offeringID uuid.UUID, phone string,
	startsAt, paidAt time.Time, amountMinor int64, txRef string) (contactID, bookingID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	contactID, bookingID = uuid.New(), uuid.New()
	if _, err := rig.raw.Exec(ctx,
		`INSERT INTO contacts (id, tenant_id, name, phone) VALUES ($1,$2,$3,$4)`,
		contactID, tenantID, "Contact "+phone, phone); err != nil {
		t.Fatalf("contact: %v", err)
	}
	if _, err := rig.raw.Exec(ctx,
		`INSERT INTO bookings (id, tenant_id, offering_id, contact_id, starts_at, ends_at, status, source, payment_ref, payment_status)
		 VALUES ($1,$2,$3,$4,$5,$6,'confirmed','api',$7,'paid')`,
		bookingID, tenantID, offeringID, contactID, startsAt, startsAt.Add(time.Hour), txRef); err != nil {
		t.Fatalf("booking: %v", err)
	}
	if _, err := rig.raw.Exec(ctx,
		`INSERT INTO booking_payment_attempts (tenant_id, booking_id, tx_ref, status, amount_minor, currency, created_at, updated_at)
		 VALUES ($1,$2,$3,'paid',$4,'NGN',$5,$5)`,
		tenantID, bookingID, txRef, amountMinor, paidAt); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	return contactID, bookingID
}

func seedReceiptCapture(t *testing.T, rig reconRig, tenantID uuid.UUID, clientID, kind string,
	payload, result, receiptFields string, capturedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	var res any
	if result != "" {
		res = json.RawMessage(result)
	}
	var fields any
	if receiptFields != "" {
		fields = json.RawMessage(receiptFields)
	}
	if _, err := rig.raw.Exec(ctx,
		`INSERT INTO field_captures (tenant_id, client_id, kind, payload, result, receipt_document_id, receipt_fields, captured_at, server_received_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now())`,
		tenantID, clientID, kind, json.RawMessage(payload), res, uuid.New(), fields, capturedAt); err != nil {
		t.Fatalf("capture %s: %v", clientID, err)
	}
}

func hasBooking(m map[uuid.UUID]Finding, id uuid.UUID) bool {
	_, ok := m[id]
	return ok
}

func TestSweepEndToEnd(t *testing.T) {
	rig := newReconRig(t)
	ctx := context.Background()
	tenant := uuid.New()
	offering := uuid.New()
	if _, err := rig.raw.Exec(ctx,
		`INSERT INTO offerings (id, tenant_id, name, price_cents, currency) VALUES ($1,$2,'Consult',5000,'NGN')`,
		offering, tenant); err != nil {
		t.Fatalf("offering: %v", err)
	}
	now := time.Now().UTC()
	capturedAt := now.Add(-time.Hour)
	today := now.Format("2006-01-02")

	// B1: exact contact-chain match, amounts agree → NO finding; B1 linked.
	// starts_at sits EXACTLY at captured_at+7d — the inclusive window edge.
	c1, _ := seedPaidBooking(t, rig, tenant, offering, "+2348010000001", capturedAt.AddDate(0, 0, 7), now, 5000, "tx-1")
	seedReceiptCapture(t, rig, tenant, "cap-exact", "checkin",
		fmt.Sprintf(`{"contact_id":%q}`, c1), "", fmt.Sprintf(`{"amount":50.00,"date":%q}`, today), capturedAt)

	// B2: exact match, paper 45.00 vs rail 5000 → amount_mismatch.
	c2, b2 := seedPaidBooking(t, rig, tenant, offering, "+2348010000002", now, now, 5000, "tx-2")
	seedReceiptCapture(t, rig, tenant, "cap-mismatch", "checkin",
		fmt.Sprintf(`{"contact_id":%q}`, c2), "", fmt.Sprintf(`{"amount":45.00,"date":%q}`, today), capturedAt)

	// B3: heuristic lead-phone chain match, amounts agree → NO finding.
	_, _ = seedPaidBooking(t, rig, tenant, offering, "+2348010000003", now, now, 7500, "tx-3")
	lead3 := uuid.New()
	if _, err := rig.raw.Exec(ctx,
		`INSERT INTO leads (id, tenant_id, phone_e164) VALUES ($1,$2,'+2348010000003')`, lead3, tenant); err != nil {
		t.Fatalf("lead3: %v", err)
	}
	seedReceiptCapture(t, rig, tenant, "cap-lead", "lead_capture",
		`{"phone_e164":"+2348010000003"}`, fmt.Sprintf(`{"lead_id":%q}`, lead3),
		fmt.Sprintf(`{"amount":75.00,"date":%q}`, today), capturedAt)

	// cap-orphan: checkin for a contact with NO bookings → receipt_without_payment.
	seedReceiptCapture(t, rig, tenant, "cap-orphan", "checkin",
		fmt.Sprintf(`{"contact_id":%q}`, uuid.New()), "", `{"amount":10.00}`, capturedAt)

	// cap-ambiguous: TWO paid bookings of the same contact in window →
	// confidence none + candidates in detail (kind receipt_without_payment).
	c5, _ := seedPaidBooking(t, rig, tenant, offering, "+2348010000005", now, now, 5000, "tx-5a")
	b5b := uuid.New()
	if _, err := rig.raw.Exec(ctx,
		`INSERT INTO bookings (id, tenant_id, offering_id, contact_id, starts_at, ends_at, status, source, payment_ref, payment_status)
		 VALUES ($1,$2,$3,$4,$5,$6,'confirmed','api','tx-5b','paid')`,
		b5b, tenant, offering, c5, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("b5b: %v", err)
	}
	if _, err := rig.raw.Exec(ctx,
		`INSERT INTO booking_payment_attempts (tenant_id, booking_id, tx_ref, status, amount_minor, currency)
		 VALUES ($1,$2,'tx-5b','paid',5000,'NGN')`, tenant, b5b); err != nil {
		t.Fatalf("a5b: %v", err)
	}
	seedReceiptCapture(t, rig, tenant, "cap-ambiguous", "checkin",
		fmt.Sprintf(`{"contact_id":%q}`, c5), "", fmt.Sprintf(`{"amount":50.00,"date":%q}`, today), capturedAt)

	// B6: rung 3 amount+date heuristic (no contact/lead anchor), agree → none.
	_, _ = seedPaidBooking(t, rig, tenant, offering, "+2348010000006", now.AddDate(0, 0, 20), now, 9900, "tx-6")
	seedReceiptCapture(t, rig, tenant, "cap-amount", "checkin", `{"note":"no contact"}`, "",
		fmt.Sprintf(`{"amount":99.00,"date":%q}`, today), capturedAt)

	// B7: paid, NO receipt anywhere → payment_without_receipt.
	_, b7 := seedPaidBooking(t, rig, tenant, offering, "+2348010000007", now, now, 12300, "tx-7")

	// B8: exact match but receipt amount UNREADABLE (null) → the missing
	// amount/currency blocks the otherwise-exact pair → currency_unknown.
	c8, b8 := seedPaidBooking(t, rig, tenant, offering, "+2348010000008", now, now, 8000, "tx-8")
	seedReceiptCapture(t, rig, tenant, "cap-noamount", "checkin",
		fmt.Sprintf(`{"contact_id":%q}`, c8), "", `{"vendor":"Stall"}`, capturedAt)

	// B9: heuristic match with null amount → NO standalone currency_unknown
	// finding (it only blocks otherwise-EXACT pairs; here nothing emits).
	_, _ = seedPaidBooking(t, rig, tenant, offering, "+2348010000009", now, now, 6100, "tx-9")
	lead9 := uuid.New()
	if _, err := rig.raw.Exec(ctx,
		`INSERT INTO leads (id, tenant_id, phone_e164) VALUES ($1,$2,'+2348010000009')`, lead9, tenant); err != nil {
		t.Fatalf("lead9: %v", err)
	}
	seedReceiptCapture(t, rig, tenant, "cap-lead-noamount", "lead_capture",
		`{"phone_e164":"+2348010000009"}`, fmt.Sprintf(`{"lead_id":%q}`, lead9), `{"vendor":"Stall"}`, capturedAt)

	// cap-outside: booking starts_at is captured_at+8d — OUTSIDE the ±7d
	// window; null amount skips rung 3 → receipt_without_payment. The
	// booking's payment is therefore ALSO receipt-less: it shows up as a
	// second payment_without_receipt (honest dual signal for the pair that
	// fell outside the window).
	c10, b10 := seedPaidBooking(t, rig, tenant, offering, "+2348010000010", capturedAt.AddDate(0, 0, 8), now, 5000, "tx-10")
	seedReceiptCapture(t, rig, tenant, "cap-outside", "checkin",
		fmt.Sprintf(`{"contact_id":%q}`, c10), "", `{"vendor":"Stall"}`, capturedAt)

	// Tenant T2: paid attempts but ZERO receipt captures → the pinned-4
	// gate must keep it out of the sweep entirely (no payment_without_receipt).
	tenant2 := uuid.New()
	if _, err := rig.raw.Exec(ctx,
		`INSERT INTO offerings (id, tenant_id, name, price_cents, currency) VALUES ($1,$2,'Consult',5000,'NGN')`,
		uuid.New(), tenant2); err != nil {
		t.Fatalf("offering t2: %v", err)
	}
	var t2Offering uuid.UUID
	if err := rig.raw.QueryRow(ctx, `SELECT id FROM offerings WHERE tenant_id=$1`, tenant2).Scan(&t2Offering); err != nil {
		t.Fatalf("t2 offering id: %v", err)
	}
	seedPaidBooking(t, rig, tenant2, t2Offering, "+2348099999999", now, now, 5000, "tx-t2")

	sum, err := rig.svc.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if sum.TenantsScanned != 1 || sum.TenantsFailed != 0 {
		t.Fatalf("summary: %+v (tenant2 must be excluded by the pinned-4 gate)", sum)
	}

	type row struct {
		kind       string
		confidence string
		capture    *string
		booking    *uuid.UUID
		detail     map[string]any
	}
	rows, err := rig.st.ListFindings(ctx, tenant, "all", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byCapture := map[string]row{}
	orphanBookings := map[uuid.UUID]Finding{}
	for _, f := range rows {
		var detail map[string]any
		if err := json.Unmarshal(f.Detail, &detail); err != nil {
			t.Fatalf("detail json: %v", err)
		}
		// R2 honesty: EVERY finding detail carries currency_unknown:true.
		if cu, _ := detail["currency_unknown"].(bool); !cu {
			t.Fatalf("finding %s missing currency_unknown:true (%+v)", f.Kind, detail)
		}
		if f.Kind == KindPaymentWithoutReceipt {
			if f.BookingID == nil {
				t.Fatalf("payment_without_receipt without booking_id: %+v", f)
			}
			orphanBookings[*f.BookingID] = f
			continue
		}
		if f.CaptureClientID == nil {
			t.Fatalf("capture finding without capture_client_id: %+v", f)
		}
		byCapture[*f.CaptureClientID] = row{f.Kind, f.Confidence, f.CaptureClientID, f.BookingID, detail}
	}

	// Exactly: amount_mismatch (cap-mismatch), receipt_without_payment ×3
	// (cap-orphan, cap-ambiguous, cap-outside), currency_unknown ×1
	// (cap-noamount), payment_without_receipt ×2 (b7 — no receipt at all;
	// b10 — its receipt fell outside the match window). cap-exact,
	// cap-lead, cap-amount, cap-lead-noamount produce NOTHING.
	if len(rows) != 7 {
		t.Fatalf("want 7 findings, got %d: %+v", len(rows), rows)
	}
	if len(orphanBookings) != 2 || !hasBooking(orphanBookings, b7) || !hasBooking(orphanBookings, b10) {
		t.Fatalf("payment_without_receipt must point at b7+b10: %+v", orphanBookings)
	}
	if f := orphanBookings[b7]; f.AttemptTxRef == nil || *f.AttemptTxRef != "tx-7" ||
		f.CapturedAmountMinor == nil || *f.CapturedAmountMinor != 12300 {
		t.Fatalf("payment_without_receipt b7 fields: %+v", f)
	}
	if r := byCapture["cap-mismatch"]; r.kind != KindAmountMismatch || r.confidence != ConfidenceExact ||
		r.booking == nil || *r.booking != b2 {
		t.Fatalf("cap-mismatch: %+v", r)
	}
	if r := byCapture["cap-orphan"]; r.kind != KindReceiptWithoutPayment || r.confidence != ConfidenceNone {
		t.Fatalf("cap-orphan: %+v", r)
	}
	if r := byCapture["cap-ambiguous"]; r.kind != KindReceiptWithoutPayment || r.confidence != ConfidenceNone {
		t.Fatalf("cap-ambiguous: %+v", r)
	} else {
		if r.detail["reason"] != "ambiguous_match" || r.detail["winning_rung"].(float64) != 1 {
			t.Fatalf("cap-ambiguous detail: %+v", r.detail)
		}
		cands, _ := r.detail["candidates"].([]any)
		if len(cands) != 2 {
			t.Fatalf("cap-ambiguous must list both candidates: %+v", r.detail)
		}
	}
	if r := byCapture["cap-noamount"]; r.kind != KindCurrencyUnknown || r.confidence != ConfidenceExact ||
		r.booking == nil || *r.booking != b8 {
		t.Fatalf("cap-noamount: %+v", r)
	}
	if r := byCapture["cap-outside"]; r.kind != KindReceiptWithoutPayment || r.confidence != ConfidenceNone {
		t.Fatalf("cap-outside (8d > ±7d window): %+v", r)
	}

	// The read path doubles as the first receipt_fields read (R4): the
	// capture-linked findings carry the OCR fields verbatim.
	var sawReceiptFields bool
	for _, f := range rows {
		if f.CaptureClientID != nil && *f.CaptureClientID == "cap-mismatch" {
			var fields map[string]any
			if err := json.Unmarshal(f.ReceiptFields, &fields); err != nil {
				t.Fatalf("receipt_fields join: %v", err)
			}
			if fields["amount"].(float64) != 45.00 {
				t.Fatalf("receipt_fields amount: %+v", fields)
			}
			sawReceiptFields = true
		}
	}
	if !sawReceiptFields {
		t.Fatal("cap-mismatch finding must carry the linked receipt_fields")
	}

	// Every finding emitted exactly one outbox event; none for clean pairs.
	var outboxRows int
	if err := rig.raw.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&outboxRows); err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	if outboxRows != 7 {
		t.Fatalf("want 7 outbox rows, got %d", outboxRows)
	}

	// Re-sweep idempotence: no new findings, no new outbox rows; every
	// finding dedupes onto its key.
	sum2, err := rig.svc.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("re-sweep: %v", err)
	}
	if sum2.FindingsNew != 0 || sum2.FindingsDuped != 7 {
		t.Fatalf("re-sweep summary: %+v (want 0 new / 7 deduped)", sum2)
	}
	var findingsAfter, outboxAfter int
	_ = rig.raw.QueryRow(ctx, `SELECT count(*) FROM receipt_recon_findings WHERE tenant_id=$1`, tenant).Scan(&findingsAfter)
	_ = rig.raw.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&outboxAfter)
	if findingsAfter != 7 || outboxAfter != 7 {
		t.Fatalf("re-sweep must not duplicate: findings=%d outbox=%d", findingsAfter, outboxAfter)
	}

	// Status filter: everything is open; resolved is empty.
	open, err := rig.st.ListFindings(ctx, tenant, "open", 100)
	if err != nil || len(open) != 7 {
		t.Fatalf("open: %d %v", len(open), err)
	}
	resolved, err := rig.st.ListFindings(ctx, tenant, "resolved", 100)
	if err != nil || len(resolved) != 0 {
		t.Fatalf("resolved: %d %v", len(resolved), err)
	}
	// Tenant isolation at the read path.
	other, err := rig.st.ListFindings(ctx, tenant2, "all", 100)
	if err != nil || len(other) != 0 {
		t.Fatalf("tenant2 must see zero findings: %d %v", len(other), err)
	}
}
