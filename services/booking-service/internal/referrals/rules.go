package referrals

import (
	"sort"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Commission rules engine (contract §2): a PURE function — no I/O, no clock,
// deterministic. Table-driven tests live in rules_test.go.
// ---------------------------------------------------------------------------

// Award is the outcome of one fired rule: the resolved beneficiary and the
// commission amount in kobo (integer math only — no floats).
type Award struct {
	RuleID          uuid.UUID `json:"rule_id"`
	RuleName        string    `json:"rule_name"`
	BeneficiaryType string    `json:"beneficiary_type"` // referrer|agent|staff (the rule's beneficiary)
	BeneficiaryID   string    `json:"beneficiary_id"`   // resolved from the referral
	AmountKobo      int64     `json:"amount_kobo"`
}

// EvaluateRules fires every ACTIVE rule whose trigger matches, in priority
// order (asc; ties broken by created_at then id — the store already orders
// ListRules that way, and EvaluateRules re-sorts a copy defensively so the
// pure function is total over any input order). Multiple rules may fire;
// each returns its own Award.
//
// Amount math (integer kobo, floor):
//   - flat:    amount = rule.AmountNGN
//   - percent: amount = baseKobo * rule.Bps / 10000
//   - cap:     amount = min(amount, rule.CapNGN) when CapNGN is set
//
// Awards computing to <= 0 are skipped (e.g. a percent rule on a
// signup_verified verify whose baseKobo is 0).
//
// Beneficiary resolution: the contract rule has no beneficiary_id column,
// so the beneficiary is resolved FROM THE REFERRAL:
//   - referrer → the referral's referrer (any referrer_type)
//   - agent    → the referrer, only when referrer_type == "agent"
//   - staff    → the referrer, only when referrer_type == "staff"
//
// (a rule targeting agents/staff simply does not fire for a contact
// referral — documented in docs/referrals-commissions.md).
func EvaluateRules(rules []CommissionRule, trigger string, baseKobo int64, ref Referral) []Award {
	sorted := make([]CommissionRule, 0, len(rules))
	for _, r := range rules {
		if r.Active && r.Trigger == trigger {
			sorted = append(sorted, r)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Priority != sorted[j].Priority {
			return sorted[i].Priority < sorted[j].Priority
		}
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
		}
		return sorted[i].ID.String() < sorted[j].ID.String()
	})

	awards := []Award{}
	for _, r := range sorted {
		beneficiaryID, ok := resolveBeneficiary(r.Beneficiary, ref)
		if !ok {
			continue
		}
		amount := ruleAmountKobo(r, baseKobo)
		if amount <= 0 {
			continue
		}
		awards = append(awards, Award{
			RuleID:          r.ID,
			RuleName:        r.Name,
			BeneficiaryType: r.Beneficiary,
			BeneficiaryID:   beneficiaryID,
			AmountKobo:      amount,
		})
	}
	return awards
}

// ruleAmountKobo computes the (capped) commission of one rule in kobo with
// pure integer arithmetic: percent = floor(baseKobo * bps / 10000).
func ruleAmountKobo(r CommissionRule, baseKobo int64) int64 {
	var amount int64
	switch r.AmountType {
	case AmountFlat:
		amount = r.AmountNGN
	case AmountPercent:
		if baseKobo < 0 {
			baseKobo = 0
		}
		amount = baseKobo * int64(r.Bps) / 10_000
	}
	if r.CapNGN != nil && amount > *r.CapNGN {
		amount = *r.CapNGN
	}
	return amount
}

// resolveBeneficiary maps the rule's beneficiary kind to a concrete
// beneficiary id using the referral. ok=false = rule does not fire for this
// referral (agent/staff rule on a contact referrer).
func resolveBeneficiary(beneficiary string, ref Referral) (id string, ok bool) {
	switch beneficiary {
	case BeneficiaryReferrer:
		return ref.ReferrerID, true
	case BeneficiaryAgent:
		if ref.ReferrerType == ReferrerAgent {
			return ref.ReferrerID, true
		}
	case BeneficiaryStaff:
		if ref.ReferrerType == ReferrerStaff {
			return ref.ReferrerID, true
		}
	}
	return "", false
}
