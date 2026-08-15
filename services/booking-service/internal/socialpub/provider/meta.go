package provider

// Meta (Facebook/Instagram) provider (SPEC-W21 Agent B). The mock is an
// explicit dev opt-in (META_MOCK / SOCIAL_MOCK, default OFF — see the
// package doc). The
// real-API path is an HONEST STUB: the Meta Marketing API / Graph API
// wiring (app review, pages tokens, and — for political ads — the
// separate Meta political-ads authorization, an EXTERNAL multi-week
// process) is a documented follow-up in docs/apps/social-publisher.md.

import "context"

// Meta implements Publisher for meta.
type Meta struct {
	mock bool
	m    mockPublisher
}

// Name implements Publisher.
func (p *Meta) Name() string { return "meta" }

// IsMock reports the mock posture (metering must not count simulated
// publishes as real usage — W39 SIM-006).
func (p *Meta) IsMock() bool { return p.mock }

func (p *Meta) mockRef() *mockPublisher {
	if p.m.name == "" {
		p.m.name = "meta"
	}
	return &p.m
}

// PublishPost implements Publisher.
func (p *Meta) PublishPost(ctx context.Context, req PostRequest) (string, error) {
	_ = ctx
	if !p.mock {
		return "", notConfigured("meta")
	}
	return p.mockRef().publishPost(req)
}

// LaunchAd implements Publisher.
func (p *Meta) LaunchAd(ctx context.Context, req AdRequest) (string, bool, string, error) {
	_ = ctx
	if !p.mock {
		return "", false, "", notConfigured("meta")
	}
	return p.mockRef().launchAd(req)
}

// AdStats implements Publisher.
func (p *Meta) AdStats(ctx context.Context, providerAdID string) (Stats, error) {
	_ = ctx
	if !p.mock {
		return Stats{}, notConfigured("meta")
	}
	return p.mockRef().adStats(providerAdID)
}
