package socialpub

// Pure unit tests for the SPEC-W21 hard gates (no database): budget rules,
// age targeting bounds, the political-ads launch gate and the effective
// disclaimer precedence. The HTTP status mapping of these gates is covered
// in handlers_test.go.

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestValidateBudget(t *testing.T) {
	cases := []struct {
		name        string
		total       int64
		daily       int64
		wantInvalid bool
	}{
		{"valid", 500000, 100000, false},
		{"daily equals total", 100000, 100000, false},
		{"zero total", 0, 100, true},
		{"negative total", -5, 100, true},
		{"zero daily", 500000, 0, true},
		{"daily exceeds total", 100000, 100001, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBudget(tc.total, tc.daily)
			if tc.wantInvalid && !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("total=%d daily=%d: err=%v, want ErrInvalidInput", tc.total, tc.daily, err)
			}
			if !tc.wantInvalid && err != nil {
				t.Fatalf("total=%d daily=%d: err=%v, want nil", tc.total, tc.daily, err)
			}
		})
	}
}

func TestTargetingValidate(t *testing.T) {
	cases := []struct {
		name        string
		min, max    int
		wantInvalid bool
	}{
		{"valid band", 18, 65, false},
		{"full band", 18, 100, false},
		{"equal", 35, 35, false},
		{"below 18", 17, 65, true},
		{"above 100", 18, 101, true},
		{"min above max", 60, 40, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tg := Targeting{AgeMin: tc.min, AgeMax: tc.max}
			err := tg.Validate()
			if tc.wantInvalid && !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("age %d..%d: err=%v, want ErrInvalidInput", tc.min, tc.max, err)
			}
			if !tc.wantInvalid && err != nil {
				t.Fatalf("age %d..%d: err=%v, want nil", tc.min, tc.max, err)
			}
		})
	}
}

func strptr(s string) *string { return &s }

func TestEffectiveDisclaimer(t *testing.T) {
	ad := &Ad{}
	c := &Creative{}
	if got := EffectiveDisclaimer(ad, c); got != "" {
		t.Fatalf("empty: got %q", got)
	}
	c.DisclaimerText = strptr("creative disclaimer")
	if got := EffectiveDisclaimer(ad, c); got != "creative disclaimer" {
		t.Fatalf("creative fallback: got %q", got)
	}
	ad.DisclaimerText = strptr("ad disclaimer")
	if got := EffectiveDisclaimer(ad, c); got != "ad disclaimer" {
		t.Fatalf("ad wins: got %q", got)
	}
	ad.DisclaimerText = strptr("   ") // blank ad disclaimer falls back
	if got := EffectiveDisclaimer(ad, c); got != "creative disclaimer" {
		t.Fatalf("blank ad disclaimer falls back: got %q", got)
	}
}

func TestCheckLaunchGate(t *testing.T) {
	connected := &Account{ID: uuid.New(), Status: AccountConnected}
	expired := &Account{ID: uuid.New(), Status: AccountExpired}
	revoked := &Account{ID: uuid.New(), Status: AccountRevoked}
	authorized := &Account{ID: uuid.New(), Status: AccountConnected, PoliticalAuth: true}
	creative := &Creative{}
	disclaimed := &Creative{DisclaimerText: strptr("Paid for by X")}

	// Non-political ad: connected → ok; expired|revoked → ErrAccountInactive.
	plain := &Ad{}
	if err := checkLaunchGate(plain, connected, creative); err != nil {
		t.Fatalf("plain launch: %v", err)
	}
	if err := checkLaunchGate(plain, expired, creative); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("expired account: %v, want ErrAccountInactive", err)
	}
	if err := checkLaunchGate(plain, revoked, creative); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("revoked account: %v, want ErrAccountInactive", err)
	}

	// Political ad: unauthorized → 422 gate; authorized without disclaimer
	// → 422 gate; authorized with ad's own or creative's disclaimer → ok.
	pol := &Ad{Political: true}
	if err := checkLaunchGate(pol, connected, disclaimed); !errors.Is(err, ErrPoliticalGate) {
		t.Fatalf("political without authorization: %v, want ErrPoliticalGate", err)
	}
	if err := checkLaunchGate(pol, authorized, creative); !errors.Is(err, ErrPoliticalGate) {
		t.Fatalf("political without disclaimer: %v, want ErrPoliticalGate", err)
	}
	if err := checkLaunchGate(pol, authorized, disclaimed); err != nil {
		t.Fatalf("political with creative disclaimer: %v", err)
	}
	polWithDisc := &Ad{Political: true, DisclaimerText: strptr("Paid for by Y")}
	if err := checkLaunchGate(polWithDisc, authorized, creative); err != nil {
		t.Fatalf("political with ad disclaimer: %v", err)
	}

	// Account check precedes the political gate (expired + political → 409).
	if err := checkLaunchGate(pol, expired, disclaimed); !errors.Is(err, ErrAccountInactive) {
		t.Fatalf("political on expired account: %v, want ErrAccountInactive", err)
	}
}

func TestAdStateMachine(t *testing.T) {
	legal := [][2]string{
		{AdDraft, AdReview}, {AdDraft, AdRejected},
		{AdReview, AdActive}, {AdReview, AdRejected}, {AdReview, AdDraft},
		{AdActive, AdPaused}, {AdActive, AdRejected},
		{AdPaused, AdActive}, {AdPaused, AdRejected},
	}
	for _, e := range legal {
		if !CanTransitionAd(e[0], e[1]) {
			t.Fatalf("%s → %s should be legal", e[0], e[1])
		}
	}
	illegal := [][2]string{
		{AdDraft, AdActive}, {AdDraft, AdPaused},
		{AdRejected, AdDraft}, {AdRejected, AdActive},
		{AdActive, AdDraft}, {AdPaused, AdDraft},
	}
	for _, e := range illegal {
		if CanTransitionAd(e[0], e[1]) {
			t.Fatalf("%s → %s should be illegal", e[0], e[1])
		}
	}
}

func TestRowValidation(t *testing.T) {
	tenantID := uuid.New()

	// Account: bad provider / status rejected.
	a := mkAccount(tenantID, "myspace")
	if err := a.Validate(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad provider: %v", err)
	}
	a = mkAccount(tenantID, ProviderMeta)
	a.Status = "limbo"
	if err := a.Validate(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad status: %v", err)
	}

	// Creative: image requires media_url.
	c := mkCreative(tenantID, "img")
	c.Kind = CreativeImage
	if err := c.Validate(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("image without media_url: %v", err)
	}
	c.MediaURL = strptr("https://cdn.example/img.png")
	if err := c.Validate(); err != nil {
		t.Fatalf("image with media_url: %v", err)
	}

	// Ad: budget gate fires inside Validate.
	ad := mkAd(tenantID, uuid.New(), uuid.New())
	ad.DailyBudgetKobo = ad.BudgetKobo + 1
	if err := ad.Validate(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("daily > total: %v", err)
	}
	ad = mkAd(tenantID, uuid.New(), uuid.New())
	ad.Targeting.AgeMin = 12
	if err := ad.Validate(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("age < 18: %v", err)
	}
}
