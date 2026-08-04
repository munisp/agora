// Package provider implements the social-publisher provider seam
// (SPEC-W21 Agent B): one Publisher interface, three providers
// (meta/tiktok/x), each with a DETERMINISTIC MOCK as the zero-config
// default (mirroring the W16 FCM_MOCK posture — no network, documented
// test hooks) and an HONEST real-API stub that refuses with "not
// configured" until credentials are wired (same posture as the W16 APNs
// stub — no fake implementation claims; the credential runbook lives in
// docs/apps/social-publisher.md).
//
// Mock switches (documented for the integrator; resolved by
// MockEnabled):
//
//	SOCIAL_MOCK                         master switch (default "1")
//	META_MOCK / TIKTOK_MOCK / X_MOCK    per-provider (each default "1")
//
// A provider is in mock mode when the master switch is unset/truthy OR
// its per-provider switch is unset/truthy — with all four unset every
// provider is a mock and the app works with zero config. Setting
// SOCIAL_MOCK=0 AND META_MOCK=0 selects the real Meta stub (not yet
// credential-wired: every call answers "not configured").
//
// Mock determinism:
//   - PublishPost → "mock-post-<provider>-<sha256[:16]>" of the stable
//     request key; account_ref "mock-fail" → provider error (post → failed).
//   - LaunchAd → "mock-ad-<provider>-<sha256[:16]>"; ad name containing
//     "mock-reject" → Rejected=true with a policy-style reason (drives the
//     AdRejected event + rejected status).
//   - AdStats → plausible numbers derived deterministically from the ad id
//     hash (same id → same stats; no randomness, no time dependence).
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// Publisher is the contract every social provider implements.
type Publisher interface {
	// Name is the provider id ("meta" | "tiktok" | "x").
	Name() string
	// PublishPost publishes one post; returns the provider-side post id.
	PublishPost(ctx context.Context, req PostRequest) (string, error)
	// LaunchAd submits one ad for review/activation. Rejected=true means
	// the provider refused the ad on policy grounds (reason is
	// operator-facing); err is reserved for transport/local failures.
	LaunchAd(ctx context.Context, req AdRequest) (providerAdID string, rejected bool, reason string, err error)
	// AdStats returns lifetime stats for one provider ad id.
	AdStats(ctx context.Context, providerAdID string) (Stats, error)
}

// PostRequest is one publish call. Body travels ONLY on this wire — it
// must never appear in CloudEvents/metering payloads (privacy contract).
type PostRequest struct {
	TenantID   string
	AccountID  string
	AccountRef string
	CreativeID string
	Body       string
	MediaURL   string
	Disclaimer string
}

// AdRequest is one launch call.
type AdRequest struct {
	TenantID        string
	AccountID       string
	AccountRef      string
	AdID            string
	CreativeID      string
	Name            string
	Objective       string
	BudgetKobo      int64
	DailyBudgetKobo int64
	Political       bool
	Disclaimer      string
}

// Stats are the (mock-plausible) lifetime ad stats.
type Stats struct {
	Impressions int64 `json:"impressions"`
	Reach       int64 `json:"reach"`
	Clicks      int64 `json:"clicks"`
	SpendKobo   int64 `json:"spend_kobo"`
}

// Error is a provider-side failure (mirrors the notification-worker
// provider.Error shape).
type Error struct {
	StatusCode int
	Body       string
}

func (e *Error) Error() string {
	if e.StatusCode == 0 {
		return "provider unreachable: " + e.Body
	}
	return fmt.Sprintf("provider status %d: %s", e.StatusCode, e.Body)
}

// MockSentinelFailAccount is the account_ref that makes the mock
// PublishPost/LaunchAd fail with a provider error (documented test hook,
// mirroring FCM's "mock-fail" token).
const MockSentinelFailAccount = "mock-fail"

// MockSentinelRejectName is the substring in an ad name that makes the
// mock LaunchAd reject on policy grounds (documented test hook).
const MockSentinelRejectName = "mock-reject"

// MockEnabled resolves the mock switch for one provider: truthy/unset
// master (SOCIAL_MOCK) or truthy/unset per-provider switch → mock. Only
// BOTH explicitly falsy selects the real-API stub.
func MockEnabled(master, perProvider string) bool {
	return truthyDefault(master) || truthyDefault(perProvider)
}

func truthyDefault(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return false
	default: // unset or any truthy spelling → mock (zero-config default)
		return true
	}
}

// New returns the Publisher for a provider id. mock selects the
// deterministic mock; !mock selects the honest real-API stub (not yet
// credential-wired). Unknown provider ids return nil, false.
func New(providerID string, mock bool) (Publisher, bool) {
	switch providerID {
	case "meta":
		return &Meta{mock: mock}, true
	case "tiktok":
		return &TikTok{mock: mock}, true
	case "x":
		return &X{mock: mock}, true
	default:
		return nil, false
	}
}

// ---------------------------------------------------------------------------
// Shared mock machinery
// ---------------------------------------------------------------------------

// mockPublisher carries the deterministic mock behavior shared by all
// three providers (per-provider files only set the name).
type mockPublisher struct {
	name string
}

func (m *mockPublisher) publishPost(req PostRequest) (string, error) {
	if req.AccountRef == MockSentinelFailAccount {
		return "", &Error{StatusCode: 500, Body: m.name + " mock: publish failed (account_ref mock-fail)"}
	}
	if strings.TrimSpace(req.Body) == "" {
		return "", &Error{StatusCode: 400, Body: m.name + " mock: empty post body"}
	}
	key := strings.Join([]string{req.TenantID, req.AccountID, req.CreativeID, req.Body}, "|")
	return fmt.Sprintf("mock-post-%s-%s", m.name, shortHash(key)), nil
}

func (m *mockPublisher) launchAd(req AdRequest) (string, bool, string, error) {
	if req.AccountRef == MockSentinelFailAccount {
		return "", false, "", &Error{StatusCode: 500, Body: m.name + " mock: launch failed (account_ref mock-fail)"}
	}
	if strings.Contains(req.Name, MockSentinelRejectName) {
		return "", true, m.name + " mock: ad rejected by automated policy review (name contains " + MockSentinelRejectName + ")", nil
	}
	key := strings.Join([]string{req.TenantID, req.AccountID, req.AdID}, "|")
	return fmt.Sprintf("mock-ad-%s-%s", m.name, shortHash(key)), false, "", nil
}

func (m *mockPublisher) adStats(providerAdID string) (Stats, error) {
	if strings.TrimSpace(providerAdID) == "" {
		return Stats{}, &Error{StatusCode: 400, Body: m.name + " mock: empty provider ad id"}
	}
	sum := sha256.Sum256([]byte(providerAdID))
	// Plausible funnel: impressions 5k..105k, reach ≤ impressions,
	// clicks ≤ reach, spend derived from impressions at a mock CPM.
	impressions := int64(5000 + binary.BigEndian.Uint64(sum[0:8])%100000)
	reach := impressions * (60 + int64(sum[8])%35) / 100 // 60..94% of impressions
	clicks := reach * (1 + int64(sum[9])%8) / 100        // 1..8% CTR
	spend := impressions * (20 + int64(sum[10])%30)      // ₦0.20..₦0.49 per impression (kobo)
	return Stats{Impressions: impressions, Reach: reach, Clicks: clicks, SpendKobo: spend}, nil
}

func shortHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

// notConfigured is the honest real-API stub failure (same posture as the
// W16 APNs stub: the seam exists, the credential wiring is a documented
// follow-up — no fake implementation claims).
func notConfigured(name string) error {
	return &Error{StatusCode: 0, Body: name + " real API not configured: credential wiring is a follow-up (set " + strings.ToUpper(name) + "_MOCK=1 or see docs/apps/social-publisher.md)"}
}
