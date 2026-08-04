package provider

// Meta (Facebook/Instagram) provider (SPEC-W21 Agent B). Mock is the
// zero-config default (META_MOCK / SOCIAL_MOCK, see the package doc). The
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
