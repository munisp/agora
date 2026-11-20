package httpapi

// SPEC-W95 tests: public intake endpoints — capability-token discipline
// (uniform 404), submit flow + outbox event, payload caps, GET round-trip.
// Mirrors the W94 fixture (embedded Postgres, portalTestSchema); the intake
// secret is the only added Dep.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

type w95Fixture struct {
	handler  http.Handler
	store    *store.Store
	tenantID uuid.UUID
	secret   string
}

func newW95Fixture(t *testing.T, intakeSecret string) *w95Fixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres w95 test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_w95_test"))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5432/booking_w95_test?sslmode=disable"
	if pool, err := pgxpool.New(ctx, dsn); err != nil {
		t.Fatalf("raw pool: %v", err)
	} else {
		if _, err := pool.Exec(ctx, portalTestSchema); err != nil {
			t.Fatalf("test schema: %v", err)
		}
		pool.Close()
	}
	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)

	f := &w95Fixture{store: st, tenantID: uuid.New(), secret: intakeSecret}
	ops := &bookingops.Service{Store: st, EventsTopic: "test.events", Logger: zap.NewNop()}
	f.handler = NewRouter(Deps{
		Store:              st,
		Ops:                ops,
		AuthzDisabled:      true,
		Logger:             zap.NewNop(),
		PortalSecret:       "test-portal-secret",
		PubSubName:         "test-pubsub",
		NotificationsTopic: "test.notifications",
		Publisher:          &fakePublisher{},
		IntakeTokenSecret:  intakeSecret,
	})

	site := store.Site{TenantID: f.tenantID, TenantSlug: "acme", Slug: "acme-books", DisplayName: "Acme"}
	if err := st.CreateSite(ctx, &site); err != nil {
		t.Fatalf("site: %v", err)
	}
	return f
}

// w95SeedBooking inserts one confirmed booking and returns its id.
func (f *w95Fixture) w95SeedBooking(t *testing.T) uuid.UUID {
	t.Helper()
	b := store.Booking{
		TenantID: f.tenantID, OfferingID: uuid.New(), TeamMemberID: uuid.New(),
		ContactID: uuid.New(), StartsAt: time.Now().Add(24 * time.Hour),
		EndsAt: time.Now().Add(25 * time.Hour), Status: store.StatusConfirmed, Source: "web",
	}
	if err := f.store.CreateBookingTx(context.Background(), &b, store.SlotGuard{}, "test.events", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	return b.ID
}

func (f *w95Fixture) intakeURL(bookingID uuid.UUID, token string) string {
	return fmt.Sprintf("/public/sites/acme-books/intake?booking=%s&token=%s", bookingID, token)
}

func TestW95IntakeSubmitFlow(t *testing.T) {
	f := newW95Fixture(t, "w95-intake-secret")
	bookingID := f.w95SeedBooking(t)
	token := intakeToken(f.secret, bookingID)

	// GET before submit: pending.
	code, resp := doJSON(t, f.handler, "GET", f.intakeURL(bookingID, token), "", nil)
	if code != http.StatusOK || resp["status"] != "pending" {
		t.Fatalf("pending GET = %d %v", code, resp)
	}

	// Submit: 200 submitted, resubmitted=false.
	code, resp = doJSON(t, f.handler, "POST", "/public/sites/acme-books/intake", "", map[string]any{
		"booking": bookingID.String(),
		"token":   token,
		"fields":  map[string]string{"allergies": "penicillin", "notes": "first visit"},
	})
	if code != http.StatusOK || resp["status"] != "submitted" || resp["resubmitted"] != false {
		t.Fatalf("submit = %d %v", code, resp)
	}

	// Outbox event: the IntakeCompleted cloudevent the signals bridge maps.
	rows, err := f.store.FetchUnsentOutbox(context.Background(), 10)
	if err != nil {
		t.Fatalf("FetchUnsentOutbox: %v", err)
	}
	var found bool
	for _, r := range rows {
		var evt struct {
			Type    string            `json:"type"`
			Subject string            `json:"subject"`
			Data    map[string]string `json:"data"`
		}
		if err := json.Unmarshal(r.Payload, &evt); err != nil {
			continue
		}
		if evt.Type == "com.opendesk.booking.IntakeCompleted" {
			found = true
			if evt.Data["booking_id"] != bookingID.String() {
				t.Fatalf("event booking_id = %q, want %s", evt.Data["booking_id"], bookingID)
			}
			if evt.Subject != "acme" {
				t.Fatalf("event subject = %q, want acme", evt.Subject)
			}
		}
	}
	if !found {
		t.Fatal("no IntakeCompleted outbox event")
	}

	// GET after submit: submitted with the contact's own fields back.
	code, resp = doJSON(t, f.handler, "GET", f.intakeURL(bookingID, token), "", nil)
	if code != http.StatusOK || resp["status"] != "submitted" {
		t.Fatalf("submitted GET = %d %v", code, resp)
	}
	if resp["submitted_at"] == nil {
		t.Fatal("submitted GET missing submitted_at")
	}
	fields, _ := resp["fields"].(map[string]any)
	if fields["allergies"] != "penicillin" {
		t.Fatalf("submitted GET fields = %v", fields)
	}

	// Resubmit: idempotent replace, resubmitted=true.
	code, resp = doJSON(t, f.handler, "POST", "/public/sites/acme-books/intake", "", map[string]any{
		"booking": bookingID.String(),
		"token":   token,
		"fields":  map[string]string{"allergies": "none"},
	})
	if code != http.StatusOK || resp["resubmitted"] != true {
		t.Fatalf("resubmit = %d %v", code, resp)
	}
	code, resp = doJSON(t, f.handler, "GET", f.intakeURL(bookingID, token), "", nil)
	fields, _ = resp["fields"].(map[string]any)
	if fields["allergies"] != "none" {
		t.Fatalf("payload not replaced: %v", fields)
	}
}

func TestW95IntakeTokenDiscipline(t *testing.T) {
	f := newW95Fixture(t, "w95-intake-secret")
	bookingA := f.w95SeedBooking(t)
	bookingB := f.w95SeedBooking(t)
	tokenA := intakeToken(f.secret, bookingA)
	tokenB := intakeToken(f.secret, bookingB)

	// Garbage token -> uniform 404.
	code, _ := doJSON(t, f.handler, "GET", f.intakeURL(bookingA, "deadbeef"), "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("bad token GET = %d, want 404", code)
	}

	// Valid token for booking B presented on booking A -> 404 (the HMAC
	// binds the booking id).
	code, _ = doJSON(t, f.handler, "GET", f.intakeURL(bookingA, tokenB), "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("cross-booking token GET = %d, want 404", code)
	}

	// Well-formed token for an UNKNOWN booking -> same 404 (no enumeration).
	unknown := uuid.New()
	code, _ = doJSON(t, f.handler, "GET", f.intakeURL(unknown, intakeToken(f.secret, unknown)), "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown booking GET = %d, want 404", code)
	}

	// Malformed booking id -> same 404.
	code, _ = doJSON(t, f.handler, "GET", "/public/sites/acme-books/intake?booking=not-a-uuid&token="+tokenA, "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("malformed booking GET = %d, want 404", code)
	}

	// POST shares the discipline.
	code, _ = doJSON(t, f.handler, "POST", "/public/sites/acme-books/intake", "", map[string]any{
		"booking": bookingA.String(), "token": tokenB, "fields": map[string]string{"a": "b"},
	})
	if code != http.StatusNotFound {
		t.Fatalf("cross-booking token POST = %d, want 404", code)
	}
}

func TestW95IntakePayloadCaps(t *testing.T) {
	f := newW95Fixture(t, "w95-intake-secret")
	bookingID := f.w95SeedBooking(t)
	token := intakeToken(f.secret, bookingID)

	post := func(fields map[string]string) int {
		code, _ := doJSON(t, f.handler, "POST", "/public/sites/acme-books/intake", "", map[string]any{
			"booking": bookingID.String(), "token": token, "fields": fields,
		})
		return code
	}

	// 65 fields -> 400.
	tooMany := map[string]string{}
	for i := 0; i < 65; i++ {
		tooMany[fmt.Sprintf("k%02d", i)] = "v"
	}
	if code := post(tooMany); code != http.StatusBadRequest {
		t.Fatalf("65 fields = %d, want 400", code)
	}
	// Value over 2000 chars -> 400.
	if code := post(map[string]string{"notes": strings.Repeat("x", 2001)}); code != http.StatusBadRequest {
		t.Fatalf("long value = %d, want 400", code)
	}
	// Key over 64 chars -> 400.
	if code := post(map[string]string{strings.Repeat("k", 65): "v"}); code != http.StatusBadRequest {
		t.Fatalf("long key = %d, want 400", code)
	}
	// Empty fields -> 400.
	if code := post(map[string]string{}); code != http.StatusBadRequest {
		t.Fatalf("empty fields = %d, want 400", code)
	}
	// At the caps: accepted.
	if code := post(map[string]string{strings.Repeat("k", 64): strings.Repeat("v", 2000)}); code != http.StatusOK {
		t.Fatalf("at-cap fields = %d, want 200", code)
	}
}

func TestW95IntakeEmptySecret(t *testing.T) {
	f := newW95Fixture(t, "") // intake disabled
	bookingID := f.w95SeedBooking(t)

	// Even a well-formed request 404s uniformly when the secret is unset.
	code, _ := doJSON(t, f.handler, "GET", f.intakeURL(bookingID, "whatever"), "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("disabled GET = %d, want 404", code)
	}
	code, _ = doJSON(t, f.handler, "POST", "/public/sites/acme-books/intake", "", map[string]any{
		"booking": bookingID.String(), "token": "whatever", "fields": map[string]string{"a": "b"},
	})
	if code != http.StatusNotFound {
		t.Fatalf("disabled POST = %d, want 404", code)
	}
}
