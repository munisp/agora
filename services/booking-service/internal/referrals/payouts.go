// Package referrals — SPEC-W14 Agent B slice: commission payouts + nightly
// reconciliation. Agent A owns the referrals domain (model, store, rules,
// ledger, service, handlers); this file adds the payout store, the payout
// provider client (Paystack-shape, mockable) and the Temporal execution
// activities. See payout_workflow.go / recon_workflow.go for the workflows
// and docs/commission-payouts.md for the operator-facing contract.
package referrals

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/opendesk/booking-service/internal/events"
	"go.temporal.io/sdk/temporal"
	"go.uber.org/zap"
)

// Shared contract shapes (Payout, BalancedPosting, Ledger, account codes
// 300–303, PayoutStatus*/ProviderPaystack/ProviderFlutterwave consts,
// RefTypePayout) come from Agent A's model.go / ledger.go (+
// internal/store/referrals.go) — the placeholder block that stood in
// before A landed was deleted at integration per SPEC-W14 §Agent B
// COORDINATION. ProviderMock stays here (A does not export it).

// ProviderMock is the PAYOUT_MOCK provider name (Agent B impl detail —
// deliberately NOT part of the contract §4 enum A exports).
const ProviderMock = "mock"

// Usage metrics (outbox metering rows on opendesk.usage.events, same
// pattern as geo_campaign_message / incident_alert_message).
const (
	// UsageMetricCommissionPayout is metered once per paid payout.
	// COORDINATION FLAG: SPEC-W14 Agent D lists commission_payout under its
	// usage-metering item — it is emitted HERE because the payout-paid
	// transition lives in this workflow; D must not duplicate the row.
	UsageMetricCommissionPayout = "commission_payout"
	// UsageMetricCommissionReconAlert is metered once per recon mismatch
	// alert (contract §5 "metered notification").
	UsageMetricCommissionReconAlert = "commission_recon_alert"
)

// EventTypeCommissionReconAlert is the CloudEvents type of the recon
// mismatch alert row (contract §5: outbox alert row, kind
// commission_recon_alert) published to the notifications outbox topic. The
// notification-worker consumer is forward-compatible (unknown types are
// acknowledged), so the row is a durable alert record + Kafka signal today.
const EventTypeCommissionReconAlert = "com.opendesk.notifications.CommissionReconAlert"

// ---------------------------------------------------------------------------
// Payout provider client (contract §4)
// ---------------------------------------------------------------------------

// TransferRequest is one provider cash-out. Reference is deterministic
// (derived from the payout ID) so activity retries are idempotent
// end-to-end at the provider.
type TransferRequest struct {
	PayoutID      uuid.UUID `json:"payout_id"`
	TenantID      uuid.UUID `json:"tenant_id"`
	BeneficiaryID string    `json:"beneficiary_id"`
	AmountNGN     int64     `json:"amount_ngn"` // kobo
	Currency      string    `json:"currency"`   // NGN
	Reason        string    `json:"reason"`
	Reference     string    `json:"reference"`
}

// TransferResult is the provider's synchronous answer to a transfer.
type TransferResult struct {
	ProviderRef string `json:"provider_ref"`
	Status      string `json:"status"` // success | pending | failed
}

// TransferStatusResult is the provider's current view of a transfer (recon).
type TransferStatusResult struct {
	ProviderRef string `json:"provider_ref"`
	Status      string `json:"status"` // success | pending | failed | reversed
	AmountNGN   int64  `json:"amount_ngn"`
}

// PayoutProvider abstracts the cash-out rail so the workflows stay
// mockable (contract §4/§5; the mock is an explicit PAYOUT_MOCK=1 dev
// opt-in since W39 — default fail-closed).
type PayoutProvider interface {
	Name() string // paystack | flutterwave | mock
	Transfer(ctx context.Context, in TransferRequest) (TransferResult, error)
	TransferStatus(ctx context.Context, providerRef string) (TransferStatusResult, error)
}

// PayoutReference derives the deterministic provider reference / ledger
// idempotency key for a payout (sha256 of the payout UUID, prefixed).
func PayoutReference(payoutID uuid.UUID) string {
	sum := sha256.Sum256([]byte("commission-payout|" + payoutID.String()))
	return "cpay_" + hex.EncodeToString(sum[:])[:24]
}

// ---------------------------------------------------------------------------
// Paystack-shape HTTP client
// ---------------------------------------------------------------------------
//
// ASSUMPTION (annotated per SPEC-W14 §4): the Paystack transfer API shape
// below is an ASSUMPTION — payments-service has no Paystack transfer
// endpoint today (its /v1/payouts is a Mojaloop rail). The assumed shape:
//
//	POST {BaseURL}/transfer
//	  Authorization: Bearer <PAYOUT_PROVIDER_SECRET>
//	  {"source":"balance","amount":<kobo>,"recipient":<beneficiary_id>,
//	   "reason":...,"reference":...}
//	  → {"status":true,"message":"...","data":{"reference":"...",
//	     "transfer_code":"...","status":"success"}}
//	GET {BaseURL}/transfer/{reference}
//	  → {"status":true,"data":{"reference":"...","status":"success","amount":<kobo>}}
//
// Deployment per contract §4 ("execution via payments-service Dapr invoke"):
// point PAYOUT_PROVIDER_BASE_URL at the Dapr invoke gateway of
// payments-service, e.g.
// http://daprd-booking:3500/v1.0/invoke/payments/method — the client then
// POSTs {gateway}/transfer through the sidecar. If payments-service adopts
// a different wire shape, only this file changes.

// PaystackClient is the Paystack-shape PayoutProvider (see ASSUMPTION).
type PaystackClient struct {
	BaseURL string // default https://api.paystack.co
	Secret  string
	hc      *http.Client
}

// NewPaystackClient builds the client; baseURL empty → Paystack live API.
func NewPaystackClient(baseURL, secret string) *PaystackClient {
	if baseURL == "" {
		baseURL = "https://api.paystack.co"
	}
	return &PaystackClient{BaseURL: strings.TrimRight(baseURL, "/"), Secret: secret, hc: &http.Client{Timeout: 20 * time.Second}}
}

// Name implements PayoutProvider.
func (c *PaystackClient) Name() string { return ProviderPaystack }

// paystackEnvelope mirrors the assumed Paystack response envelope.
type paystackEnvelope struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    struct {
		Reference    string `json:"reference"`
		TransferCode string `json:"transfer_code"`
		Status       string `json:"status"`
		Amount       int64  `json:"amount"`
	} `json:"data"`
}

func (c *PaystackClient) do(ctx context.Context, method, path string, body any, out *paystackEnvelope) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal paystack request: %w", err)
		}
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.Secret)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("paystack %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("paystack read body: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("paystack %s %s: HTTP %d: %s", method, path, resp.StatusCode, truncate(string(raw), 256))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("paystack decode: %w", err)
	}
	if !out.Status {
		return fmt.Errorf("paystack: %s", out.Message)
	}
	return nil
}

// Transfer initiates the payout (idempotent via the deterministic
// reference; ASSUMPTION shape — see file comment).
func (c *PaystackClient) Transfer(ctx context.Context, in TransferRequest) (TransferResult, error) {
	var env paystackEnvelope
	err := c.do(ctx, http.MethodPost, "/transfer", map[string]any{
		"source":    "balance",
		"amount":    in.AmountNGN, // kobo
		"recipient": in.BeneficiaryID,
		"reason":    in.Reason,
		"reference": in.Reference,
		"currency":  in.Currency,
	}, &env)
	if err != nil {
		return TransferResult{}, err
	}
	ref := env.Data.TransferCode
	if ref == "" {
		ref = env.Data.Reference
	}
	return TransferResult{ProviderRef: ref, Status: env.Data.Status}, nil
}

// TransferStatus fetches the provider's current transfer state (recon).
func (c *PaystackClient) TransferStatus(ctx context.Context, providerRef string) (TransferStatusResult, error) {
	var env paystackEnvelope
	if err := c.do(ctx, http.MethodGet, "/transfer/"+providerRef, nil, &env); err != nil {
		return TransferStatusResult{}, err
	}
	ref := env.Data.TransferCode
	if ref == "" {
		ref = env.Data.Reference
	}
	return TransferStatusResult{ProviderRef: ref, Status: env.Data.Status, AmountNGN: env.Data.Amount}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// Deterministic mock provider (PAYOUT_MOCK=1 dev opt-in; default OFF)
// ---------------------------------------------------------------------------

// MockProvider is the DEV-ONLY simulated provider (PAYOUT_MOCK=1 opt-in
// since W39 — previously the silent default): deterministic, no network,
// NO money movement. Transfer succeeds with provider_ref = the
// deterministic PayoutReference; TransferStatus reports "success" for any
// reference it issued. Test hooks (documented, still deterministic):
//   - beneficiary "mock-fail"          → Transfer errors (declined)
//   - beneficiary "mock-pending"       → Transfer status "pending"
//   - status lookup of an unknown ref  → "failed" (never issued ⇒ mismatch)
type MockProvider struct{}

// Name implements PayoutProvider.
func (MockProvider) Name() string { return ProviderMock }

// Transfer implements PayoutProvider.
func (MockProvider) Transfer(_ context.Context, in TransferRequest) (TransferResult, error) {
	switch in.BeneficiaryID {
	case "mock-fail":
		return TransferResult{}, errors.New("mock provider: transfer declined (beneficiary mock-fail)")
	case "mock-pending":
		return TransferResult{ProviderRef: in.Reference, Status: "pending"}, nil
	default:
		return TransferResult{ProviderRef: in.Reference, Status: "success"}, nil
	}
}

// TransferStatus implements PayoutProvider.
func (MockProvider) TransferStatus(_ context.Context, providerRef string) (TransferStatusResult, error) {
	if strings.HasPrefix(providerRef, "cpay_") {
		return TransferStatusResult{ProviderRef: providerRef, Status: "success"}, nil
	}
	return TransferStatusResult{ProviderRef: providerRef, Status: "failed"}, nil
}

// ---------------------------------------------------------------------------
// Env-driven provider selection (contract §7)
// ---------------------------------------------------------------------------

const (
	// EnvPayoutMock gates the mock: DEFAULT OFF since W39 (SIM-002) —
	// truthy (1/true/yes/on) explicitly opts into the deterministic
	// MockProvider for dev; otherwise a real rail must be configured or
	// ProviderFromEnv fails closed.
	EnvPayoutMock = "PAYOUT_MOCK"
	// EnvPayoutProvider selects the real rail: paystack (default) | flutterwave.
	EnvPayoutProvider = "PAYOUT_PROVIDER"
	// EnvPayoutProviderBaseURL overrides the provider base URL (Dapr invoke
	// gateway of payments-service in deployment — see ASSUMPTION above).
	EnvPayoutProviderBaseURL = "PAYOUT_PROVIDER_BASE_URL"
	// EnvPayoutProviderSecret is the provider API secret.
	EnvPayoutProviderSecret = "PAYOUT_PROVIDER_SECRET"
	// EnvPayoutMinNGN is the minimum payout, WHOLE NAIRA (contract §7
	// PAYOUT_MIN_NGN=100 → 10000 kobo). ASSUMPTION: the env var is denominated
	// in naira even though ledger amounts are kobo; see MinPayoutKobo.
	EnvPayoutMinNGN = "PAYOUT_MIN_NGN"
)

// ErrPayoutProviderNotConfigured is the explicit fail-closed error when
// the mock is not opted into (PAYOUT_MOCK off) and no real rail is
// configured (neither PAYOUT_PROVIDER_BASE_URL nor
// PAYOUT_PROVIDER_SECRET set). No payout may ever be marked sent without
// a real rail (W39 SIM-002).
var ErrPayoutProviderNotConfigured = errors.New("payout provider not configured: set PAYOUT_PROVIDER_BASE_URL/PAYOUT_PROVIDER_SECRET for the live rail, or PAYOUT_MOCK=1 to opt into the dev simulation")

// payoutMockAllowed reports whether the deterministic mock provider is
// explicitly opted in (PAYOUT_MOCK truthy). Default OFF (fail closed).
func payoutMockAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvPayoutMock))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ProviderFromEnv builds the PayoutProvider from the environment.
// PAYOUT_MOCK truthy → deterministic MockProvider (dev opt-in).
// Otherwise PAYOUT_PROVIDER (default paystack) selects the real client —
// but only when a real rail is actually configured
// (PAYOUT_PROVIDER_BASE_URL or PAYOUT_PROVIDER_SECRET); with neither, it
// returns ErrPayoutProviderNotConfigured so the caller can fail closed
// instead of silently simulating payouts.
// Flutterwave is contract-listed but not yet implemented — it falls back to
// the Paystack-shape client (same assumed envelope) and is flagged in logs
// by the caller via Name().
func ProviderFromEnv() (PayoutProvider, error) {
	if payoutMockAllowed() {
		return MockProvider{}, nil
	}
	baseURL, secret := os.Getenv(EnvPayoutProviderBaseURL), os.Getenv(EnvPayoutProviderSecret)
	if strings.TrimSpace(baseURL) == "" && strings.TrimSpace(secret) == "" {
		return nil, ErrPayoutProviderNotConfigured
	}
	switch strings.ToLower(os.Getenv(EnvPayoutProvider)) {
	case ProviderFlutterwave:
		// Contract §4 lists flutterwave; its transfer shape is not specified
		// in this wave — reuse the Paystack-shape client against the
		// flutterwave base URL (flagged ASSUMPTION, single-file change).
		c := NewPaystackClient(baseURL, secret)
		return &flutterwaveShim{PaystackClient: c}, nil
	default:
		return NewPaystackClient(baseURL, secret), nil
	}
}

// flutterwaveShim reports the flutterwave provider name while reusing the
// (assumed-identical) transfer envelope.
type flutterwaveShim struct{ *PaystackClient }

// Name implements PayoutProvider.
func (f *flutterwaveShim) Name() string { return ProviderFlutterwave }

// MinPayoutKobo converts PAYOUT_MIN_NGN (whole naira) to kobo.
// ASSUMPTION annotated at EnvPayoutMinNGN.
func MinPayoutKobo(minNGN int64) int64 { return minNGN * 100 }

// MinPayoutFromEnv reads PAYOUT_MIN_NGN (default 100 naira → 10000 kobo).
func MinPayoutFromEnv() int64 {
	v := strings.TrimSpace(os.Getenv(EnvPayoutMinNGN))
	if v == "" {
		return MinPayoutKobo(100)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return MinPayoutKobo(100)
	}
	return MinPayoutKobo(n)
}

// Payout store (SPEC-W14 §4, RLS incidents pattern). The store is
// self-contained (own pool) so it compiles and runs independently of Agent
// A's referrals store; if A's store grows payout methods at integration,
// this type can be retired in favor of it (my call sites: activities in
// payouts_activities.go, wiring in main.go's additive block).
//
// Table: commission_payouts — tenant-scoped, RLS forced, tenant_isolation
// policy identical to incidents/leads (defence-in-depth on top of the
// application-level tenant filters).

// errPayoutNotFound mirrors store.ErrNotFound without importing the store
// package (keeps the referrals package dependency surface minimal).
var errPayoutNotFound = errors.New("payout not found")

// OutboxRow is one transactional-outbox row written in the same
// transaction as a payout mutation (mirrors store.ExtraOutbox).
type OutboxRow struct {
	Topic   string
	Payload []byte
}

// PayoutStore persists commission payouts in Postgres.
type PayoutStore struct {
	pool    *pgxpool.Pool
	ownPool bool // true when opened via DialPayoutStore
}

// NewPayoutStore wraps an existing pool and ensures the schema.
func NewPayoutStore(ctx context.Context, pool *pgxpool.Pool) (*PayoutStore, error) {
	s := &PayoutStore{pool: pool}
	if err := s.ensureSchema(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// DialPayoutStore opens a small dedicated pool (main wiring path — the
// shared store.Store does not expose its pool). maxConns 4: payouts/recon
// are low-QPS background paths.
func DialPayoutStore(ctx context.Context, databaseURL string) (*PayoutStore, error) {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}
	poolCfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	s, err := NewPayoutStore(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	s.ownPool = true
	return s, nil
}

// Close releases the pool when this store opened it.
func (s *PayoutStore) Close() {
	if s.ownPool {
		s.pool.Close()
	}
}

// ensureSchema bootstraps commission_payouts idempotently (same pattern as
// store.ensureIncidentTables).
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *PayoutStore) ensureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS commission_payouts (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL,
    beneficiary_id TEXT NOT NULL,
    amount_ngn     BIGINT NOT NULL CHECK (amount_ngn > 0),
    status         TEXT NOT NULL DEFAULT 'queued'
                   CHECK (status IN ('queued','processing','paid','failed')),
    provider       TEXT NOT NULL DEFAULT 'paystack',
    provider_ref   TEXT NOT NULL DEFAULT '',
    failure_reason TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at        TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_commission_payouts_tenant_status
    ON commission_payouts (tenant_id, status, created_at);
CREATE INDEX IF NOT EXISTS idx_commission_payouts_provider_ref
    ON commission_payouts (provider, provider_ref);
ALTER TABLE commission_payouts ENABLE ROW LEVEL SECURITY;
ALTER TABLE commission_payouts FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'commission_payouts' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON commission_payouts
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure commission_payouts table: %w", err)
	}
	return nil
}

// withTenant runs fn inside a transaction with `SET LOCAL app.tenant_id`
// applied so the tenant_isolation RLS policy scopes every statement.
// Duplicated from store.Store's private helper (that one is unexported;
// same parameter-binding-safe set_config call).
func (s *PayoutStore) withTenant(ctx context.Context, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const payoutCols = `id, tenant_id, beneficiary_id, amount_ngn, status, provider, provider_ref, failure_reason, created_at, paid_at`

func scanPayout(row pgx.Row) (Payout, error) {
	var p Payout
	err := row.Scan(&p.ID, &p.TenantID, &p.BeneficiaryID, &p.AmountNGN, &p.Status,
		&p.Provider, &p.ProviderRef, &p.FailureReason, &p.CreatedAt, &p.PaidAt)
	return p, err
}

// CreatePayout inserts a queued payout (contract §4). The caller (Agent A's
// service / rules path) enforces tenant authorization.
func (s *PayoutStore) CreatePayout(ctx context.Context, p *Payout) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if p.Status == "" {
		p.Status = PayoutStatusQueued
	}
	if p.Provider == "" {
		p.Provider = ProviderPaystack
	}
	const q = `INSERT INTO commission_payouts (` + payoutCols + `)
	           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now(),NULL) RETURNING created_at`
	return s.withTenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, p.ID, p.TenantID, p.BeneficiaryID, p.AmountNGN,
			p.Status, p.Provider, p.ProviderRef, p.FailureReason).Scan(&p.CreatedAt)
	})
}

// GetPayout fetches one payout scoped to a tenant.
func (s *PayoutStore) GetPayout(ctx context.Context, tenantID, id uuid.UUID) (Payout, error) {
	var p Payout
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		p, err = scanPayout(tx.QueryRow(ctx,
			`SELECT `+payoutCols+` FROM commission_payouts WHERE tenant_id=$1 AND id=$2`, tenantID, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return p, errPayoutNotFound
	}
	return p, err
}

// ListPayouts returns a tenant's payouts newest-first (payout queue, Agent
// C's admin page).
func (s *PayoutStore) ListPayouts(ctx context.Context, tenantID uuid.UUID, status string, limit int) ([]Payout, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT ` + payoutCols + ` FROM commission_payouts WHERE tenant_id=$1`
	args := []any{tenantID}
	if status != "" {
		q += ` AND status=$2`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT ` + fmt.Sprint(limit)
	var out []Payout
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPayout(rows)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// BeginProcessing CAS-transitions queued → processing and records the
// deterministic provider reference. Idempotent: a payout already
// processing with the SAME reference is a retried activity attempt and
// succeeds; any other state is an error (the workflow re-reads and
// short-circuits paid payouts before calling this).
func (s *PayoutStore) BeginProcessing(ctx context.Context, tenantID, id uuid.UUID, providerRef string) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE commission_payouts SET status='processing', provider_ref=$3
			 WHERE tenant_id=$1 AND id=$2 AND status IN ('queued','processing') AND (provider_ref='' OR provider_ref=$3)`,
			tenantID, id, providerRef)
		if err != nil {
			return fmt.Errorf("begin processing: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("begin processing: payout not in a processable state (queued/processing with matching ref)")
		}
		return nil
	})
}

// MarkPaid transitions processing → paid and writes any extra outbox rows
// (usage metering) in the SAME transaction — and ONLY on the actual
// transition. A replay with the same provider_ref (retried activity after
// a partial failure) succeeds as a no-op WITHOUT rewriting the extra rows,
// so metering can neither drift from the paid state nor double-count.
// Never overwrites a failed→paid or paid-with-different-ref payout.
func (s *PayoutStore) MarkPaid(ctx context.Context, tenantID, id uuid.UUID, providerRef string, extra ...OutboxRow) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE commission_payouts SET status='paid', paid_at=now(), provider_ref=$3, failure_reason=''
			 WHERE tenant_id=$1 AND id=$2 AND status='processing' AND (provider_ref='' OR provider_ref=$3)`,
			tenantID, id, providerRef)
		if err != nil {
			return fmt.Errorf("mark paid: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// Not a fresh transition: tolerate an exact idempotent replay
			// (already paid with the same provider_ref), reject anything
			// else.
			var one int
			err := tx.QueryRow(ctx,
				`SELECT 1 FROM commission_payouts WHERE tenant_id=$1 AND id=$2 AND status='paid' AND provider_ref=$3`,
				tenantID, id, providerRef).Scan(&one)
			if err != nil {
				return fmt.Errorf("mark paid: payout not in a payable state (processing, or paid with matching ref)")
			}
			return nil // idempotent replay — no status change, no extra rows
		}
		for _, e := range extra {
			if e.Topic == "" || len(e.Payload) == 0 {
				continue
			}
			// NOTE (RLS): the outbox table is not tenant-scoped (drained
			// cross-tenant by the dispatcher) — same insert as
			// store.insertExtraOutbox.
			if _, err := tx.Exec(ctx,
				`INSERT INTO outbox (aggregate_id, topic, payload) VALUES ($1,$2,$3)`,
				id, e.Topic, e.Payload); err != nil {
				return fmt.Errorf("insert extra outbox: %w", err)
			}
		}
		return nil
	})
}

// MarkFailed transitions queued/processing → failed with the reason.
// Terminal: never overrides a paid payout (a paid payout that later fails
// at the provider is a RECON mismatch, not a status flip).
func (s *PayoutStore) MarkFailed(ctx context.Context, tenantID, id uuid.UUID, reason string) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE commission_payouts SET status='failed', failure_reason=$3
			 WHERE tenant_id=$1 AND id=$2 AND status IN ('queued','processing','failed')`,
			tenantID, id, truncate(reason, 500))
		if err != nil {
			return fmt.Errorf("mark failed: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("mark failed: payout not in a failable state (already paid?)")
		}
		return nil
	})
}

// ReconCandidates returns payouts needing reconciliation (contract §5):
// still processing (in-flight or stuck), or paid within the last 72h
// (provider could still report a reversal).
//
// NOTE (RLS): cross-tenant reconciliation path — like the outbox
// dispatcher it intentionally runs outside withTenant; the nightly recon
// workflow is a platform-level cron, not a tenant request.
func (s *PayoutStore) ReconCandidates(ctx context.Context, limit int) ([]Payout, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+payoutCols+` FROM commission_payouts
		 WHERE status='processing'
		    OR (status='paid' AND paid_at > now() - interval '72 hours')
		 ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Payout
	for rows.Next() {
		p, err := scanPayout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// EnqueueOutbox appends one row to the transactional outbox (mirrors
// store.EnqueueOutbox; recon alert + metered notification rows).
//
// NOTE (RLS): the outbox table is not tenant-scoped (no RLS policy — the
// dispatcher drains it cross-tenant by design).
func (s *PayoutStore) EnqueueOutbox(ctx context.Context, aggregateID uuid.UUID, topic string, payload []byte) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO outbox (aggregate_id, topic, payload) VALUES ($1,$2,$3)`,
		aggregateID, topic, payload); err != nil {
		return fmt.Errorf("enqueue outbox: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Execution activities (contract §4)
// ---------------------------------------------------------------------------

// PayoutActivities bundles the payout execution dependencies. Registered
// on booking-service's Temporal worker (main.go additive block).
type PayoutActivities struct {
	Store    *PayoutStore
	Ledger   Ledger // TOUCHPOINT: Agent A's Postgres impl at integration
	Provider PayoutProvider
	// MinKobo is the payout floor in kobo (PAYOUT_MIN_NGN × 100).
	MinKobo int64
	// UsageTopic is opendesk.usage.events (empty disables metering).
	UsageTopic string
	Logger     *zap.Logger
}

func (a *PayoutActivities) log() *zap.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return zap.NewNop()
}

// TransferActivityInput identifies the payout to execute.
type TransferActivityInput struct {
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	PayoutID   string `json:"payout_id"`
}

// FinalizeActivityInput carries the transfer outcome for finalization.
type FinalizeActivityInput struct {
	TenantID    string `json:"tenant_id"`
	TenantSlug  string `json:"tenant_slug"`
	PayoutID    string `json:"payout_id"`
	ProviderRef string `json:"provider_ref"`
}

// FailActivityInput carries the terminal failure reason.
type FailActivityInput struct {
	TenantID string `json:"tenant_id"`
	PayoutID string `json:"payout_id"`
	Reason   string `json:"reason"`
}

func parsePayoutIDs(tenant, payout string) (uuid.UUID, uuid.UUID, error) {
	tenantID, err := uuid.Parse(tenant)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("tenant_id: %w", err)
	}
	payoutID, err := uuid.Parse(payout)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("payout_id: %w", err)
	}
	return tenantID, payoutID, nil
}

// ExecuteTransfer loads the payout, enforces the minimum, CAS-transitions
// to processing and executes the provider transfer. Idempotent: a paid
// payout short-circuits (retried/recovered workflow), and the provider
// reference is deterministic so a crashed attempt retries safely.
func (a *PayoutActivities) ExecuteTransfer(ctx context.Context, in TransferActivityInput) (TransferResult, error) {
	tenantID, payoutID, err := parsePayoutIDs(in.TenantID, in.PayoutID)
	if err != nil {
		return TransferResult{}, temporal.NewNonRetryableApplicationError("invalid transfer input", "ValidationError", err)
	}
	p, err := a.Store.GetPayout(ctx, tenantID, payoutID)
	if err != nil {
		return TransferResult{}, fmt.Errorf("load payout: %w", err)
	}
	if p.Status == PayoutStatusPaid {
		// Idempotent replay: a previous attempt finished the job.
		return TransferResult{ProviderRef: p.ProviderRef, Status: "success"}, nil
	}
	if p.Status == PayoutStatusFailed {
		return TransferResult{}, temporal.NewNonRetryableApplicationError(
			"payout already failed: "+p.FailureReason, "PayoutStateError", nil)
	}
	if min := a.MinKobo; min > 0 && p.AmountNGN < min {
		return TransferResult{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("amount %d kobo below minimum %d kobo", p.AmountNGN, min), "BelowMinimumError", nil)
	}
	ref := p.ProviderRef
	if ref == "" {
		ref = PayoutReference(p.ID)
	}
	if err := a.Store.BeginProcessing(ctx, tenantID, payoutID, ref); err != nil {
		return TransferResult{}, err
	}
	res, err := a.Provider.Transfer(ctx, TransferRequest{
		PayoutID:      p.ID,
		TenantID:      p.TenantID,
		BeneficiaryID: p.BeneficiaryID,
		AmountNGN:     p.AmountNGN,
		Currency:      "NGN",
		Reason:        "OpenDesk commission payout " + p.ID.String(),
		Reference:     ref,
	})
	if err != nil {
		return TransferResult{}, err // retryable: the workflow policy gives 3 attempts w/ backoff
	}
	if res.ProviderRef == "" {
		res.ProviderRef = ref
	}
	a.log().Info("commission payout transfer executed",
		zap.String("payout_id", p.ID.String()), zap.String("provider", a.Provider.Name()),
		zap.String("provider_ref", res.ProviderRef), zap.String("provider_status", res.Status))
	return res, nil
}

// FinalizePaid marks the payout paid, posts the balanced payout posting
// (debit 300 commission_payable / credit 302 agent_float — contract §3/§4)
// and meters commission_payout. The status flip + metering row commit in
// one transaction; the ledger posting goes through the Ledger interface
// (idempotent on ref_type/ref_id/account_code) so an activity retry after
// a partial failure cannot double-post.
func (a *PayoutActivities) FinalizePaid(ctx context.Context, in FinalizeActivityInput) error {
	tenantID, payoutID, err := parsePayoutIDs(in.TenantID, in.PayoutID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError("invalid finalize input", "ValidationError", err)
	}
	p, err := a.Store.GetPayout(ctx, tenantID, payoutID)
	if err != nil {
		return fmt.Errorf("load payout: %w", err)
	}
	ref := in.ProviderRef
	if ref == "" {
		ref = p.ProviderRef
	}
	var extra []OutboxRow
	if a.UsageTopic != "" {
		payload, err := MarshalPayoutUsageRecord(in.TenantSlug, p)
		if err != nil {
			a.log().Warn("commission payout usage marshal failed; skipping metering", zap.Error(err))
		} else {
			extra = append(extra, OutboxRow{Topic: a.UsageTopic, Payload: payload})
		}
	}
	if err := a.Store.MarkPaid(ctx, tenantID, payoutID, ref, extra...); err != nil {
		return err
	}
	// Balanced payout posting: debit 300 commission_payable /
	// credit 302 agent_float (contract §3: "payout: debit 300 / credit
	// 302-or-303" — agent_float is the default beneficiary rail).
	if a.Ledger != nil {
		if err := a.Ledger.PostBalanced(ctx, BalancedPosting{
			TenantID:      tenantID,
			DebitAccount:  AccountCommissionPayable,
			CreditAccount: AccountAgentFloat,
			AmountNGN:     p.AmountNGN,
			RefType:       RefTypePayout,
			RefID:         payoutID.String(),
			// The debit MUST carry the beneficiary or the per-beneficiary
			// 300 balance (credits − debits) never decreases on payout.
			BeneficiaryID: p.BeneficiaryID,
		}); err != nil {
			return fmt.Errorf("ledger payout posting: %w", err)
		}
	}
	a.log().Info("commission payout finalized",
		zap.String("payout_id", payoutID.String()), zap.Int64("amount_kobo", p.AmountNGN))
	return nil
}

// MarkFailed records the terminal failure after the transfer activity
// exhausted its retries (contract §4: "failed after retries w/ reason").
func (a *PayoutActivities) MarkFailed(ctx context.Context, in FailActivityInput) error {
	tenantID, payoutID, err := parsePayoutIDs(in.TenantID, in.PayoutID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError("invalid fail input", "ValidationError", err)
	}
	if err := a.Store.MarkFailed(ctx, tenantID, payoutID, in.Reason); err != nil {
		return err
	}
	a.log().Warn("commission payout failed",
		zap.String("payout_id", payoutID.String()), zap.String("reason", truncate(in.Reason, 200)))
	return nil
}

// MarshalPayoutUsageRecord builds the commission_payout usage CloudEvent
// (opendesk.usage.events), mirroring geo.MarshalGeoUsageRecord.
func MarshalPayoutUsageRecord(tenantSlug string, p Payout) ([]byte, error) {
	evt := events.New("booking-service", "com.opendesk.usage.UsageRecord", tenantSlug, p.TenantID.String(), map[string]any{
		"tenant_id": p.TenantID.String(),
		"metric":    UsageMetricCommissionPayout,
		"value":     1,
		"ts":        time.Now().UTC(),
		"meta": map[string]any{
			"payout_id":      p.ID.String(),
			"beneficiary_id": p.BeneficiaryID,
			"amount_ngn":     p.AmountNGN,
			"provider":       p.Provider,
		},
	})
	return json.Marshal(evt)
}

// ---------------------------------------------------------------------------
// Recon activities (contract §5)
// ---------------------------------------------------------------------------

// Recon mismatch kinds (alert payload "mismatch" field).
const (
	// MismatchPaidNotSuccessful: ledger says paid, provider says
	// failed/reversed (money never moved or came back).
	MismatchPaidNotSuccessful = "ledger_paid_provider_not_successful"
	// MismatchProcessingSucceeded: ledger stuck processing, provider says
	// success (finalization lost — needs operator re-drive).
	MismatchProcessingSucceeded = "ledger_processing_provider_successful"
	// MismatchProcessingFailed: ledger processing, provider says
	// failed/reversed (workflow failure path missed).
	MismatchProcessingFailed = "ledger_processing_provider_failed"
)

// ReconActivities bundles the nightly reconciliation dependencies.
type ReconActivities struct {
	Store    *PayoutStore
	Provider PayoutProvider
	// UsageTopic is opendesk.usage.events (metered notification, §5).
	UsageTopic string
	// NotificationsTopic is opendesk.notifications.outbox (alert row, §5).
	NotificationsTopic string
	Logger             *zap.Logger
}

func (a *ReconActivities) log() *zap.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return zap.NewNop()
}

// ReconFetchInput bounds one nightly scan.
type ReconFetchInput struct {
	Limit int `json:"limit"`
}

// ReconCheckInput identifies one payout to reconcile.
type ReconCheckInput struct {
	TenantID string `json:"tenant_id"`
	PayoutID string `json:"payout_id"`
}

// ReconMismatch describes one detected ledger↔provider divergence.
type ReconMismatch struct {
	TenantID       string `json:"tenant_id"`
	PayoutID       string `json:"payout_id"`
	Mismatch       string `json:"mismatch"`
	LedgerStatus   string `json:"ledger_status"`
	ProviderStatus string `json:"provider_status"`
	ProviderRef    string `json:"provider_ref"`
	AmountNGN      int64  `json:"amount_ngn"`
}

// FetchCandidates lists payouts needing reconciliation (cross-tenant,
// annotated at PayoutStore.ReconCandidates).
func (a *ReconActivities) FetchCandidates(ctx context.Context, in ReconFetchInput) ([]Payout, error) {
	return a.Store.ReconCandidates(ctx, in.Limit)
}

// CheckTransfer compares one payout's ledger status with the provider's
// transfer status. On mismatch it writes the §5 artifacts — outbox alert
// row (kind commission_recon_alert) + metered usage row — and returns the
// mismatch. A clean comparison returns nil.
func (a *ReconActivities) CheckTransfer(ctx context.Context, in ReconCheckInput) (*ReconMismatch, error) {
	tenantID, payoutID, err := parsePayoutIDs(in.TenantID, in.PayoutID)
	if err != nil {
		return nil, temporal.NewNonRetryableApplicationError("invalid recon input", "ValidationError", err)
	}
	p, err := a.Store.GetPayout(ctx, tenantID, payoutID)
	if err != nil {
		return nil, fmt.Errorf("load payout: %w", err)
	}
	if p.ProviderRef == "" {
		// Never reached the provider: still queued (not a recon candidate)
		// or failed before a reference existed — nothing to compare.
		return nil, nil
	}
	st, err := a.Provider.TransferStatus(ctx, p.ProviderRef)
	if err != nil {
		return nil, fmt.Errorf("provider status: %w", err) // retryable
	}
	kind := classifyMismatch(p.Status, st.Status)
	if kind == "" {
		return nil, nil
	}
	m := &ReconMismatch{
		TenantID:       p.TenantID.String(),
		PayoutID:       p.ID.String(),
		Mismatch:       kind,
		LedgerStatus:   p.Status,
		ProviderStatus: st.Status,
		ProviderRef:    p.ProviderRef,
		AmountNGN:      p.AmountNGN,
	}
	a.alert(ctx, p, m)
	return m, nil
}

// classifyMismatch is the pure ledger↔provider comparison (table-driven
// tested). "" means consistent.
func classifyMismatch(ledgerStatus, providerStatus string) string {
	switch ledgerStatus {
	case PayoutStatusPaid:
		switch providerStatus {
		case "success":
			return ""
		default: // failed | reversed | pending(!) | unknown
			return MismatchPaidNotSuccessful
		}
	case PayoutStatusProcessing:
		switch providerStatus {
		case "success":
			return MismatchProcessingSucceeded
		case "failed", "reversed":
			return MismatchProcessingFailed
		default: // pending — still in flight, consistent
			return ""
		}
	default:
		return ""
	}
}

// alert writes the §5 artifacts for one mismatch: the alert outbox row
// (kind commission_recon_alert, notifications topic) and the metered usage
// row. Best-effort: a failed enqueue is logged, never blocks the scan.
func (a *ReconActivities) alert(ctx context.Context, p Payout, m *ReconMismatch) {
	if a.NotificationsTopic != "" {
		payload, err := json.Marshal(events.New("booking-service", EventTypeCommissionReconAlert, p.ID.String(), p.TenantID.String(), map[string]any{
			"kind":            UsageMetricCommissionReconAlert,
			"tenant_id":       p.TenantID.String(),
			"payout_id":       p.ID.String(),
			"mismatch":        m.Mismatch,
			"ledger_status":   m.LedgerStatus,
			"provider_status": m.ProviderStatus,
			"provider_ref":    m.ProviderRef,
			"amount_ngn":      m.AmountNGN,
		}))
		if err != nil {
			a.log().Warn("recon alert marshal failed", zap.Error(err))
		} else if err := a.Store.EnqueueOutbox(ctx, p.ID, a.NotificationsTopic, payload); err != nil {
			a.log().Warn("recon alert enqueue failed", zap.Error(err))
		}
	}
	if a.UsageTopic != "" {
		payload, err := json.Marshal(events.New("booking-service", "com.opendesk.usage.UsageRecord", p.TenantID.String(), p.TenantID.String(), map[string]any{
			"tenant_id": p.TenantID.String(),
			"metric":    UsageMetricCommissionReconAlert,
			"value":     1,
			"ts":        time.Now().UTC(),
			"meta": map[string]any{
				"payout_id": p.ID.String(),
				"mismatch":  m.Mismatch,
			},
		}))
		if err != nil {
			a.log().Warn("recon usage marshal failed", zap.Error(err))
		} else if err := a.Store.EnqueueOutbox(ctx, p.ID, a.UsageTopic, payload); err != nil {
			a.log().Warn("recon usage enqueue failed", zap.Error(err))
		}
	}
	a.log().Warn("commission recon mismatch",
		zap.String("payout_id", p.ID.String()), zap.String("mismatch", m.Mismatch),
		zap.String("ledger_status", m.LedgerStatus), zap.String("provider_status", m.ProviderStatus))
}
