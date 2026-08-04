package provider

// TikTok provider (SPEC-W21 Agent B). Mock is the zero-config default
// (TIKTOK_MOCK / SOCIAL_MOCK, see the package doc). The real-API path is
// an HONEST STUB: TikTok Marketing API wiring (app approval, advertiser
// access tokens) is a documented follow-up in
// docs/apps/social-publisher.md.

import "context"

// TikTok implements Publisher for tiktok.
type TikTok struct {
	mock bool
	m    mockPublisher
}

// Name implements Publisher.
func (p *TikTok) Name() string { return "tiktok" }

func (p *TikTok) mockRef() *mockPublisher {
	if p.m.name == "" {
		p.m.name = "tiktok"
	}
	return &p.m
}

// PublishPost implements Publisher.
func (p *TikTok) PublishPost(ctx context.Context, req PostRequest) (string, error) {
	_ = ctx
	if !p.mock {
		return "", notConfigured("tiktok")
	}
	return p.mockRef().publishPost(req)
}

// LaunchAd implements Publisher.
func (p *TikTok) LaunchAd(ctx context.Context, req AdRequest) (string, bool, string, error) {
	_ = ctx
	if !p.mock {
		return "", false, "", notConfigured("tiktok")
	}
	return p.mockRef().launchAd(req)
}

// AdStats implements Publisher.
func (p *TikTok) AdStats(ctx context.Context, providerAdID string) (Stats, error) {
	_ = ctx
	if !p.mock {
		return Stats{}, notConfigured("tiktok")
	}
	return p.mockRef().adStats(providerAdID)
}
