package apps

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store-level app registry tests run against a real (embedded) Postgres so
// the provision upsert idempotency, partial-PATCH semantics, soft delete and
// the tenant_isolation RLS policy are exercised for real (consent store_test
// idiom). Run with -short to skip in constrained environments.

// testSchema is the minimal slice of 02-identity-schema.sql the apps tests
// need (the init script contains \c meta-commands and cannot be replayed
// through pgx).
const testSchema = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE IF NOT EXISTS tenants (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`

var testCatalog = []PlatformApp{
	{AppID: "cac", Name: "Customer Acquisition", Version: "1.0.0", DefaultPlanTier: "standard",
		RequiredPerms: []string{"manage_catalog"}},
	{AppID: "receptionist", Name: "AI Receptionist", Version: "1.0.0", DefaultPlanTier: "free",
		RequiredPerms: []string{"manage_bookings"}},
}

func newTestStore(t *testing.T) (*Store, uuid.UUID, uuid.UUID) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres store test in -short mode")
	}
	// Dedicated port + data dir so parallel packages don't race on the
	// consent tests' 5434 (booking-service newTestStore note).
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("identity_test").
		Port(5435).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	dsn := "postgres://postgres:postgres@localhost:5435/identity_test?sslmode=disable"
	// The tenants table must exist before apps.New bootstraps tenant_apps
	// (FK reference).
	raw, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("raw pool: %v", err)
	}
	if _, err := raw.Exec(ctx, testSchema); err != nil {
		t.Fatalf("test schema: %v", err)
	}
	raw.Close()

	st, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("apps.New: %v", err)
	}
	t.Cleanup(st.Close)

	if _, err := st.EnsureCatalog(ctx, testCatalog); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}

	mkTenant := func(slug string) uuid.UUID {
		id := uuid.New()
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO tenants (id, slug, name) VALUES ($1, $2, $3)`,
			id, slug, "Test Tenant"); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
		return id
	}
	return st, mkTenant("t-a-"+uuid.NewString()[:8]), mkTenant("t-b-"+uuid.NewString()[:8])
}

func TestStoreEnsureCatalogIdempotent(t *testing.T) {
	st, _, _ := newTestStore(t)
	ctx := context.Background()

	before, err := st.ListCatalog(ctx)
	if err != nil || len(before) != 2 {
		t.Fatalf("catalog: %v, n=%d", err, len(before))
	}

	// Boot replay: same content, created_at preserved.
	updated := []PlatformApp{
		{AppID: "cac", Name: "Customer Acquisition v2", Version: "1.1.0", DefaultPlanTier: "pro"},
		{AppID: "receptionist", Name: "AI Receptionist", Version: "1.0.0", DefaultPlanTier: "free"},
	}
	n, err := st.EnsureCatalog(ctx, updated)
	if err != nil || n != 2 {
		t.Fatalf("re-upsert: %v, n=%d", err, n)
	}
	after, err := st.ListCatalog(ctx)
	if err != nil || len(after) != 2 {
		t.Fatalf("catalog after replay: %v, n=%d", err, len(after))
	}
	byID := map[string]PlatformApp{}
	for _, a := range after {
		byID[a.AppID] = a
	}
	if byID["cac"].Name != "Customer Acquisition v2" || byID["cac"].DefaultPlanTier != "pro" {
		t.Errorf("content refresh failed: %+v", byID["cac"])
	}
	if !byID["cac"].CreatedAt.Equal(before[0].CreatedAt) && !byID["cac"].CreatedAt.Equal(before[1].CreatedAt) {
		t.Errorf("created_at must be preserved on upsert")
	}

	// Unknown vs known app lookup.
	if _, err := st.GetApp(ctx, "nope"); err != ErrUnknownApp {
		t.Errorf("GetApp unknown: %v, want ErrUnknownApp", err)
	}
	if _, err := st.GetApp(ctx, "cac"); err != nil {
		t.Errorf("GetApp known: %v", err)
	}
}

func TestStoreProvisionIdempotentAndSoftDelete(t *testing.T) {
	st, tenantA, _ := newTestStore(t)
	ctx := context.Background()

	row, prev, created, err := st.Provision(ctx, tenantA, "cac", "u-admin")
	if err != nil || !created || prev != "" {
		t.Fatalf("first provision: row=%+v prev=%q created=%v err=%v", row, prev, created, err)
	}
	if row.Status != StatusEnabled || row.Config == nil {
		t.Errorf("row: %+v", row)
	}

	// Replay: same provisioned_at/provisioned_by, not created.
	time.Sleep(10 * time.Millisecond)
	replay, prev, created, err := st.Provision(ctx, tenantA, "cac", "u-other")
	if err != nil || created || prev != StatusEnabled {
		t.Fatalf("replay: prev=%q created=%v err=%v", prev, created, err)
	}
	if !replay.ProvisionedAt.Equal(row.ProvisionedAt) || replay.ProvisionedBy != "u-admin" {
		t.Errorf("replay must keep provisioned_at/by: %+v", replay)
	}

	// Unknown app violates the FK -> ErrUnknownApp.
	if _, _, _, err := st.Provision(ctx, tenantA, "nope", "u-admin"); err != ErrUnknownApp {
		t.Errorf("provision unknown app: %v, want ErrUnknownApp", err)
	}

	// Soft delete: row retained, status disabled.
	dis, err := st.Disable(ctx, tenantA, "cac")
	if err != nil || dis.Status != StatusDisabled {
		t.Fatalf("disable: %+v err=%v", dis, err)
	}
	kept, err := st.GetTenantApp(ctx, tenantA, "cac")
	if err != nil || kept.Status != StatusDisabled {
		t.Errorf("row must be retained after soft delete: %+v err=%v", kept, err)
	}
	if _, err := st.Disable(ctx, tenantA, "receptionist"); err != ErrNotFound {
		t.Errorf("disable unprovisioned: %v, want ErrNotFound", err)
	}

	// Re-provision re-enables against the same row (audit continuity).
	re, prev, created, err := st.Provision(ctx, tenantA, "cac", "u-admin")
	if err != nil || created || prev != StatusDisabled || re.Status != StatusEnabled {
		t.Errorf("re-provision: prev=%q created=%v status=%q err=%v", prev, created, re.Status, err)
	}
	if !re.ProvisionedAt.Equal(row.ProvisionedAt) {
		t.Errorf("re-provision must keep original provisioned_at")
	}
}

func TestStorePatchPartialSemantics(t *testing.T) {
	st, tenantA, _ := newTestStore(t)
	ctx := context.Background()
	if _, _, _, err := st.Provision(ctx, tenantA, "cac", "u-admin"); err != nil {
		t.Fatal(err)
	}

	// Config-only patch: status untouched, whole document replaced.
	row, err := st.Patch(ctx, tenantA, "cac", nil, []byte(`{"greeting":"hi","n":1}`))
	if err != nil {
		t.Fatalf("config patch: %v", err)
	}
	if row.Status != StatusEnabled {
		t.Errorf("config-only patch changed status: %q", row.Status)
	}

	// Status-only patch: config preserved.
	susp := StatusSuspended
	row, err = st.Patch(ctx, tenantA, "cac", &susp, nil)
	if err != nil {
		t.Fatalf("status patch: %v", err)
	}
	if row.Status != StatusSuspended {
		t.Errorf("status = %q, want suspended", row.Status)
	}
	if string(row.Config) == "" || string(row.Config) == "{}" {
		t.Errorf("status-only patch must preserve config, got %s", row.Config)
	}

	// Both fields at once (jsonb normalizes formatting — compare decoded).
	en := StatusEnabled
	row, err = st.Patch(ctx, tenantA, "cac", &en, []byte(`{"v":2}`))
	if err != nil || row.Status != StatusEnabled {
		t.Fatalf("combined patch: %+v err=%v", row, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(row.Config, &cfg); err != nil || cfg["v"] != float64(2) {
		t.Errorf("combined patch config = %s", row.Config)
	}

	// Patch of a never-provisioned app -> ErrNotFound.
	if _, err := st.Patch(ctx, tenantA, "receptionist", &en, nil); err != ErrNotFound {
		t.Errorf("patch unprovisioned: %v, want ErrNotFound", err)
	}
}

func TestStoreListTenantAppsLeftJoin(t *testing.T) {
	st, tenantA, _ := newTestStore(t)
	ctx := context.Background()
	if _, _, _, err := st.Provision(ctx, tenantA, "cac", "u-admin"); err != nil {
		t.Fatal(err)
	}
	views, err := st.ListTenantApps(ctx, tenantA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("views = %d, want 2 (full catalog)", len(views))
	}
	byID := map[string]TenantAppView{}
	for _, v := range views {
		byID[v.AppID] = v
	}
	if byID["cac"].Status != StatusEnabled || byID["cac"].ProvisionedAt == nil {
		t.Errorf("cac view: %+v", byID["cac"])
	}
	v := byID["receptionist"]
	if v.Status != StatusNotProvisioned || string(v.Config) != "{}" || v.ProvisionedAt != nil {
		t.Errorf("receptionist view: %+v", v)
	}
	// Catalog fields ride along on every row.
	if v.Name != "AI Receptionist" || v.DefaultPlanTier != "free" {
		t.Errorf("catalog fields missing from view: %+v", v)
	}
}

func TestStoreTenantIsolation(t *testing.T) {
	st, tenantA, tenantB := newTestStore(t)
	ctx := context.Background()
	if _, _, _, err := st.Provision(ctx, tenantA, "cac", "u-admin"); err != nil {
		t.Fatal(err)
	}

	// Repository level: tenant B sees cac as not_provisioned everywhere.
	if _, err := st.GetTenantApp(ctx, tenantB, "cac"); err != ErrNotFound {
		t.Errorf("tenant B GetTenantApp: %v, want ErrNotFound", err)
	}
	views, err := st.ListTenantApps(ctx, tenantB)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		if v.Status != StatusNotProvisioned {
			t.Errorf("tenant B sees %s as %q (A's row leaked)", v.AppID, v.Status)
		}
	}

	// Database level: RLS enforced for a non-superuser role (the embedded
	// postgres superuser bypasses RLS, so create the app role the deployment
	// would use and verify the policy hides cross-tenant rows for real).
	if _, err := st.pool.Exec(ctx, `
		DROP ROLE IF EXISTS apps_rls_user;
		CREATE ROLE apps_rls_user LOGIN PASSWORD 'rls';
		GRANT USAGE ON SCHEMA public TO apps_rls_user;
		GRANT SELECT, INSERT, UPDATE ON tenant_apps TO apps_rls_user;
		GRANT SELECT ON platform_apps TO apps_rls_user;`); err != nil {
		t.Fatalf("create rls role: %v", err)
	}
	rlsPool, err := pgxpool.New(ctx,
		"postgres://apps_rls_user:rls@localhost:5435/identity_test?sslmode=disable")
	if err != nil {
		t.Fatalf("rls pool: %v", err)
	}
	defer rlsPool.Close()

	// No tenant context -> policy hides everything.
	var n int
	if err := rlsPool.QueryRow(ctx, `SELECT count(*) FROM tenant_apps`).Scan(&n); err != nil {
		t.Fatalf("count without context: %v", err)
	}
	if n != 0 {
		t.Errorf("RLS: %d rows visible without tenant context, want 0", n)
	}
	// Authoritative check inside an explicit tx (SET LOCAL semantics via
	// set_config(..., true)): tenant B sees 0 rows, tenant A sees its 1 row.
	tx, err := rlsPool.Begin(ctx)
	if err != nil {
		t.Fatalf("rls tx: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantB.String()); err != nil {
		t.Fatalf("set_config: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenant_apps`).Scan(&n); err != nil {
		t.Fatalf("count as B: %v", err)
	}
	if n != 0 {
		t.Errorf("RLS: tenant B sees %d rows, want 0 (A's row must be hidden)", n)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA.String()); err != nil {
		t.Fatalf("set_config A: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenant_apps`).Scan(&n); err != nil {
		t.Fatalf("count as A: %v", err)
	}
	if n != 1 {
		t.Errorf("RLS: tenant A sees %d rows, want 1", n)
	}
}

func TestStoreRLSPolicyPresent(t *testing.T) {
	st, _, _ := newTestStore(t)
	ctx := context.Background()
	var enabled, forced bool
	if err := st.pool.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'tenant_apps'`).
		Scan(&enabled, &forced); err != nil {
		t.Fatalf("rls flags: %v", err)
	}
	if !enabled || !forced {
		t.Errorf("RLS enabled=%v forced=%v, want true/true", enabled, forced)
	}
	var policy string
	if err := st.pool.QueryRow(ctx,
		`SELECT policyname FROM pg_policies WHERE tablename = 'tenant_apps' AND policyname = 'tenant_isolation'`).
		Scan(&policy); err != nil {
		t.Errorf("tenant_isolation policy missing: %v", err)
	}
	// platform_apps must stay RLS-free (global reference data — see Store doc).
	if err := st.pool.QueryRow(ctx,
		`SELECT relrowsecurity FROM pg_class WHERE relname = 'platform_apps'`).
		Scan(&enabled); err != nil {
		t.Fatalf("platform_apps rls flag: %v", err)
	}
	if enabled {
		t.Errorf("platform_apps must NOT have RLS (global catalog)")
	}
}
