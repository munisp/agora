package lending

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// SPEC-W20 Agent C package unit tests (no database): state machine,
// naive scoring, money math and validation.

func TestTransitions(t *testing.T) {
	legal := [][2]string{
		{StatusDraft, StatusSubmitted},
		{StatusSubmitted, StatusUnderReview},
		{StatusUnderReview, StatusApproved},
		{StatusUnderReview, StatusDeclined},
		{StatusUnderReview, StatusDefaulted},
		{StatusApproved, StatusDefaulted},
		{StatusDisbursed, StatusDefaulted},
	}
	for _, tr := range legal {
		if !CanTransition(tr[0], tr[1]) {
			t.Fatalf("CanTransition(%s,%s) = false, want true", tr[0], tr[1])
		}
		if err := ValidateTransition(tr[0], tr[1]); err != nil {
			t.Fatalf("ValidateTransition(%s,%s) = %v", tr[0], tr[1], err)
		}
	}
	illegal := [][2]string{
		{StatusDraft, StatusApproved},
		{StatusDraft, StatusUnderReview},
		{StatusSubmitted, StatusApproved},
		{StatusApproved, StatusDeclined},
		{StatusDeclined, StatusUnderReview}, // terminal
		{StatusRepaid, StatusDefaulted},     // terminal
		{StatusDefaulted, StatusApproved},   // terminal
		{StatusUnderReview, StatusUnderReview},
	}
	for _, tr := range illegal {
		if CanTransition(tr[0], tr[1]) {
			t.Fatalf("CanTransition(%s,%s) = true, want false", tr[0], tr[1])
		}
		if err := ValidateTransition(tr[0], tr[1]); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("ValidateTransition(%s,%s) = %v, want ErrInvalidTransition", tr[0], tr[1], err)
		}
	}
	// disbursed/repaid are owned by the disburse/repay flows — PATCH never.
	for _, tr := range [][2]string{
		{StatusApproved, StatusDisbursed},
		{StatusDisbursed, StatusRepaid},
	} {
		if err := ValidateTransition(tr[0], tr[1]); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("ValidateTransition(%s,%s) = %v, want ErrInvalidTransition", tr[0], tr[1], err)
		}
	}
	// Unknown statuses.
	if err := ValidateTransition("nope", StatusApproved); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unknown from = %v", err)
	}
	if err := ValidateStatus("nope"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ValidateStatus(nope) = %v", err)
	}
}

func TestScore(t *testing.T) {
	cases := []struct {
		name string
		sig  ScoreSignals
		want int
	}{
		{"zero", ScoreSignals{}, 0},
		{"one month", ScoreSignals{TenureDays: 30}, 3},
		{"partial month floors", ScoreSignals{TenureDays: 59}, 3},
		{"tenure cap", ScoreSignals{TenureDays: 3650}, ScoreMaxTenure},
		{"bookings", ScoreSignals{CompletedBookings: 3}, 12},
		{"bookings cap", ScoreSignals{CompletedBookings: 100}, ScoreMaxBookings},
		{"repaid loans", ScoreSignals{RepaidLoans: 2}, 20},
		{"repaid cap", ScoreSignals{RepaidLoans: 9}, ScoreMaxRepaidLoans},
		{"negative tenure clamps", ScoreSignals{TenureDays: -5}, 0},
		{"combined", ScoreSignals{TenureDays: 120, CompletedBookings: 5, RepaidLoans: 1}, 12 + 20 + 10},
		{"max 100", ScoreSignals{TenureDays: 3650, CompletedBookings: 100, RepaidLoans: 100}, 100},
	}
	for _, tc := range cases {
		if got := Score(tc.sig); got != tc.want {
			t.Fatalf("%s: Score(%+v) = %d, want %d", tc.name, tc.sig, got, tc.want)
		}
	}
}

func TestInterestMath(t *testing.T) {
	p := Product{InterestBps: 1500} // 15%
	if got := p.InterestFor(100000); got != 15000 {
		t.Fatalf("interest = %d, want 15000", got)
	}
	// Integer division floors (kobo precision).
	p.InterestBps = 333
	if got := p.InterestFor(100); got != 3 { // 100*333/10000 = 3.33 → 3
		t.Fatalf("floored interest = %d, want 3", got)
	}
	p.InterestBps = 0
	if got := p.InterestFor(100000); got != 0 {
		t.Fatalf("zero interest = %d, want 0", got)
	}
}

func TestProductValidate(t *testing.T) {
	ok := Product{
		TenantID: uuid.New(), Name: "Trader", PrincipalMinKobo: 1000,
		PrincipalMaxKobo: 500000, TermDays: 30, InterestBps: 1500,
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid product: %v", err)
	}
	bad := []Product{
		{Name: "x", PrincipalMinKobo: 0, PrincipalMaxKobo: 1, TermDays: 1},                       // no tenant + min 0
		{TenantID: uuid.New(), Name: " ", PrincipalMinKobo: 1, PrincipalMaxKobo: 1, TermDays: 1}, // blank name
		{TenantID: uuid.New(), Name: "x", PrincipalMinKobo: 10, PrincipalMaxKobo: 5, TermDays: 1},
		{TenantID: uuid.New(), Name: "x", PrincipalMinKobo: 1, PrincipalMaxKobo: 1, TermDays: 0},
		{TenantID: uuid.New(), Name: "x", PrincipalMinKobo: 1, PrincipalMaxKobo: 1, TermDays: 1, InterestBps: 10001},
		{TenantID: uuid.New(), Name: "x", PrincipalMinKobo: 1, PrincipalMaxKobo: 1, TermDays: 1, FeeFlatKobo: -1},
	}
	for i, p := range bad {
		p.TenantID = uuid.New()
		if err := p.Validate(); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("bad[%d] = %v, want ErrInvalidInput", i, err)
		}
	}
}

func TestPrincipalBand(t *testing.T) {
	p := Product{PrincipalMinKobo: 1000, PrincipalMaxKobo: 5000}
	if err := ValidatePrincipalAgainst(p, 2500); err != nil {
		t.Fatalf("in band: %v", err)
	}
	if err := ValidatePrincipalAgainst(p, 999); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("below min = %v", err)
	}
	if err := ValidatePrincipalAgainst(p, 5001); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("above max = %v", err)
	}
}

func TestRepaymentInputValidate(t *testing.T) {
	if err := ValidateRepaymentInput(100, "ref-1"); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if err := ValidateRepaymentInput(0, "ref-1"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero amount = %v", err)
	}
	if err := ValidateRepaymentInput(100, " "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("blank ref = %v", err)
	}
}

func TestDaysPastDue(t *testing.T) {
	now := time.Now().UTC()
	l := LoanAccount{DueAt: now.AddDate(0, 0, -45)}
	if got := l.DaysPastDue(now); got != 45 {
		t.Fatalf("days past due = %d, want 45", got)
	}
	l.DueAt = now.Add(24 * time.Hour)
	if got := l.DaysPastDue(now); got != 0 {
		t.Fatalf("not yet due = %d, want 0", got)
	}
	if l.TotalKobo() != 0 {
		t.Fatalf("zero loan total = %d", l.TotalKobo())
	}
}
