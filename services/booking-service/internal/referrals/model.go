// Package referrals implements SPEC-W14 Agent A: the referral & commission
// domain of the CAC app — the Referral entity (contract §1) with one-open-
// per-(tenant,referee_phone) dedupe, tenant-editable commission rules (§2),
// the double-entry commission ledger with the TigerBeetle adapter seam (§3),
// the shared Payout type (§4, store/activities owned by Agent B) and the
// verify flow that fires rules → balanced postings → referral verified,
// emitting FunnelEvent hooks on cac.events and converting the referee's
// lead via the Wave-13 leads service (§6).
package referrals

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
)

// ---------------------------------------------------------------------------
// Shared row types (Agent B codes against these — SPEC-W14 §Delivery).
// The canonical definitions live in internal/store/referrals.go (same
// pattern as store.Lead); the aliases below are the referrals-package API.
// ---------------------------------------------------------------------------

// Referral is the contract §1 entity.
type Referral = store.Referral

// CommissionRule is the contract §2 tenant-editable rule.
type CommissionRule = store.CommissionRule

// LedgerEntry is one contract §3 double-entry row.
type LedgerEntry = store.LedgerEntry

// Payout is the contract §4 payout row (payout store + Temporal activities
// are Agent B's internal/referrals/payouts.go).
type Payout = store.Payout

// Referral statuses (contract §1 enum).
const (
	StatusPending   = "pending"
	StatusVerified  = "verified"
	StatusConverted = "converted"
	StatusPaid      = "paid"
	StatusRejected  = "rejected"
)

// Referrer types (contract §1 enum).
const (
	ReferrerContact = "contact"
	ReferrerAgent   = "agent"
	ReferrerStaff   = "staff"
)

var validReferrerTypes = map[string]bool{
	ReferrerContact: true, ReferrerAgent: true, ReferrerStaff: true,
}

// Commission rule triggers (contract §2 enum).
const (
	TriggerSignupVerified = "signup_verified"
	TriggerFirstBooking   = "first_booking"
	TriggerFirstTxn       = "first_txn"
	TriggerSale           = "sale"
)

var validTriggers = map[string]bool{
	TriggerSignupVerified: true, TriggerFirstBooking: true,
	TriggerFirstTxn: true, TriggerSale: true,
}

// Commission rule beneficiaries (contract §2 enum).
const (
	BeneficiaryReferrer = "referrer"
	BeneficiaryAgent    = "agent"
	BeneficiaryStaff    = "staff"
)

var validBeneficiaries = map[string]bool{
	BeneficiaryReferrer: true, BeneficiaryAgent: true, BeneficiaryStaff: true,
}

// Amount types (contract §2 enum).
const (
	AmountFlat    = "flat"
	AmountPercent = "percent"
)

// Commission ledger account codes (contract §3) — TigerBeetle-compatible
// chart of accounts (see ledger.go for the TB adapter seam).
const (
	// AccountCommissionPayable (300) is the liability owed to beneficiaries.
	AccountCommissionPayable = 300
	// AccountCommissionExpense (301) is the house expense side of an accrual.
	AccountCommissionExpense = 301
	// AccountAgentFloat (302) is the agent-float settlement account (payouts).
	AccountAgentFloat = 302
	// AccountHouseClearing (303) is the house clearing account (payouts).
	AccountHouseClearing = 303
)

// Ledger ref types (ref_type of a posting; ref_id carries the anchor).
const (
	// RefTypeCommissionAccrual anchors the verify-time accrual pair;
	// ref_id = "<referral_id>:<rule_id>" so every (referral, rule) pair
	// posts exactly once (contract §3 idempotency).
	RefTypeCommissionAccrual = "commission_accrual"
	// RefTypePayout anchors the payout settlement pair (Agent B's payout
	// workflow); ref_id = "<payout_id>".
	RefTypePayout = "commission_payout"
)

// Payout statuses + providers (contract §4 enums). Names match Agent B's
// payout_store call sites exactly (B deletes its placeholder block once A's
// exports land — SPEC-W14 §Agent B COORDINATION). The "mock" provider is
// Agent B's PAYOUT_MOCK impl detail and stays in B's file.
const (
	PayoutStatusQueued     = "queued"
	PayoutStatusProcessing = "processing"
	PayoutStatusPaid       = "paid"
	PayoutStatusFailed     = "failed"

	ProviderPaystack    = "paystack"
	ProviderFlutterwave = "flutterwave"
)

// ErrInvalidInput marks deterministic validation failures (→ HTTP 400).
var ErrInvalidInput = errors.New("invalid referral input")

// ErrInvalidTransition marks an illegal status transition (→ HTTP 409).
var ErrInvalidTransition = errors.New("invalid referral status transition")

// ErrUnbalancedJournal marks a journal whose debits != credits (contract §3:
// every posting is a balanced pair) — a programming error, never retried.
var ErrUnbalancedJournal = errors.New("unbalanced journal")

// ValidateReferral checks the contract §1 field set.
func ValidateReferral(r *Referral) error {
	if r.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if !validReferrerTypes[r.ReferrerType] {
		return fmt.Errorf("%w: referrer_type %q (want contact|agent|staff)", ErrInvalidInput, r.ReferrerType)
	}
	if strings.TrimSpace(r.ReferrerID) == "" {
		return fmt.Errorf("%w: referrer_id is required", ErrInvalidInput)
	}
	if strings.TrimSpace(r.RefereePhone) == "" {
		return fmt.Errorf("%w: referee_phone is required", ErrInvalidInput)
	}
	return nil
}

// ValidateRule checks the contract §2 field set of a tenant-editable rule.
func ValidateRule(r *CommissionRule) error {
	if r.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if !validTriggers[r.Trigger] {
		return fmt.Errorf("%w: trigger %q (want signup_verified|first_booking|first_txn|sale)", ErrInvalidInput, r.Trigger)
	}
	if !validBeneficiaries[r.Beneficiary] {
		return fmt.Errorf("%w: beneficiary %q (want referrer|agent|staff)", ErrInvalidInput, r.Beneficiary)
	}
	switch r.AmountType {
	case AmountFlat:
		if r.AmountNGN <= 0 {
			return fmt.Errorf("%w: amount_ngn (kobo) must be > 0 for flat rules", ErrInvalidInput)
		}
	case AmountPercent:
		if r.Bps <= 0 || r.Bps > 1_000_000 {
			return fmt.Errorf("%w: bps must be in 1..1000000 for percent rules", ErrInvalidInput)
		}
	default:
		return fmt.Errorf("%w: amount_type %q (want flat|percent)", ErrInvalidInput, r.AmountType)
	}
	if r.CapNGN != nil && *r.CapNGN <= 0 {
		return fmt.Errorf("%w: cap_ngn must be > 0 when set", ErrInvalidInput)
	}
	return nil
}

// IsRevenueTrigger reports whether a verify trigger also marks the referral
// converted (the referee produced revenue): first_booking | first_txn | sale
// (contract §1 status machine pending → verified → converted).
func IsRevenueTrigger(trigger string) bool {
	switch trigger {
	case TriggerFirstBooking, TriggerFirstTxn, TriggerSale:
		return true
	}
	return false
}
