package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store-level tests run against a real (embedded) Postgres so the RLS
// plumbing of SPEC-W43 I-03 / SPEC-W44 W-I is exercised for real: fail-closed
// policies, the app.tenant_id GUC scoping on memberships, and the
// app_identity_internal escape used by the tenants-table paths.
// Set -short to skip in constrained environments.
//
// Roles mirror 05-app-roles.sql: app_identity(_login) is the request-scoped
// runtime role; app_identity_internal(_login) is the cross-tenant escape
// (billing 0002 idiom).
//
// PARITY CONTRACT (SPEC-W44 F4 / V2-D1): this schema must stay an exact copy
// of infra/postgres/init-scripts/02-identity-schema.sql (columns, CHECK
// constraints, RLS policies). V2-D1 shipped because the embedded schema
// dropped the tenants.plan CHECK and masked the 23514 createTwin hit on real
// installs — do NOT weaken constraints here.
const storeTestSchema = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE IF NOT EXISTS tenants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    timezone    TEXT NOT NULL DEFAULT 'UTC',
    currency    CHAR(3) NOT NULL DEFAULT 'USD',
    locale      TEXT NOT NULL DEFAULT 'en-US',
    terminology JSONB NOT NULL DEFAULT '{}'::jsonb,
    industry    TEXT NOT NULL DEFAULT 'salon',
    plan        TEXT NOT NULL DEFAULT 'free'
                CHECK (plan IN ('free','pro','enterprise','twin')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS memberships (
    tenant_id UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id   UUID NOT NULL,
    role      TEXT NOT NULL DEFAULT 'staff'
              CHECK (role IN ('owner','admin','staff','viewer')),
    PRIMARY KEY (tenant_id, user_id)
);
ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants FORCE ROW LEVEL SECURITY;
ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenants
    USING (id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON memberships
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_identity') THEN
        CREATE ROLE app_identity NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_identity_login') THEN
        CREATE ROLE app_identity_login LOGIN PASSWORD 'pw' IN ROLE app_identity;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_identity_internal') THEN
        CREATE ROLE app_identity_internal NOLOGIN NOINHERIT;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_identity_internal_login') THEN
        CREATE ROLE app_identity_internal_login LOGIN PASSWORD 'pw' IN ROLE app_identity_internal;
    END IF;
END
$$;
GRANT USAGE ON SCHEMA public TO app_identity, app_identity_internal;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenants, memberships TO app_identity;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenants, memberships TO app_identity_internal;`

// setupStoreTest boots embedded PG, applies the schema as superuser, runs
// the bootstrap DDL as superuser (fresh-install path), and returns DSNs for
// the least-privilege roles.
func setupStoreTest(t *testing.T) (appDSN, internalDSN string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres store test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("identity_store_test").
		Port(5436).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	superDSN := "postgres://postgres:postgres@localhost:5436/identity_store_test?sslmode=disable"
	raw, err := pgxpool.New(ctx, superDSN)
	if err != nil {
		t.Fatalf("raw pool: %v", err)
	}
	if _, err := raw.Exec(ctx, storeTestSchema); err != nil {
		t.Fatalf("test schema: %v", err)
	}
	raw.Close()

	// Superuser bootstraps columns + policies (fresh-install path).
	st, err := New(ctx, superDSN)
	if err != nil {
		t.Fatalf("store.New (superuser): %v", err)
	}
	st.Close()
	return "postgres://app_identity_login:pw@localhost:5436/identity_store_test?sslmode=disable",
		"postgres://app_identity_internal_login:pw@localhost:5436/identity_store_test?sslmode=disable"
}

// TestBootstrapLeastPrivilegeTolerated: connecting as the least-privilege
// app role must boot cleanly (bootstrap DDL skipped with WARN, 42501) while
// the schema is already present.
func TestBootstrapLeastPrivilegeTolerated(t *testing.T) {
	appDSN, internalDSN := setupStoreTest(t)
	ctx := context.Background()
	st, err := New(ctx, appDSN, internalDSN)
	if err != nil {
		t.Fatalf("store.New (least-privilege): %v", err)
	}
	defer st.Close()
	if st.tenants == st.pool {
		t.Errorf("distinct internal DSN must yield a second pool")
	}
}

// TestIsTwinColumn: bootstrap added tenants.is_twin; CreateTenant persists it
// (internal-role pool) and GetTenantBySlug reads it back.
func TestIsTwinColumn(t *testing.T) {
	appDSN, internalDSN := setupStoreTest(t)
	ctx := context.Background()
	st, err := New(ctx, appDSN, internalDSN)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	twin := Tenant{Slug: "acme-twin-zz9pww", Name: "Twin", Plan: "twin", IsTwin: true}
	if err := st.CreateTenant(ctx, &twin); err != nil {
		t.Fatalf("create twin (internal role): %v", err)
	}
	got, err := st.GetTenantBySlug(ctx, "acme-twin-zz9pww")
	if err != nil {
		t.Fatalf("get twin: %v", err)
	}
	if !got.IsTwin {
		t.Errorf("is_twin not persisted: %+v", got)
	}
	if got.Plan != "twin" {
		t.Errorf("plan = %q", got.Plan)
	}
}

// TestPlanCheckParity (SPEC-W44 F4 / V2-D1 regression): the embedded schema
// carries the SAME tenants.plan CHECK as 02-identity-schema.sql, so this test
// fails if either side drifts again. 'twin' must be accepted (createTwin
// INSERTs it); anything outside free|pro|enterprise|twin must hit 23514.
// ('twin' staying out of the PUBLIC POST /v1/tenants plan set is app-level —
// httpapi validPlans — the DB only needs to accept it.)
func TestPlanCheckParity(t *testing.T) {
	appDSN, internalDSN := setupStoreTest(t)
	ctx := context.Background()
	st, err := New(ctx, appDSN, internalDSN)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	// Twin plan accepted (the V2-D1 500 path).
	if err := st.CreateTenant(ctx, &Tenant{
		Slug: "acme-twin-pp3kqq", Name: "Twin", Plan: "twin", IsTwin: true,
	}); err != nil {
		t.Fatalf("plan='twin' rejected by embedded schema CHECK (V2-D1 drift): %v", err)
	}

	// Bogus plan rejected with a CHECK violation (23514), proving the
	// constraint EXISTS in the embedded schema (its absence masked V2-D1).
	err = st.CreateTenant(ctx, &Tenant{Slug: "bogus-plan", Name: "Bogus", Plan: "gold"})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("plan='gold': err = %v, want pg 23514 check_violation", err)
	}

	// memberships.role CHECK parity probe (same drift class, raw insert via
	// the internal escape pool bypasses nothing — CHECKs are row-level).
	internalPool, err := pgxpool.New(ctx, internalDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer internalPool.Close()
	tenant := Tenant{Slug: "role-check", Name: "RoleCheck"}
	if err := st.CreateTenant(ctx, &tenant); err != nil {
		t.Fatal(err)
	}
	_, err = internalPool.Exec(ctx,
		`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'superuser')`,
		tenant.ID, uuid.New().String())
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("role='superuser': err = %v, want pg 23514 check_violation", err)
	}
}

// TestRLSFailClosedAndEscape proves the policy semantics with real roles:
//   - the app role sees ZERO tenants rows without a GUC (fail-closed), even
//     though rows exist;
//   - the tenants paths (lookup/create/delete) work via the internal escape
//     role (pg_has_role cannot be forged by a GUC);
//   - the app role cannot write tenants at all.
func TestRLSFailClosedAndEscape(t *testing.T) {
	appDSN, internalDSN := setupStoreTest(t)
	ctx := context.Background()
	st, err := New(ctx, appDSN, internalDSN)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	// Seed via the internal escape pool.
	if err := st.CreateTenant(ctx, &Tenant{Slug: "acme", Name: "Acme"}); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	// Fail-closed: the plain app pool (no GUC set on the tenants path —
	// tenants ops run on the internal pool, so use a raw app-role pool to
	// probe the policy directly).
	appPool, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer appPool.Close()
	var n int
	if err := appPool.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&n); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if n != 0 {
		t.Errorf("app role saw %d tenant rows without GUC, want 0 (fail-closed)", n)
	}
	// App role cannot insert tenants (WITH CHECK fails: id cannot match the
	// unset GUC, and it lacks the internal escape).
	if _, err := appPool.Exec(ctx,
		`INSERT INTO tenants (slug, name) VALUES ('evil', 'Evil')`); err == nil {
		t.Errorf("app role inserted into tenants — policy escape broken")
	}

	// Internal escape: lookup + delete through the service store work.
	if _, err := st.GetTenantBySlug(ctx, "acme"); err != nil {
		t.Errorf("internal lookup: %v", err)
	}
	if err := st.DeleteTenant(ctx, "acme"); err != nil {
		t.Errorf("internal delete: %v", err)
	}
	if _, err := st.GetTenantBySlug(ctx, "acme"); err != ErrNotFound {
		t.Errorf("post-delete lookup: %v, want ErrNotFound", err)
	}
}

// TestMembershipsGUCPlumbing: ListMembers/AddMember set app.tenant_id
// per-transaction, so the app role can read/write ONLY the scoped tenant's
// memberships (fail-closed for anything else).
func TestMembershipsGUCPlumbing(t *testing.T) {
	appDSN, internalDSN := setupStoreTest(t)
	ctx := context.Background()
	st, err := New(ctx, appDSN, internalDSN)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	a := Tenant{Slug: "acme", Name: "Acme"}
	b := Tenant{Slug: "other", Name: "Other"}
	if err := st.CreateTenant(ctx, &a); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTenant(ctx, &b); err != nil {
		t.Fatal(err)
	}
	uid := uuid.New()
	if err := st.AddMember(ctx, Membership{TenantID: a.ID, UserID: uid.String(), Role: "owner"}); err != nil {
		t.Fatalf("add member (GUC-scoped): %v", err)
	}
	members, err := st.ListMembers(ctx, a.ID)
	if err != nil || len(members) != 1 || members[0].Role != "owner" {
		t.Fatalf("list members: %v, %+v", err, members)
	}
	// Cross-tenant read via the app role fails closed (no rows), not an error.
	members, err = st.ListMembers(ctx, b.ID)
	if err != nil || len(members) != 0 {
		t.Errorf("other tenant members: %v, n=%d, want 0", err, len(members))
	}
	// Raw probe: without the GUC the app role sees nothing.
	appPool, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer appPool.Close()
	var n int
	if err := appPool.QueryRow(ctx, `SELECT count(*) FROM memberships`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("app role saw %d membership rows without GUC, want 0", n)
	}
	// Bootstrap applied the fail-closed + escape policy idiom.
	var qual string
	if err := appPool.QueryRow(ctx,
		`SELECT pg_get_expr(polqual, polrelid) FROM pg_policy
		 WHERE polrelid = 'memberships'::regclass AND polname = 'tenant_isolation'`).Scan(&qual); err != nil {
		t.Fatalf("policy expr: %v", err)
	}
	if !strings.Contains(qual, "NULLIF") || !strings.Contains(qual, "app_identity_internal") {
		t.Errorf("memberships policy = %s", qual)
	}
}
