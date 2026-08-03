package referrals

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// SPEC-W14 Agent A: table-driven tests of the PURE rules engine (contract
// §2) — percent+bps integer math (kobo, no float), cap, priority order,
// inactive skip, beneficiary resolution. No DB.

func i64(v int64) *int64 { return &v }

func mkRule(name, trigger, beneficiary, amountType string, amountNGN int64, bps int, cap *int64, active bool, priority int) CommissionRule {
	return CommissionRule{
		ID:          uuid.New(),
		TenantID:    uuid.New(),
		Name:        name,
		Trigger:     trigger,
		Beneficiary: beneficiary,
		AmountType:  amountType,
		AmountNGN:   amountNGN,
		Bps:         bps,
		CapNGN:      cap,
		Active:      active,
		Priority:    priority,
		CreatedAt:   time.Now(),
	}
}

func mkReferral(referrerType, referrerID string) Referral {
	return Referral{
		ID:           uuid.New(),
		TenantID:     uuid.New(),
		ReferrerType: referrerType,
		ReferrerID:   referrerID,
		RefereePhone: "+2348011111111",
		Status:       StatusPending,
	}
}

func TestEvaluateRules(t *testing.T) {
	contactRef := mkReferral(ReferrerContact, "contact-1")
	agentRef := mkReferral(ReferrerAgent, "agent-9")

	tests := []struct {
		name      string
		rules     []CommissionRule
		trigger   string
		baseKobo  int64
		ref       Referral
		wantIDs   []string // expected beneficiary ids in firing order
		wantKobo  []int64  // expected amounts in firing order
		wantRules []string // expected rule names in firing order
	}{
		{
			name: "flat rule fires with exact amount",
			rules: []CommissionRule{
				mkRule("signup500", TriggerSignupVerified, BeneficiaryReferrer, AmountFlat, 50000, 0, nil, true, 1),
			},
			trigger:   TriggerSignupVerified,
			ref:       contactRef,
			wantIDs:   []string{"contact-1"},
			wantKobo:  []int64{50000},
			wantRules: []string{"signup500"},
		},
		{
			name: "percent bps integer math floors (123456 kobo * 250bps = 3086.4 → 3086)",
			rules: []CommissionRule{
				mkRule("pct250", TriggerFirstTxn, BeneficiaryReferrer, AmountPercent, 0, 250, nil, true, 1),
			},
			trigger:  TriggerFirstTxn,
			baseKobo: 123456,
			ref:      contactRef,
			wantIDs:  []string{"contact-1"},
			wantKobo: []int64{3086},
		},
		{
			name: "percent capped at cap_ngn (5% of 2,000,000 = 100,000 → cap 60,000)",
			rules: []CommissionRule{
				mkRule("pct500capped", TriggerFirstBooking, BeneficiaryReferrer, AmountPercent, 0, 500, i64(60000), true, 1),
			},
			trigger:  TriggerFirstBooking,
			baseKobo: 2_000_000,
			ref:      contactRef,
			wantIDs:  []string{"contact-1"},
			wantKobo: []int64{60000},
		},
		{
			name: "flat capped",
			rules: []CommissionRule{
				mkRule("flatcap", TriggerSale, BeneficiaryReferrer, AmountFlat, 90000, 0, i64(55000), true, 1),
			},
			trigger:  TriggerSale,
			baseKobo: 999_999_999,
			ref:      contactRef,
			wantKobo: []int64{55000},
		},
		{
			name: "priority order asc (multiple rules fire, highest priority first)",
			rules: []CommissionRule{
				mkRule("low", TriggerFirstTxn, BeneficiaryReferrer, AmountFlat, 100, 0, nil, true, 30),
				mkRule("high", TriggerFirstTxn, BeneficiaryReferrer, AmountFlat, 300, 0, nil, true, 10),
				mkRule("mid", TriggerFirstTxn, BeneficiaryReferrer, AmountFlat, 200, 0, nil, true, 20),
			},
			trigger:   TriggerFirstTxn,
			baseKobo:  1000,
			ref:       contactRef,
			wantKobo:  []int64{300, 200, 100},
			wantRules: []string{"high", "mid", "low"},
		},
		{
			name: "inactive rule skipped",
			rules: []CommissionRule{
				mkRule("off", TriggerSignupVerified, BeneficiaryReferrer, AmountFlat, 50000, 0, nil, false, 1),
				mkRule("on", TriggerSignupVerified, BeneficiaryReferrer, AmountFlat, 10000, 0, nil, true, 2),
			},
			trigger:   TriggerSignupVerified,
			ref:       contactRef,
			wantKobo:  []int64{10000},
			wantRules: []string{"on"},
		},
		{
			name: "trigger mismatch skipped",
			rules: []CommissionRule{
				mkRule("other", TriggerSale, BeneficiaryReferrer, AmountFlat, 50000, 0, nil, true, 1),
			},
			trigger:  TriggerSignupVerified,
			ref:      contactRef,
			wantKobo: nil,
		},
		{
			name: "agent rule does not fire for contact referrer",
			rules: []CommissionRule{
				mkRule("agentonly", TriggerSignupVerified, BeneficiaryAgent, AmountFlat, 50000, 0, nil, true, 1),
			},
			trigger:  TriggerSignupVerified,
			ref:      contactRef,
			wantKobo: nil,
		},
		{
			name: "agent rule fires for agent referrer (beneficiary = referrer id)",
			rules: []CommissionRule{
				mkRule("agentonly", TriggerSignupVerified, BeneficiaryAgent, AmountFlat, 50000, 0, nil, true, 1),
			},
			trigger:  TriggerSignupVerified,
			ref:      agentRef,
			wantIDs:  []string{"agent-9"},
			wantKobo: []int64{50000},
		},
		{
			name: "staff rule skipped for agent referrer",
			rules: []CommissionRule{
				mkRule("staffonly", TriggerSignupVerified, BeneficiaryStaff, AmountFlat, 50000, 0, nil, true, 1),
			},
			trigger:  TriggerSignupVerified,
			ref:      agentRef,
			wantKobo: nil,
		},
		{
			name: "referrer rule fires for any referrer type",
			rules: []CommissionRule{
				mkRule("any", TriggerSignupVerified, BeneficiaryReferrer, AmountFlat, 7000, 0, nil, true, 1),
			},
			trigger:  TriggerSignupVerified,
			ref:      agentRef,
			wantIDs:  []string{"agent-9"},
			wantKobo: []int64{7000},
		},
		{
			name: "percent on zero base computes 0 → skipped",
			rules: []CommissionRule{
				mkRule("pct", TriggerSignupVerified, BeneficiaryReferrer, AmountPercent, 0, 500, nil, true, 1),
			},
			trigger:  TriggerSignupVerified,
			baseKobo: 0,
			ref:      contactRef,
			wantKobo: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			awards := EvaluateRules(tc.rules, tc.trigger, tc.baseKobo, tc.ref)
			if len(awards) != len(tc.wantKobo) {
				t.Fatalf("awards = %+v, want %d awards", awards, len(tc.wantKobo))
			}
			for i, a := range awards {
				if a.AmountKobo != tc.wantKobo[i] {
					t.Fatalf("award[%d].AmountKobo = %d, want %d", i, a.AmountKobo, tc.wantKobo[i])
				}
				if len(tc.wantIDs) > i && a.BeneficiaryID != tc.wantIDs[i] {
					t.Fatalf("award[%d].BeneficiaryID = %q, want %q", i, a.BeneficiaryID, tc.wantIDs[i])
				}
				if len(tc.wantRules) > i && a.RuleName != tc.wantRules[i] {
					t.Fatalf("award[%d].RuleName = %q, want %q", i, a.RuleName, tc.wantRules[i])
				}
			}
		})
	}
}

// Deterministic evaluation: input order must not change the outcome
// (priority/created_at/id tie-breaks).
func TestEvaluateRulesDeterministicOrder(t *testing.T) {
	t0 := time.Now()
	r1 := mkRule("a", TriggerFirstTxn, BeneficiaryReferrer, AmountFlat, 100, 0, nil, true, 10)
	r1.CreatedAt = t0
	r2 := mkRule("b", TriggerFirstTxn, BeneficiaryReferrer, AmountFlat, 200, 0, nil, true, 10)
	r2.CreatedAt = t0
	ref := mkReferral(ReferrerContact, "c-1")

	fwd := EvaluateRules([]CommissionRule{r1, r2}, TriggerFirstTxn, 0, ref)
	rev := EvaluateRules([]CommissionRule{r2, r1}, TriggerFirstTxn, 0, ref)
	if len(fwd) != 2 || len(rev) != 2 {
		t.Fatalf("want 2 awards each, got %d/%d", len(fwd), len(rev))
	}
	if fwd[0].RuleID != rev[0].RuleID || fwd[1].RuleID != rev[1].RuleID {
		t.Fatalf("order not deterministic: fwd=%v rev=%v", fwd, rev)
	}
}

// Validation guards (contract §2): bad enum / bad amount combos rejected.
func TestValidateRule(t *testing.T) {
	ref := mkReferral(ReferrerContact, "c-1")
	if err := ValidateReferral(&ref); err != nil {
		t.Fatalf("valid referral rejected: %v", err)
	}
	bad := mkReferral("partner", "x")
	if err := ValidateReferral(&bad); err == nil {
		t.Fatal("invalid referrer_type accepted")
	}

	good := mkRule("ok", TriggerFirstTxn, BeneficiaryReferrer, AmountPercent, 0, 250, i64(1000), true, 1)
	if err := ValidateRule(&good); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}
	for name, r := range map[string]CommissionRule{
		"flat without amount":  mkRule("x", TriggerSale, BeneficiaryReferrer, AmountFlat, 0, 0, nil, true, 1),
		"percent without bps":  mkRule("x", TriggerSale, BeneficiaryReferrer, AmountPercent, 100, 0, nil, true, 1),
		"bad trigger":          mkRule("x", "monthly", BeneficiaryReferrer, AmountFlat, 100, 0, nil, true, 1),
		"bad beneficiary":      mkRule("x", TriggerSale, "house", AmountFlat, 100, 0, nil, true, 1),
		"negative cap":         mkRule("x", TriggerSale, BeneficiaryReferrer, AmountFlat, 100, 0, i64(-5), true, 1),
		"unknown amount type":  mkRule("x", TriggerSale, BeneficiaryReferrer, "hybrid", 100, 0, nil, true, 1),
		"percent bps too high": mkRule("x", TriggerSale, BeneficiaryReferrer, AmountPercent, 0, 2_000_000, nil, true, 1),
	} {
		if err := ValidateRule(&r); err == nil {
			t.Fatalf("%s: accepted, want ErrInvalidInput", name)
		}
	}
}
