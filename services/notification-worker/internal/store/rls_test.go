package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const rlsTestRole = "notif_rls_test"

func newRLSRolePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NOTIF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NOTIF_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	// Create the unprivileged subject role and grant table access.
	_, err = pool.Exec(ctx, fmt.Sprintf(`
		DO $$ BEGIN
		    CREATE ROLE %[1]s NOLOGIN;
		EXCEPTION WHEN duplicate_object THEN NULL;
		END $$;
		GRANT USAGE ON SCHEMA public TO %[1]s;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %[1]s;
	`, rlsTestRole))
	if err != nil {
		pool.Close()
		t.Fatalf("role setup: %v", err)
	}
	if err := EnsureSchema(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("ensure schema: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func countAsRole(ctx context.Context, pool *pgxpool.Pool, table, tenant string, unset bool) (int, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+rlsTestRole); err != nil {
		return 0, err
	}
	if unset {
		if _, err := tx.Exec(ctx, "RESET app.tenant_id"); err != nil {
			return 0, err
		}
	} else if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
		return 0, err
	}
	var n int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func TestRLSPoliciesPresent(t *testing.T) {
	pool := newRLSRolePool(t)
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
		       coalesce(array_agg(p.polname) FILTER (WHERE p.polname IS NOT NULL), '{}')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_policy p ON p.polrelid = c.oid
		WHERE n.nspname = 'public' AND c.relname = ANY($1)
		GROUP BY c.relname, c.relrowsecurity, c.relforcerowsecurity
	`, []string{"notification_deliveries", "notification_preferences", "digest_queue"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		var rls, force bool
		var policies []string
		if err := rows.Scan(&name, &rls, &force, &policies); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !rls || !force {
			t.Errorf("%s: rls=%v force=%v", name, rls, force)
		}
		found := false
		for _, p := range policies {
			if p == "tenant_isolation" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: tenant_isolation policy missing (%v)", name, policies)
		}
		seen[name] = true
	}
	for _, tbl := range []string{"notification_deliveries", "notification_preferences", "digest_queue"} {
		if !seen[tbl] {
			t.Errorf("table %s missing", tbl)
		}
	}
}

func TestRLSCrossTenantInvisibility(t *testing.T) {
	pool := newRLSRolePool(t)
	ctx := context.Background()
	ta := "11111111-1111-1111-1111-111111111111"
	tb := "22222222-2222-2222-2222-222222222222"
	store := New(pool)
	for _, tenant := range []string{ta, tb} {
		if err := store.Enqueue(ctx, tenant, "user-"+tenant[:4], "email", "subj", "body", time.Now()); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if n, err := countAsRole(ctx, pool, "notification_deliveries", ta, false); err != nil || n != 1 {
		t.Errorf("tenant a sees %d rows (err %v), want 1", n, err)
	}
	if n, err := countAsRole(ctx, pool, "notification_deliveries", tb, false); err != nil || n != 1 {
		t.Errorf("tenant b sees %d rows (err %v), want 1", n, err)
	}
	// Documented escape hatch: an UNSET GUC (NULL) still allows internal
	// tooling to see all rows; only a wrong/empty tenant denies.
	fresh := newRLSRolePool(t)
	if n, err := countAsRole(ctx, fresh, "notification_deliveries", "", true); err != nil || n != 2 {
		t.Errorf("unset GUC sees %d rows (err %v), want 2", n, err)
	}
}

func TestRLSEmptyStringTenantGucDeniesWithoutError(t *testing.T) {
	pool := newRLSRolePool(t)
	ctx := context.Background()
	ta := "11111111-1111-1111-1111-111111111111"
	store := New(pool)
	if err := store.Enqueue(ctx, ta, "user-x", "email", "s", "b", time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	for _, table := range []string{"notification_deliveries", "notification_preferences", "digest_queue"} {
		n, err := countAsRole(ctx, pool, table, "", false)
		if err != nil {
			t.Errorf("%s: empty tenant GUC raised error: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s: empty tenant GUC sees %d rows, want 0", table, n)
		}
	}
}

func TestRLSStoreMethodTenantScoping(t *testing.T) {
	pool := newRLSRolePool(t)
	ctx := context.Background()
	ta := "11111111-1111-1111-1111-111111111111"
	tb := "22222222-2222-2222-2222-222222222222"
	store := New(pool)
	if err := store.SetPreference(ctx, ta, "u1", "email", true); err != nil {
		t.Fatalf("set pref a: %v", err)
	}
	if err := store.SetPreference(ctx, tb, "u1", "email", false); err != nil {
		t.Fatalf("set pref b: %v", err)
	}
	enabled, ok, err := store.Preference(ctx, ta, "u1", "email")
	if err != nil || !ok || !enabled {
		t.Errorf("tenant a pref = (%v,%v,%v), want (true,true,nil)", enabled, ok, err)
	}
	enabled, ok, err = store.Preference(ctx, tb, "u1", "email")
	if err != nil || !ok || enabled {
		t.Errorf("tenant b pref = (%v,%v,%v), want (false,true,nil)", enabled, ok, err)
	}
}
