package fieldcapture

// SPEC-W64 Scope 3 tests: AttachReceipt emits exactly one
// com.opendesk.booking.ReceiptIngested CloudEvent via the outbox in the
// SAME tx (pinned payload keys {tenant_id, channel:"field_pwa",
// capture_client_id, document_id, booking_id?}); an empty EventsTopic
// disables emission (house idiom). Embedded Postgres (w57Rig, port 5562,
// -short skips).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// w64OutboxDDL mirrors the production outbox shape (01-booking-schema.sql;
// no RLS — the dispatcher is a cross-tenant background path).
const w64OutboxDDL = `CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    sent_at TIMESTAMPTZ
);`

// w64OutboxPayloads reads every outbox payload on the topic.
func w64OutboxPayloads(t *testing.T, conn *pgx.Conn, topic string) []map[string]any {
	t.Helper()
	rows, err := conn.Query(context.Background(),
		`SELECT payload FROM outbox WHERE topic=$1 ORDER BY id`, topic)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		var ev map[string]any
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("unmarshal outbox payload: %v", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("outbox rows: %v", err)
	}
	return out
}

func TestAttachReceiptEmitsReceiptIngested(t *testing.T) {
	rig, conn := w57Rig(t)
	ctx := context.Background()
	if _, err := conn.Exec(ctx, w64OutboxDDL); err != nil {
		t.Fatalf("outbox ddl: %v", err)
	}
	const topic = "opendesk.booking.events"

	// Anchor a payment_receipt capture linked to a real booking so the
	// event carries the optional booking_id.
	bookingID := w57SeedBooking(t, conn, rig.tenant.ID)
	item := mkItem(uuid.NewString(), KindPaymentReceipt,
		PaymentReceiptPayload{BookingID: &bookingID, Note: "till slip"}, nil)
	res := rig.svc.Capture(ctx, rig.tenant, []CaptureItem{item})
	if len(res) != 1 || res[0].Status != StatusApplied {
		t.Fatalf("anchor payment_receipt: %+v", res)
	}

	docID := uuid.New()
	if err := rig.fstore.AttachReceipt(ctx, rig.tenant.ID, item.ClientID, docID,
		json.RawMessage(`{"total":42}`), topic); err != nil {
		t.Fatalf("AttachReceipt: %v", err)
	}

	evs := w64OutboxPayloads(t, conn, topic)
	if len(evs) != 1 {
		t.Fatalf("outbox rows = %d, want exactly 1", len(evs))
	}
	ev := evs[0]
	if ev["type"] != "com.opendesk.booking.ReceiptIngested" {
		t.Fatalf("type = %v", ev["type"])
	}
	if ev["tenantid"] != rig.tenant.ID.String() {
		t.Fatalf("tenantid = %v", ev["tenantid"])
	}
	data, ok := ev["data"].(map[string]any)
	if !ok {
		t.Fatalf("data missing/not object: %v", ev["data"])
	}
	want := map[string]any{
		"tenant_id":         rig.tenant.ID.String(),
		"channel":           "field_pwa",
		"capture_client_id": item.ClientID,
		"document_id":       docID.String(),
		"booking_id":        bookingID.String(),
	}
	for k, v := range want {
		if data[k] != v {
			t.Fatalf("data[%s] = %v, want %v (data=%v)", k, data[k], v, data)
		}
	}
	// Zero-PII assertion: the marshalled envelope must never carry sender
	// phone fields in any form.
	raw, _ := json.Marshal(ev)
	if s := string(raw); strings.Contains(s, "sender_phone") {
		t.Fatalf("payload leaks sender phone keys: %s", s)
	}
}

func TestAttachReceiptUnlinkedOmitsBookingID(t *testing.T) {
	rig, conn := w57Rig(t)
	ctx := context.Background()
	if _, err := conn.Exec(ctx, w64OutboxDDL); err != nil {
		t.Fatalf("outbox ddl: %v", err)
	}
	const topic = "opendesk.booking.events"

	// A checkin anchor carries no payload booking link — the optional key
	// must be ABSENT, not null/empty.
	clientID := w53AnchorCapture(t, rig)
	docID := uuid.New()
	if err := rig.fstore.AttachReceipt(ctx, rig.tenant.ID, clientID, docID, nil, topic); err != nil {
		t.Fatalf("AttachReceipt: %v", err)
	}
	evs := w64OutboxPayloads(t, conn, topic)
	if len(evs) != 1 {
		t.Fatalf("outbox rows = %d, want exactly 1", len(evs))
	}
	data := evs[0]["data"].(map[string]any)
	if _, present := data["booking_id"]; present {
		t.Fatalf("booking_id must be omitted for unlinked receipts: %v", data)
	}
	if data["capture_client_id"] != clientID || data["document_id"] != docID.String() {
		t.Fatalf("pinned keys wrong: %v", data)
	}
}

func TestAttachReceiptEmptyTopicDisablesEmission(t *testing.T) {
	rig, conn := w57Rig(t)
	ctx := context.Background()
	if _, err := conn.Exec(ctx, w64OutboxDDL); err != nil {
		t.Fatalf("outbox ddl: %v", err)
	}
	clientID := w53AnchorCapture(t, rig)
	if err := rig.fstore.AttachReceipt(ctx, rig.tenant.ID, clientID, uuid.New(), nil, ""); err != nil {
		t.Fatalf("AttachReceipt (topic disabled): %v", err)
	}
	// The attach persisted (receipt_document_id set) but NOTHING emitted.
	// (RLS: read through withTenant so app.tenant_id is set.)
	var docID *uuid.UUID
	if err := rig.fstore.withTenant(ctx, rig.tenant.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT receipt_document_id FROM field_captures WHERE tenant_id=$1 AND client_id=$2`,
			rig.tenant.ID, clientID).Scan(&docID)
	}); err != nil {
		t.Fatalf("read capture: %v", err)
	}
	if docID == nil {
		t.Fatalf("attach did not persist")
	}
	if evs := w64OutboxPayloads(t, conn, "opendesk.booking.events"); len(evs) != 0 {
		t.Fatalf("empty topic must disable emission, got %d rows", len(evs))
	}
}
