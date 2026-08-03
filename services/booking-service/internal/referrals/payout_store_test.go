package referrals

import (
	"context"
	"encoding/json"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
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
