package referrals

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
)

// ---------------------------------------------------------------------------
// Commission ledger (contract §3) — Ledger interface, Postgres impl and the
// TigerBeetle adapter seam.
// ---------------------------------------------------------------------------

// Ledger is the commission double-entry ledger seam (contract §3). Every
// posting is a BALANCED journal: sum(debit_ngn) == sum(credit_ngn) across
// the entries sharing one journal_id. Implementations must be idempotent on
// (ref_type, ref_id, account_code).
//
// ── TigerBeetle adapter seam (interface + documentation only, no TB client
// code per SPEC-W14 §3) ────────────────────────────────────────────────────
// A TigerBeetle implementation of Ledger maps the scheme 1:1:
//   - account codes 300/301/302/303 become four TigerBeetle accounts per
//     TENANT (TB account id = hash(tenant_id, account_code), or a TB
//     accounts registry keyed by (tenant_id, account_code); beneficiary
//     sub-ledgers use TB user_data_128 = beneficiary id hash);
//   - one journal becomes a chain of TB transfers linked with
//     TransferFlags.Linked, committed atomically (all-or-nothing == the
//     Postgres transaction used by PostgresLedger);
//   - idempotency: TB transfer id = UUIDv5(ref_type|ref_id|account_code),
//     matching the Postgres UNIQUE (tenant_id, ref_type, ref_id, account_code)
//     — TB rejects duplicate transfer ids with Exists*, the adapter maps
//     that to the same no-op semantics as ON CONFLICT DO NOTHING;
//   - Balance() = TB lookup_accounts debits/credits (credit-posted minus
//     debit-posted, same formula as Postgres);
//   - amounts are already integer kobo (uint64-safe) — no unit conversion.
// Swap point: main.go wires PostgresLedger today; a TBLedger{Client: ...}
// satisfying Ledger drops in without touching service/handler code. The
// verify path additionally couples the accrual to the referral status flip
// (store.VerifyReferralTx) — under TB that coupling moves to an outbox row
// committed with the status flip and a relay calling Ledger.Post, the same
// at-least-once + idempotent-consumer posture the cac.events outbox uses.
// ──────────────────────────────────────────────────────────────────────────
type Ledger interface {
	// Post commits one balanced journal. Entries must share journalID,
	// carry a valid account code (300..303), exactly one non-zero side and
	// sum to balanced debits/credits — violations return ErrUnbalancedJournal
	// (no partial commit). Replays are no-ops.
	Post(ctx context.Context, tenantID, journalID uuid.UUID, entries []LedgerEntry) error
	// PostBalanced commits ONE balanced pair (the shape Agent B's payout
	// workflow posts: debit 300 / credit 302-or-303). Convenience wrapper
	// over Post with the journal id derived deterministically from
	// (ref_type, ref_id).
	PostBalanced(ctx context.Context, p BalancedPosting) error
	// Balance returns (credits − debits) of one account for one beneficiary
	// ("" = house side), in kobo.
	Balance(ctx context.Context, tenantID uuid.UUID, accountCode int, beneficiaryID string) (int64, error)
	// Entries lists ledger rows of a tenant in [from,to] (nil = unbounded),
	// oldest first.
	Entries(ctx context.Context, tenantID uuid.UUID, from, to *time.Time) ([]LedgerEntry, error)
}

// BalancedPosting is one balanced double-entry pair (one journal) — the
// compact posting shape Agent B's payout workflow uses (contract §3/§4:
// payout = debit 300 commission_payable / credit 302 agent_float or
// 303 house_clearing; ref_type RefTypePayout, ref_id = payout id).
//
// BeneficiaryID is an EXTENSION beyond Agent B's placeholder shape: the
// per-beneficiary balance (account 300 credits − debits) only decreases on
// payout when the debit carries the beneficiary — payout postings MUST set
// it (flagged to Agent B; the house-side entry of any pair uses "").
type BalancedPosting struct {
	TenantID      uuid.UUID `json:"tenant_id"`
	DebitAccount  int       `json:"debit_account"`
	CreditAccount int       `json:"credit_account"`
	AmountNGN     int64     `json:"amount_ngn"` // kobo
	RefType       string    `json:"ref_type"`
	RefID         string    `json:"ref_id"`
	BeneficiaryID string    `json:"beneficiary_id,omitempty"`
}

// PostgresLedger is the contract §3 Postgres implementation of Ledger on
// top of the RLS'd commission_ledger table (internal/store/referrals.go).
type PostgresLedger struct {
	Store *store.Store
}

// NewPostgresLedger wires the Postgres Ledger impl (main.go swap point for
// the TigerBeetle adapter — see the Ledger seam comment above).
func NewPostgresLedger(st *store.Store) *PostgresLedger { return &PostgresLedger{Store: st} }

// validateJournal enforces the contract §3 invariants before any row hits
// the database: shared tenant, valid account codes, one-sided entries and a
// balanced journal.
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
		if e.AccountCode < 300 || e.AccountCode > 303 {
			return fmt.Errorf("%w: unknown account_code %d", ErrUnbalancedJournal, e.AccountCode)
		}
		if (e.DebitNGN > 0) == (e.CreditNGN > 0) {
			return fmt.Errorf("%w: entry must carry exactly one side (account %d)", ErrUnbalancedJournal, e.AccountCode)
		}
		if e.DebitNGN < 0 || e.CreditNGN < 0 {
			return fmt.Errorf("%w: negative amounts", ErrUnbalancedJournal)
		}
		debits += e.DebitNGN
		credits += e.CreditNGN
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

// PostBalanced implements Ledger: expands the compact pair into two entries
// (debit carries the beneficiary, credit settles to the float/clearing
// account) and posts them with a deterministic journal id, so a retried
// payout activity is a ledger no-op (idempotency key
// (ref_type, ref_id, account_code), contract §3).
func (l *PostgresLedger) PostBalanced(ctx context.Context, p BalancedPosting) error {
	if p.AmountNGN <= 0 {
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
	debit.DebitNGN = p.AmountNGN
	credit := base
	credit.AccountCode = p.CreditAccount
	credit.BeneficiaryID = p.BeneficiaryID
	credit.CreditNGN = p.AmountNGN
	return l.Post(ctx, p.TenantID, journalID, []LedgerEntry{debit, credit})
}

// Balance returns (credits − debits) in kobo for one account + beneficiary.
func (l *PostgresLedger) Balance(ctx context.Context, tenantID uuid.UUID, accountCode int, beneficiaryID string) (int64, error) {
	return l.Store.LedgerBalance(ctx, tenantID, accountCode, beneficiaryID)
}

// Entries lists ledger rows of a tenant in [from,to], oldest first.
func (l *PostgresLedger) Entries(ctx context.Context, tenantID uuid.UUID, from, to *time.Time) ([]LedgerEntry, error) {
	return l.Store.ListLedgerEntries(ctx, tenantID, from, to)
}

// ---------------------------------------------------------------------------
// Journal builders (shared by Agent A's verify flow and Agent B's payout
// workflow — the balanced-pair shapes are contract §3).
// ---------------------------------------------------------------------------

// AccrualJournalID derives the deterministic journal id of one (referral,
// rule) accrual: replays of the same verify produce the same journal.
func AccrualJournalID(referralID, ruleID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID,
		[]byte("opendesk:commission_accrual:"+referralID.String()+":"+ruleID.String()))
}

// AccrualRefID is the idempotency anchor of one (referral, rule) accrual
// posting (contract §3: idempotent on (ref_type, ref_id, account_code)).
func AccrualRefID(referralID, ruleID uuid.UUID) string {
	return referralID.String() + ":" + ruleID.String()
}

// NewAccrualPair builds the balanced commission accrual pair for one award
// (contract §3): DEBIT 301 commission_expense (house side) / CREDIT 300
// commission_payable (beneficiary side).
func NewAccrualPair(tenantID uuid.UUID, ref Referral, a Award) []LedgerEntry {
	journalID := AccrualJournalID(ref.ID, a.RuleID)
	refID := AccrualRefID(ref.ID, a.RuleID)
	base := LedgerEntry{TenantID: tenantID, JournalID: journalID, RefType: RefTypeCommissionAccrual, RefID: refID}
	debit := base // house expense
	debit.AccountCode = AccountCommissionExpense
	debit.BeneficiaryID = ""
	debit.DebitNGN = a.AmountKobo
	credit := base // liability towards the beneficiary
	credit.AccountCode = AccountCommissionPayable
	credit.BeneficiaryID = a.BeneficiaryID
	credit.CreditNGN = a.AmountKobo
	return []LedgerEntry{debit, credit}
}

// NewPayoutPair builds the balanced payout settlement pair (contract §3:
// debit 300 commission_payable / credit 302 agent_float-or-303
// house_clearing) with ref_type RefTypePayout and ref_id = payout id — the
// same posting PostBalanced produces for a payout BalancedPosting; kept as
// the explicit-pair form for callers that need the journal id up front.
func NewPayoutPair(tenantID, payoutID uuid.UUID, beneficiaryID string, amountKobo int64, creditAccount int) []LedgerEntry {
	journalID := uuid.NewSHA1(uuid.NameSpaceOID,
		[]byte("opendesk:balanced:"+RefTypePayout+":"+payoutID.String()))
	base := LedgerEntry{TenantID: tenantID, JournalID: journalID, RefType: RefTypePayout, RefID: payoutID.String()}
	debit := base // reduce the liability
	debit.AccountCode = AccountCommissionPayable
	debit.BeneficiaryID = beneficiaryID
	debit.DebitNGN = amountKobo
	credit := base // settle via agent float (302) or house clearing (303)
	credit.AccountCode = creditAccount
	credit.BeneficiaryID = beneficiaryID
	credit.CreditNGN = amountKobo
	return []LedgerEntry{debit, credit}
}
