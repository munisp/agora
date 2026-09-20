package socialpub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opendesk/booking-service/internal/socialpub/provider"
)

// SPEC-W45 item 5: GET /v1/social/settings reports the honest per-provider
// config posture (configured | not_configured), keyed by provider with a
// providers list (CODER-I consumes map-or-list).

func TestSettingsNotConfiguredByDefault(t *testing.T) {
	t.Setenv("SOCIAL_MOCK", "")
	t.Setenv("META_MOCK", "")
	t.Setenv("TIKTOK_MOCK", "")
	t.Setenv("X_MOCK", "")
	d := &Deps{}
	s := d.Settings()
	if len(s.Providers) != 3 {
		t.Fatalf("providers = %d, want 3", len(s.Providers))
	}
	for _, p := range s.Providers {
		if p.ConfigStatus != ConfigNotConfigured {
			t.Fatalf("%s = %q, want not_configured (zero-config must be honest)", p.Provider, p.ConfigStatus)
		}
	}
	if s.Meta.ConfigStatus != ConfigNotConfigured || s.TikTok.ConfigStatus != ConfigNotConfigured || s.X.ConfigStatus != ConfigNotConfigured {
		t.Fatalf("keyed rows = %+v", s)
	}
}

func TestSettingsConfiguredStates(t *testing.T) {
	t.Setenv("SOCIAL_MOCK", "")
	t.Setenv("META_MOCK", "")
	t.Setenv("TIKTOK_MOCK", "")
	t.Setenv("X_MOCK", "1") // explicit per-provider mock opt-in

	// meta: integrator-wired publisher; x: env mock; tiktok: neither.
	metaPub, ok := provider.New("meta", true)
	if !ok {
		t.Fatal("provider.New meta")
	}
	d := &Deps{Publishers: map[string]provider.Publisher{"meta": metaPub}}
	s := d.Settings()
	if s.Meta.ConfigStatus != ConfigConfigured {
		t.Fatalf("meta = %q, want configured (integrator-wired)", s.Meta.ConfigStatus)
	}
	if s.X.ConfigStatus != ConfigConfigured {
		t.Fatalf("x = %q, want configured (X_MOCK=1)", s.X.ConfigStatus)
	}
	if s.TikTok.ConfigStatus != ConfigNotConfigured {
		t.Fatalf("tiktok = %q, want not_configured", s.TikTok.ConfigStatus)
	}
}

func TestSettingsHandlerServesBothShapes(t *testing.T) {
	t.Setenv("SOCIAL_MOCK", "1") // master switch → every provider configured
	d := &Deps{}
	rec := httptest.NewRecorder()
	SettingsHandler(d).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/social/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Meta      ProviderSetting   `json:"meta"`
		Providers []ProviderSetting `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Meta.ConfigStatus != ConfigConfigured {
		t.Fatalf("meta keyed row = %+v, want configured (SOCIAL_MOCK=1)", body.Meta)
	}
	if len(body.Providers) != 3 || body.Providers[0].Provider != "meta" {
		t.Fatalf("providers list = %+v, want ordered meta/tiktok/x", body.Providers)
	}
	for _, p := range body.Providers {
		if p.ConfigStatus != ConfigConfigured {
			t.Fatalf("%s = %q, want configured (master mock on)", p.Provider, p.ConfigStatus)
		}
	}
}
