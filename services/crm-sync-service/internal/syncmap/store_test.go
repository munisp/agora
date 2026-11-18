package syncmap

import (
	"context"
	"errors"
	"fmt"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SPEC-W43 K-13: webhook replay dedupe tests run against a real (embedded)
// Postgres so the ON CONFLICT claim semantics are exercised for real.
// Skipped under -short. Port 5572 is dedicated to this package (booking's
// embedded-PG collision set is 5432/5433/5544+/5561-5566/5570/5571).
const syncmapTestPort = 5572

func newTestStore(t *testing.T) *Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres syncmap test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("crm_sync_test").
		Port(syncmapTestPort).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	st, err := New(ctx, fmt.Sprintf("postgres://postgres:postgres@localhost:%d/crm_sync_test?sslmode=disable", syncmapTestPort))
	if err != nil {
		t.Fatalf("syncmap.New: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestMarkWebhookSeenDedupes(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	fresh, err := st.MarkWebhookSeen(ctx, "evt-1")
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Fatal("first delivery of evt-1 must be fresh")
	}
	fresh, err = st.MarkWebhookSeen(ctx, "evt-1")
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Fatal("replay of evt-1 must be reported as duplicate")
	}
	// A different id is still fresh.
	fresh, err = st.MarkWebhookSeen(ctx, "evt-2")
	if err != nil || !fresh {
		t.Fatalf("evt-2 fresh=%v err=%v, want true/nil", fresh, err)
	}
	// Empty id never blocks (nothing to dedupe on).
	fresh, err = st.MarkWebhookSeen(ctx, "")
	if err != nil || !fresh {
		t.Fatalf("empty id fresh=%v err=%v, want true/nil", fresh, err)
	}
}

func TestMarkWebhookSeenPrunesOldRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, err := st.MarkWebhookSeen(ctx, "evt-old"); err != nil {
		t.Fatal(err)
	}
	// Backdate the row beyond the 24h replay window via a raw pool (Store
	// does not expose its pool).
	pool, err := pgxpool.New(ctx, fmt.Sprintf("postgres://postgres:postgres@localhost:%d/crm_sync_test?sslmode=disable", syncmapTestPort))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx,
		`UPDATE webhook_events_seen SET seen_at = now() - interval '25 hours' WHERE event_id='evt-old'`); err != nil {
		t.Fatal(err)
	}
	// The next Mark call prunes the expired row...
	if _, err := st.MarkWebhookSeen(ctx, "evt-new"); err != nil {
		t.Fatal(err)
	}
	// ...so a same-id delivery after the window is processed again.
	fresh, err := st.MarkWebhookSeen(ctx, "evt-old")
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Fatal("evt-old after the 24h window must be fresh again (pruned)")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM webhook_events_seen`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("rows = %d, want 2 (evt-new + re-seen evt-old)", n)
	}
}

// SPEC-W45 K9/K21: DisableTenant tombstones a tenant's rows; Get and
// GetByTwentyID treat them as unmapped. Idempotent on redelivery.
func TestDisableTenantIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tidA := uuid.New()
	tidB := uuid.New()

	if err := st.Put(ctx, "booking", "bk-a", "task-a", &tidA); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "contact_identity", "email:j@x.com", "person-a", &tidA); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "booking", "bk-b", "task-b", &tidB); err != nil {
		t.Fatal(err)
	}

	disabled, err := st.DisableTenant(ctx, tidA)
	if err != nil {
		t.Fatal(err)
	}
	if disabled != 2 {
		t.Fatalf("disabled = %d, want 2", disabled)
	}
	// Get: disabled rows are invisible.
	if _, err := st.Get(ctx, "booking", "bk-a", &tidA); !errors.Is(err, ErrNotFound) {
		t.Errorf("disabled row via Get: err = %v, want ErrNotFound", err)
	}
	// GetByTwentyID: disabled rows are invisible to the reverse lookup too.
	if _, err := st.GetByTwentyID(ctx, "booking", "task-a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("disabled row via GetByTwentyID: err = %v, want ErrNotFound", err)
	}
	// Other tenant untouched.
	if _, err := st.Get(ctx, "booking", "bk-b", &tidB); err != nil {
		t.Errorf("tenant B row must survive: %v", err)
	}
	// Idempotent: second run disables nothing and errors nothing.
	disabled, err = st.DisableTenant(ctx, tidA)
	if err != nil {
		t.Fatal(err)
	}
	if disabled != 0 {
		t.Errorf("second DisableTenant = %d, want 0 (idempotent)", disabled)
	}
}

// SPEC-W80 A7: the UNIQUE(kind, opendesk_id, tenant_id) constraint is
// retrofitted onto pre-existing tables by a guarded, idempotent migration
// (pg_constraint existence check) — simulate a legacy table by dropping the
// constraint, then re-run the retrofit.
func TestUniqueConstraintRetrofit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, fmt.Sprintf("postgres://postgres:postgres@localhost:%d/crm_sync_test?sslmode=disable", syncmapTestPort))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// Simulate a pre-A7 table (constraint missing).
	if _, err := pool.Exec(ctx, `ALTER TABLE sync_map DROP CONSTRAINT IF EXISTS sync_map_kind_opendesk_id_tenant_id_key`); err != nil {
		t.Fatal(err)
	}
	// The guarded retrofit adds it...
	if _, err := pool.Exec(ctx, constraintRetrofit); err != nil {
		t.Fatalf("retrofit: %v", err)
	}
	// ...and is idempotent (second run is a no-op, no error).
	if _, err := pool.Exec(ctx, constraintRetrofit); err != nil {
		t.Fatalf("retrofit must be idempotent: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		WHERE t.relname = 'sync_map' AND c.conname = 'sync_map_kind_opendesk_id_tenant_id_key'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("constraint count = %d, want 1", n)
	}
	// ON CONFLICT Put dedupes (no duplicate rows, no error).
	tid := uuid.New()
	if err := st.Put(ctx, "contact_identity", "email:j@x.com", "person-1", &tid); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "contact_identity", "email:j@x.com", "person-2", &tid); err != nil {
		t.Fatalf("upsert Put: %v", err)
	}
	m, err := st.Get(ctx, "contact_identity", "email:j@x.com", &tid)
	if err != nil || m.TwentyID != "person-2" {
		t.Errorf("after upsert: %+v, %v; want person-2", m, err)
	}
}

// SPEC-W80 A1: ListByOpendeskID enumerates EVERY tenant's mapping of a
// natural key (GDPR erase must find all per-tenant person matches);
// disabled rows stay invisible; first-synced (id ASC) order.
func TestListByOpendeskIDAcrossTenants(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tidA, tidB := uuid.New(), uuid.New()
	if err := st.Put(ctx, KindContactIdentity, "email:j@x.com", "person-a", &tidA); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, KindContactIdentity, "email:j@x.com", "person-b", &tidB); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListByOpendeskID(ctx, KindContactIdentity, "email:j@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (one per tenant)", len(rows))
	}
	if rows[0].TwentyID != "person-a" || rows[1].TwentyID != "person-b" {
		t.Errorf("order = %q, %q; want first-synced first (person-a, person-b)",
			rows[0].TwentyID, rows[1].TwentyID)
	}
	// Disabled (tombstoned) rows are invisible.
	if _, err := st.DisableTenant(ctx, tidA); err != nil {
		t.Fatal(err)
	}
	rows, err = st.ListByOpendeskID(ctx, KindContactIdentity, "email:j@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TwentyID != "person-b" {
		t.Errorf("after tombstone: %+v, want only person-b", rows)
	}
	// Unknown key -> empty, not error.
	rows, err = st.ListByOpendeskID(ctx, KindContactIdentity, "email:nobody@x.com")
	if err != nil || len(rows) != 0 {
		t.Errorf("unknown key: rows=%v err=%v, want empty/nil", rows, err)
	}
}
