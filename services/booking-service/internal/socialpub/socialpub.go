// Package socialpub implements SPEC-W21 Agent B: the SOCIAL PUBLISHER
// enterprise app (social accounts → creatives → posts queue → paid ads
// with the political-ads gates) — app_id "social-publisher" (the
// integrator wires the appgate entitlement with that id over the whole
// /v1/social route group).
//
// Model (all four tables FORCE-RLS tenant_isolation — the
// devices/store.go idiom; money is kobo int64 per the shared contract):
//
//	social_accounts  — provider connection record (meta|tiktok|x),
//	    status connected|expired|revoked, political_ads_authorized flag.
//	    "Connect" is a RECORD ONLY — there is NO real OAuth flow (see
//	    Limitations); tokens are provisioned out-of-band (docs runbook).
//	social_creatives — reusable creative {kind text|image|video, body,
//	    media_url, disclaimer_text}. Creative bodies NEVER appear in
//	    CloudEvent payloads (privacy contract §Shared contracts).
//	social_posts     — draft|queued|publishing|published|failed; publish
//	    goes through the provider seam (mock default).
//	social_ads       — draft|review|active|paused|rejected; budget_kobo
//	    int64 (total) + daily_budget_kobo int64, targeting jsonb
//	    {lgas, age_min, age_max, interests}, political flag, disclaimer.
//
// Hard gates (SPEC-W21, each covered by a handler test):
//   - launch with political=true → 422 UNLESS the account has
//     political_ads_authorized=true AND the effective disclaimer
//     (ad's own disclaimer_text, else the creative's) is non-empty.
//   - publish/launch on an expired|revoked account → 409.
//   - budget_kobo > 0 and daily_budget_kobo ≤ budget_kobo (400).
//   - targeting.age_min ≤ age_max, both within 18..100 (400).
//
// Provider seam: internal/socialpub/provider — interface Publisher
// {PublishPost, LaunchAd, AdStats} with per-provider mocks as the
// zero-config default (SOCIAL_MOCK=1 master switch, or per-provider
// META_MOCK/TIKTOK_MOCK/X_MOCK — see the provider package doc), returning
// deterministic sandbox ids (mock-post-*/mock-ad-*) and plausible stats.
// Real API wiring is an honest stub posture (same as W16 APNs): the
// credential-follow-up runbook lives in docs/apps/social-publisher.md.
//
// Metering: one social_ad_launched usage record per successful launch
// (opendesk.usage.events; the shared W19/W20 metering idiom). Events:
// topic opendesk.social.events.v1 — com.opendesk.social.PostPublished /
// AdLaunched / AdRejected; payloads carry ids + metadata, NEVER the
// creative body.
//
// Anti-collision contract (SPEC-W21): this package is SELF-CONTAINED — it
// exposes NewStore/DialStore and RegisterRoutes(r, d, mw...) (mirroring
// internal/helpdesk); the integrator wires Deps, route mounting, config
// envs and the appgate entitlement flag (app_id "social-publisher"). This
// package touches NO shared files.
//
// Config envs (documented for the integrator — no config code here; every
// one is optional and the app is functional with zero config):
//
//	SOCIAL_EVENTS_TOPIC — lifecycle CloudEvents topic
//	    (default opendesk.social.events.v1; empty disables events)
//	USAGE_EVENTS_TOPIC  — shared usage-metering topic
//	    (default opendesk.usage.events; empty disables social_ad_launched
//	    metering)
//	SOCIAL_MOCK         — master provider mock switch (default "1";
//	    SOCIAL_MOCK=0 lets per-provider switches decide)
//	META_MOCK / TIKTOK_MOCK / X_MOCK — per-provider mock switches
//	    (each default "1"; "0" selects the honest real-API stub which
//	    answers "not configured" until credentials land — follow-up)
//	DATABASE_URL        — DialStore fallback pool (same idiom as W16 devices)
//
// Permissions (integrator wires; recommended shape composed from httpapi's
// require(), applied group-wide via the variadic mw of RegisterRoutes):
// GET/HEAD → require("view_analytics"), everything else →
// require("manage_bookings").
package socialpub

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Enums
// ---------------------------------------------------------------------------

// Account providers (SPEC-W21).
const (
	ProviderMeta   = "meta"
	ProviderTikTok = "tiktok"
	ProviderX      = "x"
)

// Providers lists every supported provider id.
var Providers = []string{ProviderMeta, ProviderTikTok, ProviderX}

// Account statuses.
const (
	AccountConnected = "connected"
	AccountExpired   = "expired"
	AccountRevoked   = "revoked"
)

// AccountStatuses lists every account status.
var AccountStatuses = []string{AccountConnected, AccountExpired, AccountRevoked}

// Creative kinds.
const (
	CreativeText  = "text"
	CreativeImage = "image"
	CreativeVideo = "video"
)

// CreativeKinds lists every creative kind.
var CreativeKinds = []string{CreativeText, CreativeImage, CreativeVideo}

// Post statuses (SPEC-W21 state machine):
//
//	draft → queued → publishing → published
//	                  └────────→ failed
//
// failed may be retried via the publish endpoint (→ publishing).
const (
	PostDraft      = "draft"
	PostQueued     = "queued"
	PostPublishing = "publishing"
	PostPublished  = "published"
	PostFailed     = "failed"
)

// PostStatuses lists every post status in machine order.
var PostStatuses = []string{PostDraft, PostQueued, PostPublishing, PostPublished, PostFailed}

// Ad objectives.
const (
	ObjectiveAwareness  = "awareness"
	ObjectiveTraffic    = "traffic"
	ObjectiveEngagement = "engagement"
)

// Objectives lists every ad objective.
var Objectives = []string{ObjectiveAwareness, ObjectiveTraffic, ObjectiveEngagement}

// Ad statuses (SPEC-W21 state machine):
//
//	draft → review → active ⇄ paused
//	              └→ rejected
const (
	AdDraft    = "draft"
	AdReview   = "review"
	AdActive   = "active"
	AdPaused   = "paused"
	AdRejected = "rejected"
)

// AdStatuses lists every ad status.
var AdStatuses = []string{AdDraft, AdReview, AdActive, AdPaused, AdRejected}

// adTransitions is the operator-driven ad state machine. →review happens
// ONLY via the launch endpoint (the political/account gates live there);
// →rejected is emitted by the provider seam at launch (mock rejects the
// documented sentinel) or by an operator PATCH.
var adTransitions = map[string][]string{
	AdDraft:  {AdReview, AdRejected},
	AdReview: {AdActive, AdRejected, AdDraft},
	AdActive: {AdPaused, AdRejected},
	AdPaused: {AdActive, AdRejected},
}

// CanTransitionAd reports whether from→to is a legal operator edge of the
// ad state machine (launch itself uses the draft→review edge).
func CanTransitionAd(from, to string) bool {
	for _, next := range adTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// ErrInvalidInput marks deterministic validation failures (400 at the API).
var ErrInvalidInput = errors.New("invalid social input")

// ErrInvalidTransition marks state-machine violations (409 at the API).
var ErrInvalidTransition = errors.New("invalid social status transition")

// ErrAccountInactive marks publish/launch attempts against an
// expired|revoked account (409 at the API — the operator can fix it by
// reconnecting).
var ErrAccountInactive = errors.New("social account is not connected")

// ErrPoliticalGate marks the political-ads gate failures (422 at the API):
// political=true launch without account political_ads_authorized or
// without an effective disclaimer.
var ErrPoliticalGate = errors.New("political ads requirements not met")

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

// maxLen bounds (free-text columns).
const (
	maxDisplayNameLen = 200
	maxAccountRefLen  = 200
	maxNameLen        = 300
	maxBodyLen        = 8000
	maxMediaURLLen    = 2000
	maxDisclaimerLen  = 1000
	maxErrorLen       = 2000
	maxTargetingItems = 200
)

// Account mirrors social_accounts (SPEC-W21 Agent B).
type Account struct {
	ID            uuid.UUID `json:"id"`
	TenantID      uuid.UUID `json:"tenant_id"`
	Provider      string    `json:"provider"`
	AccountRef    string    `json:"account_ref"`
	DisplayName   string    `json:"display_name"`
	Status        string    `json:"status"`
	PoliticalAuth bool      `json:"political_ads_authorized"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Validate checks the account field set.
func (a *Account) Validate() error {
	if a.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if !member(Providers, a.Provider) {
		return fmt.Errorf("%w: provider %q (want meta|tiktok|x)", ErrInvalidInput, a.Provider)
	}
	a.AccountRef = strings.TrimSpace(a.AccountRef)
	if a.AccountRef == "" || len(a.AccountRef) > maxAccountRefLen {
		return fmt.Errorf("%w: account_ref must be 1-%d bytes", ErrInvalidInput, maxAccountRefLen)
	}
	a.DisplayName = strings.TrimSpace(a.DisplayName)
	if a.DisplayName == "" || len(a.DisplayName) > maxDisplayNameLen {
		return fmt.Errorf("%w: display_name must be 1-%d bytes", ErrInvalidInput, maxDisplayNameLen)
	}
	if !member(AccountStatuses, a.Status) {
		return fmt.Errorf("%w: status %q (want connected|expired|revoked)", ErrInvalidInput, a.Status)
	}
	return nil
}

// Connected reports whether the account can publish/launch.
func (a *Account) Connected() bool { return a.Status == AccountConnected }

// Creative mirrors social_creatives.
type Creative struct {
	ID             uuid.UUID `json:"id"`
	TenantID       uuid.UUID `json:"tenant_id"`
	Name           string    `json:"name"`
	Kind           string    `json:"kind"`
	Body           string    `json:"body"`
	MediaURL       *string   `json:"media_url"`
	DisclaimerText *string   `json:"disclaimer_text"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Validate checks the creative field set.
func (c *Creative) Validate() error {
	if c.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" || len(c.Name) > maxNameLen {
		return fmt.Errorf("%w: name must be 1-%d bytes", ErrInvalidInput, maxNameLen)
	}
	if !member(CreativeKinds, c.Kind) {
		return fmt.Errorf("%w: kind %q (want text|image|video)", ErrInvalidInput, c.Kind)
	}
	if strings.TrimSpace(c.Body) == "" || len(c.Body) > maxBodyLen {
		return fmt.Errorf("%w: body must be 1-%d bytes", ErrInvalidInput, maxBodyLen)
	}
	if c.Kind != CreativeText {
		if c.MediaURL == nil || strings.TrimSpace(*c.MediaURL) == "" {
			return fmt.Errorf("%w: media_url is required for %s creatives", ErrInvalidInput, c.Kind)
		}
	}
	if c.MediaURL != nil && len(*c.MediaURL) > maxMediaURLLen {
		return fmt.Errorf("%w: media_url exceeds %d bytes", ErrInvalidInput, maxMediaURLLen)
	}
	if c.DisclaimerText != nil && len(*c.DisclaimerText) > maxDisclaimerLen {
		return fmt.Errorf("%w: disclaimer_text exceeds %d bytes", ErrInvalidInput, maxDisclaimerLen)
	}
	return nil
}

// Post mirrors social_posts.
type Post struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       uuid.UUID  `json:"tenant_id"`
	AccountID      uuid.UUID  `json:"account_id"`
	CreativeID     uuid.UUID  `json:"creative_id"`
	Status         string     `json:"status"`
	ProviderPostID *string    `json:"provider_post_id"`
	Error          *string    `json:"error"`
	PublishedAt    *time.Time `json:"published_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Ad mirrors social_ads. Budgets are kobo int64 (SPEC-W21 money contract).
type Ad struct {
	ID              uuid.UUID `json:"id"`
	TenantID        uuid.UUID `json:"tenant_id"`
	AccountID       uuid.UUID `json:"account_id"`
	CreativeID      uuid.UUID `json:"creative_id"`
	Name            string    `json:"name"`
	Objective       string    `json:"objective"`
	BudgetKobo      int64     `json:"budget_kobo"`
	DailyBudgetKobo int64     `json:"daily_budget_kobo"`
	Targeting       Targeting `json:"targeting"`
	Political       bool      `json:"political"`
	DisclaimerText  *string   `json:"disclaimer_text"`
	Status          string    `json:"status"`
	ProviderAdID    *string   `json:"provider_ad_id"`
	Error           *string   `json:"error"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Targeting is the social_ads.targeting jsonb (SPEC-W21: {lgas []text,
// age_min int, age_max int, interests []text}).
type Targeting struct {
	LGAs      []string `json:"lgas"`
	AgeMin    int      `json:"age_min"`
	AgeMax    int      `json:"age_max"`
	Interests []string `json:"interests"`
}

// Age bounds (SPEC-W21 hard gate: 18..100, min ≤ max).
const (
	AgeMinBound = 18
	AgeMaxBound = 100
)

// Validate enforces the targeting shape: age_min ≤ age_max within 18..100,
// list bounds, non-empty trimmed items.
func (tg *Targeting) Validate() error {
	if tg.AgeMin < AgeMinBound || tg.AgeMin > AgeMaxBound {
		return fmt.Errorf("%w: targeting.age_min must be within %d..%d", ErrInvalidInput, AgeMinBound, AgeMaxBound)
	}
	if tg.AgeMax < AgeMinBound || tg.AgeMax > AgeMaxBound {
		return fmt.Errorf("%w: targeting.age_max must be within %d..%d", ErrInvalidInput, AgeMinBound, AgeMaxBound)
	}
	if tg.AgeMin > tg.AgeMax {
		return fmt.Errorf("%w: targeting.age_min must be ≤ age_max", ErrInvalidInput)
	}
	if len(tg.LGAs) > maxTargetingItems || len(tg.Interests) > maxTargetingItems {
		return fmt.Errorf("%w: targeting lists exceed %d items", ErrInvalidInput, maxTargetingItems)
	}
	for i, v := range tg.LGAs {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%w: targeting.lgas[%d] is empty", ErrInvalidInput, i)
		}
	}
	for i, v := range tg.Interests {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%w: targeting.interests[%d] is empty", ErrInvalidInput, i)
		}
	}
	return nil
}

// ValidateBudget enforces the SPEC-W21 budget gates: total > 0 and
// daily ≤ total (daily itself must be > 0 — a zero daily budget is a
// non-sensical ad).
func ValidateBudget(totalKobo, dailyKobo int64) error {
	if totalKobo <= 0 {
		return fmt.Errorf("%w: budget_kobo must be > 0", ErrInvalidInput)
	}
	if dailyKobo <= 0 {
		return fmt.Errorf("%w: daily_budget_kobo must be > 0", ErrInvalidInput)
	}
	if dailyKobo > totalKobo {
		return fmt.Errorf("%w: daily_budget_kobo must be ≤ budget_kobo", ErrInvalidInput)
	}
	return nil
}

// Validate checks the ad field set (create/update input; launch gates are
// separate — checkLaunchGate).
func (ad *Ad) Validate() error {
	if ad.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	ad.Name = strings.TrimSpace(ad.Name)
	if ad.Name == "" || len(ad.Name) > maxNameLen {
		return fmt.Errorf("%w: name must be 1-%d bytes", ErrInvalidInput, maxNameLen)
	}
	if !member(Objectives, ad.Objective) {
		return fmt.Errorf("%w: objective %q (want awareness|traffic|engagement)", ErrInvalidInput, ad.Objective)
	}
	if err := ValidateBudget(ad.BudgetKobo, ad.DailyBudgetKobo); err != nil {
		return err
	}
	if err := ad.Targeting.Validate(); err != nil {
		return err
	}
	if ad.DisclaimerText != nil && len(*ad.DisclaimerText) > maxDisclaimerLen {
		return fmt.Errorf("%w: disclaimer_text exceeds %d bytes", ErrInvalidInput, maxDisclaimerLen)
	}
	return nil
}

// EffectiveDisclaimer returns the ad's own disclaimer when non-empty, else
// the creative's, else "" (SPEC-W21: "effective disclaimer (ad's own or
// creative's)").
func EffectiveDisclaimer(ad *Ad, c *Creative) string {
	if ad != nil && ad.DisclaimerText != nil && strings.TrimSpace(*ad.DisclaimerText) != "" {
		return strings.TrimSpace(*ad.DisclaimerText)
	}
	if c != nil && c.DisclaimerText != nil && strings.TrimSpace(*c.DisclaimerText) != "" {
		return strings.TrimSpace(*c.DisclaimerText)
	}
	return ""
}

// checkLaunchGate enforces the SPEC-W21 hard gates at launch:
//   - the account must be connected (expired|revoked → ErrAccountInactive,
//     409 at the API);
//   - a political ad requires account.political_ads_authorized AND a
//     non-empty effective disclaimer (ErrPoliticalGate, 422 at the API).
func checkLaunchGate(ad *Ad, account *Account, creative *Creative) error {
	if !account.Connected() {
		return fmt.Errorf("%w: account %s is %s — reconnect before launch",
			ErrAccountInactive, account.ID, account.Status)
	}
	if ad.Political {
		if !account.PoliticalAuth {
			return fmt.Errorf("%w: account %s is not authorized for political ads (Meta political-ads authorization is an external process — see docs/apps/social-publisher.md)",
				ErrPoliticalGate, account.ID)
		}
		if EffectiveDisclaimer(ad, creative) == "" {
			return fmt.Errorf("%w: political ads require a disclaimer (set ad.disclaimer_text or creative.disclaimer_text)",
				ErrPoliticalGate)
		}
	}
	return nil
}

// ValidateAdTransition enforces the operator ad status machine.
func ValidateAdTransition(from, to string) error {
	if !member(AdStatuses, from) || !member(AdStatuses, to) {
		return fmt.Errorf("%w: ad status %q → %q (unknown status)", ErrInvalidTransition, from, to)
	}
	if !CanTransitionAd(from, to) {
		return fmt.Errorf("%w: ad status %s → %s", ErrInvalidTransition, from, to)
	}
	return nil
}

// ValidatePostStatus enforces the post status enum (filters).
func ValidatePostStatus(s string) error {
	if !member(PostStatuses, s) {
		return fmt.Errorf("%w: status %q (want draft|queued|publishing|published|failed)", ErrInvalidInput, s)
	}
	return nil
}

// ValidateAdStatus enforces the ad status enum (filters).
func ValidateAdStatus(s string) error {
	if !member(AdStatuses, s) {
		return fmt.Errorf("%w: status %q (want draft|review|active|paused|rejected)", ErrInvalidInput, s)
	}
	return nil
}

func member(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
