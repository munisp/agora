package loyalty

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// Pure-function coverage (no DB): program validation, event enum, tier
// recompute and the journal invariants.
func TestValidateProgram(t *testing.T) {
	tenantID := uuid.New()
	good := mkProgram(tenantID)
	if err := ValidateProgram(&good); err != nil {
		t.Fatalf("valid program rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Program){
		"missing tenant": func(p *Program) { p.TenantID = uuid.Nil },
		"empty name":     func(p *Program) { p.Name = "  " },
		"negative cap":   func(p *Program) { p.CapPerDay = -1 },
		"unknown event": func(p *Program) {
			p.EarnRules = []EarnRule{{Event: "purchase", Points: 5}}
		},
		"zero points": func(p *Program) {
			p.EarnRules = []EarnRule{{Event: EventFirstTxn, Points: 0}}
		},
		"duplicate event": func(p *Program) {
			p.EarnRules = []EarnRule{
				{Event: EventFirstTxn, Points: 1},
				{Event: EventFirstTxn, Points: 2},
			}
		},
		"nameless tier": func(p *Program) {
			p.Tiers = []Tier{{Name: " ", MinPoints: 0}}
		},
		"negative tier min": func(p *Program) {
			p.Tiers = []Tier{{Name: "silver", MinPoints: -1}}
		},
	} {
		p := mkProgram(tenantID)
		mutate(&p)
		if err := ValidateProgram(&p); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s = %v, want ErrInvalidInput", name, err)
		}
	}
}

func TestTierFor(t *testing.T) {
	tiers := []Tier{
		{Name: "silver", MinPoints: 100},
		{Name: "gold", MinPoints: 200},
		{Name: "platinum", MinPoints: 1000},
	}
	for want, lifetime := range map[string]int64{
		"":       99,
		"silver": 100,
		"gold":   250,
	} {
		if got := TierFor(lifetime, tiers); got != want {
			t.Fatalf("TierFor(%d) = %q, want %q", lifetime, got, want)
		}
	}
	if got := TierFor(5000, nil); got != "" {
		t.Fatalf("no tiers = %q, want empty", got)
	}
	// Unordered input still picks the highest qualifying threshold.
	messy := []Tier{{Name: "gold", MinPoints: 200}, {Name: "silver", MinPoints: 100}}
	if got := TierFor(150, messy); got != "silver" {
		t.Fatalf("unordered tiers = %q, want silver", got)
	}
}

func TestValidateJournal(t *testing.T) {
	tenantID := uuid.New()
	journalID := uuid.New()
	entry := func(code int, debit, credit int64) LedgerEntry {
		return LedgerEntry{TenantID: tenantID, JournalID: journalID, AccountCode: code,
			DebitPoints: debit, CreditPoints: credit}
	}
	balanced := []LedgerEntry{entry(AccountPointsRedeemed, 50, 0), entry(AccountPointsIssued, 0, 50)}
	if err := validateJournal(tenantID, journalID, balanced); err != nil {
		t.Fatalf("balanced journal rejected: %v", err)
	}
	for name, entries := range map[string][]LedgerEntry{
		"single entry": {entry(AccountPointsIssued, 0, 50)},
		"unbalanced":   {entry(AccountPointsRedeemed, 40, 0), entry(AccountPointsIssued, 0, 50)},
		"unknown code": {entry(300, 50, 0), entry(AccountPointsIssued, 0, 50)},
		"two-sided":    {entry(AccountPointsRedeemed, 50, 50), entry(AccountPointsIssued, 0, 100)},
		"zero-amount":  {entry(AccountPointsRedeemed, 0, 0), entry(AccountPointsIssued, 0, 0)},
		"tenant mismatch": {
			entry(AccountPointsRedeemed, 50, 0),
			{TenantID: uuid.New(), JournalID: journalID, AccountCode: AccountPointsIssued, CreditPoints: 50},
		},
	} {
		if err := validateJournal(tenantID, journalID, entries); !errors.Is(err, ErrUnbalancedJournal) {
			t.Fatalf("%s = %v, want ErrUnbalancedJournal", name, err)
		}
	}
	if err := validateJournal(tenantID, uuid.Nil, balanced); !errors.Is(err, ErrUnbalancedJournal) {
		t.Fatalf("nil journal = %v, want ErrUnbalancedJournal", err)
	}
}

// The mirrored ledger enforces points-not-money semantics: referrals'
// money codes (300..303) are rejected here, loyalty codes are rejected
// there — the two ledgers can never cross-post.
func TestCodeRangesAreDisjoint(t *testing.T) {
	for _, code := range []int{300, 301, 302, 303} {
		e := []LedgerEntry{
			{TenantID: uuid.Nil, JournalID: uuid.New(), AccountCode: code, DebitPoints: 1},
			{TenantID: uuid.Nil, JournalID: uuid.New(), AccountCode: AccountPointsIssued, CreditPoints: 1},
		}
		if err := validateJournal(uuid.Nil, uuid.New(), e); !errors.Is(err, ErrUnbalancedJournal) {
			t.Fatalf("money code %d accepted by loyalty journal validation", code)
		}
	}
}
