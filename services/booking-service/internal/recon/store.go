// Package recon implements SPEC-W56 Scope A: receipt ↔ payment
// reconciliation. Field-captured receipts (field_captures.receipt_document_id
// /receipt_fields, W53) are matched against payment state
// (bookings.payment_*, booking_payment_attempts) via the RECON §3.3 ladder,
// confidence-tagged findings are persisted to receipt_recon_findings, and
// each NEW finding emits one com.opendesk.booking.ReceiptReconFinding
// CloudEvent through the transactional outbox (same tx as the finding
// insert) onto opendesk.booking.events.
//
// Honesty posture (SPEC pinned decisions): receipts carry NO currency and
// amounts are float MAJOR units read off paper — every finding's detail
// carries currency_unknown:true and amount comparisons convert with
// int64(math.Round(amount*100)) at the comparison site ONLY. Matches are
// confidence-tagged (exact = checkin contact chain only; heuristic = lead
// phone chain or amount+date); ambiguity at the winning rung yields
// confidence='none' with the candidate list in detail. Null receipt amounts
// never guess (R5).
package recon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/opendesk/booking-service/internal/config"
)

// Finding kinds (SPEC-W56 Scope A item 2, RECON §3.1).
const (
	// KindReceiptWithoutPayment: a receipt-bearing capture matched no paid
	// booking (or the winning rung was ambiguous — confidence 'none' with
	// the candidates in detail).
	KindReceiptWithoutPayment = "receipt_without_payment"
	// KindPaymentWithoutReceipt: a paid attempt/booking has no linked
	// receipt-bearing capture. Emitted ONLY for tenants with ≥1
	// receipt-bearing capture in the sweep window (pinned decision 4 —
	// data-gated, no flag).
	KindPaymentWithoutReceipt = "payment_without_receipt"
	// KindAmountMismatch: a matched pair whose receipt amount diverges from
	// the rail-captured amount (paper-vs-rail — DISTINCT from the W49
	// rail-vs-offering-price mismatch).
	KindAmountMismatch = "amount_mismatch"
	// KindCurrencyUnknown: emitted as a standalone finding ONLY when the
	// missing receipt amount/currency blocks verifying an otherwise-exact
	// (contact-chain) pair; otherwise currency_unknown rides in detail.
	KindCurrencyUnknown = "currency_unknown"
)

// Confidence levels (the §3.3 ladder outcome).
const (
	ConfidenceExact     = "exact"     // rung 1: checkin contact chain
	ConfidenceHeuristic = "heuristic" // rungs 2 (lead phone) / 3 (amount+date)
	ConfidenceNone      = "none"      // no candidate, or ambiguous winning rung
)

// Finding is one row of receipt_recon_findings. ReceiptFields is populated
// ONLY by ListFindings (LEFT JOIN field_captures) — it is the first read
// path for receipt data (R4) and is not a findings-table column.
type Finding struct {
	ID                  uuid.UUID       `json:"id"`
	TenantID            uuid.UUID       `json:"tenant_id"`
	RunID               uuid.UUID       `json:"run_id"`
	Kind                string          `json:"kind"`
	CaptureClientID     *string         `json:"capture_client_id,omitempty"`
	BookingID           *uuid.UUID      `json:"booking_id,omitempty"`
	AttemptTxRef        *string         `json:"attempt_tx_ref,omitempty"`
	ExpectedAmountMinor *int64          `json:"expected_amount_minor,omitempty"`
	CapturedAmountMinor *int64          `json:"captured_amount_minor,omitempty"`
	ReceiptAmountMajor  *float64        `json:"receipt_amount_major,omitempty"`
	ReceiptCurrency     string          `json:"receipt_currency"` // always "" today (R2 — receipts carry no currency)
	Confidence          string          `json:"confidence"`
	Detail              json.RawMessage `json:"detail"`
	CreatedAt           time.Time       `json:"created_at"`
	ResolvedAt          *time.Time      `json:"resolved_at,omitempty"`
	// ReceiptFields is the linked capture's receipt_fields JSONB verbatim
	// (nulls allowed) — joined in ListFindings, never stored here.
	ReceiptFields json.RawMessage `json:"receipt_fields,omitempty"`
}

// ReceiptCapture is the recon view of one receipt-bearing field_captures
// row (receipt_document_id IS NOT NULL).
type ReceiptCapture struct {
	ClientID         string
	Kind             string
	Payload          json.RawMessage
	Result           json.RawMessage
	ReceiptFields    json.RawMessage
	CapturedAt       *time.Time // client-claimed (nullable, untrusted — W45 OOS-21)
	ServerReceivedAt time.Time  // trusted receipt timestamp
}

// anchorTime is the match-window anchor: the client-claimed captured_at
// when present (SPEC rung 1 windows on captured_at), falling back to the
// trusted server_received_at for legacy rows with a NULL captured_at.
func (c ReceiptCapture) anchorTime() time.Time {
	if c.CapturedAt != nil {
		return c.CapturedAt.UTC()
	}
	return c.ServerReceivedAt.UTC()
}

// PaymentCandidate is one paid booking/attempt pair the ladder may match a
// receipt against. AmountMinor is NULL on pre-W56 attempt rows (the W56
// additive columns are filled at capture-transition time only) — a NULL
// amount never satisfies the amount rung and never triggers
// amount_mismatch (honest "unknown", never a guess).
type PaymentCandidate struct {
	BookingID          uuid.UUID
	TxRef              string
	AmountMinor        *int64
	Currency           string
	PaidAt             time.Time // attempt.updated_at — the transition stamp
	AttemptStatus      string    // paid | amount_mismatch
	OfferingPriceMinor *int64    // offerings.price_cents context for detail
}

// Store persists receipt_recon_findings and runs the recon reads. Same
// packaging idiom as fieldcapture.Store: NewStore wraps an existing pool
// (tests), DialStore opens a small dedicated pool from the shared satellite
// budget (main wiring path).
type Store struct {
	pool    *pgxpool.Pool
	ownPool bool
}

// NewStore wraps an existing pool and ensures the schema.
func NewStore(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	s := &Store{pool: pool}
	if err := s.ensureSchema(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// DialStore opens a small dedicated pool and ensures the schema.
func DialStore(ctx context.Context, databaseURL string) (*Store, error) {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}
	poolCfg.MaxConns = config.SatellitePoolMaxConns()
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	s, err := NewStore(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	s.ownPool = true
	return s, nil
}

// Close releases the pool when this store opened it.
func (s *Store) Close() {
	if s.ownPool {
		s.pool.Close()
	}
}

// ensureSchema bootstraps receipt_recon_findings idempotently (same
// pattern as fieldcapture.ensureSchema): RLS enabled + forced with the
// tenant_isolation policy, guarded by a pg_policies check.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
//
// NULL-dedupe note: the SPEC-mandated UNIQUE(tenant_id, kind,
// capture_client_id, booking_id) constraint treats NULLs as distinct
// (Postgres semantics), so payment_without_receipt rows (capture_client_id
// NULL) would never conflict and every sweep would re-emit them. The
// COALESCE expression unique index below is the real ON CONFLICT DO
// NOTHING arbiter; the plain UNIQUE constraint stays as the documented
// key per SPEC item 2.
func (s *Store) ensureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS receipt_recon_findings (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL,
    run_id                UUID NOT NULL,
    kind                  TEXT NOT NULL
                          CHECK (kind IN ('receipt_without_payment','payment_without_receipt','amount_mismatch','currency_unknown')),
    capture_client_id     TEXT,
    booking_id            UUID,
    attempt_tx_ref        TEXT,
    expected_amount_minor BIGINT,
    captured_amount_minor BIGINT,
    receipt_amount_major  DOUBLE PRECISION,
    receipt_currency      TEXT,
    confidence            TEXT NOT NULL CHECK (confidence IN ('exact','heuristic','none')),
    detail                JSONB NOT NULL DEFAULT '{}',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at           TIMESTAMPTZ,
    UNIQUE (tenant_id, kind, capture_client_id, booking_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_receipt_recon_findings_dedupe
    ON receipt_recon_findings (tenant_id, kind, COALESCE(capture_client_id, ''), COALESCE(booking_id::text, ''));
CREATE INDEX IF NOT EXISTS idx_receipt_recon_findings_tenant_created
    ON receipt_recon_findings (tenant_id, created_at DESC);
ALTER TABLE receipt_recon_findings ENABLE ROW LEVEL SECURITY;
ALTER TABLE receipt_recon_findings FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'receipt_recon_findings' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON receipt_recon_findings
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure receipt_recon_findings table: %w", err)
	}
	return nil
}

// withTenant mirrors fieldcapture.Store.withTenant: every tenant-scoped
// statement runs in a transaction with SET LOCAL app.tenant_id so the RLS
// policies scope it to the tenant (parameter-binding-safe set_config).
func (s *Store) withTenant(ctx context.Context, tenantID uuid.UUID, fn func(tx pgx.Tx) error) error {
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

// TenantsWithReceiptCaptures enumerates the tenants with ≥1 receipt-bearing
// capture whose claimed or received timestamp falls inside the sweep window.
// This is the pinned-decision-4 gate: payment_without_receipt findings are
// generated ONLY for these tenants (receipts are a field-collection
// artifact; a tenant with zero receipt captures gets zero noise).
//
// NOTE (RLS): sweep enumeration is a superuser batch path — same posture
// as store.ExpireStaleWaitlist; per-tenant work then runs under withTenant.
func (s *Store) TenantsWithReceiptCaptures(ctx context.Context, since time.Time) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT tenant_id FROM field_captures
		  WHERE receipt_document_id IS NOT NULL
		    AND (server_received_at >= $1 OR captured_at >= $1)
		  ORDER BY tenant_id`, since)
	if err != nil {
		return nil, fmt.Errorf("enumerate receipt tenants: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// receiptCaptures returns the tenant's receipt-bearing captures inside the
// sweep window (either the trusted server_received_at or the client-claimed
// captured_at inside — max recall, both timestamps are loaded so the
// matcher can anchor honestly). Caller holds a withTenant tx.
func receiptCaptures(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, since time.Time) ([]ReceiptCapture, error) {
	rows, err := tx.Query(ctx,
		`SELECT client_id, kind, payload, COALESCE(result, 'null'), COALESCE(receipt_fields, 'null'),
		        captured_at, server_received_at
		 FROM field_captures
		 WHERE tenant_id=$1 AND receipt_document_id IS NOT NULL
		   AND (server_received_at >= $2 OR captured_at >= $2)
		 ORDER BY server_received_at, client_id`, tenantID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReceiptCapture
	for rows.Next() {
		var c ReceiptCapture
		if err := rows.Scan(&c.ClientID, &c.Kind, &c.Payload, &c.Result, &c.ReceiptFields,
			&c.CapturedAt, &c.ServerReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// candidateScanCols is the shared candidate SELECT list: the paid attempt
// joined to its booking, with the offering price as detail context.
const candidateScanCols = `a.booking_id, a.tx_ref, a.amount_minor, COALESCE(a.currency, ''), a.updated_at, a.status, o.price_cents`

func scanCandidates(rows pgx.Rows) ([]PaymentCandidate, error) {
	defer rows.Close()
	var out []PaymentCandidate
	for rows.Next() {
		var c PaymentCandidate
		if err := rows.Scan(&c.BookingID, &c.TxRef, &c.AmountMinor, &c.Currency,
			&c.PaidAt, &c.AttemptStatus, &c.OfferingPriceMinor); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// candidatesByContact (rung 1 / rung 2 tail): paid bookings of one contact
// whose appointment starts within ±matchWindowDays of the anchor, joined to
// their paid/amount_mismatch attempt (latest first).
func candidatesByContact(ctx context.Context, tx pgx.Tx, tenantID, contactID uuid.UUID, anchor time.Time, matchWindowDays int) ([]PaymentCandidate, error) {
	from, to := windowBounds(anchor, matchWindowDays)
	rows, err := tx.Query(ctx,
		`SELECT `+candidateScanCols+`
		 FROM bookings b
		 JOIN booking_payment_attempts a
		   ON a.tenant_id=b.tenant_id AND a.booking_id=b.id AND a.status IN ('paid','amount_mismatch')
		 LEFT JOIN offerings o ON o.tenant_id=b.tenant_id AND o.id=b.offering_id
		 WHERE b.tenant_id=$1 AND b.contact_id=$2
		   AND b.payment_status IN ('paid','paid_amount_mismatch')
		   AND b.starts_at BETWEEN $3 AND $4
		 ORDER BY a.updated_at DESC`, tenantID, contactID, from, to)
	if err != nil {
		return nil, err
	}
	return scanCandidates(rows)
}

// leadPhone resolves rung 2's first hop: result->>'lead_id' →
// leads.phone_e164. Empty when the lead is gone (never an error — the rung
// simply yields no candidates).
func leadPhone(ctx context.Context, tx pgx.Tx, tenantID, leadID uuid.UUID) (string, error) {
	var phone string
	err := tx.QueryRow(ctx,
		`SELECT phone_e164 FROM leads WHERE tenant_id=$1 AND id=$2`, tenantID, leadID).Scan(&phone)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return phone, err
}

// contactIDsByPhone resolves rung 2's second hop: leads.phone_e164 →
// contacts.phone (exact string match — both sides store E.164).
func contactIDsByPhone(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, phone string) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx,
		`SELECT id FROM contacts WHERE tenant_id=$1 AND phone=$2`, tenantID, phone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// candidatesByAmountDate (rung 3): paid attempts whose captured amount
// equals the receipt amount (converted major→minor at the comparison site)
// AND whose payment transition stamp is within ±amountWindowDays of the
// receipt date. amount_minor NULL (pre-W56 rows) never matches — the SQL
// comparison is NULL-honest.
func candidatesByAmountDate(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, amountMinor int64, receiptDate time.Time, amountWindowDays int) ([]PaymentCandidate, error) {
	from, to := windowBounds(receiptDate, amountWindowDays)
	rows, err := tx.Query(ctx,
		`SELECT `+candidateScanCols+`
		 FROM booking_payment_attempts a
		 JOIN bookings b ON b.tenant_id=a.tenant_id AND b.id=a.booking_id
		 LEFT JOIN offerings o ON o.tenant_id=b.tenant_id AND o.id=b.offering_id
		 WHERE a.tenant_id=$1 AND a.status IN ('paid','amount_mismatch')
		   AND a.amount_minor = $2
		   AND a.updated_at BETWEEN $3 AND $4
		 ORDER BY a.updated_at DESC`, tenantID, amountMinor, from, to)
	if err != nil {
		return nil, err
	}
	return scanCandidates(rows)
}

// paidAttemptsInWindow loads the tenant's paid/amount_mismatch attempts
// whose transition stamp is inside the sweep window — the
// payment_without_receipt candidate pool.
func paidAttemptsInWindow(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, since time.Time) ([]PaymentCandidate, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+candidateScanCols+`
		 FROM booking_payment_attempts a
		 JOIN bookings b ON b.tenant_id=a.tenant_id AND b.id=a.booking_id
		 LEFT JOIN offerings o ON o.tenant_id=b.tenant_id AND o.id=b.offering_id
		 WHERE a.tenant_id=$1 AND a.status IN ('paid','amount_mismatch')
		   AND a.updated_at >= $2
		 ORDER BY a.updated_at DESC`, tenantID, since)
	if err != nil {
		return nil, err
	}
	return scanCandidates(rows)
}

// InsertFindingWithOutbox persists one finding and — ONLY when the row was
// newly inserted — its outbox event, in ONE transaction (pinned decision
// 7). ON CONFLICT DO NOTHING dedupes on the (tenant, kind, capture,
// booking) key (the COALESCE expression index makes NULL halves dedupe
// too); a conflict returns inserted=false and NO outbox row is written.
// The finding ID is caller-allocated (uuid) so the outbox payload can carry
// finding_id in the SAME transaction.
func (s *Store) InsertFindingWithOutbox(ctx context.Context, f *Finding, topic string, payload []byte) (inserted bool, err error) {
	if f.ID == uuid.Nil {
		f.ID = uuid.New()
	}
	if len(f.Detail) == 0 {
		f.Detail = json.RawMessage(`{}`)
	}
	err = s.withTenant(ctx, f.TenantID, func(tx pgx.Tx) error {
		scanErr := tx.QueryRow(ctx,
			`INSERT INTO receipt_recon_findings
			     (id, tenant_id, run_id, kind, capture_client_id, booking_id, attempt_tx_ref,
			      expected_amount_minor, captured_amount_minor, receipt_amount_major,
			      receipt_currency, confidence, detail)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			 ON CONFLICT DO NOTHING
			 RETURNING created_at`,
			f.ID, f.TenantID, f.RunID, f.Kind, f.CaptureClientID, f.BookingID, f.AttemptTxRef,
			f.ExpectedAmountMinor, f.CapturedAmountMinor, f.ReceiptAmountMajor,
			f.ReceiptCurrency, f.Confidence, f.Detail).Scan(&f.CreatedAt)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return nil // deduped — inserted stays false, no outbox row
		}
		if scanErr != nil {
			return fmt.Errorf("insert recon finding: %w", scanErr)
		}
		inserted = true
		if topic != "" && len(payload) > 0 {
			if _, err := tx.Exec(ctx,
				`INSERT INTO outbox (aggregate_id, topic, payload) VALUES ($1,$2,$3)`,
				f.ID, topic, payload); err != nil {
				return fmt.Errorf("insert recon finding outbox: %w", err)
			}
		}
		return nil
	})
	return inserted, err
}

// findingReadCols is the ListFindings column list (table-qualified).
const findingReadCols = `f.id, f.tenant_id, f.run_id, f.kind, f.capture_client_id, f.booking_id,
	f.attempt_tx_ref, f.expected_amount_minor, f.captured_amount_minor, f.receipt_amount_major,
	f.receipt_currency, f.confidence, f.detail, f.created_at, f.resolved_at, c.receipt_fields`

// ListFindings backs GET /v1/recon/receipts: findings newest first, with
// the linked capture's receipt_fields joined in (the first receipt read
// path — R4). status: "open" (resolved_at IS NULL, the default),
// "resolved", "all". limit is clamped by the handler.
func (s *Store) ListFindings(ctx context.Context, tenantID uuid.UUID, status string, limit int) ([]Finding, error) {
	q := `SELECT ` + findingReadCols + `
		 FROM receipt_recon_findings f
		 LEFT JOIN field_captures c
		   ON c.tenant_id=f.tenant_id AND c.client_id=f.capture_client_id
		 WHERE f.tenant_id=$1`
	switch status {
	case "resolved":
		q += ` AND f.resolved_at IS NOT NULL`
	case "all":
	default: // "open"
		q += ` AND f.resolved_at IS NULL`
	}
	q += ` ORDER BY f.created_at DESC, f.id LIMIT $2`
	out := []Finding{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f Finding
			if err := rows.Scan(&f.ID, &f.TenantID, &f.RunID, &f.Kind, &f.CaptureClientID,
				&f.BookingID, &f.AttemptTxRef, &f.ExpectedAmountMinor, &f.CapturedAmountMinor,
				&f.ReceiptAmountMajor, &f.ReceiptCurrency, &f.Confidence, &f.Detail,
				&f.CreatedAt, &f.ResolvedAt, &f.ReceiptFields); err != nil {
				return err
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	return out, err
}
