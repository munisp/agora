package provider

// Provider seam tests (no network): mock determinism, documented test
// hooks (mock-fail / mock-reject sentinels), MockEnabled switch
// resolution, and the honest not-configured posture of the real stubs.

import (
	"context"
	"strings"
	"testing"
)

func TestMockPublishPostDeterministic(t *testing.T) {
	p, ok := New("meta", true)
	if !ok || p.Name() != "meta" {
		t.Fatalf("New(meta) = %v, %v", p, ok)
	}
	req := PostRequest{
		TenantID:   "t-1",
		AccountID:  "a-1",
		AccountRef: "acct-1",
		CreativeID: "c-1",
		Body:       "hello world",
	}
	id1, err := p.PublishPost(context.Background(), req)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !strings.HasPrefix(id1, "mock-post-meta-") {
		t.Fatalf("id = %q, want mock-post-meta-*", id1)
	}
	id2, err := p.PublishPost(context.Background(), req)
	if err != nil || id2 != id1 {
		t.Fatalf("not deterministic: %q vs %q (%v)", id1, id2, err)
	}
	// Different body → different id.
	req.Body = "different"
	id3, err := p.PublishPost(context.Background(), req)
	if err != nil || id3 == id1 {
		t.Fatalf("body change should change id: %q vs %q", id1, id3)
	}
}

func TestMockSentinels(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"meta", "tiktok", "x"} {
		p, _ := New(name, true)

		// mock-fail account_ref → provider error.
		_, err := p.PublishPost(ctx, PostRequest{AccountRef: MockSentinelFailAccount, Body: "x"})
		if err == nil {
			t.Fatalf("%s: mock-fail publish should error", name)
		}
		_, _, _, err = p.LaunchAd(ctx, AdRequest{AccountRef: MockSentinelFailAccount})
		if err == nil {
			t.Fatalf("%s: mock-fail launch should error", name)
		}

		// mock-reject ad name → policy rejection (not an error).
		id, rejected, reason, err := p.LaunchAd(ctx, AdRequest{
			Name: "promo " + MockSentinelRejectName, TenantID: "t", AccountID: "a", AdID: "ad",
		})
		if err != nil || !rejected || reason == "" || id != "" {
			t.Fatalf("%s: mock-reject = (%q,%v,%q,%v)", name, id, rejected, reason, err)
		}

		// Happy path launch → mock-ad-<provider>-*.
		id, rejected, _, err = p.LaunchAd(ctx, AdRequest{
			Name: "promo", TenantID: "t", AccountID: "a", AdID: "ad",
		})
		if err != nil || rejected || !strings.HasPrefix(id, "mock-ad-"+name+"-") {
			t.Fatalf("%s: launch = (%q,%v,%v)", name, id, rejected, err)
		}

		// Stats: deterministic + plausible funnel.
		s1, err := p.AdStats(ctx, id)
		if err != nil {
			t.Fatalf("%s: stats: %v", name, err)
		}
		s2, _ := p.AdStats(ctx, id)
		if s1 != s2 {
			t.Fatalf("%s: stats not deterministic: %+v vs %+v", name, s1, s2)
		}
		if s1.Impressions <= 0 || s1.Reach > s1.Impressions || s1.Clicks > s1.Reach || s1.SpendKobo <= 0 {
			t.Fatalf("%s: implausible stats: %+v", name, s1)
		}
	}
}

func TestMockEnabled(t *testing.T) {
	cases := []struct {
		master, per string
		want        bool
	}{
		{"", "", true},    // zero-config default
		{"1", "1", true},  // explicit mocks
		{"0", "1", true},  // per-provider wins
		{"1", "0", true},  // master wins
		{"0", "0", false}, // BOTH falsy → real stub
		{"false", "no", false},
	}
	for _, tc := range cases {
		if got := MockEnabled(tc.master, tc.per); got != tc.want {
			t.Fatalf("MockEnabled(%q,%q) = %v, want %v", tc.master, tc.per, got, tc.want)
		}
	}
}

// The real-API stubs are honest: every call refuses with "not configured"
// (no fake success claims), pointing at the docs runbook.
func TestRealStubsNotConfigured(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"meta", "tiktok", "x"} {
		p, ok := New(name, false)
		if !ok {
			t.Fatalf("New(%s, false) missing", name)
		}
		if _, err := p.PublishPost(ctx, PostRequest{Body: "x"}); err == nil ||
			!strings.Contains(err.Error(), "not configured") {
			t.Fatalf("%s publish stub: %v", name, err)
		}
		if _, _, _, err := p.LaunchAd(ctx, AdRequest{}); err == nil ||
			!strings.Contains(err.Error(), "not configured") {
			t.Fatalf("%s launch stub: %v", name, err)
		}
		if _, err := p.AdStats(ctx, "id"); err == nil ||
			!strings.Contains(err.Error(), "not configured") {
			t.Fatalf("%s stats stub: %v", name, err)
		}
	}
	if _, ok := New("myspace", true); ok {
		t.Fatalf("unknown provider should return ok=false")
	}
}
