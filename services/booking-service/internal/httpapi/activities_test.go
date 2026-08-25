package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// SPEC-W43 K-06/K-09: saga-activity state guards and consumer-liveness
// /healthz — against a real (embedded) Postgres (portal_test.go idiom; same
// 5432 default config, packages tests run sequentially).

type activityFixture struct {
	handler http.Handler
	store   *store.Store
	tenant  uuid.UUID
	health  *ConsumerHealth
}

// newPortalStore spins up the shared embedded Postgres (5432, same as
// portal_test.go — package tests run sequentially) with the minimal schema.
func newPortalStore(t *testing.T) *store.Store {
	t.Helper()
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_portal_test"))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5432/booking_portal_test?sslmode=disable"
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
	return st
}

func newActivityFixture(t *testing.T) *activityFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres activity test in -short mode")
	}
	st := newPortalStore(t)
	health := NewConsumerHealth()
	ops := &bookingops.Service{Store: st, EventsTopic: "test.events", Logger: zap.NewNop()}
	h := NewRouter(Deps{
		Store:         st,
		Ops:           ops,
		AuthzDisabled: true,
		Logger:        zap.NewNop(),
		Health:        health,
	})
	return &activityFixture{handler: h, store: st, tenant: uuid.New(), health: health}
}

// addBooking seeds one booking with the given status.
func (f *activityFixture) addBooking(t *testing.T, status string) store.Booking {
	t.Helper()
	ctx := context.Background()
	start := time.Now().UTC().Add(48 * time.Hour)
	b := store.Booking{
		TenantID: f.tenant, OfferingID: uuid.New(), TeamMemberID: uuid.New(), ContactID: uuid.New(),
		StartsAt: start, EndsAt: start.Add(30 * time.Minute),
		Status: status, Source: "api",
	}
	if err := f.store.CreateBookingTx(ctx, &b, store.SlotGuard{}, "test.events", []byte(`{}`)); err != nil {
		t.Fatalf("seed %s booking: %v", status, err)
	}
	return b
}

func (f *activityFixture) postActivity(t *testing.T, path string, b store.Booking) (int, map[string]any) {
	t.Helper()
	return doJSON(t, f.handler, "POST", path, "", map[string]string{
		"booking_id":  b.ID.String(),
		"tenant_id":   f.tenant.String(),
		"tenant_slug": "acme",
	})
}

// K-06 table test: mark-no-show rejects terminal states with 409 and still
// processes live ones.
func TestActivityMarkNoShowStateGuards(t *testing.T) {
	f := newActivityFixture(t)
	cases := []struct {
		status   string
		wantCode int
	}{
		{store.StatusPending, http.StatusOK},
		{store.StatusConfirmed, http.StatusOK},
		{store.StatusCancelled, http.StatusConflict},
		{store.StatusCompleted, http.StatusConflict},
		{store.StatusNoShow, http.StatusConflict},
	}
	for _, c := range cases {
		b := f.addBooking(t, c.status)
		code, out := f.postActivity(t, "/activities/mark-no-show", b)
		if code != c.wantCode {
			t.Fatalf("mark-no-show from %q: code=%d (%v), want %d", c.status, code, out, c.wantCode)
		}
		// Terminal-state bookings must not have moved.
		got, err := f.store.GetBooking(context.Background(), f.tenant, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := c.status
		if c.status == store.StatusPending || c.status == store.StatusConfirmed {
			want = store.StatusNoShow
		}
		if got.Status != want {
			t.Fatalf("booking from %q: status=%q, want %q", c.status, got.Status, want)
		}
	}
}

// K-06: reserve-slot on a cancelled booking reports reserved:false (the
// saga must compensate, not confirm a dead booking); pending stays true.
func TestActivityReserveSlotCancelled(t *testing.T) {
	f := newActivityFixture(t)

	cancelled := f.addBooking(t, store.StatusCancelled)
	code, out := f.postActivity(t, "/activities/reserve-slot", cancelled)
	if code != http.StatusOK {
		t.Fatalf("reserve-slot cancelled code=%d (%v)", code, out)
	}
	if out["reserved"] != false {
		t.Fatalf("reserve-slot cancelled reserved=%v, want false", out["reserved"])
	}

	pending := f.addBooking(t, store.StatusPending)
	code, out = f.postActivity(t, "/activities/reserve-slot", pending)
	if code != http.StatusOK || out["reserved"] != true {
		t.Fatalf("reserve-slot pending code=%d out=%v, want 200 reserved:true", code, out)
	}
}

// K-09: a cleared consumer liveness flag turns /healthz into a 503.
func TestHealthzConsumerLiveness(t *testing.T) {
	f := newActivityFixture(t)

	code, out := doJSON(t, f.handler, "GET", "/healthz", "", nil)
	if code != http.StatusOK {
		t.Fatalf("healthz code=%d (%v), want 200", code, out)
	}
	f.health.Register("commands").Store(false)
	code, out = doJSON(t, f.handler, "GET", "/healthz", "", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("healthz with dead consumer code=%d (%v), want 503", code, out)
	}
	failed, _ := out["failed_consumers"].([]any)
	if len(failed) != 1 || failed[0] != "commands" {
		t.Fatalf("failed_consumers = %v, want [commands]", out["failed_consumers"])
	}
}
