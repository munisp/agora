// Package loyalty implements SPEC-W19 Agent C: the LOYALTY & WALLET
// enterprise app (points, tiers, redemption) — app_id "loyalty-wallet"
// (integrator wires the appgate entitlement with that id).
//
// Model: tenant-editable programs (earn_rules / tiers as jsonb, cap_per_day)
// and per-contact wallets (balance / lifetime counters / tier, PK
// (tenant_id, contact_id)). Point movements are double-entry ledger
// journals on the package-local loyalty_ledger table — a MIRROR of the W14
// referrals.Ledger pattern (internal/referrals/ledger.go): referrals'
// PostgresLedger cannot be reused directly because its validateJournal and
// the commission_ledger CHECK constraint hard-pin account codes 300..303
// (money, kobo), while loyalty posts codes 400/401 (points). The interface
// shape (Post / PostBalanced / Balance / Entries) and the idempotency
// anchor UNIQUE (tenant_id, ref_type, ref_id, account_code) are mirrored
// verbatim; referrals itself is untouched.
//
// Account codes (points, not kobo):
//   - 400 loyalty_points_issued   — liability towards the contact; credits
//     increase outstanding points, beneficiary_id = contact_id.
//   - 401 loyalty_points_redeemed — house-side flow account ("" beneficiary).
//
// Postings: accrue = DEBIT 401 (house) / CREDIT 400 (contact); redeem =
// DEBIT 400 (contact) / CREDIT 401 (house). A wallet's spendable balance is
// therefore Balance(400, contact) = credits − debits; the wallets.balance
// column is the in-tx maintained cache of that sum.
//
// Config envs (integrator wires; apps functional with zero config):
//   - LOYALTY_EVENTS_TOPIC — CloudEvents topic for points lifecycle
//     (default "opendesk.loyalty.events.v1"; empty disables emission).
//   - USAGE_EVENTS_TOPIC — shared usage-metering topic (default
//     "opendesk.usage.events"; empty disables points_redeemed metering).
//   - DATABASE_URL — DialStore fallback pool (same idiom as W16 devices).
//
// Permissions (integrator wires via Deps.Require): writes
// (POST/PATCH programs, POST accrue/redeem) = manage_bookings; reads
// (programs list, wallet, leaderboard) = view_analytics.
package loyalty

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Earn-rule events (SPEC-W19 Agent C enum).
const (
	EventBookingCompleted  = "booking_completed"
	EventFirstTxn          = "first_txn"
	EventReferralConverted = "referral_converted"
)

var validEvents = map[string]bool{
	EventBookingCompleted: true, EventFirstTxn: true, EventReferralConverted: true,
}

// Ledger account codes (points).
const (
	// AccountPointsIssued (400) is the liability towards the contact
	// (credits = points issued, debits = points redeemed).
	AccountPointsIssued = 400
	// AccountPointsRedeemed (401) is the house-side flow account balancing
	// every accrual (debit) and redemption (credit).
	AccountPointsRedeemed = 401
)

// Ledger ref types (idempotency anchors).
const (
	// RefTypeAccrual anchors POST /accrue journals; ref_id = event:ref_id so
	// a replayed accrual is a ledger no-op (SPEC: idempotent on ref_id+event).
	RefTypeAccrual = "loyalty_accrual"
	// RefTypeRedeem anchors POST /redeem journals; ref_id is the
	// caller-supplied ref_id (recommended) or a freshly minted redemption id.
	RefTypeRedeem = "loyalty_redeem"
)

// CloudEvents (contract §5): points lifecycle on LOYALTY_EVENTS_TOPIC
// (default opendesk.loyalty.events.v1).
const (
	EventTypePointsIssued   = "com.opendesk.loyalty.PointsIssued"
	EventTypePointsRedeemed = "com.opendesk.loyalty.PointsRedeemed"
)

// Sentinel errors mapped by the handlers.
var (
	// ErrInvalidInput marks deterministic validation failures (400).
	ErrInvalidInput = errors.New("invalid loyalty input")
	// ErrNotFound is a missing program / wallet (404).
	ErrNotFound = errors.New("not found")
	// ErrInsufficientPoints rejects a redemption above the spendable
	// balance (409).
	ErrInsufficientPoints = errors.New("insufficient loyalty points")
	// ErrUnbalancedJournal rejects a journal that violates the double-entry
	// invariants (mirrors referrals.ErrUnbalancedJournal).
	ErrUnbalancedJournal = errors.New("unbalanced loyalty journal")
	// ErrNoActiveProgram rejects accruals when the tenant has no active
	// program to resolve earn rules from (400).
	ErrNoActiveProgram = errors.New("no active loyalty program")
)

// EarnRule is one element of programs.earn_rules: points awarded when the
// named event is accrued.
type EarnRule struct {
	Event  string `json:"event"`
	Points int64  `json:"points"`
}

// Tier is one element of programs.tiers. The wallet tier is the
// highest-min_points tier whose threshold the contact's lifetime_earned
// reaches ("" when none qualifies).
type Tier struct {
	Name      string `json:"name"`
	MinPoints int64  `json:"min_points"`
	Benefits  string `json:"benefits,omitempty"`
}

// Program mirrors booking.loyalty_programs (SPEC-W19 Agent C).
type Program struct {
	ID        uuid.UUID  `json:"program_id"`
	TenantID  uuid.UUID  `json:"tenant_id"`
	Name      string     `json:"name"`
	Active    bool       `json:"active"`
	EarnRules []EarnRule `json:"earn_rules"`
	Tiers     []Tier     `json:"tiers"`
	// CapPerDay caps points one contact can EARN per UTC day across all
	// accruals (0 = uncapped). Over-cap accruals are clamped, not rejected.
	CapPerDay int64     `json:"cap_per_day"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Wallet mirrors booking.loyalty_wallets — PK (tenant_id, contact_id).
type Wallet struct {
	TenantID         uuid.UUID `json:"tenant_id"`
	ContactID        uuid.UUID `json:"contact_id"`
	Balance          int64     `json:"balance"`
	LifetimeEarned   int64     `json:"lifetime_earned"`
	LifetimeRedeemed int64     `json:"lifetime_redeemed"`
	Tier             string    `json:"tier"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// PointsForEvent resolves the earn rule for one event (0 when the program
// does not award that event).
func (p *Program) PointsForEvent(event string) int64 {
	for _, r := range p.EarnRules {
		if r.Event == event {
			return r.Points
		}
	}
	return 0
}

// TierFor computes the tier name for a lifetime_earned total: the
// qualifying tier with the highest min_points; "" when no tier qualifies
// (or the program defines no tiers).
func TierFor(lifetimeEarned int64, tiers []Tier) string {
	best := ""
	var bestMin int64 = -1
	for _, t := range tiers {
		if lifetimeEarned >= t.MinPoints && t.MinPoints >= bestMin && t.Name != "" {
			best = t.Name
			bestMin = t.MinPoints
		}
	}
	return best
}

// maxNameLen bounds the program name column.
const maxNameLen = 200

// ValidateEvent enforces the earn-rule event enum.
func ValidateEvent(event string) error {
	if validEvents[event] {
		return nil
	}
	return fmt.Errorf("%w: event %q (want booking_completed|first_txn|referral_converted)", ErrInvalidInput, event)
}

// ValidateProgram checks a program payload before persistence. Earn rules
// may be empty (program exists but awards nothing yet); duplicate events
// are rejected so PointsForEvent stays unambiguous.
func ValidateProgram(p *Program) error {
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
	if p.CapPerDay < 0 {
		return fmt.Errorf("%w: cap_per_day must be >= 0", ErrInvalidInput)
	}
	seen := map[string]bool{}
	for _, r := range p.EarnRules {
		if err := ValidateEvent(r.Event); err != nil {
			return err
		}
		if r.Points <= 0 {
			return fmt.Errorf("%w: earn rule %q points must be > 0", ErrInvalidInput, r.Event)
		}
		if seen[r.Event] {
			return fmt.Errorf("%w: duplicate earn rule for event %q", ErrInvalidInput, r.Event)
		}
		seen[r.Event] = true
	}
	for _, t := range p.Tiers {
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("%w: tier name is required", ErrInvalidInput)
		}
		if t.MinPoints < 0 {
			return fmt.Errorf("%w: tier %q min_points must be >= 0", ErrInvalidInput, t.Name)
		}
	}
	return nil
}
