package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Referrals, commission rules, commission ledger + payouts (SPEC-W14 Agent A,
// CAC app wave 3): referrals / commission_rules / commission_ledger /
// payouts, all tenant-scoped with RLS (same bootstrap + pg_policies pattern
// as incidents.go / leads.go).
// ---------------------------------------------------------------------------

// Referral mirrors booking.referrals (contract SPEC-W14 §1). Dedupe rule:
// ONE OPEN referral per (tenant_id, referee_phone) — open = pending|verified
// (partial unique index; converted/paid/rejected are closed).
type Referral struct {
	ID           uuid.UUID  `json:"referral_id"`
	TenantID     uuid.UUID  `json:"tenant_id"`
	ReferrerType string     `json:"referrer_type"` // contact | agent | staff
	ReferrerID   string     `json:"referrer_id"`
	RefereePhone string     `json:"referee_phone"`
	CampaignID   *uuid.UUID `json:"campaign_id"`
	Status       string     `json:"status"` // pending|verified|converted|paid|rejected
	BountyRuleID *uuid.UUID `json:"bounty_rule_id"`
	CreatedAt    time.Time  `json:"created_at"`
	VerifiedAt   *time.Time `json:"verified_at"`
	PaidAt       *time.Time `json:"paid_at"`
}

// CommissionRule mirrors booking.commission_rules (contract SPEC-W14 §2).
// AmountNGN is kobo (integer, no float) and is used when AmountType=flat;
// Bps (basis points of the verify-call base amount) is used when
// AmountType=percent. CapNGN nil = uncapped. Evaluation order = Priority asc.
type CommissionRule struct {
	ID          uuid.UUID `json:"rule_id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	Name        string    `json:"name"`
	Trigger     string    `json:"trigger"`     // signup_verified|first_booking|first_txn|sale
	Beneficiary string    `json:"beneficiary"` // referrer|agent|staff
	AmountType  string    `json:"amount_type"` // flat|percent
	AmountNGN   int64     `json:"amount_ngn"`  // kobo (flat)
	Bps         int       `json:"bps"`         // basis points (percent)
	CapNGN      *int64    `json:"cap_ngn"`     // kobo, null = uncapped
	Active      bool      `json:"active"`
	Priority    int       `json:"priority"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// LedgerEntry mirrors booking.commission_ledger (contract SPEC-W14 §3):
// double-entry rows; every journal groups a balanced pair (sum debits ==
// sum credits). Account codes: 300 commission_payable (liability),
// 301 commission_expense, 302 agent_float, 303 house_clearing.
//
// beneficiary_id is a documented extension beyond the contract column set:
// the balance endpoint (GET /v1/commissions/balance/{beneficiary}) must sum
// 300-account credits−debits PER BENEFICIARY, and the contract row has no
// other field that identifies the beneficiary of an entry. "" = house side.
type LedgerEntry struct {
	ID            uuid.UUID `json:"entry_id"`
	TenantID      uuid.UUID `json:"tenant_id"`
	JournalID     uuid.UUID `json:"journal_id"`
	AccountCode   int       `json:"account_code"`
	BeneficiaryID string    `json:"beneficiary_id"`
	DebitNGN      int64     `json:"debit_ngn"`  // kobo
	CreditNGN     int64    `json:"credit_ngn"` // kobo
	RefType       string    `json:"ref_type"`
	RefID         string    `json:"ref_id"`
	CreatedAt     time.Time `json:"created_at"`
}

// Payout mirrors booking.commission_payouts (contract SPEC-W14 §4). The
// payout STORE (queries + Temporal activities) is owned by Wave-14 Agent B
// (internal/referrals/payouts.go); the row shape + table bootstrap live here
// so A and B share ONE schema — field-for-field identical to Agent B's
// PayoutStore row (paid_at, NOT NULL DEFAULT '' provider_ref/failure_reason)
// so B's ensureSchema and this bootstrap are interchangeable. AmountNGN is
// kobo (contract §2 unit).
type Payout struct {
	ID            uuid.UUID  `json:"payout_id"`
	TenantID      uuid.UUID  `json:"tenant_id"`
	BeneficiaryID string     `json:"beneficiary_id"`
	AmountNGN     int64      `json:"amount_ngn"` // kobo
	Status        string     `json:"status"`     // queued|processing|paid|failed
	Provider      string     `json:"provider"`   // paystack|flutterwave
	ProviderRef   string     `json:"provider_ref,omitempty"`
	FailureReason string     `json:"failure_reason,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	PaidAt        *time.Time `json:"paid_at,omitempty"`
}

// ensureReferralTables bootstraps the SPEC-W14 tables idempotently (same
// pattern as ensureIncidentTables / ensureLeadTables). RLS: enabled + forced
// with the tenant_isolation policy, guarded by a pg_policies existence check.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureReferralTables(ctx context.Context) error {
	const ddl = referralDDL1 + referralDDL2
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure referral tables: %w", err)
	}
	return nil
}

const referralDDL1 = `
CREATE TABLE IF NOT EXISTS referrals (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL,
    referrer_type  TEXT NOT NULL CHECK (referrer_type IN ('contact','agent','staff')),
    referrer_id    TEXT NOT NULL,
    referee_phone  TEXT NOT NULL,
    campaign_id    UUID,
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending','verified','converted','paid','rejected')),
    bounty_rule_id UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    verified_at    TIMESTAMPTZ,
    paid_at        TIMESTAMPTZ
);
-- Contract §1 dedupe: one OPEN referral per (tenant, referee_phone).
CREATE UNIQUE INDEX IF NOT EXISTS idx_referrals_open_phone
    ON referrals (tenant_id, referee_phone)
    WHERE status IN ('pending','verified');
CREATE INDEX IF NOT EXISTS idx_referrals_tenant_status ON referrals (tenant_id, status, created_at);
CREATE INDEX IF NOT EXISTS idx_referrals_tenant_referrer ON referrals (tenant_id, referrer_type, referrer_id);
ALTER TABLE referrals ENABLE ROW LEVEL SECURITY;
ALTER TABLE referrals FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'referrals' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON referrals
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS commission_rules (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    name        TEXT NOT NULL,
    trigger     TEXT NOT NULL
                CHECK (trigger IN ('signup_verified','first_booking','first_txn','sale')),
    beneficiary TEXT NOT NULL CHECK (beneficiary IN ('referrer','agent','staff')),
    amount_type TEXT NOT NULL CHECK (amount_type IN ('flat','percent')),
    amount_ngn  BIGINT NOT NULL DEFAULT 0,
    bps         INTEGER NOT NULL DEFAULT 0,
    cap_ngn     BIGINT,
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    priority    INTEGER NOT NULL DEFAULT 100,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_commission_rules_tenant ON commission_rules (tenant_id, active, priority);
ALTER TABLE commission_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE commission_rules FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'commission_rules' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON commission_rules
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;
`

const referralDDL2 = `
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
    -- Contract §3: idempotent on (ref_type, ref_id, account_code).
    UNIQUE (tenant_id, ref_type, ref_id, account_code),
    -- Double-entry hygiene: exactly one side carries the amount.
    CHECK ((debit_ngn > 0 AND credit_ngn = 0) OR (credit_ngn > 0 AND debit_ngn = 0))
);
CREATE INDEX IF NOT EXISTS idx_commission_ledger_tenant_time ON commission_ledger (tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_commission_ledger_balance
    ON commission_ledger (tenant_id, account_code, beneficiary_id);
ALTER TABLE commission_ledger ENABLE ROW LEVEL SECURITY;
ALTER TABLE commission_ledger FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'commission_ledger' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON commission_ledger
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;

-- Contract §4 payout rows. The payout store/activities are Agent B's
-- (internal/referrals/payouts.go); the schema is bootstrapped here too so
-- the table exists even when the Temporal worker is down. IDENTICAL to
-- Agent B's PayoutStore.ensureSchema (interchangeable, both idempotent).
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
END $$;
`

// ---------------------------------------------------------------------------
// Referrals
// ---------------------------------------------------------------------------

const referralCols = `id, tenant_id, referrer_type, referrer_id, referee_phone, campaign_id, status, bounty_rule_id, created_at, verified_at, paid_at`

func scanReferral(row pgx.Row) (Referral, error) {
	var r Referral
	err := row.Scan(&r.ID, &r.TenantID, &r.ReferrerType, &r.ReferrerID, &r.RefereePhone,
		&r.CampaignID, &r.Status, &r.BountyRuleID, &r.CreatedAt, &r.VerifiedAt, &r.PaidAt)
	return r, err
}

// InsertReferral persists one referral. Idempotent per contract §1: one OPEN
// referral per (tenant_id, referee_phone) — a duplicate open referral is a
// no-op and the EXISTING row is loaded into in (created=false), mirroring
// the leads first-touch dedupe.
func (s *Store) InsertReferral(ctx context.Context, in *Referral) (created bool, err error) {
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	const q = `INSERT INTO referrals (id, tenant_id, referrer_type, referrer_id, referee_phone, campaign_id, bounty_rule_id, created_at)
		           VALUES ($1,$2,$3,$4,$5,$6,$7,now())
		           ON CONFLICT (tenant_id, referee_phone) WHERE status IN ('pending','verified') DO NOTHING
		           RETURNING created_at`
	err = s.withTenant(ctx, in.TenantID, func(tx pgx.Tx) error {
		scanErr := tx.QueryRow(ctx, q, in.ID, in.TenantID, in.ReferrerType, in.ReferrerID,
			in.RefereePhone, in.CampaignID, in.BountyRuleID).Scan(&in.CreatedAt)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			// Open-referral dedupe hit: return the existing row unchanged.
			existing, getErr := scanReferral(tx.QueryRow(ctx,
				`SELECT `+referralCols+` FROM referrals
				 WHERE tenant_id=$1 AND referee_phone=$2 AND status IN ('pending','verified')
				 ORDER BY created_at DESC LIMIT 1`,
				in.TenantID, in.RefereePhone))
			if getErr != nil {
				return fmt.Errorf("load existing referral: %w", getErr)
			}
			*in = existing
			created = false
			return nil
		}
		if scanErr != nil {
			return fmt.Errorf("insert referral: %w", scanErr)
		}
		in.Status = "pending"
		created = true
		return nil
	})
	return created, err
}

// GetReferral fetches one referral scoped to a tenant.
func (s *Store) GetReferral(ctx context.Context, tenantID, id uuid.UUID) (Referral, error) {
	const q = `SELECT ` + referralCols + ` FROM referrals WHERE tenant_id=$1 AND id=$2`
	var r Referral
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		r, err = scanReferral(tx.QueryRow(ctx, q, tenantID, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// ListReferrals returns referrals of a tenant, newest first, with an
// optional status filter ("" = all).
func (s *Store) ListReferrals(ctx context.Context, tenantID uuid.UUID, status string) ([]Referral, error) {
	q := `SELECT ` + referralCols + ` FROM referrals WHERE tenant_id=$1`
	args := []any{tenantID}
	if status != "" {
		q += ` AND status=$2`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT 500`
	var out []Referral
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanReferral(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// VerifyReferralTx atomically performs the SPEC-W14 verify (contract §1+§3):
//  1. lock the referral row (FOR UPDATE) — a replay on an already
//     verified/converted/paid referral short-circuits here and returns the
//     current row with already=true (no new postings; the ledger is also
//     idempotent on (ref_type, ref_id, account_code) as defence-in-depth);
//  2. insert the balanced accrual entries (ON CONFLICT DO NOTHING);
//  3. flip status pending → toStatus (verified|converted), set verified_at
//     and the winning bounty_rule_id.
func (s *Store) VerifyReferralTx(ctx context.Context, tenantID, id uuid.UUID, toStatus string, bountyRuleID *uuid.UUID, entries []LedgerEntry) (ref Referral, already bool, err error) {
	err = s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		ref, err = scanReferral(tx.QueryRow(ctx,
			`SELECT `+referralCols+` FROM referrals WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
			tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if ref.Status != "pending" {
			already = true
			return nil
		}
		if err := insertLedgerEntries(ctx, tx, entries); err != nil {
			return err
		}
		ref, err = scanReferral(tx.QueryRow(ctx,
			`UPDATE referrals SET status=$3, verified_at=now(), bounty_rule_id=$4
			 WHERE tenant_id=$1 AND id=$2 RETURNING `+referralCols,
			tenantID, id, toStatus, bountyRuleID))
		return err
	})
	return ref, already, err
}

// RejectReferral moves a referral pending|verified → rejected. Converted /
// paid / already-rejected rows are terminal: ErrConflict is returned (the
// service maps it to 409).
func (s *Store) RejectReferral(ctx context.Context, tenantID, id uuid.UUID) (Referral, error) {
	const q = `UPDATE referrals SET status='rejected'
		           WHERE tenant_id=$1 AND id=$2 AND status IN ('pending','verified')
		           RETURNING ` + referralCols
	var r Referral
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		r, err = scanReferral(tx.QueryRow(ctx, q, tenantID, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish missing row from illegal state for a honest 404/409.
		if _, getErr := s.GetReferral(ctx, tenantID, id); errors.Is(getErr, ErrNotFound) {
			return r, ErrNotFound
		}
		return r, ErrConflict
	}
	return r, err
}

// ---------------------------------------------------------------------------
// Commission rules
// ---------------------------------------------------------------------------

const ruleCols = `id, tenant_id, name, trigger, beneficiary, amount_type, amount_ngn, bps, cap_ngn, active, priority, created_at, updated_at`

func scanRule(row pgx.Row) (CommissionRule, error) {
	var r CommissionRule
	err := row.Scan(&r.ID, &r.TenantID, &r.Name, &r.Trigger, &r.Beneficiary, &r.AmountType,
		&r.AmountNGN, &r.Bps, &r.CapNGN, &r.Active, &r.Priority, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// InsertRule persists one commission rule.
func (s *Store) InsertRule(ctx context.Context, in *CommissionRule) error {
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	const q = `INSERT INTO commission_rules (` + ruleCols + `)
		           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,now(),now())
		           RETURNING created_at, updated_at`
	return s.withTenant(ctx, in.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, in.ID, in.TenantID, in.Name, in.Trigger, in.Beneficiary,
			in.AmountType, in.AmountNGN, in.Bps, in.CapNGN, in.Active, in.Priority).
			Scan(&in.CreatedAt, &in.UpdatedAt)
	})
}

// GetRule fetches one commission rule scoped to a tenant.
func (s *Store) GetRule(ctx context.Context, tenantID, id uuid.UUID) (CommissionRule, error) {
	const q = `SELECT ` + ruleCols + ` FROM commission_rules WHERE tenant_id=$1 AND id=$2`
	var r CommissionRule
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		r, err = scanRule(tx.QueryRow(ctx, q, tenantID, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// ListRules returns all commission rules of a tenant ordered for evaluation:
// priority asc, then created_at asc, then id (stable, deterministic).
func (s *Store) ListRules(ctx context.Context, tenantID uuid.UUID) ([]CommissionRule, error) {
	const q = `SELECT ` + ruleCols + ` FROM commission_rules WHERE tenant_id=$1
		           ORDER BY priority ASC, created_at ASC, id ASC LIMIT 500`
	var out []CommissionRule
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRule(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateRule replaces the mutable fields of one commission rule
// (tenant-editable per contract §2, incl. the active toggle).
func (s *Store) UpdateRule(ctx context.Context, in *CommissionRule) error {
	const q = `UPDATE commission_rules SET name=$3, trigger=$4, beneficiary=$5, amount_type=$6,
		           amount_ngn=$7, bps=$8, cap_ngn=$9, active=$10, priority=$11, updated_at=now()
		           WHERE tenant_id=$1 AND id=$2
		           RETURNING created_at, updated_at`
	return s.withTenant(ctx, in.TenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, q, in.TenantID, in.ID, in.Name, in.Trigger, in.Beneficiary,
			in.AmountType, in.AmountNGN, in.Bps, in.CapNGN, in.Active, in.Priority).
			Scan(&in.CreatedAt, &in.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
}

// DeleteRule removes one commission rule.
func (s *Store) DeleteRule(ctx context.Context, tenantID, id uuid.UUID) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM commission_rules WHERE tenant_id=$1 AND id=$2`, tenantID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Commission ledger (contract §3)
// ---------------------------------------------------------------------------

const ledgerCols = `id, tenant_id, journal_id, account_code, beneficiary_id, debit_ngn, credit_ngn, ref_type, ref_id, created_at`

func scanLedgerEntry(row pgx.Row) (LedgerEntry, error) {
	var e LedgerEntry
	err := row.Scan(&e.ID, &e.TenantID, &e.JournalID, &e.AccountCode, &e.BeneficiaryID,
		&e.DebitNGN, &e.CreditNGN, &e.RefType, &e.RefID, &e.CreatedAt)
	return e, err
}

// insertLedgerEntries writes the entries of one journal inside an existing
// tenant transaction. Idempotent per contract §3: UNIQUE
// (tenant_id, ref_type, ref_id, account_code) + ON CONFLICT DO NOTHING —
// replaying the same posting is a no-op.
func insertLedgerEntries(ctx context.Context, tx pgx.Tx, entries []LedgerEntry) error {
	const q = `INSERT INTO commission_ledger (` + ledgerCols + `)
		           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now())
		           ON CONFLICT (tenant_id, ref_type, ref_id, account_code) DO NOTHING`
	for i := range entries {
		e := &entries[i]
		if e.ID == uuid.Nil {
			e.ID = uuid.New()
		}
		if _, err := tx.Exec(ctx, q, e.ID, e.TenantID, e.JournalID, e.AccountCode,
			e.BeneficiaryID, e.DebitNGN, e.CreditNGN, e.RefType, e.RefID); err != nil {
			return fmt.Errorf("insert ledger entry (account %d, ref %s/%s): %w",
				e.AccountCode, e.RefType, e.RefID, err)
		}
	}
	return nil
}

// PostLedgerJournal writes the entries of one journal (sharing the journal
// id) in its own tenant transaction. Balance validation is the caller's (the
// Ledger interface impl enforces it before reaching the store); idempotency
// is the (ref_type, ref_id, account_code) unique key.
func (s *Store) PostLedgerJournal(ctx context.Context, tenantID uuid.UUID, entries []LedgerEntry) error {
	if len(entries) == 0 {
		return nil
	}
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return insertLedgerEntries(ctx, tx, entries)
	})
}

// ListLedgerEntries returns ledger rows of a tenant in [from,to] (nil =
// unbounded), oldest first — backs GET /v1/commissions/ledger?from&to.
func (s *Store) ListLedgerEntries(ctx context.Context, tenantID uuid.UUID, from, to *time.Time) ([]LedgerEntry, error) {
	q := `SELECT ` + ledgerCols + ` FROM commission_ledger WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if from != nil {
		n++
		q += fmt.Sprintf(` AND created_at >= $%d`, n)
		args = append(args, *from)
	}
	if to != nil {
		n++
		q += fmt.Sprintf(` AND created_at <= $%d`, n)
		args = append(args, *to)
	}
	q += ` ORDER BY created_at ASC, id ASC LIMIT 2000`
	out := []LedgerEntry{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanLedgerEntry(rows)
			if err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// LedgerBalance sums (credits − debits) of one account for one beneficiary
// ("" = the house side). GET /v1/commissions/balance/{beneficiary} reads
// account 300 (commission_payable).
func (s *Store) LedgerBalance(ctx context.Context, tenantID uuid.UUID, accountCode int, beneficiaryID string) (int64, error) {
	const q = `SELECT COALESCE(SUM(credit_ngn),0) - COALESCE(SUM(debit_ngn),0)
		           FROM commission_ledger
		           WHERE tenant_id=$1 AND account_code=$2 AND beneficiary_id=$3`
	var bal int64
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, tenantID, accountCode, beneficiaryID).Scan(&bal)
	})
	return bal, err
}

// ---------------------------------------------------------------------------
// Referral → lead conversion seam (SPEC-W14 §6)
// ---------------------------------------------------------------------------

// FindOpenLeadByPhone returns the newest lead of a tenant matching phone
// that is NOT in a terminal status (converted|lost). Wave-13's leads
// service exposes Transition(tenantID, leadID, ...) but no by-phone lookup;
// this additive store query is the documented seam the referral verify flow
// uses to resolve the referee's lead before walking it to `converted` via
// the leads SERVICE (never touching leads rows directly).
func (s *Store) FindOpenLeadByPhone(ctx context.Context, tenantID uuid.UUID, phone string) (Lead, error) {
	const q = `SELECT ` + leadCols + ` FROM leads
		           WHERE tenant_id=$1 AND phone_e164=$2 AND status NOT IN ('converted','lost')
		           ORDER BY created_at DESC LIMIT 1`
	var l Lead
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		l, err = scanLead(tx.QueryRow(ctx, q, tenantID, phone))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}
