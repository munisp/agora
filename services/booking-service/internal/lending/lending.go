// Package lending implements SPEC-W20 Agent C: the LENDING enterprise app
// (micro-loans: products → applications → decision → disbursement →
// repayment) — app_id "lending" (the integrator wires the appgate
// entitlement with that id over the whole /v1/lending route group).
//
// Money rules (SPEC-W20): all amounts are kobo int64; interest is basis
// points int with interest = principal*bps/10000 (integer division);
// outstanding = principal + interest + fee. Repayments are idempotent on
// UNIQUE (tenant_id, loan_id, ref_id) — a replay answers 200 with the same
// body — and overpay is CLAMPED to the outstanding balance (the clamp is
// noted in the response, never recorded). Disbursement is idempotent via
// the application status guard (a replayed disburse returns the existing
// loan account).
//
// Mirrored ledger: movements post double-entry journals on the
// package-local lending_ledger table — a MIRROR of the W14 referrals /
// W19 loyalty ledger idiom (internal/loyalty/ledger.go), instantiated
// package-locally with codes 500 loan_principal_disbursed /
// 501 loan_repayment_received (disjoint from referrals 300-303 and loyalty
// 400-401; referrals and loyalty are NEVER edited). The ledger mirrors
// CASH movement (principal out, repayments in) — not the interest/fee
// schedule; loan_accounts.outstanding_kobo is the schedule-side cache.
//
// Real money movement (TigerBeetle / payments rail) is OUT of scope:
// disbursement emits a com.opendesk.lending.DisbursementIntent CloudEvent
// on the lending events topic as the documented integration point for the
// payments rail.
//
// KYC gate: approving an application requires a KYC check. When the
// integrator configures LENDING_KYC_URL (Deps.KYCURL) the handler calls
// kyc-service (POST {url}/v1/kyc/resolve — the consent-gated BVN/NIN
// resolution endpoint, SPEC-W12) and requires status "verified". When no
// KYC URL is configured the operator must pass an explicit
// {kyc_override: true, reason} which is recorded in the decision event
// payload. Both paths are documented in docs/apps/lending.md.
//
// Scoring: a NAIVE rule-based score 0..100 (NOT a credit-bureau score)
// computed on submit from three signals (weights documented at Score):
// contact tenure (≤30), completed bookings (≤40), prior repaid loans
// (≤30). External sources (contacts/bookings) are read defensively — a
// missing table/column contributes 0, never a 500.
//
// Anti-collision contract (SPEC-W20): this package is SELF-CONTAINED — it
// exposes NewStore/DialStore and RegisterRoutes(r, d, mw...) (mirroring
// internal/workorders); the integrator wires Deps, route mounting, config
// envs and the appgate entitlement flag (app_id "lending"). This package
// touches NO shared files.
//
// Config envs (documented for the integrator — no config code here; every
// one is optional and the app is functional with zero config):
//
//	LENDING_EVENTS_TOPIC — lifecycle + disbursement-intent CloudEvents topic
//	    (default opendesk.lending.events.v1; empty disables events)
//	USAGE_EVENTS_TOPIC   — shared usage-metering topic
//	    (default opendesk.usage.events; empty disables loan_disbursed metering)
//	LENDING_KYC_URL      — kyc-service base URL for the approve gate
//	    (empty = KYC service not wired: approvals then REQUIRE an explicit
//	    kyc_override + reason, recorded in the decision event payload)
//	DATABASE_URL         — DialStore fallback pool (same idiom as W16 devices)
//
// Permissions (integrator wires; recommended shape composed from httpapi's
// require(), applied group-wide via the variadic mw of RegisterRoutes):
// GET/HEAD → require("view_analytics"), everything else →
// require("manage_bookings").
package lending

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Application statuses (SPEC-W20 Agent C state machine).
const (
	StatusDraft       = "draft"
	StatusSubmitted   = "submitted"
	StatusUnderReview = "under_review"
	StatusApproved    = "approved"
	StatusDeclined    = "declined"
	StatusDisbursed   = "disbursed"
	StatusRepaid      = "repaid"
	StatusDefaulted   = "defaulted"
)

// Statuses lists every application status (filters, UI lanes).
var Statuses = []string{
	StatusDraft, StatusSubmitted, StatusUnderReview, StatusApproved,
	StatusDeclined, StatusDisbursed, StatusRepaid, StatusDefaulted,
}

// transitions is the operator-driven PATCH machine (SPEC-W20):
// submitted→under_review→approved|declined, draft→submitted (score is
// (re)computed on submit), and operator-driven default marking
// approved|disbursed→defaulted. approved→disbursed happens ONLY via the
// disburse endpoint; disbursed→repaid ONLY via the repay flow (outstanding
// hits zero). declined/repaid/defaulted are terminal.
var transitions = map[string][]string{
	StatusDraft:       {StatusSubmitted},
	StatusSubmitted:   {StatusUnderReview, StatusDefaulted},
	StatusUnderReview: {StatusApproved, StatusDeclined, StatusDefaulted},
	StatusApproved:    {StatusDefaulted},
	StatusDisbursed:   {StatusDefaulted},
}

// CanTransition reports whether from→to is a legal PATCH edge.
func CanTransition(from, to string) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// ValidateTransition returns ErrInvalidTransition when from→to is illegal.
// disbursed/repaid targets are always rejected here — they are owned by the
// disburse/repay flows, not the PATCH machine.
func ValidateTransition(from, to string) error {
	if !validStatus(from) || !validStatus(to) {
		return fmt.Errorf("%w: %q → %q (unknown status)", ErrInvalidTransition, from, to)
	}
	if to == StatusDisbursed || to == StatusRepaid {
		return fmt.Errorf("%w: %s → %s (use the disburse/repay endpoints)", ErrInvalidTransition, from, to)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, from, to)
	}
	return nil
}

func validStatus(s string) bool {
	for _, st := range Statuses {
		if st == s {
			return true
		}
	}
	return false
}

// ValidateStatus enforces the status enum (filters).
func ValidateStatus(s string) error {
	if !validStatus(s) {
		return fmt.Errorf("%w: status %q (want %s)", ErrInvalidInput, s, strings.Join(Statuses, "|"))
	}
	return nil
}

// Loan account statuses.
const (
	LoanActive    = "active"
	LoanRepaid    = "repaid"
	LoanDefaulted = "defaulted"
)

// Ledger account codes (kobo). Disjoint from referrals 300-303 and loyalty
// 400-401 (SPEC-W20 Agent C).
const (
	// AccountPrincipalDisbursed (500) tracks the borrower's principal:
	// CREDIT at disbursement (beneficiary = contact_id), DEBIT as
	// repayments arrive. Balance(500, contact) = principal − repayments
	// (cash-movement view; interest/fee live on the account schedule).
	AccountPrincipalDisbursed = 500
	// AccountRepaymentReceived (501) is the house-side flow account
	// ("" beneficiary) balancing every disbursement (debit) and repayment
	// (credit).
	AccountRepaymentReceived = 501
)

// Ledger ref types (idempotency anchors).
const (
	// RefTypeDisbursement anchors disbursement journals; ref_id =
	// application id — a replayed disburse is a ledger no-op.
	RefTypeDisbursement = "loan_disbursement"
	// RefTypeRepayment anchors repayment journals; ref_id = the
	// caller-supplied repayment ref_id.
	RefTypeRepayment = "loan_repayment"
)

// Sentinel errors mapped by the handlers.
var (
	// ErrInvalidInput marks deterministic validation failures (400).
	ErrInvalidInput = errors.New("invalid lending input")
	// ErrNotFound is a missing product / application / loan (404).
	ErrNotFound = errors.New("not found")
	// ErrInvalidTransition marks state-machine violations (409).
	ErrInvalidTransition = errors.New("invalid application status transition")
	// ErrKYCRequired rejects an approval that failed (or skipped) the KYC
	// gate (409).
	ErrKYCRequired = errors.New("kyc check required to approve")
	// ErrForbidden marks an authenticated-but-not-allowed operation (403):
	// kyc_override by an operator without a LENDING_KYC_OVERRIDE_ROLES role,
	// or a disbursement by the same operator who approved (SPEC-W44
	// W-B/S1-F7-07 separation of duties).
	ErrForbidden = errors.New("forbidden")
	// ErrUnbalancedJournal rejects a journal violating the double-entry
	// invariants (mirrors loyalty.ErrUnbalancedJournal).
	ErrUnbalancedJournal = errors.New("unbalanced lending journal")
)

// ---------------------------------------------------------------------------
// Products
// ---------------------------------------------------------------------------

// maxNameLen bounds the product name column.
const maxNameLen = 200

// Product mirrors booking.loan_products (SPEC-W20 Agent C).
type Product struct {
	ID               uuid.UUID `json:"id"`
	TenantID         uuid.UUID `json:"tenant_id"`
	Name             string    `json:"name"`
	Active           bool      `json:"active"`
	PrincipalMinKobo int64     `json:"principal_min_kobo"`
	PrincipalMaxKobo int64     `json:"principal_max_kobo"`
	TermDays         int       `json:"term_days"`
	InterestBps      int       `json:"interest_bps"`
	FeeFlatKobo      int64     `json:"fee_flat_kobo"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Validate checks a product payload before persistence.
func (p *Product) Validate() error {
	if p.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if len(p.Name) > maxNameLen {
		return fmt.Errorf("%w: name exceeds %d bytes", ErrInvalidInput, maxNameLen)
	}
	if p.PrincipalMinKobo <= 0 {
		return fmt.Errorf("%w: principal_min_kobo must be > 0", ErrInvalidInput)
	}
	if p.PrincipalMaxKobo < p.PrincipalMinKobo {
		return fmt.Errorf("%w: principal_max_kobo must be >= principal_min_kobo", ErrInvalidInput)
	}
	if p.TermDays <= 0 {
		return fmt.Errorf("%w: term_days must be > 0", ErrInvalidInput)
	}
	if p.InterestBps < 0 || p.InterestBps > 10000 {
		return fmt.Errorf("%w: interest_bps must be in [0,10000]", ErrInvalidInput)
	}
	if p.FeeFlatKobo < 0 {
		return fmt.Errorf("%w: fee_flat_kobo must be >= 0", ErrInvalidInput)
	}
	return nil
}

// InterestFor computes the flat interest for a principal under this
// product: principal*bps/10000 (integer division — SPEC-W20 money rule).
func (p *Product) InterestFor(principalKobo int64) int64 {
	return principalKobo * int64(p.InterestBps) / 10000
}

// ---------------------------------------------------------------------------
// Applications
// ---------------------------------------------------------------------------

// Application mirrors booking.loan_applications.
type Application struct {
	ID            uuid.UUID  `json:"id"`
	TenantID      uuid.UUID  `json:"tenant_id"`
	ContactID     uuid.UUID  `json:"contact_id"`
	ProductID     uuid.UUID  `json:"product_id"`
	PrincipalKobo int64      `json:"principal_kobo"`
	Status        string     `json:"status"`
	Score         *int       `json:"score"` // naive 0..100, computed on submit
	DeclineReason *string    `json:"decline_reason"`
	DecidedBy     *string    `json:"decided_by"`
	DecidedAt     *time.Time `json:"decided_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Validate checks the minimal field set required for persistence.
func (a *Application) Validate() error {
	if a.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if a.ContactID == uuid.Nil {
		return fmt.Errorf("%w: contact_id is required", ErrInvalidInput)
	}
	if a.ProductID == uuid.Nil {
		return fmt.Errorf("%w: product_id is required", ErrInvalidInput)
	}
	if a.PrincipalKobo <= 0 {
		return fmt.Errorf("%w: principal_kobo must be > 0", ErrInvalidInput)
	}
	if !validStatus(a.Status) {
		return fmt.Errorf("%w: unknown status %q", ErrInvalidInput, a.Status)
	}
	if a.Score != nil && (*a.Score < 0 || *a.Score > 100) {
		return fmt.Errorf("%w: score must be in [0,100]", ErrInvalidInput)
	}
	return nil
}

// ValidatePrincipalAgainst enforces the product min/max band (400 at the
// API).
func ValidatePrincipalAgainst(p Product, principalKobo int64) error {
	if principalKobo < p.PrincipalMinKobo || principalKobo > p.PrincipalMaxKobo {
		return fmt.Errorf("%w: principal_kobo %d outside product band [%d,%d]",
			ErrInvalidInput, principalKobo, p.PrincipalMinKobo, p.PrincipalMaxKobo)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Loan accounts & repayments
// ---------------------------------------------------------------------------

// LoanAccount mirrors booking.loan_accounts. OutstandingKobo =
// principal + interest + fee − Σ(applied repayments); the schedule-side
// cache (the ledger mirrors cash movement only — see the package doc).
type LoanAccount struct {
	ID              uuid.UUID  `json:"id"`
	TenantID        uuid.UUID  `json:"tenant_id"`
	ApplicationID   uuid.UUID  `json:"application_id"`
	ContactID       uuid.UUID  `json:"contact_id"`
	PrincipalKobo   int64      `json:"principal_kobo"`
	InterestKobo    int64      `json:"interest_kobo"`
	FeeKobo         int64      `json:"fee_kobo"`
	OutstandingKobo int64      `json:"outstanding_kobo"`
	DisbursedAt     time.Time  `json:"disbursed_at"`
	DueAt           time.Time  `json:"due_at"`
	Status          string     `json:"status"` // active|repaid|defaulted
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
}

// TotalKobo is the full schedule amount (principal + interest + fee).
func (l *LoanAccount) TotalKobo() int64 { return l.PrincipalKobo + l.InterestKobo + l.FeeKobo }

// DaysPastDue reports whole days past due_at (0 when not past due).
func (l *LoanAccount) DaysPastDue(now time.Time) int {
	if !l.DueAt.Before(now) {
		return 0
	}
	return int(now.Sub(l.DueAt).Hours() / 24)
}

// Repayment mirrors booking.repayments — amount_kobo is the APPLIED
// (clamped) amount, idempotent on UNIQUE (tenant_id, loan_id, ref_id).
type Repayment struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenant_id"`
	LoanID     uuid.UUID `json:"loan_id"`
	AmountKobo int64     `json:"amount_kobo"`
	RefID      string    `json:"ref_id"`
	PaidAt     time.Time `json:"paid_at"`
}

// maxRefIDLen bounds the caller idempotency key.
const maxRefIDLen = 128

// ValidateRepaymentInput checks a repay request before the store tx.
func ValidateRepaymentInput(amountKobo int64, refID string) error {
	if amountKobo <= 0 {
		return fmt.Errorf("%w: amount_kobo must be > 0", ErrInvalidInput)
	}
	refID = strings.TrimSpace(refID)
	if refID == "" {
		return fmt.Errorf("%w: ref_id is required (idempotency key)", ErrInvalidInput)
	}
	if len(refID) > maxRefIDLen {
		return fmt.Errorf("%w: ref_id exceeds %d bytes", ErrInvalidInput, maxRefIDLen)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Naive scoring (NOT a credit bureau — see the package doc)
// ---------------------------------------------------------------------------

// Score weights (documented per SPEC-W20 "weights documented in code"):
//
//   - Tenure: +3 points per started 30-day month since the contact's
//     earliest known activity (first booking; contacts.created_at when a
//     schema carries it), capped at 30 (≈10 months).
//   - Completed bookings: +4 per completed booking, capped at 40
//     (10 bookings).
//   - Prior repaid loans: +10 per repaid loan application of this contact,
//     capped at 30 (3 loans).
//
// Maximum score is therefore exactly 100. The signals are intentionally
// crude tenancy/behaviour counters — an honest starting point for
// operator review, never an automated credit decision.
const (
	ScoreMaxTenure           = 30
	ScoreTenurePerMonth      = 3
	ScoreMaxBookings         = 40
	ScorePerCompletedBooking = 4
	ScoreMaxRepaidLoans      = 30
	ScorePerRepaidLoan       = 10
)

// ScoreSignals carries the raw inputs of the naive score.
type ScoreSignals struct {
	TenureDays        int `json:"tenure_days"`
	CompletedBookings int `json:"completed_bookings"`
	RepaidLoans       int `json:"repaid_loans"`
}

// Score computes the naive 0..100 rule-based score from the signals.
func Score(sig ScoreSignals) int {
	if sig.TenureDays < 0 {
		sig.TenureDays = 0
	}
	tenure := (sig.TenureDays / 30) * ScoreTenurePerMonth
	if tenure > ScoreMaxTenure {
		tenure = ScoreMaxTenure
	}
	bookings := sig.CompletedBookings * ScorePerCompletedBooking
	if bookings > ScoreMaxBookings {
		bookings = ScoreMaxBookings
	}
	repaid := sig.RepaidLoans * ScorePerRepaidLoan
	if repaid > ScoreMaxRepaidLoans {
		repaid = ScoreMaxRepaidLoans
	}
	total := tenure + bookings + repaid
	if total > 100 {
		total = 100
	}
	return total
}

// ---------------------------------------------------------------------------
// Portfolio
// ---------------------------------------------------------------------------

// Portfolio is the GET /v1/lending/portfolio aggregate. PAR30 (Portfolio
// at Risk 30) = outstanding of ACTIVE loans >30 days past due_at /
// total outstanding of active loans (nil when there is no outstanding —
// an honest empty state, not 0%).
type Portfolio struct {
	TotalOutstandingKobo int64     `json:"total_outstanding_kobo"`
	ActiveCount          int       `json:"active_count"`
	RepaidCount          int       `json:"repaid_count"`
	DefaultedCount       int       `json:"defaulted_count"`
	PAR30                *float64  `json:"par30"` // 0..1, null when total outstanding == 0
	PAR30OutstandingKobo int64     `json:"par30_outstanding_kobo"`
	ComputedAt           time.Time `json:"computed_at"`
}
