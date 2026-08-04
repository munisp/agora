package loyalty

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Points ledger — a MIRROR of the W14 referrals.Ledger contract
// (internal/referrals/ledger.go), adapted to points (codes 400/401).
//
// WHY A MIRROR AND NOT REUSE: referrals.PostgresLedger.validateJournal and
// the commission_ledger CHECK constraint both hard-pin account codes
// 300..303 (kobo, money); SPEC-W19 assigns loyalty codes 400
// loyalty_points_issued / 401 loyalty_points_redeemed (points). Reusing the
// referrals impl would fail its own validation, and editing referrals is
// forbidden (W19 anti-collision). The interface shape, the balanced-
// journal invariants and the idempotency anchor are mirrored 1:1 so a
// future unified ledger (or TigerBeetle adapter — see the referrals seam
// comment) can fold both back together.
// ---------------------------------------------------------------------------

// LedgerEntry is one double-entry row of booking.loyalty_ledger. Exactly
// one side carries the amount (CHECK constraint); beneficiary_id is the
// contact on the 400 account and "" on the house-side 401 rows.
type LedgerEntry struct {
	ID            uuid.UUID `json:"entry_id"`
	TenantID      uuid.UUID `json:"tenant_id"`
	JournalID     uuid.UUID `json:"journal_id"`
	AccountCode   int       `json:"account_code"` // 400 | 401
	BeneficiaryID string    `json:"beneficiary_id"`
	DebitPoints   int64     `json:"debit_points"`
	CreditPoints  int64     `json:"credit_points"`
	RefType       string    `json:"ref_type"` // loyalty_accrual | loyalty_redeem
	RefID         string    `json:"ref_id"`
	CreatedAt     time.Time `json:"created_at"`
}

// Ledger is the points double-entry ledger seam (mirrors
// referrals.Ledger). Every posting is a BALANCED journal: sum(debit) ==
// sum(credit) across the entries sharing one journal_id. Implementations
// must be idempotent on (ref_type, ref_id, account_code).
type Ledger interface {
	// Post commits one balanced journal. Entries must share journalID,
	// carry a valid account code (400|401), exactly one non-zero side and
	// sum to balanced debits/credits — violations return
	// ErrUnbalancedJournal (no partial commit). Replays are no-ops.
	Post(ctx context.Context, tenantID, journalID uuid.UUID, entries []LedgerEntry) error
	// PostBalanced commits ONE balanced pair with the journal id derived
	// deterministically from (ref_type, ref_id).
	PostBalanced(ctx context.Context, p BalancedPosting) error
	// Balance returns (credits − debits) of one account for one beneficiary
	// ("" = house side), in points.
	Balance(ctx context.Context, tenantID uuid.UUID, accountCode int, beneficiaryID string) (int64, error)
	// Entries lists ledger rows of a tenant in [from,to] (nil = unbounded),
	// oldest first.
	Entries(ctx context.Context, tenantID uuid.UUID, from, to *time.Time) ([]LedgerEntry, error)
}

// BalancedPosting is one balanced double-entry pair (mirrors
// referrals.BalancedPosting): accrual = debit 401 / credit 400;
// redemption = debit 400 / credit 401. BeneficiaryID is the contact on the
// 400 side; "" is the house side.
type BalancedPosting struct {
	TenantID      uuid.UUID `json:"tenant_id"`
	DebitAccount  int       `json:"debit_account"`
	CreditAccount int       `json:"credit_account"`
	AmountPoints  int64     `json:"amount_points"`
	RefType       string    `json:"ref_type"`
	RefID         string    `json:"ref_id"`
	BeneficiaryID string    `json:"beneficiary_id,omitempty"`
}

// PostgresLedger is the Postgres implementation of Ledger on top of the
// RLS'd loyalty_ledger table.
type PostgresLedger struct {
	Store *Store
}

// NewPostgresLedger wires the Postgres Ledger impl (swap point for a
// future TigerBeetle adapter — see internal/referrals/ledger.go).
func NewPostgresLedger(st *Store) *PostgresLedger { return &PostgresLedger{Store: st} }

// validateJournal enforces the mirrored invariants before any row hits the
// database: shared tenant, valid account codes (400|401), one-sided
// entries and a balanced journal.
func validateJournal(tenantID, journalID uuid.UUID, entries []LedgerEntry) error {
	if len(entries) < 2 {
		return fmt.Errorf("%w: a journal needs at least 2 entries", ErrUnbalancedJournal)
	}
	var debits, credits int64
	for i := range entries {
		e := &entries[i]
		if e.TenantID != tenantID {
			return fmt.Errorf("%w: entry tenant mismatch", ErrUnbalancedJournal)
		}
		if e.AccountCode != AccountPointsIssued && e.AccountCode != AccountPointsRedeemed {
			return fmt.Errorf("%w: unknown account_code %d", ErrUnbalancedJournal, e.AccountCode)
		}
		if (e.DebitPoints > 0) == (e.CreditPoints > 0) {
			return fmt.Errorf("%w: entry must carry exactly one side (account %d)", ErrUnbalancedJournal, e.AccountCode)
		}
		if e.DebitPoints < 0 || e.CreditPoints < 0 {
			return fmt.Errorf("%w: negative amounts", ErrUnbalancedJournal)
		}
		debits += e.DebitPoints
		credits += e.CreditPoints
	}
	if debits != credits || debits == 0 {
		return fmt.Errorf("%w: debits %d != credits %d", ErrUnbalancedJournal, debits, credits)
	}
	if journalID == uuid.Nil {
		return fmt.Errorf("%w: journal_id is required", ErrUnbalancedJournal)
	}
	return nil
}

// Post commits one balanced journal via the store (idempotent on
// (ref_type, ref_id, account_code)).
func (l *PostgresLedger) Post(ctx context.Context, tenantID, journalID uuid.UUID, entries []LedgerEntry) error {
	for i := range entries {
		entries[i].JournalID = journalID
	}
	if err := validateJournal(tenantID, journalID, entries); err != nil {
		return err
	}
	return l.Store.PostLedgerJournal(ctx, tenantID, entries)
}

// PostBalanced implements Ledger: expands the compact pair into two
// entries and posts them with a deterministic journal id, so a retried
// posting is a ledger no-op.
func (l *PostgresLedger) PostBalanced(ctx context.Context, p BalancedPosting) error {
	if p.AmountPoints <= 0 {
		return fmt.Errorf("%w: amount must be > 0", ErrUnbalancedJournal)
	}
	if p.RefType == "" || p.RefID == "" {
		return fmt.Errorf("%w: ref_type and ref_id are required", ErrUnbalancedJournal)
	}
	journalID := uuid.NewSHA1(uuid.NameSpaceOID,
		[]byte("opendesk:balanced:"+p.RefType+":"+p.RefID))
	base := LedgerEntry{TenantID: p.TenantID, JournalID: journalID, RefType: p.RefType, RefID: p.RefID}
	debit := base
	debit.AccountCode = p.DebitAccount
	debit.BeneficiaryID = p.BeneficiaryID
	debit.DebitPoints = p.AmountPoints
	credit := base
	credit.AccountCode = p.CreditAccount
	credit.BeneficiaryID = p.BeneficiaryID
	credit.CreditPoints = p.AmountPoints
	return l.Post(ctx, p.TenantID, journalID, []LedgerEntry{debit, credit})
}

// Balance returns (credits − debits) in points for one account +
// beneficiary.
func (l *PostgresLedger) Balance(ctx context.Context, tenantID uuid.UUID, accountCode int, beneficiaryID string) (int64, error) {
	return l.Store.LedgerBalance(ctx, tenantID, accountCode, beneficiaryID)
}

// Entries lists ledger rows of a tenant in [from,to], oldest first.
func (l *PostgresLedger) Entries(ctx context.Context, tenantID uuid.UUID, from, to *time.Time) ([]LedgerEntry, error) {
	return l.Store.ListLedgerEntries(ctx, tenantID, from, to, "")
}
