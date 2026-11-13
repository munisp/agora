package fieldcapture

// SPEC-W57 Scope A (A9): kind=payment_receipt tests. The service tests run
// against the same embedded-Postgres harness as service_test.go (port
// 5562, -short skips); the booking/team_member anchors the new kind reads
// are created as a minimal slice (the full booking schema init script
// cannot be replayed through pgx — same posture as recon's store_test.go).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// w57AnchorSchema is the minimal slice of bookings/team_members the
// payment_receipt apply path reads (existence check + email resolution).
// Mirrors the production column nullability for the touched columns.
const w57AnchorSchema = `
CREATE TABLE IF NOT EXISTS team_members (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    email TEXT,
    role TEXT NOT NULL DEFAULT 'staff',
    active BOOLEAN NOT NULL DEFAULT TRUE,
    user_id UUID
);
CREATE TABLE IF NOT EXISTS bookings (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL,
    offering_id UUID,
    team_member_id UUID,
    contact_id UUID,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'confirmed',
    source TEXT NOT NULL DEFAULT 'api',
    payment_ref TEXT NOT NULL DEFAULT '',
    payment_status TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`

// w57Rig extends newTestRig with a raw connection and the anchor tables.
func w57Rig(t *testing.T) (testRig, *pgx.Conn) {
	t.Helper()
	rig := newTestRig(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, "postgres://postgres:postgres@localhost:5562/booking_fieldcapture_test?sslmode=disable")
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) }) //nolint:errcheck
	if _, err := conn.Exec(ctx, w57AnchorSchema); err != nil {
		t.Fatalf("anchor schema: %v", err)
	}
	return rig, conn
}

func w57SeedBooking(t *testing.T, conn *pgx.Conn, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	if _, err := conn.Exec(context.Background(),
		`INSERT INTO bookings (id, tenant_id, starts_at, ends_at) VALUES ($1,$2,$3,$4)`,
		id, tenantID, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	return id
}

func w57SeedMember(t *testing.T, conn *pgx.Conn, tenantID uuid.UUID, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := conn.Exec(context.Background(),
		`INSERT INTO team_members (id, tenant_id, name, email) VALUES ($1,$2,$3,$4)`,
		id, tenantID, "Agent "+email, email); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	return id
}

// A9: the new kind passes Validate (and the error string names it); bogus
// kinds still fail.
func TestPaymentReceiptKindValidate(t *testing.T) {
	ok := mkItem("c1", KindPaymentReceipt, map[string]any{}, nil)
	if err := ok.Validate(); err != nil {
		t.Fatalf("payment_receipt must validate: %v", err)
	}
	bad := mkItem("c2", "survey", map[string]any{}, nil)
	err := bad.Validate()
	if err == nil || !strings.Contains(err.Error(), "payment_receipt") {
		t.Fatalf("bogus kind error must name the full allow-list: %v", err)
	}
}

// A9 happy path: booking exists → applied, res.BookingID echoes the link;
// a client_id replay dedupes onto the ORIGINAL result (booking_id and
// team_member_id included).
func TestPaymentReceiptApplyAndDedupe(t *testing.T) {
	rig, conn := w57Rig(t)
	ctx := context.Background()
	bookingID := w57SeedBooking(t, conn, rig.tenant.ID)

	item := mkItem(uuid.NewString(), KindPaymentReceipt,
		PaymentReceiptPayload{BookingID: &bookingID, Note: "cash at stall"}, nil)
	res := rig.svc.CaptureWithCaller(ctx, rig.tenant, []CaptureItem{item}, "")
	if len(res) != 1 || res[0].Status != StatusApplied {
		t.Fatalf("apply: %+v", res)
	}
	if res[0].BookingID == nil || *res[0].BookingID != bookingID {
		t.Fatalf("booking link must echo in the result: %+v", res[0])
	}

	replay := rig.svc.CaptureWithCaller(ctx, rig.tenant, []CaptureItem{item}, "")
	if replay[0].Status != StatusDeduped || replay[0].BookingID == nil || *replay[0].BookingID != bookingID {
		t.Fatalf("replay must return the original booking_id: %+v", replay[0])
	}
}

// A9: unknown booking_id → deterministic error (recorded on the anchor);
// the replay dedupes onto the SAME error instead of re-validating.
func TestPaymentReceiptUnknownBookingDeterministic(t *testing.T) {
	rig, conn := w57Rig(t)
	ctx := context.Background()
	_ = w57SeedBooking(t, conn, rig.tenant.ID) // table exists; the linked id does not

	item := mkItem(uuid.NewString(), KindPaymentReceipt,
		PaymentReceiptPayload{BookingID: ptrUUID(uuid.New())}, nil)
	res := rig.svc.CaptureWithCaller(ctx, rig.tenant, []CaptureItem{item}, "")
	if res[0].Status != StatusError || !strings.Contains(res[0].Error, "booking_id") {
		t.Fatalf("unknown booking must be a deterministic error: %+v", res[0])
	}
	replay := rig.svc.CaptureWithCaller(ctx, rig.tenant, []CaptureItem{item}, "")
	if replay[0].Status != StatusDeduped || replay[0].Error != res[0].Error {
		t.Fatalf("replay must dedupe onto the recorded error: %+v", replay[0])
	}
}

// A9: an empty payload is a legal unlinked receipt — it applies with no
// booking link (recon falls back to the heuristic ladder).
func TestPaymentReceiptEmptyPayloadUnlinked(t *testing.T) {
	rig, _ := w57Rig(t)
	ctx := context.Background()

	item := mkItem(uuid.NewString(), KindPaymentReceipt, map[string]any{}, nil)
	res := rig.svc.CaptureWithCaller(ctx, rig.tenant, []CaptureItem{item}, "nobody@example.com")
	if res[0].Status != StatusApplied || res[0].BookingID != nil || res[0].TeamMemberID != nil {
		t.Fatalf("empty payload must apply unlinked: %+v", res[0])
	}
}

// A9 P3 matrix: team_member_id is server-RESOLVED, never client-trusted.
func TestPaymentReceiptTeamMemberResolution(t *testing.T) {
	rig, conn := w57Rig(t)
	ctx := context.Background()
	bookingID := w57SeedBooking(t, conn, rig.tenant.ID)
	memberID := w57SeedMember(t, conn, rig.tenant.ID, "ada@example.com")
	otherID := w57SeedMember(t, conn, rig.tenant.ID, "obi@example.com")

	mk := func(hint *uuid.UUID) CaptureItem {
		return mkItem(uuid.NewString(), KindPaymentReceipt,
			PaymentReceiptPayload{BookingID: &bookingID, TeamMemberID: hint}, nil)
	}

	// 1. No hint → resolved id lands in the result.
	res := rig.svc.CaptureWithCaller(ctx, rig.tenant, []CaptureItem{mk(nil)}, "ada@example.com")
	if res[0].Status != StatusApplied || res[0].TeamMemberID == nil || *res[0].TeamMemberID != memberID {
		t.Fatalf("resolve without hint: %+v", res[0])
	}
	// Replay carries the resolved id too (result JSON round-trip).
	item1 := mk(nil)
	first := rig.svc.CaptureWithCaller(ctx, rig.tenant, []CaptureItem{item1}, "ADA@example.com") // case-insensitive
	if first[0].Status != StatusApplied || first[0].TeamMemberID == nil || *first[0].TeamMemberID != memberID {
		t.Fatalf("email match must be case-insensitive: %+v", first[0])
	}
	rep := rig.svc.CaptureWithCaller(ctx, rig.tenant, []CaptureItem{item1}, "ada@example.com")
	if rep[0].Status != StatusDeduped || rep[0].TeamMemberID == nil || *rep[0].TeamMemberID != memberID {
		t.Fatalf("replay must carry the resolved team_member_id: %+v", rep[0])
	}

	// 2. Hint AGREES with the resolved member → applies.
	res = rig.svc.CaptureWithCaller(ctx, rig.tenant, []CaptureItem{mk(&memberID)}, "ada@example.com")
	if res[0].Status != StatusApplied || res[0].TeamMemberID == nil || *res[0].TeamMemberID != memberID {
		t.Fatalf("agreeing hint: %+v", res[0])
	}

	// 3. Hint DISAGREES → deterministic error; replay dedupes onto it.
	bad := mk(&otherID)
	res = rig.svc.CaptureWithCaller(ctx, rig.tenant, []CaptureItem{bad}, "ada@example.com")
	if res[0].Status != StatusError || !strings.Contains(res[0].Error, "team_member_id") {
		t.Fatalf("disagreeing hint must be a deterministic error: %+v", res[0])
	}
	res = rig.svc.CaptureWithCaller(ctx, rig.tenant, []CaptureItem{bad}, "ada@example.com")
	if res[0].Status != StatusDeduped {
		t.Fatalf("disagreeing-hint replay must dedupe: %+v", res[0])
	}

	// 4. Caller matches NO team member (walk-in supervisor) → applies with
	// NO resolved attribution, even when a hint rides along.
	res = rig.svc.CaptureWithCaller(ctx, rig.tenant, []CaptureItem{mk(&otherID)}, "walkin@example.com")
	if res[0].Status != StatusApplied || res[0].TeamMemberID != nil {
		t.Fatalf("no-match caller must apply with nil resolution: %+v", res[0])
	}

	// 5. No caller identity at all → the hint is NEVER trusted.
	res = rig.svc.CaptureWithCaller(ctx, rig.tenant, []CaptureItem{mk(&otherID)}, "")
	if res[0].Status != StatusApplied || res[0].TeamMemberID != nil {
		t.Fatalf("hint without caller identity must not be trusted: %+v", res[0])
	}
}

// A9: the handler boundary resolves the caller email from the JWT email
// claim first, then the X-User-Email fallback; malformed input is
// error-closed ("").
func TestCallerEmailExtraction(t *testing.T) {
	jwt := func(email string) string {
		payload, _ := json.Marshal(map[string]any{"email": email})
		return "Bearer x." + base64.RawURLEncoding.EncodeToString(payload) + ".y"
	}
	r := httptest.NewRequest("POST", "/v1/field/capture", nil)
	r.Header.Set("Authorization", jwt("ada@example.com"))
	r.Header.Set("X-User-Email", "fallback@example.com")
	if got := callerEmail(r); got != "ada@example.com" {
		t.Fatalf("JWT email claim wins: %q", got)
	}
	r = httptest.NewRequest("POST", "/v1/field/capture", nil)
	r.Header.Set("Authorization", jwt(""))
	r.Header.Set("X-User-Email", "fallback@example.com")
	if got := callerEmail(r); got != "fallback@example.com" {
		t.Fatalf("X-User-Email fallback: %q", got)
	}
	r = httptest.NewRequest("POST", "/v1/field/capture", nil)
	r.Header.Set("Authorization", "Bearer not-a-jwt")
	if got := callerEmail(r); got != "" {
		t.Fatalf("malformed token must be error-closed: %q", got)
	}
	r = httptest.NewRequest("POST", "/v1/field/capture", nil)
	if got := callerEmail(r); got != "" {
		t.Fatalf("anonymous request: %q", got)
	}
}

func ptrUUID(id uuid.UUID) *uuid.UUID { return &id }

// Guard against a wiring regression (P2): the apply switch must dispatch
// KindPaymentReceipt — a validated item can never "apply" silently.
func TestPaymentReceiptNeverSilentSwallow(t *testing.T) {
	rig, _ := w57Rig(t)
	ctx := context.Background()
	item := mkItem(uuid.NewString(), KindPaymentReceipt,
		map[string]any{"note": fmt.Sprintf("n-%s", uuid.NewString())}, nil)
	res := rig.svc.Capture(ctx, rig.tenant, []CaptureItem{item})
	if res[0].Status != StatusApplied {
		t.Fatalf("payment_receipt must apply through the switch: %+v", res[0])
	}
}
