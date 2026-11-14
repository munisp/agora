package store

// SPEC-W64 Scope 3 tests: IngestMediaInbox emits exactly one
// com.opendesk.booking.ReceiptIngested CloudEvent via the outbox in the
// SAME tx — ONLY on the fresh-INSERT branch (a replay emits NOTHING, so
// bronze event_id dedupe stays honest). Pinned payload keys {tenant_id,
// channel, document_id, media_inbox_id, sha256_prefix, mime, bytes, state,
// booking_id?}; the payload is PII-safe by construction (NEVER the sender
// phone, raw or digits). Empty topic disables emission (house idiom).
// Embedded Postgres (newMediaInboxTestStore, -short skips).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// w64OutboxPayloads reads every outbox payload on the topic (no RLS on
// outbox — the dispatcher is a cross-tenant background path).
func w64OutboxPayloads(t *testing.T, st *Store, topic string) []map[string]any {
	t.Helper()
	rows, err := st.pool.Query(context.Background(),
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

// w64AssertZeroPII hostile-greps the marshalled envelope: no sender phone
// keys, no raw sender number anywhere.
func w64AssertZeroPII(t *testing.T, ev map[string]any, rawPhone string) {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("re-marshal event: %v", err)
	}
	s := string(raw)
	if strings.Contains(s, "sender_phone") {
		t.Fatalf("payload leaks sender phone keys: %s", s)
	}
	if rawPhone != "" && strings.Contains(s, rawPhone) {
		t.Fatalf("payload leaks the raw sender number: %s", s)
	}
}

func TestIngestMediaInboxEmitsReceiptIngestedFreshOnly(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	const topic = "opendesk.booking.events"
	const rawPhone = "+15557654321"

	// Fresh insert (no contact => parked) emits exactly one event.
	in := miIngestInput(tenantID, "wamid.w64-fresh", rawPhone)
	item, err := st.IngestMediaInbox(ctx, in, topic)
	if err != nil {
		t.Fatalf("fresh ingest: %v", err)
	}
	if item.State != "parked" {
		t.Fatalf("state = %s, want parked", item.State)
	}
	evs := w64OutboxPayloads(t, st, topic)
	if len(evs) != 1 {
		t.Fatalf("outbox rows = %d, want exactly 1", len(evs))
	}
	ev := evs[0]
	if ev["type"] != "com.opendesk.booking.ReceiptIngested" {
		t.Fatalf("type = %v", ev["type"])
	}
	if ev["tenantid"] != tenantID.String() {
		t.Fatalf("tenantid = %v", ev["tenantid"])
	}
	data, ok := ev["data"].(map[string]any)
	if !ok {
		t.Fatalf("data missing/not object: %v", ev["data"])
	}
	want := map[string]any{
		"tenant_id":      tenantID.String(),
		"channel":        "whatsapp",
		"document_id":    in.DocumentID.String(),
		"media_inbox_id": item.ID.String(),
		"sha256_prefix":  "0123456789abcdef",
		"mime":           "image/jpeg",
		"bytes":          float64(12345), // JSON numbers decode as float64
		"state":          "parked",
	}
	for k, v := range want {
		if data[k] != v {
			t.Fatalf("data[%s] = %v, want %v (data=%v)", k, data[k], v, data)
		}
	}
	if _, present := data["booking_id"]; present {
		t.Fatalf("parked row must omit booking_id: %v", data)
	}
	w64AssertZeroPII(t, ev, rawPhone)

	// Replay (same unique key, NEW document id): returns the EXISTING row
	// and emits NOTHING.
	replay := in
	replay.DocumentID = uuid.New()
	second, err := st.IngestMediaInbox(ctx, replay, topic)
	if err != nil {
		t.Fatalf("replay ingest: %v", err)
	}
	if second.ID != item.ID || second.State != item.State {
		t.Fatalf("replay must return the existing row: %+v vs %+v", second, item)
	}
	if evs := w64OutboxPayloads(t, st, topic); len(evs) != 1 {
		t.Fatalf("replay emitted: outbox rows = %d, want still 1", len(evs))
	}
}

func TestIngestMediaInboxAnchoredCarriesBookingID(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	const topic = "opendesk.booking.events"

	// Exactly one candidate booking => anchored, event carries booking_id.
	contactID := miSeedContact(t, st, tenantID, "+49666111999")
	bookingID := miSeedBooking(t, st, tenantID, contactID, "confirmed", time.Now().UTC().Add(24*time.Hour))
	item, err := st.IngestMediaInbox(ctx, miIngestInput(tenantID, "wamid.w64-anchored", "+49666111999"), topic)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if item.State != "anchored" || item.BookingID == nil || *item.BookingID != bookingID {
		t.Fatalf("want anchored to %s: %+v", bookingID, item)
	}
	evs := w64OutboxPayloads(t, st, topic)
	if len(evs) != 1 {
		t.Fatalf("outbox rows = %d, want 1", len(evs))
	}
	data := evs[0]["data"].(map[string]any)
	if data["booking_id"] != bookingID.String() || data["state"] != "anchored" {
		t.Fatalf("anchored payload wrong: %v", data)
	}
	w64AssertZeroPII(t, evs[0], "+49666111999")
}

func TestIngestMediaInboxEmptyTopicDisablesEmission(t *testing.T) {
	st := newMediaInboxTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	item, err := st.IngestMediaInbox(ctx, miIngestInput(tenantID, "wamid.w64-silent", "+15554443333"), "")
	if err != nil {
		t.Fatalf("ingest (topic disabled): %v", err)
	}
	if item.State != "parked" {
		t.Fatalf("state = %s, want parked", item.State)
	}
	if evs := w64OutboxPayloads(t, st, "opendesk.booking.events"); len(evs) != 0 {
		t.Fatalf("empty topic must disable emission, got %d rows", len(evs))
	}
}
