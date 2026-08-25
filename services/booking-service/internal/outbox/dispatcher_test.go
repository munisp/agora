package outbox

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/opendesk/booking-service/internal/daprc"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// SPEC-W44 W-B/F15-13: three consecutive FAILED dispatch cycles clear the
// ConsumerHealth flag (→ /healthz 503); the next successful cycle restores
// it. Embedded-postgres harness (dedicated port 5572; -short skips) with a
// fake Dapr sidecar that 500s until flipped healthy.
func TestDispatcherHealthFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping embedded-postgres dispatcher test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_outbox_test").
		Port(5572).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5572/booking_outbox_test?sslmode=disable"
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS outbox (
	    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	    aggregate_id UUID NOT NULL,
	    topic TEXT NOT NULL,
	    payload JSONB NOT NULL,
	    sent_at TIMESTAMPTZ
	)`); err != nil {
		t.Fatalf("outbox ddl: %v", err)
	}
	conn.Close(ctx) //nolint:errcheck
	st, err := store.New(ctx, dsn, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)

	// Fake Dapr sidecar: 500 (publish failure) until flipped healthy.
	var healthy atomic.Bool
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, "sidecar down", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sidecar.Close()
	host, portStr, err := net.SplitHostPort(sidecar.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	flag := &atomic.Bool{}
	flag.Store(true)
	d := New(st, daprc.New(host, port), "pubsub-kafka", time.Hour, zap.NewNop()).WithHealth(flag)

	// Seed one outbox row so a cycle has something to publish.
	if err := st.EnqueueOutbox(ctx, uuid.New(), "opendesk.booking.events",
		[]byte(`{"specversion":"1.0","type":"com.opendesk.booking.Test","source":"test","id":"1"}`)); err != nil {
		t.Fatalf("enqueue outbox: %v", err)
	}

	cycle := func() { d.noteCycle(d.dispatchOnce(ctx)) }

	// Failures 1-2 keep the flag set; the 3rd clears it.
	cycle()
	cycle()
	if !flag.Load() {
		t.Fatal("health flag cleared before the 3rd consecutive failure")
	}
	cycle()
	if flag.Load() {
		t.Fatal("health flag still set after 3 consecutive failed cycles")
	}

	// Recovery: the sidecar heals, the next successful cycle restores the
	// flag (and drains the outbox row).
	healthy.Store(true)
	cycle()
	if !flag.Load() {
		t.Fatal("health flag not restored after a successful cycle")
	}
	rows, err := st.FetchUnsentOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("fetch unsent: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("outbox not drained after recovery: %d rows", len(rows))
	}
	fmt.Println("cycles completed")
}
