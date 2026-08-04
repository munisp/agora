package socialpub

import (
	"context"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SPEC-W21 Agent B store tests run against embedded Postgres (same harness
// as the workorders/helpdesk tests; dedicated port 5566 avoids the
// postmaster.pid race with sibling packages under `go test ./...`; -short
// skips).
//
// The harness also bootstraps the shared outbox table the package enqueues
// against (owned by the shared store package in production — mirrored here
// exactly like workorders/store_test.go does).

const testSupportDDL = `
CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at TIMESTAMPTZ
);`

func newTestStore(t *testing.T) *Store {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres socialpub store test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_socialpub_test").
		Port(5566).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })
	st, err := DialStore(context.Background(),
		"postgres://postgres:postgres@localhost:5566/booking_socialpub_test?sslmode=disable")
	if err != nil {
		t.Fatalf("DialStore: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.pool.Exec(context.Background(), testSupportDDL); err != nil {
		t.Fatalf("support DDL: %v", err)
	}
	return st
}

func mkAccount(tenantID uuid.UUID, providerID string) Account {
	return Account{
		TenantID:    tenantID,
		Provider:    providerID,
		AccountRef:  "acct-12345",
		DisplayName: "Campaign Page",
		Status:      AccountConnected,
	}
}

func mkCreative(tenantID uuid.UUID, name string) Creative {
	return Creative{
		TenantID: tenantID,
		Name:     name,
		Kind:     CreativeText,
		Body:     "Vote for progress — town hall on Saturday.",
	}
}

func mkAd(tenantID, accountID, creativeID uuid.UUID) Ad {
	return Ad{
		TenantID:        tenantID,
		AccountID:       accountID,
		CreativeID:      creativeID,
		Name:            "Ward 4 awareness",
		Objective:       ObjectiveAwareness,
		BudgetKobo:      500000, // ₦5,000
		DailyBudgetKobo: 100000, // ₦1,000/day
		Targeting: Targeting{
			LGAs:      []string{"Ikeja"},
			AgeMin:    18,
			AgeMax:    65,
			Interests: []string{"politics"},
		},
	}
}

// Account + creative + post + ad round-trips; jsonb fidelity; timestamps.
func TestCreateGetRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	a := mkAccount(tenantID, ProviderMeta)
	a.PoliticalAuth = true
	if err := st.CreateAccount(ctx, &a); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if a.ID == uuid.Nil || a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() {
		t.Fatalf("account id/timestamps not stamped: %+v", a)
	}
	gotA, err := st.GetAccount(ctx, tenantID, a.ID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if gotA.Provider != ProviderMeta || !gotA.PoliticalAuth || gotA.Status != AccountConnected {
		t.Fatalf("account round-trip mismatch: %+v", gotA)
	}

	c := mkCreative(tenantID, "Town hall")
	disc := "Paid for by the Progress Committee"
	c.DisclaimerText = &disc
	if err := st.CreateCreative(ctx, &c); err != nil {
		t.Fatalf("create creative: %v", err)
	}
	gotC, err := st.GetCreative(ctx, tenantID, c.ID)
	if err != nil {
		t.Fatalf("get creative: %v", err)
	}
	if gotC.DisclaimerText == nil || *gotC.DisclaimerText != disc {
		t.Fatalf("creative disclaimer fidelity: %+v", gotC)
	}

	p := Post{TenantID: tenantID, AccountID: a.ID, CreativeID: c.ID, Status: PostQueued}
	if err := st.CreatePost(ctx, &p); err != nil {
		t.Fatalf("create post: %v", err)
	}
	if err := st.CompletePublish(ctx, tenantID, p.ID, "mock-post-meta-abc", ""); err != nil {
		t.Fatalf("complete publish: %v", err)
	}
	gotP, err := st.GetPost(ctx, tenantID, p.ID)
	if err != nil {
		t.Fatalf("get post: %v", err)
	}
	if gotP.Status != PostPublished || gotP.ProviderPostID == nil || *gotP.ProviderPostID != "mock-post-meta-abc" || gotP.PublishedAt == nil {
		t.Fatalf("post publish round-trip mismatch: %+v", gotP)
	}

	ad := mkAd(tenantID, a.ID, c.ID)
	if err := st.CreateAd(ctx, &ad); err != nil {
		t.Fatalf("create ad: %v", err)
	}
	if ad.Status != AdDraft {
		t.Fatalf("new ad status = %s, want draft", ad.Status)
	}
	gotAd, err := st.GetAd(ctx, tenantID, ad.ID)
	if err != nil {
		t.Fatalf("get ad: %v", err)
	}
	if gotAd.BudgetKobo != 500000 || gotAd.DailyBudgetKobo != 100000 ||
		len(gotAd.Targeting.LGAs) != 1 || gotAd.Targeting.AgeMin != 18 || gotAd.Targeting.AgeMax != 65 {
		t.Fatalf("ad round-trip mismatch: %+v", gotAd)
	}

	// Launch edge: draft → review with provider id.
	launched, err := st.SetAdStatus(ctx, tenantID, ad.ID, AdReview, "mock-ad-meta-xyz", "")
	if err != nil {
		t.Fatalf("set review: %v", err)
	}
	if launched.Status != AdReview || launched.ProviderAdID == nil || *launched.ProviderAdID != "mock-ad-meta-xyz" {
		t.Fatalf("launch transition mismatch: %+v", launched)
	}
	// Illegal edge: review → draft is legal; draft → active is not.
	if _, err := st.SetAdStatus(ctx, tenantID, ad.ID, AdDraft, "", ""); err != nil {
		t.Fatalf("review → draft should be legal: %v", err)
	}
	if _, err := st.SetAdStatus(ctx, tenantID, ad.ID, AdActive, "", ""); err == nil {
		t.Fatalf("draft → active should be rejected")
	}
}

// List filters (provider/status for accounts, status for posts/ads).
func TestListFilters(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	m := mkAccount(tenantID, ProviderMeta)
	if err := st.CreateAccount(ctx, &m); err != nil {
		t.Fatalf("create meta: %v", err)
	}
	x := mkAccount(tenantID, ProviderX)
	x.Status = AccountExpired
	if err := st.CreateAccount(ctx, &x); err != nil {
		t.Fatalf("create x: %v", err)
	}

	all, err := st.ListAccounts(ctx, tenantID, "", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("list all = %d (%v), want 2", len(all), err)
	}
	metas, err := st.ListAccounts(ctx, tenantID, ProviderMeta, "")
	if err != nil || len(metas) != 1 || metas[0].Provider != ProviderMeta {
		t.Fatalf("list meta = %+v (%v)", metas, err)
	}
	expired, err := st.ListAccounts(ctx, tenantID, "", AccountExpired)
	if err != nil || len(expired) != 1 || expired[0].Status != AccountExpired {
		t.Fatalf("list expired = %+v (%v)", expired, err)
	}
}

// RLS isolation: a non-superuser role sees ONLY the rows of the tenant set
// via app.tenant_id — the database itself enforces isolation even if the
// application-level tenant_id filter were ever dropped (contract §1).
func TestRLSIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()

	aA := mkAccount(tenantA, ProviderMeta)
	if err := st.CreateAccount(ctx, &aA); err != nil {
		t.Fatalf("create A account: %v", err)
	}
	aB := mkAccount(tenantB, ProviderMeta)
	if err := st.CreateAccount(ctx, &aB); err != nil {
		t.Fatalf("create B account: %v", err)
	}
	cB := mkCreative(tenantB, "tenant B secret creative")
	if err := st.CreateCreative(ctx, &cB); err != nil {
		t.Fatalf("create B creative: %v", err)
	}

	// Restricted role with table privileges but no RLS bypass.
	if _, err := st.pool.Exec(ctx, `
		DO $$ BEGIN
		    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'socialpub_rls') THEN
		        CREATE ROLE socialpub_rls LOGIN PASSWORD 'socialpub_rls';
		    END IF;
		END $$;
		GRANT USAGE ON SCHEMA public TO socialpub_rls;
		GRANT SELECT, INSERT, UPDATE, DELETE ON social_accounts, social_creatives, social_posts, social_ads TO socialpub_rls;`); err != nil {
		t.Fatalf("create rls role: %v", err)
	}
	pool, err := pgxpool.New(ctx,
		"postgres://socialpub_rls:socialpub_rls@localhost:5566/booking_socialpub_test?sslmode=disable")
	if err != nil {
		t.Fatalf("dial rls role: %v", err)
	}
	defer pool.Close()

	// No tenant context → zero rows (policy compares against NULL).
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM social_accounts`).Scan(&n); err != nil {
		t.Fatalf("count without tenant: %v", err)
	}
	if n != 0 {
		t.Fatalf("rows visible without tenant context: %d", n)
	}

	// Tenant A context → exactly A's account; B's rows invisible to SELECT
	// and untouchable by UPDATE, across tables.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA.String()); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM social_accounts`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("tenant A sees %d accounts (%v), want 1", n, err)
	}
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM social_creatives`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("tenant A sees %d creatives (%v), want 0", n, err)
	}
	var cross string
	if err := tx.QueryRow(ctx, `SELECT display_name FROM social_accounts WHERE id=$1`, aB.ID).Scan(&cross); err == nil {
		t.Fatalf("cross-tenant account visible: %q", cross)
	}
	tag, err := tx.Exec(ctx, `UPDATE social_creatives SET name='pwned' WHERE id=$1`, cB.ID)
	if err != nil {
		t.Fatalf("cross update: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("cross-tenant UPDATE affected %d rows", tag.RowsAffected())
	}
}
