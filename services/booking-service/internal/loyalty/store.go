package loyalty

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists loyalty programs, wallets and the points ledger. Same
// packaging idiom as the W16 devices.Store / W14 referrals.PayoutStore:
// NewStore wraps an existing pool (tests), DialStore opens a small
// dedicated pool (main wiring path — the shared store.Store does not
// expose its pool). maxConns 4: loyalty accrual/redemption is a low-QPS
// operator + webhook path.
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

// ensureSchema bootstraps loyalty_programs / loyalty_wallets /
// loyalty_ledger idempotently (same pattern as devices.Store.ensureSchema,
// SPEC-W16): RLS enabled + forced with the tenant_isolation policy,
// guarded by a pg_policies existence check.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureSchema(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS loyalty_programs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL,
    name         TEXT NOT NULL,
    active       BOOLEAN NOT NULL DEFAULT true,
    earn_rules   JSONB NOT NULL DEFAULT '[]',
    tiers        JSONB NOT NULL DEFAULT '[]',
    cap_per_day  BIGINT NOT NULL DEFAULT 0 CHECK (cap_per_day >= 0),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_loyalty_programs_tenant ON loyalty_programs (tenant_id, active, updated_at DESC);
ALTER TABLE loyalty_programs ENABLE ROW LEVEL SECURITY;
ALTER TABLE loyalty_programs FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'loyalty_programs' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON loyalty_programs
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS loyalty_wallets (
    tenant_id         UUID NOT NULL,
    contact_id        UUID NOT NULL,
    balance           BIGINT NOT NULL DEFAULT 0 CHECK (balance >= 0),
    lifetime_earned   BIGINT NOT NULL DEFAULT 0,
    lifetime_redeemed BIGINT NOT NULL DEFAULT 0,
    tier              TEXT NOT NULL DEFAULT '',
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, contact_id)
);
CREATE INDEX IF NOT EXISTS idx_loyalty_wallets_leaderboard
    ON loyalty_wallets (tenant_id, lifetime_earned DESC);
ALTER TABLE loyalty_wallets ENABLE ROW LEVEL SECURITY;
ALTER TABLE loyalty_wallets FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'loyalty_wallets' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON loyalty_wallets
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;

-- Points double-entry ledger (mirror of booking.commission_ledger with
-- codes 400/401, points instead of kobo). Idempotent on
-- (tenant_id, ref_type, ref_id, account_code).
CREATE TABLE IF NOT EXISTS loyalty_ledger (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL,
    journal_id     UUID NOT NULL,
    account_code   INTEGER NOT NULL CHECK (account_code IN (400,401)),
    beneficiary_id TEXT NOT NULL DEFAULT '',
    debit_points   BIGINT NOT NULL DEFAULT 0,
    credit_points  BIGINT NOT NULL DEFAULT 0,
    ref_type       TEXT NOT NULL,
    ref_id         TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, ref_type, ref_id, account_code),
    CHECK ((debit_points > 0 AND credit_points = 0) OR (credit_points > 0 AND debit_points = 0))
);
CREATE INDEX IF NOT EXISTS idx_loyalty_ledger_tenant_time ON loyalty_ledger (tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_loyalty_ledger_balance
    ON loyalty_ledger (tenant_id, account_code, beneficiary_id);
ALTER TABLE loyalty_ledger ENABLE ROW LEVEL SECURITY;
ALTER TABLE loyalty_ledger FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'loyalty_ledger' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON loyalty_ledger
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
    END IF;
END $$;`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure loyalty tables: %w", err)
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
// Programs
// ---------------------------------------------------------------------------

const programCols = `id, tenant_id, name, active, earn_rules, tiers, cap_per_day, created_at, updated_at`

func scanProgram(row pgx.Row) (Program, error) {
	var p Program
	var rulesRaw, tiersRaw []byte
	err := row.Scan(&p.ID, &p.TenantID, &p.Name, &p.Active, &rulesRaw, &tiersRaw,
		&p.CapPerDay, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	p.EarnRules = []EarnRule{}
	p.Tiers = []Tier{}
	if len(rulesRaw) > 0 {
		if err := json.Unmarshal(rulesRaw, &p.EarnRules); err != nil {
			return p, fmt.Errorf("decode earn_rules: %w", err)
		}
	}
	if len(tiersRaw) > 0 {
		if err := json.Unmarshal(tiersRaw, &p.Tiers); err != nil {
			return p, fmt.Errorf("decode tiers: %w", err)
		}
	}
	return p, nil
}

// CreateProgram inserts one program (POST /v1/loyalty/programs).
func (s *Store) CreateProgram(ctx context.Context, p *Program) error {
	rules, err := json.Marshal(p.EarnRules)
	if err != nil {
		return fmt.Errorf("%w: earn_rules: %v", ErrInvalidInput, err)
	}
	tiers, err := json.Marshal(p.Tiers)
	if err != nil {
		return fmt.Errorf("%w: tiers: %v", ErrInvalidInput, err)
	}
	const q = `INSERT INTO loyalty_programs (` + programCols + `)
		           VALUES (gen_random_uuid(), $1,$2,$3,$4,$5,$6,now(),now())
		           RETURNING ` + programCols
	return s.withTenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		row, err := scanProgram(tx.QueryRow(ctx, q,
			p.TenantID, p.Name, p.Active, rules, tiers, p.CapPerDay))
		if err != nil {
			return fmt.Errorf("create program: %w", err)
		}
		*p = row
		return nil
	})
}

// ProgramPatch is the partial update shape of PATCH
// /v1/loyalty/programs/{id} — nil fields are left untouched.
type ProgramPatch struct {
	Name      *string     `json:"name,omitempty"`
	Active    *bool       `json:"active,omitempty"`
	EarnRules *[]EarnRule `json:"earn_rules,omitempty"`
	Tiers     *[]Tier     `json:"tiers,omitempty"`
	CapPerDay *int64      `json:"cap_per_day,omitempty"`
}

// UpdateProgram applies one patch; missing/cross-tenant ids → ErrNotFound.
// The patch is validated against the MERGED row so a partial update can
// never leave the program in an invalid state.
func (s *Store) UpdateProgram(ctx context.Context, tenantID, id uuid.UUID, patch ProgramPatch) (Program, error) {
	var out Program
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		cur, err := scanProgram(tx.QueryRow(ctx,
			`SELECT `+programCols+` FROM loyalty_programs WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
			tenantID, id))
		if err == pgx.ErrNoRows {
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
		if patch.EarnRules != nil {
			cur.EarnRules = *patch.EarnRules
		}
		if patch.Tiers != nil {
			cur.Tiers = *patch.Tiers
		}
		if patch.CapPerDay != nil {
			cur.CapPerDay = *patch.CapPerDay
		}
		if err := ValidateProgram(&cur); err != nil {
			return err
		}
		rules, err := json.Marshal(cur.EarnRules)
		if err != nil {
			return fmt.Errorf("%w: earn_rules: %v", ErrInvalidInput, err)
		}
		tiers, err := json.Marshal(cur.Tiers)
		if err != nil {
			return fmt.Errorf("%w: tiers: %v", ErrInvalidInput, err)
		}
		row, err := scanProgram(tx.QueryRow(ctx,
			`UPDATE loyalty_programs
			    SET name=$3, active=$4, earn_rules=$5, tiers=$6, cap_per_day=$7, updated_at=now()
			  WHERE tenant_id=$1 AND id=$2
			  RETURNING `+programCols,
			tenantID, id, cur.Name, cur.Active, rules, tiers, cur.CapPerDay))
		if err != nil {
			return fmt.Errorf("update program: %w", err)
		}
		out = row
		return nil
	})
	return out, err
}

// ListPrograms enumerates the tenant's programs (newest-updated first).
// Backs GET /v1/loyalty/programs.
func (s *Store) ListPrograms(ctx context.Context, tenantID uuid.UUID) ([]Program, error) {
	out := []Program{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+programCols+` FROM loyalty_programs WHERE tenant_id=$1
			 ORDER BY updated_at DESC LIMIT 100`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProgram(rows)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// ActiveProgram resolves the tenant's current earn-rule source: the most
// recently updated ACTIVE program. ErrNotFound when none is active —
// the service maps that to ErrNoActiveProgram.
func (s *Store) ActiveProgram(ctx context.Context, tenantID uuid.UUID) (Program, error) {
	var p Program
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := scanProgram(tx.QueryRow(ctx,
			`SELECT `+programCols+` FROM loyalty_programs
			  WHERE tenant_id=$1 AND active ORDER BY updated_at DESC LIMIT 1`, tenantID))
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		p = row
		return nil
	})
	return p, err
}

// ---------------------------------------------------------------------------
// Wallets
// ---------------------------------------------------------------------------

const walletCols = `tenant_id, contact_id, balance, lifetime_earned, lifetime_redeemed, tier, updated_at`

func scanWallet(row pgx.Row) (Wallet, error) {
	var w Wallet
	err := row.Scan(&w.TenantID, &w.ContactID, &w.Balance, &w.LifetimeEarned,
		&w.LifetimeRedeemed, &w.Tier, &w.UpdatedAt)
	return w, err
}

// GetWallet returns one contact's wallet; missing → ErrNotFound (the UI
// renders an honest "no wallet yet" empty state).
func (s *Store) GetWallet(ctx context.Context, tenantID, contactID uuid.UUID) (Wallet, error) {
	var w Wallet
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		row, err := scanWallet(tx.QueryRow(ctx,
			`SELECT `+walletCols+` FROM loyalty_wallets WHERE tenant_id=$1 AND contact_id=$2`,
			tenantID, contactID))
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		w = row
		return nil
	})
	return w, err
}

// lockWalletTx ensures the wallet row exists and locks it FOR UPDATE —
// accrue/redeem serialize per (tenant, contact) on this row so the
// cap_per_day computation and the balance check are race-safe.
func lockWalletTx(ctx context.Context, tx pgx.Tx, tenantID, contactID uuid.UUID) (Wallet, error) {
	if _, err := tx.Exec(ctx,
		`INSERT INTO loyalty_wallets (tenant_id, contact_id) VALUES ($1,$2)
		 ON CONFLICT (tenant_id, contact_id) DO NOTHING`, tenantID, contactID); err != nil {
		return Wallet{}, fmt.Errorf("ensure wallet: %w", err)
	}
	return scanWallet(tx.QueryRow(ctx,
		`SELECT `+walletCols+` FROM loyalty_wallets WHERE tenant_id=$1 AND contact_id=$2 FOR UPDATE`,
		tenantID, contactID))
}

// LeaderboardEntry is one row of GET /v1/loyalty/leaderboard.
type LeaderboardEntry struct {
	Rank             int       `json:"rank"`
	ContactID        uuid.UUID `json:"contact_id"`
	Balance          int64     `json:"balance"`
	LifetimeEarned   int64     `json:"lifetime_earned"`
	LifetimeRedeemed int64     `json:"lifetime_redeemed"`
	Tier             string    `json:"tier"`
}

// LeaderboardMetric selects the ranking column.
type LeaderboardMetric string

const (
	LeaderboardByEarned   LeaderboardMetric = "lifetime_earned"
	LeaderboardByBalance  LeaderboardMetric = "balance"
	LeaderboardByRedeemed LeaderboardMetric = "lifetime_redeemed"
)

// Leaderboard ranks wallets by one metric (default lifetime_earned),
// highest first. limit is clamped to [1,100].
func (s *Store) Leaderboard(ctx context.Context, tenantID uuid.UUID, metric LeaderboardMetric, limit int) ([]LeaderboardEntry, error) {
	col := string(metric)
	switch metric {
	case LeaderboardByBalance, LeaderboardByRedeemed:
	case LeaderboardByEarned, "":
		col = string(LeaderboardByEarned)
	default:
		return nil, fmt.Errorf("%w: metric %q (want lifetime_earned|balance|lifetime_redeemed)", ErrInvalidInput, metric)
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	out := []LeaderboardEntry{}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT contact_id, balance, lifetime_earned, lifetime_redeemed, tier
			   FROM loyalty_wallets WHERE tenant_id=$1
			  ORDER BY `+col+` DESC, contact_id LIMIT $2`, tenantID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		rank := 0
		for rows.Next() {
			var e LeaderboardEntry
			if err := rows.Scan(&e.ContactID, &e.Balance, &e.LifetimeEarned, &e.LifetimeRedeemed, &e.Tier); err != nil {
				return err
			}
			rank++
			e.Rank = rank
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Accrue / Redeem (single-tx: ledger journal + wallet cache)
// ---------------------------------------------------------------------------

// AccrueParams drives Store.Accrue.
type AccrueParams struct {
	TenantID  uuid.UUID
	ContactID uuid.UUID
	Event     string // validated earn-rule event
	RefID     string // caller idempotency key; ledger ref_id = Event:RefID
	Points    int64  // resolved from the active program's earn_rules
	CapPerDay int64  // active program cap (0 = uncapped)
	Tiers     []Tier // active program tiers (wallet tier recompute)
}

// AccrueResult reports the outcome of one accrual.
type AccrueResult struct {
	Wallet  Wallet `json:"wallet"`
	Awarded int64  `json:"awarded"` // points actually credited (after cap clamp)
	Applied bool   `json:"applied"` // false on an idempotent replay
	Capped  bool   `json:"capped"`  // true when cap_per_day clamped (or zeroed) the award
}

// accrualRefID is the idempotency anchor of one accrual: idempotent on
// ref_id+event via the ledger UNIQUE (tenant_id, ref_type, ref_id,
// account_code).
func accrualRefID(event, refID string) string { return event + ":" + refID }

// AccrualJournalID derives the deterministic journal id of one accrual.
func AccrualJournalID(event, refID string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID,
		[]byte("opendesk:loyalty_accrual:"+accrualRefID(event, refID)))
}

// Accrue credits the earn-rule points of one event to one contact:
// idempotent on ref_id+event, cap_per_day-clamped, tier recomputed — all in
// ONE tenant-scoped transaction on the wallet row lock. Over-cap awards are
// clamped to the remaining daily allowance (0 remaining → awarded=0,
// capped=true, no journal posted; replays stay consistent because nothing
// was written for a zero award).
func (s *Store) Accrue(ctx context.Context, p AccrueParams) (AccrueResult, error) {
	var res AccrueResult
	refID := accrualRefID(p.Event, p.RefID)
	err := s.withTenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		// Idempotency probe FIRST: a posted accrual journal for this
		// ref_id+event makes the replay a no-op.
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM loyalty_ledger
			                WHERE tenant_id=$1 AND ref_type=$2 AND ref_id=$3 AND account_code=$4)`,
			p.TenantID, RefTypeAccrual, refID, AccountPointsIssued).Scan(&exists); err != nil {
			return err
		}
		w, err := lockWalletTx(ctx, tx, p.TenantID, p.ContactID)
		if err != nil {
			return err
		}
		if exists {
			res = AccrueResult{Wallet: w, Awarded: 0, Applied: false, Capped: false}
			return nil
		}

		awarded := p.Points
		if p.CapPerDay > 0 {
			// Points already earned today (UTC day) = today's credits to the
			// 400 account for this beneficiary.
			var todayEarned int64
			if err := tx.QueryRow(ctx,
				`SELECT COALESCE(SUM(credit_points),0) FROM loyalty_ledger
				  WHERE tenant_id=$1 AND account_code=$2 AND beneficiary_id=$3
				    AND created_at >= date_trunc('day', now())`,
				p.TenantID, AccountPointsIssued, p.ContactID.String()).Scan(&todayEarned); err != nil {
				return err
			}
			if remaining := p.CapPerDay - todayEarned; remaining < awarded {
				awarded = remaining
				res.Capped = true
			}
		}
		if awarded < 0 {
			awarded = 0
		}

		if awarded > 0 {
			journalID := AccrualJournalID(p.Event, p.RefID)
			if err := postJournalTx(ctx, tx, journalID, []LedgerEntry{
				{ // house side: points flowed out of thin air
					TenantID: p.TenantID, JournalID: journalID, AccountCode: AccountPointsRedeemed,
					BeneficiaryID: "", RefType: RefTypeAccrual, RefID: refID, DebitPoints: awarded,
				},
				{ // contact side: points issued
					TenantID: p.TenantID, JournalID: journalID, AccountCode: AccountPointsIssued,
					BeneficiaryID: p.ContactID.String(), RefType: RefTypeAccrual, RefID: refID, CreditPoints: awarded,
				},
			}); err != nil {
				return err
			}
			var lifetimeEarned int64
			if err := tx.QueryRow(ctx,
				`UPDATE loyalty_wallets
				    SET balance = balance + $3,
				        lifetime_earned = lifetime_earned + $3,
				        updated_at = now()
				  WHERE tenant_id=$1 AND contact_id=$2
				  RETURNING lifetime_earned`,
				p.TenantID, p.ContactID, awarded).Scan(&lifetimeEarned); err != nil {
				return err
			}
			if tier := TierFor(lifetimeEarned, p.Tiers); tier != w.Tier {
				if _, err := tx.Exec(ctx,
					`UPDATE loyalty_wallets SET tier=$3, updated_at=now()
					  WHERE tenant_id=$1 AND contact_id=$2`,
					p.TenantID, p.ContactID, tier); err != nil {
					return err
				}
			}
		}
		w, err = scanWallet(tx.QueryRow(ctx,
			`SELECT `+walletCols+` FROM loyalty_wallets WHERE tenant_id=$1 AND contact_id=$2`,
			p.TenantID, p.ContactID))
		if err != nil {
			return err
		}
		res.Wallet = w
		res.Awarded = awarded
		res.Applied = awarded > 0
		return nil
	})
	return res, err
}

// RedeemParams drives Store.Redeem.
type RedeemParams struct {
	TenantID  uuid.UUID
	ContactID uuid.UUID
	Points    int64
	Reason    string
	// RefID anchors idempotent retries (recommended). When empty the store
	// mints a redemption id — callers that cannot retry safely may omit it.
	RefID string
}

// RedeemResult reports the outcome of one redemption.
type RedeemResult struct {
	Wallet    Wallet `json:"wallet"`
	Redeemed  int64  `json:"redeemed"`
	Applied   bool   `json:"applied"` // false on an idempotent replay
	RedeemRef string `json:"ref_id"`  // ledger ref_id of the redemption
}

// InsufficientError carries the current balance on a 409 so the UI can
// render "you have N, need M" without a second round-trip.
type InsufficientError struct {
	Balance int64 `json:"balance"`
}

func (e *InsufficientError) Error() string {
	return fmt.Sprintf("%s (balance %d)", ErrInsufficientPoints, e.Balance)
}
func (e *InsufficientError) Unwrap() error { return ErrInsufficientPoints }

// Redeem debits points from one wallet: insufficient balance →
// *InsufficientError (409 at the API); the journal is a balanced pair
// (DEBIT 400 contact / CREDIT 401 house) committed in the same transaction
// as the wallet update. Idempotent on ref_id when the caller supplies one.
func (s *Store) Redeem(ctx context.Context, p RedeemParams) (RedeemResult, error) {
	if p.Points <= 0 {
		return RedeemResult{}, fmt.Errorf("%w: points must be > 0", ErrInvalidInput)
	}
	if p.RefID == "" {
		p.RefID = uuid.NewString()
	}
	var res RedeemResult
	res.RedeemRef = p.RefID
	err := s.withTenant(ctx, p.TenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM loyalty_ledger
			                WHERE tenant_id=$1 AND ref_type=$2 AND ref_id=$3 AND account_code=$4)`,
			p.TenantID, RefTypeRedeem, p.RefID, AccountPointsIssued).Scan(&exists); err != nil {
			return err
		}
		w, err := lockWalletTx(ctx, tx, p.TenantID, p.ContactID)
		if err != nil {
			return err
		}
		if exists {
			res = RedeemResult{Wallet: w, Redeemed: 0, Applied: false, RedeemRef: p.RefID}
			return nil
		}
		if w.Balance < p.Points {
			return &InsufficientError{Balance: w.Balance}
		}
		journalID := uuid.NewSHA1(uuid.NameSpaceOID,
			[]byte("opendesk:loyalty_redeem:"+p.RefID))
		if err := postJournalTx(ctx, tx, journalID, []LedgerEntry{
			{
				TenantID: p.TenantID, JournalID: journalID, AccountCode: AccountPointsIssued,
				BeneficiaryID: p.ContactID.String(), RefType: RefTypeRedeem, RefID: p.RefID, DebitPoints: p.Points,
			},
			{
				TenantID: p.TenantID, JournalID: journalID, AccountCode: AccountPointsRedeemed,
				BeneficiaryID: "", RefType: RefTypeRedeem, RefID: p.RefID, CreditPoints: p.Points,
			},
		}); err != nil {
			return err
		}
		w, err = scanWallet(tx.QueryRow(ctx,
			`UPDATE loyalty_wallets
			    SET balance = balance - $3,
			        lifetime_redeemed = lifetime_redeemed + $3,
			        updated_at = now()
			  WHERE tenant_id=$1 AND contact_id=$2
			  RETURNING `+walletCols, p.TenantID, p.ContactID, p.Points))
		if err != nil {
			return err
		}
		res.Wallet = w
		res.Redeemed = p.Points
		res.Applied = true
		return nil
	})
	return res, err
}

// ---------------------------------------------------------------------------
// Ledger SQL (shared by PostgresLedger and the accrue/redeem txs)
// ---------------------------------------------------------------------------

const ledgerCols = `id, tenant_id, journal_id, account_code, beneficiary_id, debit_points, credit_points, ref_type, ref_id, created_at`

func scanLedgerEntry(row pgx.Row) (LedgerEntry, error) {
	var e LedgerEntry
	err := row.Scan(&e.ID, &e.TenantID, &e.JournalID, &e.AccountCode, &e.BeneficiaryID,
		&e.DebitPoints, &e.CreditPoints, &e.RefType, &e.RefID, &e.CreatedAt)
	return e, err
}

// postJournalTx inserts one journal's entries inside an EXISTING tenant tx
// (idempotent: ON CONFLICT (tenant_id, ref_type, ref_id, account_code)
// DO NOTHING — a replayed journal is a no-op).
func postJournalTx(ctx context.Context, tx pgx.Tx, journalID uuid.UUID, entries []LedgerEntry) error {
	for _, e := range entries {
		e.JournalID = journalID
		if _, err := tx.Exec(ctx,
			`INSERT INTO loyalty_ledger (tenant_id, journal_id, account_code, beneficiary_id, debit_points, credit_points, ref_type, ref_id)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			 ON CONFLICT (tenant_id, ref_type, ref_id, account_code) DO NOTHING`,
			e.TenantID, e.JournalID, e.AccountCode, e.BeneficiaryID,
			e.DebitPoints, e.CreditPoints, e.RefType, e.RefID); err != nil {
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

// LedgerBalance returns (credits − debits) of one account for one
// beneficiary ("" = house side), in points (backs PostgresLedger.Balance).
func (s *Store) LedgerBalance(ctx context.Context, tenantID uuid.UUID, accountCode int, beneficiaryID string) (int64, error) {
	var bal int64
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(credit_points),0) - COALESCE(SUM(debit_points),0)
			   FROM loyalty_ledger
			  WHERE tenant_id=$1 AND account_code=$2 AND beneficiary_id=$3`,
			tenantID, accountCode, beneficiaryID).Scan(&bal)
	})
	return bal, err
}

// ListLedgerEntries lists ledger rows of a tenant in [from,to] (nil =
// unbounded), oldest first. beneficiaryID "" = all beneficiaries; otherwise
// only rows for that beneficiary (the wallet-detail view).
func (s *Store) ListLedgerEntries(ctx context.Context, tenantID uuid.UUID, from, to *time.Time, beneficiaryID string) ([]LedgerEntry, error) {
	q := `SELECT ` + ledgerCols + ` FROM loyalty_ledger WHERE tenant_id=$1`
	args := []any{tenantID}
	n := 1
	if beneficiaryID != "" {
		n++
		q += fmt.Sprintf(` AND beneficiary_id=$%d`, n)
		args = append(args, beneficiaryID)
	}
	if from != nil {
		n++
		q += fmt.Sprintf(` AND created_at >= $%d`, n)
		args = append(args, from.UTC())
	}
	if to != nil {
		n++
		q += fmt.Sprintf(` AND created_at <= $%d`, n)
		args = append(args, to.UTC())
	}
	q += ` ORDER BY created_at, id LIMIT 1000`
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

// ---------------------------------------------------------------------------
// Outbox (best-effort CloudEvents + usage metering)
// ---------------------------------------------------------------------------

// EnqueueOutbox appends one row to the transactional outbox (drained by the
// outbox dispatcher to Kafka). Mirrors store.Store.EnqueueOutbox on this
// package's dedicated pool.
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
