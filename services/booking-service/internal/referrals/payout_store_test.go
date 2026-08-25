package referrals

import (
	"context"
	"encoding/json"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"time"
)

// Embedded-postgres store tests (same harness pattern as internal/store's
// newTestStore, dedicated port to avoid the postmaster.pid race under
// `go test ./...`).

// payoutTestSchema is the minimal outbox slice of 01-booking-schema.sql the
// payout store writes extra rows into.
const payoutTestSchema = `
CREATE TABLE IF NOT EXISTS outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_id UUID NOT NULL,
    topic TEXT NOT NULL,
    payload JSONB NOT NULL,
    sent_at TIMESTAMPTZ
);`

func newTestPayoutStore(t *testing.T) *PayoutStore {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping embedded-postgres payout store test in -short mode")
	}
	ep := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Username("postgres").Password("postgres").Database("booking_test").
		Port(5547).
		DataPath(t.TempDir()).
		RuntimePath(t.TempDir()))
	if err := ep.Start(); err != nil {
		t.Skipf("embedded postgres unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ep.Stop() })

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://postgres:postgres@localhost:5547/booking_test?sslmode=disable")
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, payoutTestSchema)
	require.NoError(t, err)
	st, err := NewPayoutStore(ctx, pool)
	require.NoError(t, err)
	return st
}

func queuedPayout(tenantID uuid.UUID) Payout {
	return Payout{
		ID:            uuid.New(),
		TenantID:      tenantID,
		BeneficiaryID: "agent-1",
		AmountNGN:     500_00,
	}
}

func TestPayoutStoreLifecycle(t *testing.T) {
	st := newTestPayoutStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	p := queuedPayout(tenantID)
	require.NoError(t, st.CreatePayout(ctx, &p))
	require.Equal(t, PayoutStatusQueued, p.Status)
	require.Equal(t, ProviderPaystack, p.Provider)
	require.False(t, p.CreatedAt.IsZero())

	got, err := st.GetPayout(ctx, tenantID, p.ID)
	require.NoError(t, err)
	require.Equal(t, PayoutStatusQueued, got.Status)

	// Other tenant cannot see it (app-level filter + RLS context).
	_, err = st.GetPayout(ctx, uuid.New(), p.ID)
	require.ErrorIs(t, err, errPayoutNotFound)

	// CAS queued → processing; a retry with the same ref is idempotent, a
	// conflicting ref is rejected.
	ref := PayoutReference(p.ID)
	require.NoError(t, st.BeginProcessing(ctx, tenantID, p.ID, ref))
	require.NoError(t, st.BeginProcessing(ctx, tenantID, p.ID, ref))
	require.Error(t, st.BeginProcessing(ctx, tenantID, p.ID, "cpay_other"))

	// Paid + metering row atomically; a replay with the same ref is a
	// no-op that must NOT rewrite the metering row (no double-metering).
	usage := OutboxRow{Topic: "opendesk.usage.events", Payload: []byte(`{"metric":"commission_payout"}`)}
	require.NoError(t, st.MarkPaid(ctx, tenantID, p.ID, ref, usage))
	require.NoError(t, st.MarkPaid(ctx, tenantID, p.ID, ref, usage)) // idempotent replay
	require.Error(t, st.MarkPaid(ctx, tenantID, p.ID, "cpay_other"))

	got, err = st.GetPayout(ctx, tenantID, p.ID)
	require.NoError(t, err)
	require.Equal(t, PayoutStatusPaid, got.Status)
	require.NotNil(t, got.PaidAt)
	require.Equal(t, ref, got.ProviderRef)

	var outboxRows int
	require.NoError(t, st.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE aggregate_id=$1 AND topic='opendesk.usage.events'`, p.ID).Scan(&outboxRows))
	require.Equal(t, 1, outboxRows, "metering row written once, on the actual transition only")

	// Terminal: a paid payout can never be marked failed.
	require.Error(t, st.MarkFailed(ctx, tenantID, p.ID, "late failure"))
	got, err = st.GetPayout(ctx, tenantID, p.ID)
	require.NoError(t, err)
	require.Equal(t, PayoutStatusPaid, got.Status)
}

func TestPayoutStoreMarkFailed(t *testing.T) {
	st := newTestPayoutStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	p := queuedPayout(tenantID)
	require.NoError(t, st.CreatePayout(ctx, &p))
	require.NoError(t, st.BeginProcessing(ctx, tenantID, p.ID, PayoutReference(p.ID)))
	require.NoError(t, st.MarkFailed(ctx, tenantID, p.ID, "provider 502: bad gateway"))

	got, err := st.GetPayout(ctx, tenantID, p.ID)
	require.NoError(t, err)
	require.Equal(t, PayoutStatusFailed, got.Status)
	require.Contains(t, got.FailureReason, "bad gateway")

	// Failed payouts are not processable.
	require.Error(t, st.BeginProcessing(ctx, tenantID, p.ID, PayoutReference(p.ID)))
}

func TestPayoutStoreReconCandidatesAndList(t *testing.T) {
	st := newTestPayoutStore(t)
	ctx := context.Background()
	t1, t2 := uuid.New(), uuid.New()

	processing := queuedPayout(t1)
	require.NoError(t, st.CreatePayout(ctx, &processing))
	require.NoError(t, st.BeginProcessing(ctx, t1, processing.ID, PayoutReference(processing.ID)))

	paid := queuedPayout(t1)
	require.NoError(t, st.CreatePayout(ctx, &paid))
	require.NoError(t, st.BeginProcessing(ctx, t1, paid.ID, PayoutReference(paid.ID)))
	require.NoError(t, st.MarkPaid(ctx, t1, paid.ID, PayoutReference(paid.ID)))

	failed := queuedPayout(t1)
	require.NoError(t, st.CreatePayout(ctx, &failed))
	require.NoError(t, st.MarkFailed(ctx, t1, failed.ID, "declined"))

	otherTenant := queuedPayout(t2)
	require.NoError(t, st.CreatePayout(ctx, &otherTenant))
	require.NoError(t, st.BeginProcessing(ctx, t2, otherTenant.ID, PayoutReference(otherTenant.ID)))

	// Recon: processing + recently-paid, cross-tenant; not failed, not queued.
	cands, err := st.ReconCandidates(ctx, 100)
	require.NoError(t, err)
	ids := map[uuid.UUID]bool{}
	for _, c := range cands {
		ids[c.ID] = true
	}
	require.True(t, ids[processing.ID])
	require.True(t, ids[paid.ID])
	require.True(t, ids[otherTenant.ID], "cross-tenant recon scan")
	require.False(t, ids[failed.ID])

	// Tenant queue listing (Agent C's payouts page).
	list, err := st.ListPayouts(ctx, t1, "", 10)
	require.NoError(t, err)
	require.Len(t, list, 3)
	list, err = st.ListPayouts(ctx, t1, PayoutStatusFailed, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, failed.ID, list[0].ID)
}

// FinalizePaid-style metering payload survives the outbox round trip
// (payload stored as JSONB and re-readable).
func TestPayoutStoreOutboxPayloadRoundTrip(t *testing.T) {
	st := newTestPayoutStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	p := queuedPayout(tenantID)
	require.NoError(t, st.CreatePayout(ctx, &p))
	ref := PayoutReference(p.ID)
	require.NoError(t, st.BeginProcessing(ctx, tenantID, p.ID, ref))
	payload, err := MarshalPayoutUsageRecord("acme", p)
	require.NoError(t, err)
	require.NoError(t, st.MarkPaid(ctx, tenantID, p.ID, ref, OutboxRow{Topic: "opendesk.usage.events", Payload: payload}))

	var raw []byte
	require.NoError(t, st.pool.QueryRow(ctx,
		`SELECT payload FROM outbox WHERE aggregate_id=$1`, p.ID).Scan(&raw))
	var evt map[string]any
	require.NoError(t, json.Unmarshal(raw, &evt))
	require.Equal(t, "com.opendesk.usage.UsageRecord", evt["type"])
}

// ---------------------------------------------------------------------------
// Payout feeding (SPEC-W44 W-B/S1-F7-08)
// ---------------------------------------------------------------------------

// feedTestSchema adds the commission_ledger + sites slices the feed query
// reads (the payout test schema only carries outbox + the PayoutStore's own
// commission_payouts).
const feedTestSchema = `
CREATE TABLE IF NOT EXISTS commission_ledger (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL,
    journal_id     UUID NOT NULL,
    account_code   INTEGER NOT NULL CHECK (account_code IN (300,301,302,303)),
    beneficiary_id TEXT NOT NULL DEFAULT '',
    debit_ngn      BIGINT NOT NULL DEFAULT 0,
    credit_ngn     BIGINT NOT NULL DEFAULT 0,
    ref_type       TEXT NOT NULL,
    ref_id         TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, ref_type, ref_id, account_code)
);
CREATE TABLE IF NOT EXISTS sites (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    tenant_slug TEXT NOT NULL,
    slug        TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    published   BOOLEAN NOT NULL DEFAULT TRUE,
    theme       JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);`

// ledgerRow inserts one commission_ledger row (feed test helper).
func ledgerRow(t *testing.T, st *PayoutStore, tenantID uuid.UUID, account int, beneficiary, refID string, debit, credit int64, age time.Duration) {
	t.Helper()
	_, err := st.pool.Exec(context.Background(),
		`INSERT INTO commission_ledger (tenant_id, journal_id, account_code, beneficiary_id, debit_ngn, credit_ngn, ref_type, ref_id, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,'accrual',$7, now() - $8::interval)`,
		tenantID, uuid.New(), account, beneficiary, debit, credit, refID, age.String())
	require.NoError(t, err)
}

func TestMaturedPayoutFeed(t *testing.T) {
	st := newTestPayoutStore(t)
	ctx := context.Background()
	_, err := st.pool.Exec(ctx, feedTestSchema)
	require.NoError(t, err)

	floor := int64(10000) // 100 NGN in kobo
	tenantA, tenantB := uuid.New(), uuid.New()

	// tenantA/agent-1: balance 40000 ≥ floor → feeds.
	ledgerRow(t, st, tenantA, 300, "agent-1", "r1", 0, 50000, 2*time.Hour)
	ledgerRow(t, st, tenantA, 300, "agent-1", "r2", 10000, 0, 2*time.Hour)
	// tenantA/agent-2: balance 5000 < floor → excluded.
	ledgerRow(t, st, tenantA, 300, "agent-2", "r3", 0, 5000, 2*time.Hour)
	// tenantB/agent-3: ≥ floor BUT has an open queued payout → excluded.
	ledgerRow(t, st, tenantB, 300, "agent-3", "r4", 0, 30000, 2*time.Hour)
	open := queuedPayout(tenantB)
	open.BeneficiaryID = "agent-3"
	require.NoError(t, st.CreatePayout(ctx, &open))
	// A non-300 account row must not count toward the payable balance.
	ledgerRow(t, st, tenantA, 303, "agent-1", "r5", 0, 99999, 2*time.Hour)

	candidates, err := st.MaturedPayoutCandidates(ctx, floor, 0, 100)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Equal(t, tenantA, candidates[0].TenantID)
	require.Equal(t, "agent-1", candidates[0].BeneficiaryID)
	require.Equal(t, int64(40000), candidates[0].BalanceKobo)

	// Maturity fence: with a 3h maturity the 2h-old accruals are not yet
	// matured → no candidates.
	candidates, err = st.MaturedPayoutCandidates(ctx, floor, 3*time.Hour, 100)
	require.NoError(t, err)
	require.Empty(t, candidates)

	// Feed queues one payout per candidate, slug resolved via the sites
	// registry, and the created row IS the open-payout fence for the next
	// cycle (double-feed safe).
	_, err = st.pool.Exec(ctx,
		`INSERT INTO sites (tenant_id, tenant_slug, slug) VALUES ($1,'acme-ng','acme-ng')`, tenantA)
	require.NoError(t, err)
	acts := &PayoutActivities{Store: st, MinKobo: floor, Logger: zap.NewNop()}
	fed, err := acts.FeedMatured(ctx, FeedMaturedInput{Limit: 100})
	require.NoError(t, err)
	require.Len(t, fed, 1)
	require.Equal(t, tenantA.String(), fed[0].TenantID)
	require.Equal(t, "acme-ng", fed[0].TenantSlug)
	p, err := st.GetPayout(ctx, tenantA, uuid.MustParse(fed[0].PayoutID))
	require.NoError(t, err)
	require.Equal(t, int64(40000), p.AmountNGN)
	require.Equal(t, PayoutStatusQueued, p.Status)
	require.Equal(t, ProviderPaystack, p.Provider)

	// Second feed run: agent-1 now has an open payout → nothing fed.
	fed, err = acts.FeedMatured(ctx, FeedMaturedInput{Limit: 100})
	require.NoError(t, err)
	require.Empty(t, fed)
}
