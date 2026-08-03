package store

import (
	"context"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// SPEC-W12 Agent B: DND registry store tests. They run against a real
// (embedded) Postgres — the same pattern as booking-service's store tests —
// so the partial unique indexes and the tenant→global check order are
// exercised for real. Skipped in -short mode; skips gracefully when the
// embedded binaries cannot be fetched.

func newDNDTestStore(t *testing.T) *Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres store test in -short mode")
	}
	// Dedicated port + data dir (other services' embedded stores use other
	// ports; DefaultConfig would share port 5432 and one data dir).
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("notifications_test").
		Port(5434).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	st, err := New(ctx, "postgres://postgres:postgres@localhost:5434/notifications_test?sslmode=disable")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestNormalizePhone(t *testing.T) {
	require.Equal(t, "+2348012345678", NormalizePhone("+234 801 234 5678"))
	require.Equal(t, "+2348012345678", NormalizePhone("+234-801-234-5678"))
	require.Equal(t, "+2348012345678", NormalizePhone("002348012345678"))
	require.Equal(t, "08012345678", NormalizePhone("0801 234 5678"))
	require.Equal(t, "", NormalizePhone("  "))
}

func TestDNDImportAndCheckOrder(t *testing.T) {
	st := newDNDTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	// Global NCC 2442 import; idempotent on re-import.
	inserted, err := st.ImportGlobalDND(ctx, []string{"+2348011111111", "+234 802 222 2222", ""}, "ncc2442")
	require.NoError(t, err)
	require.Equal(t, 2, inserted)
	inserted, err = st.ImportGlobalDND(ctx, []string{"+2348011111111", "+2348022222222"}, "")
	require.NoError(t, err)
	require.Equal(t, 0, inserted, "re-import of the same snapshot inserts nothing")

	// Global hit (normalized on the way in and on the way out).
	sup, reason, err := st.IsSuppressed(ctx, "acme", "+234 801 111 1111")
	require.NoError(t, err)
	require.True(t, sup)
	require.Equal(t, DNDReasonGlobalDND, reason)

	// Per-tenant opt-out beats/is found first for that tenant, and does not
	// leak to other tenants.
	require.NoError(t, st.AddTenantOptOut(ctx, tenantID, "acme", "+2348033333333"))
	sup, reason, err = st.IsSuppressed(ctx, "acme", "+2348033333333")
	require.NoError(t, err)
	require.True(t, sup)
	require.Equal(t, DNDReasonTenantOptOut, reason)
	sup, _, err = st.IsSuppressed(ctx, "other-tenant", "+2348033333333")
	require.NoError(t, err)
	require.False(t, sup, "tenant opt-outs must be tenant-scoped")
	sup, _, err = st.IsSuppressed(ctx, "", "+2348033333333")
	require.NoError(t, err)
	require.False(t, sup, "tenant opt-outs are not global")

	// Unknown number passes.
	sup, _, err = st.IsSuppressed(ctx, "acme", "+2348099999999")
	require.NoError(t, err)
	require.False(t, sup)
}

func TestDNDRemoveOptOutHonor(t *testing.T) {
	st := newDNDTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()

	// +2348055555555 is global-only; +2348066666666 is tenant-only (two
	// tenants), so the scoped-removal assertions isolate the tenant rows.
	_, err := st.ImportGlobalDND(ctx, []string{"+2348055555555"}, "ncc2442")
	require.NoError(t, err)
	require.NoError(t, st.AddTenantOptOut(ctx, tenantA, "acme", "+2348066666666"))
	require.NoError(t, st.AddTenantOptOut(ctx, tenantB, "beta", "+2348066666666"))

	// Tenant-scoped removal only touches that tenant's row.
	removed, err := st.RemoveDND(ctx, "+2348066666666", "acme")
	require.NoError(t, err)
	require.EqualValues(t, 1, removed)
	sup, _, err := st.IsSuppressed(ctx, "acme", "+2348066666666")
	require.NoError(t, err)
	require.False(t, sup)
	sup, _, err = st.IsSuppressed(ctx, "beta", "+2348066666666")
	require.NoError(t, err)
	require.True(t, sup, "other tenants' rows survive a tenant-scoped removal")

	// Full removal (no tenant): global + all tenant rows.
	removed, err = st.RemoveDND(ctx, "+2348055555555", "")
	require.NoError(t, err)
	require.EqualValues(t, 1, removed)
	sup, _, err = st.IsSuppressed(ctx, "acme", "+2348055555555")
	require.NoError(t, err)
	require.False(t, sup)

	// Removing an unknown number is a no-op.
	removed, err = st.RemoveDND(ctx, "+2348000000000", "")
	require.NoError(t, err)
	require.EqualValues(t, 0, removed)
}
