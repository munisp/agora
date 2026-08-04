package provider

// X (Twitter) provider (SPEC-W21 Agent B). Mock is the zero-config
// default (X_MOCK / SOCIAL_MOCK, see the package doc). The real-API path
// is an HONEST STUB: X Ads API wiring (developer account, OAuth tokens)
// is a documented follow-up in docs/apps/social-publisher.md.

import "context"

// X implements Publisher for x.
type X struct {
	mock bool
	m    mockPublisher
}

// Name implements Publisher.
func (p *X) Name() string { return "x" }

func (p *X) mockRef() *mockPublisher {
	if p.m.name == "" {
		p.m.name = "x"
	}
	return &p.m
}

// PublishPost implements Publisher.
func (p *X) PublishPost(ctx context.Context, req PostRequest) (string, error) {
	_ = ctx
	if !p.mock {
		return "", notConfigured("x")
	}
	return p.mockRef().publishPost(req)
}

// LaunchAd implements Publisher.
func (p *X) LaunchAd(ctx context.Context, req AdRequest) (string, bool, string, error) {
	_ = ctx
	if !p.mock {
		return "", false, "", notConfigured("x")
	}
	return p.mockRef().launchAd(req)
}

// AdStats implements Publisher.
func (p *X) AdStats(ctx context.Context, providerAdID string) (Stats, error) {
	_ = ctx
	if !p.mock {
		return Stats{}, notConfigured("x")
	}
	return p.mockRef().adStats(providerAdID)
}
