package lending

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists loan products, applications, accounts, repayments and the
// kobo ledger. Same packaging idiom as the W16 devices.Store / W19
// workorders.Store: NewStore wraps an existing pool (tests), DialStore
// opens a small dedicated pool (integrator wiring path — the shared
// store.Store does not expose its pool). maxConns 4: lending decisions and
// repayments are operator-paced, low-QPS paths.
type Store struct {
	pool    *pgxpool.Pool
	ownPool bool // true when opened via DialStore
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
	poolCfg.MaxConns = 4
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

// ensureSchema bootstraps the lending tables idempotently (SPEC-W20
// contract §1: RLS enabled + forced with the tenant_isolation policy,
// guarded by a pg_policies existence check — mirrors
// internal/devices/store.go).
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS loan_products (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL,
    name               TEXT NOT NULL,
    active             BOOLEAN NOT NULL DEFAULT true,
    principal_min_kobo BIGINT NOT NULL CHECK (principal_min_kobo > 0),
    principal_max_kobo BIGINT NOT NULL CHECK (principal_max_kobo >= principal_min_kobo),
    term_days          INTEGER NOT NULL CHECK (term_days > 0),
    interest_bps       INTEGER NOT NULL CHECK (interest_bps BETWEEN 0 AND 10000),
    fee_flat_kobo      BIGINT NOT NULL DEFAULT 0 CHECK (fee_flat_kobo >= 0),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_loan_products_tenant ON loan_products (tenant_id, active, updated_at DESC);
ALTER TABLE loan_products ENABLE ROW LEVEL SECURITY;
ALTER TABLE loan_products FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'loan_products' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON loan_products
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS loan_applications (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL,
    contact_id     UUID NOT NULL,
    product_id     UUID NOT NULL,
    principal_kobo BIGINT NOT NULL CHECK (principal_kobo > 0),
    status         TEXT NOT NULL DEFAULT 'draft'
                   CHECK (status IN ('draft','submitted','under_review','approved','declined','disbursed','repaid','defaulted')),
    score          INTEGER CHECK (score BETWEEN 0 AND 100),
    decline_reason TEXT,
    decided_by     TEXT,
    decided_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_loan_applications_status ON loan_applications (tenant_id, status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_loan_applications_contact ON loan_applications (tenant_id, contact_id);
ALTER TABLE loan_applications ENABLE ROW LEVEL SECURITY;
ALTER TABLE loan_applications FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'loan_applications' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON loan_applications
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS loan_accounts (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL,
    application_id   UUID NOT NULL UNIQUE,
    contact_id       UUID NOT NULL,
    principal_kobo   BIGINT NOT NULL CHECK (principal_kobo > 0),
    interest_kobo    BIGINT NOT NULL DEFAULT 0 CHECK (interest_kobo >= 0),
    fee_kobo         BIGINT NOT NULL DEFAULT 0 CHECK (fee_kobo >= 0),
    outstanding_kobo BIGINT NOT NULL CHECK (outstanding_kobo >= 0),
    disbursed_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    due_at           TIMESTAMPTZ NOT NULL,
    status           TEXT NOT NULL DEFAULT 'active'
                     CHECK (status IN ('active','repaid','defaulted')),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_loan_accounts_status ON loan_accounts (tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_loan_accounts_due ON loan_accounts (tenant_id, due_at) WHERE status = 'active';
ALTER TABLE loan_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE loan_accounts FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'loan_accounts' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON loan_accounts
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS repayments (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    loan_id     UUID NOT NULL,
    amount_kobo BIGINT NOT NULL CHECK (amount_kobo > 0),
    ref_id      TEXT NOT NULL,
    paid_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, loan_id, ref_id)
);
CREATE INDEX IF NOT EXISTS idx_repayments_loan ON repayments (tenant_id, loan_id, paid_at);
ALTER TABLE repayments ENABLE ROW LEVEL SECURITY;
ALTER TABLE repayments FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'repayments' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON repayments
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

-- Kobo double-entry ledger (mirror of the W19 loyalty_ledger with codes
-- 500/501). Idempotent on (tenant_id, ref_type, ref_id, account_code).
CREATE TABLE IF NOT EXISTS lending_ledger (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL,
    journal_id     UUID NOT NULL,
    account_code   INTEGER NOT NULL CHECK (account_code IN (500,501)),
    beneficiary_id TEXT NOT NULL DEFAULT '',
    debit_kobo     BIGINT NOT NULL DEFAULT 0,
    credit_kobo    BIGINT NOT NULL DEFAULT 0,
    ref_type       TEXT NOT NULL,
    ref_id         TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, ref_type, ref_id, account_code),
    CHECK ((debit_kobo > 0 AND credit_kobo = 0) OR (credit_kobo > 0 AND debit_kobo = 0))
);
CREATE INDEX IF NOT EXISTS idx_lending_ledger_tenant_time ON lending_ledger (tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_lending_ledger_balance
    ON lending_ledger (tenant_id, account_code, beneficiary_id);
ALTER TABLE lending_ledger ENABLE ROW LEVEL SECURITY;
ALTER TABLE lending_ledger FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'lending_ledger' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON lending_ledger
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure lending tables: %w", err)
	}
	return nil
}

// withTenant runs fn inside a transaction with `SET LOCAL app.tenant_id`
// applied (mirrors devices.Store.withTenant) so the RLS tenant_isolation
// policy scopes every statement of fn to the given tenant.
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

// ---------------------------------------------------------------------------
// Products
// ---------------------------------------------------------------------------

const productCols = `id, tenant_id, name, active, principal_min_kobo, principal_max_kobo,
	term_days, interest_bps, fee_flat_kobo, created_at, updated_at`

func scanProduct(row pgx.Row) (Product, error) {
	var p Product
	err := row.Scan(&p.ID, &p.TenantID, &p.Name, &p.Active, &p.PrincipalMinKobo,
		&p.PrincipalMaxKobo, &p.TermDays, &p.InterestBps, &p.FeeFlatKobo,
		&p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// CreateProduct inserts one product (POST /v1/lending/products).
func (s *Store) CreateProduct(ctx context.Context, p *Product) error {
	const q = `INSERT INTO loan_products (` + productCols + `)
		           VALUES (gen_random_uuid(), $1,$2,$3,$4,$5,$6,$7,$8,now(),now())
		           RETURNING ` + productCols
	return s.withTenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		row, err := scanProduct(tx.QueryRow(ctx, q,
			p.TenantID, p.Name, p.Active, p.PrincipalMinKobo, p.PrincipalMaxKobo,
			p.TermDays, p.InterestBps, p.FeeFlatKobo))
		if err != nil {
			return fmt.Errorf("create product: %w", err)
		}
		*p = row
		return nil
	})
}

// ProductPatch is the partial update shape of PATCH
// /v1/lending/products/{id} — nil fields are left untouched.
type ProductPatch struct {
	Name             *string `json:"name,omitempty"`
	Active           *bool   `json:"active,omitempty"`
	PrincipalMinKobo *int64  `json:"principal_min_kobo,omitempty"`
	PrincipalMaxKobo *int64  `json:"principal_max_kobo,omitempty"`
	TermDays         *int    `json:"term_days,omitempty"`
	InterestBps      *int    `json:"interest_bps,omitempty"`
	FeeFlatKobo      *int64  `json:"fee_flat_kobo,omitempty"`
}

// UpdateProduct applies one patch validated against the MERGED row;
// missing/cross-tenant ids → ErrNotFound.
func (s *Store) UpdateProduct(ctx context.Context, tenantID, id uuid.UUID, patch ProductPatch) (Product, error) {
	var out Product
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanProduct(tx.QueryRow(ctx,
			`SELECT `+productCols+` FROM loan_products WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
			tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if patch.Name != nil {
			cur.Name = *patch.Name
		}
		if patch.Active != nil {
			cur.Active = *patch.Active
		}
		if patch.PrincipalMinKobo != nil {
			cur.PrincipalMinKobo = *patch.PrincipalMinKobo
		}
		if patch.PrincipalMaxKobo != nil {
			cur.PrincipalMaxKobo = *patch.PrincipalMaxKobo
		}
		if patch.TermDays != nil {
			cur.TermDays = *patch.TermDays
		}
		if patch.InterestBps != nil {
			cur.InterestBps = *patch.InterestBps
		}
		if patch.FeeFlatKobo != nil {
			cur.FeeFlatKobo = *patch.FeeFlatKobo
		}
		if err := cur.Validate(); err != nil {
			return err
		}
		row, err := scanProduct(tx.QueryRow(ctx,
			`UPDATE loan_products
			    SET name=$3, active=$4, principal_min_kobo=$5, principal_max_kobo=$6,
			        term_days=$7, interest_bps=$8, fee_flat_kobo=$9, updated_at=now()
			  WHERE tenant_id=$1 AND id=$2
			  RETURNING `+productCols,
			tenantID, id, cur.Name, cur.Active, cur.PrincipalMinKobo, cur.PrincipalMaxKobo,
			cur.TermDays, cur.InterestBps, cur.FeeFlatKobo))
		if err != nil {
			return fmt.Errorf("update product: %w", err)
		}
		out = row
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Applications
// ---------------------------------------------------------------------------

const applicationCols = `id, tenant_id, contact_id, product_id, principal_kobo, status,
	score, decline_reason, decided_by, decided_at, created_at, updated_at`

func scanApplication(row pgx.Row) (Application, error) {
	var a Application
	err := row.Scan(&a.ID, &a.TenantID, &a.ContactID, &a.ProductID, &a.PrincipalKobo,
		&a.Status, &a.Score, &a.DeclineReason, &a.DecidedBy, &a.DecidedAt,
		&a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// CreateApplication inserts one application (POST /v1/lending/applications)
// — the caller has already validated the principal against the product band
// and (for status submitted) computed the score.
func (s *Store) CreateApplication(ctx context.Context, a *Application) error {
	const q = `INSERT INTO loan_applications (` + applicationCols + `)
		           VALUES (gen_random_uuid(), $1,$2,$3,$4,$5,$6,$7,$8,$9,now(),now())
		           RETURNING ` + applicationCols
	return s.withTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		row, err := scanApplication(tx.QueryRow(ctx, q,
			a.TenantID, a.ContactID, a.ProductID, a.PrincipalKobo, a.Status,
			a.Score, a.DeclineReason, a.DecidedBy, a.DecidedAt))
		if err != nil {
			return fmt.Errorf("create application: %w", err)
		}
		*a = row
		return nil
	})
}

// UpdateApplication persists the operator-driven PATCH result (the caller
// ran the state machine + KYC gate). When the new status is defaulted, the
// ACTIVE loan account of this application (if any) flips to defaulted in
// the SAME transaction and its id is returned (DefaultedLoanID).
func (s *Store) UpdateApplication(ctx context.Context, a *Application) (defaultedLoanID *uuid.UUID, err error) {
	err = s.withTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		row, err := scanApplication(tx.QueryRow(ctx,
			`UPDATE loan_applications
			    SET status=$3, score=$4, decline_reason=$5, decided_by=$6, decided_at=$7, updated_at=now()
			  WHERE tenant_id=$1 AND id=$2
			  RETURNING `+applicationCols,
			a.TenantID, a.ID, a.Status, a.Score, a.DeclineReason, a.DecidedBy, a.DecidedAt))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("update application: %w", err)
		}
		*a = row
		if a.Status == StatusDefaulted {
			var loanID uuid.UUID
			err := tx.QueryRow(ctx,
				`UPDATE loan_accounts SET status='defaulted', updated_at=now()
				  WHERE tenant_id=$1 AND application_id=$2 AND status='active'
				  RETURNING id`, a.TenantID, a.ID).Scan(&loanID)
			if err == nil {
				defaultedLoanID = &loanID
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("flip loan to defaulted: %w", err)
			}
		}
		return nil
	})
	return defaultedLoanID, err
}

// ---------------------------------------------------------------------------
// Disburse (idempotent via the application status guard)
// ---------------------------------------------------------------------------

// DisburseResult reports the outcome of POST
// /v1/lending/applications/{id}/disburse.
type DisburseResult struct {
	Loan        LoanAccount `json:"loan"`
	Application Application `json:"application"`
	// Replayed is true when the application was already disbursed — the
	// existing loan account is returned unchanged (idempotent replay).
	Replayed bool `json:"replayed"`
}

// Disburse moves approved→disbursed and creates the loan account with
// interest = principal*bps/10000, fee and outstanding =
// principal+interest+fee, due_at = now+term_days — all in ONE tenant-scoped
// transaction with the balanced ledger 500 journal. Idempotent: a replay
// (application already disbursed) returns the existing loan account.
func (s *Store) Disburse(ctx context.Context, tenantID, applicationID uuid.UUID, now time.Time) (DisburseResult, error) {
	var res DisburseResult
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		app, err := scanApplication(tx.QueryRow(ctx,
			`SELECT `+applicationCols+` FROM loan_applications WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
			tenantID, applicationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if app.Status == StatusDisbursed {
			// Idempotent replay: return the existing loan account.
			loan, err := scanLoan(tx.QueryRow(ctx,
				`SELECT `+loanCols+` FROM loan_accounts WHERE tenant_id=$1 AND application_id=$2`,
				tenantID, applicationID))
			if err != nil {
				return fmt.Errorf("load disbursed loan: %w", err)
			}
			res = DisburseResult{Loan: loan, Application: app, Replayed: true}
			return nil
		}
		if app.Status != StatusApproved {
			return fmt.Errorf("%w: %s → %s (only approved applications can be disbursed)",
				ErrInvalidTransition, app.Status, StatusDisbursed)
		}
		prod, err := scanProduct(tx.QueryRow(ctx,
			`SELECT `+productCols+` FROM loan_products WHERE tenant_id=$1 AND id=$2`,
			tenantID, app.ProductID))
		if err != nil {
			return fmt.Errorf("load product: %w", err)
		}

		interest := prod.InterestFor(app.PrincipalKobo)
		outstanding := app.PrincipalKobo + interest + prod.FeeFlatKobo
		dueAt := now.AddDate(0, 0, prod.TermDays)
		loan, err := scanLoan(tx.QueryRow(ctx,
			`INSERT INTO loan_accounts (tenant_id, application_id, contact_id, principal_kobo,
			                            interest_kobo, fee_kobo, outstanding_kobo, disbursed_at, due_at, status)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'active')
			 ON CONFLICT (application_id) DO NOTHING
			 RETURNING `+loanCols,
			tenantID, applicationID, app.ContactID, app.PrincipalKobo,
			interest, prod.FeeFlatKobo, outstanding, now, dueAt))
		if errors.Is(err, pgx.ErrNoRows) {
			// Lost a concurrent race on application_id — replay semantics.
			loan, err = scanLoan(tx.QueryRow(ctx,
				`SELECT `+loanCols+` FROM loan_accounts WHERE tenant_id=$1 AND application_id=$2`,
				tenantID, applicationID))
			if err != nil {
				return fmt.Errorf("load raced loan: %w", err)
			}
			res = DisburseResult{Loan: loan, Application: app, Replayed: true}
			return nil
		}
		if err != nil {
			return fmt.Errorf("create loan account: %w", err)
		}

		// Balanced ledger journal: DEBIT 501 (house cash out) /
		// CREDIT 500 (borrower principal). ref_id = application id.
		journalID := DisburseJournalID(applicationID)
		if err := postJournalTx(ctx, tx, journalID, []LedgerEntry{
			{
				TenantID: tenantID, JournalID: journalID, AccountCode: AccountRepaymentReceived,
				BeneficiaryID: "", RefType: RefTypeDisbursement, RefID: applicationID.String(),
				DebitKobo: app.PrincipalKobo,
			},
			{
				TenantID: tenantID, JournalID: journalID, AccountCode: AccountPrincipalDisbursed,
				BeneficiaryID: app.ContactID.String(), RefType: RefTypeDisbursement, RefID: applicationID.String(),
				CreditKobo: app.PrincipalKobo,
			},
		}); err != nil {
			return err
		}

		app, err = scanApplication(tx.QueryRow(ctx,
			`UPDATE loan_applications SET status='disbursed', updated_at=now()
			  WHERE tenant_id=$1 AND id=$2
			  RETURNING `+applicationCols, tenantID, applicationID))
		if err != nil {
			return fmt.Errorf("flip application to disbursed: %w", err)
		}
		res = DisburseResult{Loan: loan, Application: app, Replayed: false}
		return nil
	})
	return res, err
}

// DisburseJournalID derives the deterministic journal id of one
// disbursement (idempotent on the application id).
func DisburseJournalID(applicationID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID,
		[]byte("opendesk:"+RefTypeDisbursement+":"+applicationID.String()))
}

// ---------------------------------------------------------------------------
// Loan accounts & repayments
// ---------------------------------------------------------------------------

const loanCols = `id, tenant_id, application_id, contact_id, principal_kobo, interest_kobo,
	fee_kobo, outstanding_kobo, disbursed_at, due_at, status, updated_at`

func scanLoan(row pgx.Row) (LoanAccount, error) {
	var l LoanAccount
	err := row.Scan(&l.ID, &l.TenantID, &l.ApplicationID, &l.ContactID, &l.PrincipalKobo,
		&l.InterestKobo, &l.FeeKobo, &l.OutstandingKobo, &l.DisbursedAt, &l.DueAt,
		&l.Status, &l.UpdatedAt)
	return l, err
}

// RepayResult reports the outcome of POST /v1/lending/loans/{id}/repay.
type RepayResult struct {
	Repayment Repayment   `json:"repayment"`
	Loan      LoanAccount `json:"loan"`
	// RequestedKobo is the amount the caller asked for; Repayment.AmountKobo
	// is what was APPLIED (clamped to outstanding).
	RequestedKobo int64 `json:"requested_kobo"`
	// Clamped is true when the request exceeded outstanding — the overpay is
	// noted here, never recorded.
	Clamped bool `json:"clamped"`
	// Replayed is true on an idempotent replay (same ref_id): the stored
	// repayment + loan are returned unchanged, nothing was written.
	Replayed bool `json:"replayed"`
	// LoanRepaid is true when this payment brought outstanding to zero
	// (loan + application flipped to repaid — fires the repaid event).
	LoanRepaid bool `json:"loan_repaid"`
}

// Repay applies one repayment to one loan: idempotent on
// (tenant_id, loan_id, ref_id) (replay → same stored row, Replayed=true),
// amount clamped to outstanding (overpay noted, never recorded), loan +
// application flip to repaid when outstanding hits zero, balanced ledger
// 501 journal — all in ONE tenant-scoped transaction on the loan row lock.
func (s *Store) Repay(ctx context.Context, tenantID, loanID uuid.UUID, amountKobo int64, refID string) (RepayResult, error) {
	var res RepayResult
	res.RequestedKobo = amountKobo
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		// Idempotency probe FIRST (same ref_id → replay of the stored row).
		var rep Repayment
		err := tx.QueryRow(ctx,
			`SELECT id, tenant_id, loan_id, amount_kobo, ref_id, paid_at
			   FROM repayments WHERE tenant_id=$1 AND loan_id=$2 AND ref_id=$3`,
			tenantID, loanID, refID).
			Scan(&rep.ID, &rep.TenantID, &rep.LoanID, &rep.AmountKobo, &rep.RefID, &rep.PaidAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			loan, lerr := scanLoan(tx.QueryRow(ctx,
				`SELECT `+loanCols+` FROM loan_accounts WHERE tenant_id=$1 AND id=$2`, tenantID, loanID))
			if lerr != nil {
				return lerr
			}
			res.Repayment = rep
			res.Loan = loan
			res.RequestedKobo = rep.AmountKobo
			res.Replayed = true
			res.LoanRepaid = loan.Status == LoanRepaid
			return nil
		}

		loan, err := scanLoan(tx.QueryRow(ctx,
			`SELECT `+loanCols+` FROM loan_accounts WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
			tenantID, loanID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if loan.Status != LoanActive {
			return fmt.Errorf("%w: loan is %s (only active loans accept repayments)",
				ErrInvalidTransition, loan.Status)
		}

		applied := amountKobo
		if applied > loan.OutstandingKobo {
			applied = loan.OutstandingKobo
			res.Clamped = true
		}
		err = tx.QueryRow(ctx,
			`INSERT INTO repayments (tenant_id, loan_id, amount_kobo, ref_id)
			 VALUES ($1,$2,$3,$4)
			 ON CONFLICT (tenant_id, loan_id, ref_id) DO NOTHING
			 RETURNING id, tenant_id, loan_id, amount_kobo, ref_id, paid_at`,
			tenantID, loanID, applied, refID).
			Scan(&rep.ID, &rep.TenantID, &rep.LoanID, &rep.AmountKobo, &rep.RefID, &rep.PaidAt)
		if errors.Is(err, pgx.ErrNoRows) {
			// Concurrent replay won the unique race — answer as a replay.
			err = tx.QueryRow(ctx,
				`SELECT id, tenant_id, loan_id, amount_kobo, ref_id, paid_at
				   FROM repayments WHERE tenant_id=$1 AND loan_id=$2 AND ref_id=$3`,
				tenantID, loanID, refID).
				Scan(&rep.ID, &rep.TenantID, &rep.LoanID, &rep.AmountKobo, &rep.RefID, &rep.PaidAt)
			if err != nil {
				return err
			}
			res.Repayment = rep
			res.Loan = loan
			res.RequestedKobo = rep.AmountKobo
			res.Replayed = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("insert repayment: %w", err)
		}

		loan.OutstandingKobo -= applied
		res.LoanRepaid = loan.OutstandingKobo == 0
		newStatus := LoanActive
		if res.LoanRepaid {
			newStatus = LoanRepaid
		}
		loan, err = scanLoan(tx.QueryRow(ctx,
			`UPDATE loan_accounts SET outstanding_kobo=$3, status=$4, updated_at=now()
			  WHERE tenant_id=$1 AND id=$2
			  RETURNING `+loanCols, tenantID, loanID, loan.OutstandingKobo, newStatus))
		if err != nil {
			return fmt.Errorf("update loan: %w", err)
		}

		// Balanced ledger journal: DEBIT 500 (borrower principal reduced) /
		// CREDIT 501 (house cash in). ref_id = caller repayment ref_id.
		journalID := uuid.NewSHA1(uuid.NameSpaceOID,
			[]byte("opendesk:"+RefTypeRepayment+":"+loanID.String()+":"+refID))
		if err := postJournalTx(ctx, tx, journalID, []LedgerEntry{
			{
				TenantID: tenantID, JournalID: journalID, AccountCode: AccountPrincipalDisbursed,
				BeneficiaryID: loan.ContactID.String(), RefType: RefTypeRepayment, RefID: refID,
				DebitKobo: applied,
			},
			{
				TenantID: tenantID, JournalID: journalID, AccountCode: AccountRepaymentReceived,
				BeneficiaryID: "", RefType: RefTypeRepayment, RefID: refID,
				CreditKobo: applied,
			},
		}); err != nil {
			return err
		}

		if res.LoanRepaid {
			if _, err := tx.Exec(ctx,
				`UPDATE loan_applications SET status='repaid', updated_at=now()
				  WHERE tenant_id=$1 AND id=$2 AND status='disbursed'`,
				tenantID, loan.ApplicationID); err != nil {
				return fmt.Errorf("flip application to repaid: %w", err)
			}
		}

		res.Repayment = rep
		res.Loan = loan
		return nil
	})
	return res, err
}

// ---------------------------------------------------------------------------
// Ledger SQL (shared by PostgresLedger and the disburse/repay txs)
// ---------------------------------------------------------------------------

const ledgerCols = `id, tenant_id, journal_id, account_code, beneficiary_id, debit_kobo, credit_kobo, ref_type, ref_id, created_at`

func scanLedgerEntry(row pgx.Row) (LedgerEntry, error) {
	var e LedgerEntry
	err := row.Scan(&e.ID, &e.TenantID, &e.JournalID, &e.AccountCode, &e.BeneficiaryID,
		&e.DebitKobo, &e.CreditKobo, &e.RefType, &e.RefID, &e.CreatedAt)
	return e, err
}

// postJournalTx inserts one journal's entries inside an EXISTING tenant tx
// (idempotent: ON CONFLICT (tenant_id, ref_type, ref_id, account_code)
// DO NOTHING — a replayed journal is a no-op).
func postJournalTx(ctx context.Context, tx pgx.Tx, journalID uuid.UUID, entries []LedgerEntry) error {
	for _, e := range entries {
		e.JournalID = journalID
		if _, err := tx.Exec(ctx,
			`INSERT INTO lending_ledger (tenant_id, journal_id, account_code, beneficiary_id, debit_kobo, credit_kobo, ref_type, ref_id)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			 ON CONFLICT (tenant_id, ref_type, ref_id, account_code) DO NOTHING`,
			e.TenantID, e.JournalID, e.AccountCode, e.BeneficiaryID,
			e.DebitKobo, e.CreditKobo, e.RefType, e.RefID); err != nil {
			return fmt.Errorf("post ledger entry: %w", err)
		}
	}
	return nil
}

// PostLedgerJournal validates + commits one journal in its own tenant tx
// (backs PostgresLedger.Post).
func (s *Store) PostLedgerJournal(ctx context.Context, tenantID uuid.UUID, entries []LedgerEntry) error {
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var journalID uuid.UUID
		if len(entries) > 0 {
			journalID = entries[0].JournalID
		}
		return postJournalTx(ctx, tx, journalID, entries)
	})
}

// ---------------------------------------------------------------------------
// Outbox (best-effort CloudEvents + usage metering)
// ---------------------------------------------------------------------------

// EnqueueOutbox appends one row to the transactional outbox (drained by the
// outbox dispatcher to Kafka via Dapr — mirrors
// workorders.Store.EnqueueOutbox).
//
// NOTE (RLS): the outbox table is not tenant-scoped (no RLS policy — the
// dispatcher drains it cross-tenant by design), so this intentionally runs
// outside withTenant.
func (s *Store) EnqueueOutbox(ctx context.Context, aggregateID uuid.UUID, topic string, payload []byte) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO outbox (aggregate_id, topic, payload) VALUES ($1,$2,$3)`,
		aggregateID, topic, payload); err != nil {
		return fmt.Errorf("enqueue outbox: %w", err)
	}
	return nil
}
