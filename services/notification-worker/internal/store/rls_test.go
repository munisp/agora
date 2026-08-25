package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// SQL-003: the notifications-DB tables must carry ENABLE + FORCE ROW LEVEL
// SECURITY with tenant_isolation policies, and with two tenant contexts
// cross-tenant rows must be invisible.
//
// The embedded-postgres superuser ("postgres") BYPASSES RLS entirely, so
// the invisibility assertions run through a dedicated NON-superuser role
// created by the test. The store's own tenant-scoped methods (withTenant)
// are exercised as the superuser — they prove the policy + SET LOCAL wiring
// coexists with the normal code paths.

const rlsTestRole = "notif_rls_test"

// newRLSRolePool creates a non-superuser login role with CRUD grants on the
// notifications tables and returns a pool connected as that role.
func newRLSRolePool(t *testing.T, st *Store) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	_, err := st.pool.Exec(ctx, fmt.Sprintf(`
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%[1]s') THEN
        CREATE ROLE %[1]s LOGIN PASSWORD '%[1]s';
    END IF;
END
$$;
GRANT USAGE ON SCHEMA public TO %[1]s;
GRANT SELECT, INSERT, UPDATE, DELETE ON webhook_subscriptions, webhook_deliveries, dnd_numbers, civic_notifications, ops_alerts TO %[1]s;`, rlsTestRole))
	require.NoError(t, err)

	pool, err := pgxpool.New(ctx,
		fmt.Sprintf("postgres://%[1]s:%[1]s@localhost:5434/notifications_test?sslmode=disable", rlsTestRole))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pool.Ping(ctx))
	return pool
}

// asTenant runs q through a single connection with the app.tenant_id GUC
// set (session-level on the acquired connection — the pool is dedicated to
// the test).
func asTenant(t *testing.T, pool *pgxpool.Pool, tenantID string, q string, args ...any) int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctx, `SELECT set_config('app.tenant_id', $1, false)`, tenantID)
	require.NoError(t, err)
	var n int64
	require.NoError(t, conn.QueryRow(ctx, q, args...).Scan(&n))
	return n
}

func TestRLSPoliciesPresent(t *testing.T) {
	st := newDNDTestStore(t)
	ctx := context.Background()
	for _, table := range []string{"webhook_subscriptions", "webhook_deliveries", "dnd_numbers", "civic_notifications", "ops_alerts"} {
		var enabled, forced bool
		require.NoError(t, st.pool.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, table).
			Scan(&enabled, &forced))
		require.True(t, enabled, "%s: RLS must be enabled", table)
		require.True(t, forced, "%s: RLS must be forced", table)
		var policy string
		require.NoError(t, st.pool.QueryRow(ctx,
			`SELECT policyname FROM pg_policies WHERE tablename = $1 AND policyname = 'tenant_isolation'`, table).
			Scan(&policy), "%s: tenant_isolation policy missing", table)
	}
}

func TestRLSCrossTenantInvisibility(t *testing.T) {
	st := newDNDTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()

	// Seed rows for both tenants via the store's tenant-scoped methods
	// (superuser connection; also proves withTenant wiring works).
	subA := &WebhookSubscription{TenantID: tenantA, TenantSlug: "acme", URL: "https://a.example/hook", Events: []string{"*"}}
	require.NoError(t, st.CreateSubscription(ctx, subA))
	subB := &WebhookSubscription{TenantID: tenantB, TenantSlug: "beta", URL: "https://b.example/hook", Events: []string{"*"}}
	require.NoError(t, st.CreateSubscription(ctx, subB))
	require.NoError(t, st.CreateDelivery(ctx, &WebhookDelivery{SubID: subA.ID, TenantID: tenantA, EventType: "com.opendesk.test.A"}))
	require.NoError(t, st.CreateDelivery(ctx, &WebhookDelivery{SubID: subB.ID, TenantID: tenantB, EventType: "com.opendesk.test.B"}))
	require.NoError(t, st.AddTenantOptOut(ctx, tenantA, "acme", "+2348077777777"))
	require.NoError(t, st.AddTenantOptOut(ctx, tenantB, "beta", "+2348088888888"))
	require.NoError(t, st.RecordCivicNotification(ctx, tenantA.String(), "acme", "CIV-A", "received", "sms", "+2348011111111", "sent", 1, ""))
	require.NoError(t, st.RecordCivicNotification(ctx, tenantB.String(), "beta", "CIV-B", "received", "sms", "+2348022222222", "sent", 1, ""))

	pool := newRLSRolePool(t, st)

	// Tenant A context: sees exactly its own rows.
	require.EqualValues(t, 1, asTenant(t, pool, tenantA.String(),
		`SELECT count(*) FROM webhook_subscriptions`))
	require.EqualValues(t, 1, asTenant(t, pool, tenantA.String(),
		`SELECT count(*) FROM webhook_deliveries`))
	require.EqualValues(t, 1, asTenant(t, pool, tenantA.String(),
		`SELECT count(*) FROM dnd_numbers WHERE tenant_id IS NOT NULL`))
	require.EqualValues(t, 1, asTenant(t, pool, tenantA.String(),
		`SELECT count(*) FROM civic_notifications`))

	// Tenant B context: tenant A's rows are invisible.
	require.EqualValues(t, 1, asTenant(t, pool, tenantB.String(),
		`SELECT count(*) FROM webhook_subscriptions`))
	require.EqualValues(t, 0, asTenant(t, pool, tenantB.String(),
		`SELECT count(*) FROM webhook_subscriptions WHERE tenant_id = $1`, tenantA))
	require.EqualValues(t, 0, asTenant(t, pool, tenantB.String(),
		`SELECT count(*) FROM webhook_deliveries WHERE tenant_id = $1`, tenantA))
	require.EqualValues(t, 0, asTenant(t, pool, tenantB.String(),
		`SELECT count(*) FROM dnd_numbers WHERE tenant_id = $1`, tenantA))
	require.EqualValues(t, 0, asTenant(t, pool, tenantB.String(),
		`SELECT count(*) FROM civic_notifications WHERE tenant_id = $1`, tenantA.String()))

	// The global NCC 2442 list stays visible under any tenant context.
	_, err := st.ImportGlobalDND(ctx, []string{"+2348099999999"}, "ncc2442")
	require.NoError(t, err)
	require.EqualValues(t, 1, asTenant(t, pool, tenantB.String(),
		`SELECT count(*) FROM dnd_numbers WHERE tenant_id IS NULL`))

	// Cross-tenant WRITE is rejected under a tenant context (WITH CHECK).
	ctxB := context.Background()
	conn, err := pool.Acquire(ctxB)
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctxB, `SELECT set_config('app.tenant_id', $1, false)`, tenantB.String())
	require.NoError(t, err)
	_, err = conn.Exec(ctxB,
		`INSERT INTO webhook_subscriptions (tenant_id, url) VALUES ($1, 'https://evil.example/')`, tenantA)
	require.Error(t, err, "inserting tenant A's row under tenant B's context must be rejected")

	// N-08 FAIL-CLOSED: a session WITHOUT app.tenant_id (NULL — never set)
	// sees NOTHING on the tenant tables; the legacy "NULL = no scoping"
	// escape hatch is gone. The only escape is role-gated
	// (app_notifications_internal membership), proven by
	// TestRLSInternalRoleEscape below.
	nullPool := newRLSRolePool(t, st)
	connNull, err := nullPool.Acquire(ctx)
	require.NoError(t, err)
	defer connNull.Release()
	var isNull bool
	require.NoError(t, connNull.QueryRow(ctx,
		`SELECT current_setting('app.tenant_id', true) IS NULL`).Scan(&isNull))
	require.True(t, isNull, "fresh connection must have NULL (never-set) GUC")
	var allRows int64
	require.NoError(t, connNull.QueryRow(ctx,
		`SELECT count(*) FROM webhook_subscriptions`).Scan(&allRows))
	require.Zero(t, allRows,
		"NULL (unset) GUC is fail-closed (N-08): zero rows without a tenant context")
}

// W40-6: a recycled pool connection may carry app.tenant_id='' (EMPTY string,
// not NULL). '' must NOT be treated as the legacy "no context" escape hatch
// (fail-open) and must NOT raise — the NULLIF-wrapped qual makes '' evaluate
// NULL → deny-by-default: every tenant table returns 0 rows without error.
func TestRLSEmptyStringTenantGucDeniesWithoutError(t *testing.T) {
	st := newDNDTestStore(t)
	ctx := context.Background()
	tenantA := uuid.New()

	subA := &WebhookSubscription{TenantID: tenantA, TenantSlug: "acme", URL: "https://a.example/hook", Events: []string{"*"}}
	require.NoError(t, st.CreateSubscription(ctx, subA))
	require.NoError(t, st.CreateDelivery(ctx, &WebhookDelivery{SubID: subA.ID, TenantID: tenantA, EventType: "com.opendesk.test.A"}))
	require.NoError(t, st.AddTenantOptOut(ctx, tenantA, "acme", "+2348077777777"))
	require.NoError(t, st.RecordCivicNotification(ctx, tenantA.String(), "acme", "CIV-A", "received", "sms", "+2348011111111", "sent", 1, ""))

	pool := newRLSRolePool(t, st)
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()

	// Session-level empty string — the recycled-connection state.
	_, err = conn.Exec(ctx, `SELECT set_config('app.tenant_id', '', false)`)
	require.NoError(t, err)
	var guc string
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT current_setting('app.tenant_id', true)`).Scan(&guc))
	require.Equal(t, "", guc)

	for _, q := range []string{
		`SELECT count(*) FROM webhook_subscriptions`,
		`SELECT count(*) FROM webhook_deliveries`,
		`SELECT count(*) FROM dnd_numbers WHERE tenant_id IS NOT NULL`,
		`SELECT count(*) FROM civic_notifications`,
	} {
		var n int64
		require.NoError(t, conn.QueryRow(ctx, q).Scan(&n),
			"%s must not error under empty-string tenant GUC", q)
		require.Zero(t, n, "%s must return 0 rows under empty-string tenant GUC", q)
	}
}

// N-08: the ONLY way to see rows without a tenant GUC is membership in the
// app_notifications_internal role (the INTERNAL_DATABASE_URL login). The
// bootstrap DDL creates the NOLOGIN group role; the test adds a LOGIN
// member and proves the escape is role-gated, not GUC-gated.
func TestRLSInternalRoleEscape(t *testing.T) {
	st := newDNDTestStore(t)
	ctx := context.Background()
	tenantA := uuid.New()
	subA := &WebhookSubscription{TenantID: tenantA, TenantSlug: "acme", URL: "https://a.example/hook", Events: []string{"*"}}
	require.NoError(t, st.CreateSubscription(ctx, subA))

	const role = "notif_rls_internal_test"
	_, err := st.pool.Exec(ctx, fmt.Sprintf(`
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%[1]s') THEN
        CREATE ROLE %[1]s LOGIN PASSWORD '%[1]s' IN ROLE app_notifications_internal;
    END IF;
END
$$;
GRANT USAGE ON SCHEMA public TO %[1]s;
GRANT SELECT, INSERT, UPDATE, DELETE ON webhook_subscriptions, webhook_deliveries, dnd_numbers, civic_notifications, ops_alerts TO %[1]s;`, role))
	require.NoError(t, err)

	pool, err := pgxpool.New(ctx,
		fmt.Sprintf("postgres://%[1]s:%[1]s@localhost:5434/notifications_test?sslmode=disable", role))
	require.NoError(t, err)
	defer pool.Close()
	require.NoError(t, pool.Ping(ctx))

	// No GUC set at all: the role-gated escape sees cross-tenant rows.
	var n int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM webhook_subscriptions`).Scan(&n))
	require.EqualValues(t, 1, n, "app_notifications_internal member sees rows without a tenant GUC")

	// And the global DND list (tenant_id NULL) is visible without a GUC.
	_, err = st.ImportGlobalDND(ctx, []string{"+2348012341234"}, "ncc2442")
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM dnd_numbers WHERE tenant_id IS NULL`).Scan(&n))
	require.EqualValues(t, 1, n)
}

// The store's tenant-scoped methods set app.tenant_id themselves: reading
// tenant A's subscription through tenant B's context yields ErrNotFound
// even on the superuser connection when the GUC is honored — here asserted
// through the non-superuser role where RLS actually applies.
func TestRLSStoreMethodTenantScoping(t *testing.T) {
	st := newDNDTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()

	subA := &WebhookSubscription{TenantID: tenantA, TenantSlug: "acme", URL: "https://a.example/hook", Events: []string{"*"}}
	require.NoError(t, st.CreateSubscription(ctx, subA))

	pool := newRLSRolePool(t, st)

	// withTenant-scoped ListSubscriptions under the role, via a raw
	// transaction mimicking withTenant (the Store pool is superuser, which
	// bypasses RLS, so the assertion goes through the role pool).
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantB.String())
	require.NoError(t, err)
	var n int64
	require.NoError(t, tx.QueryRow(ctx,
		`SELECT count(*) FROM webhook_subscriptions WHERE tenant_id = $1`, tenantA).Scan(&n))
	require.Zero(t, n, "tenant B context must not see tenant A's subscriptions")
}
