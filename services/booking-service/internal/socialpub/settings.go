package socialpub

// Provider configuration status read API (SPEC-W45 CODER-H item 5 / UC
// social-status): GET /v1/social/settings surfaces the per-provider
// config_status so the admin UI can show an honest "connect provider"
// state instead of implying a live integration (U3 honesty contract).
//
// A provider reports "configured" when a REAL publishing path exists:
//   - the integrator wired a Publisher for it (Deps.Publishers — real
//     credential wiring or an explicit mock publisher selected by the
//     integrator), OR
//   - its mock switch is explicitly on (SOCIAL_MOCK / <PROVIDER>_MOCK,
//     provider.MockEnabledFromEnv) — the deterministic dev path.
//
// Otherwise "not_configured" — the package falls back to the honest
// real-API stub that fails closed on every publish (W39 SIM-005), which
// is exactly the state the UI must render as "not connected".

import (
	"net/http"

	"github.com/opendesk/booking-service/internal/socialpub/provider"
)

// Config statuses (SPEC-W45 UC social-status contract).
const (
	ConfigConfigured    = "configured"
	ConfigNotConfigured = "not_configured"
)

// socialProviders is the fixed provider set, in stable display order.
var socialProviders = []string{"meta", "tiktok", "x"}

// ProviderSetting is one provider's configuration status row.
type ProviderSetting struct {
	Provider     string `json:"provider"`
	ConfigStatus string `json:"config_status"` // configured | not_configured
}

// ProviderSettings is the GET /v1/social/settings response. CODER-I's UI
// consumes map-or-list, so BOTH shapes are emitted: the object is keyed by
// provider id AND carries the same rows as an ordered list.
type ProviderSettings struct {
	Meta      ProviderSetting   `json:"meta"`
	TikTok    ProviderSetting   `json:"tiktok"`
	X         ProviderSetting   `json:"x"`
	Providers []ProviderSetting `json:"providers"`
}

// Settings computes the per-provider configuration posture (pure — unit
// tested without HTTP).
func (d *Deps) Settings() ProviderSettings {
	out := ProviderSettings{Providers: make([]ProviderSetting, 0, len(socialProviders))}
	for _, id := range socialProviders {
		status := ConfigNotConfigured
		if _, wired := d.Publishers[id]; wired || provider.MockEnabledFromEnv(id) {
			status = ConfigConfigured
		}
		row := ProviderSetting{Provider: id, ConfigStatus: status}
		switch id {
		case "meta":
			out.Meta = row
		case "tiktok":
			out.TikTok = row
		case "x":
			out.X = row
		}
		out.Providers = append(out.Providers, row)
	}
	return out
}

// SettingsHandler serves GET /v1/social/settings. The route itself is
// registered by the INTEGRATOR (httpapi/server.go, SPEC-W45 exception:
// the route lives outside RegisterRoutes so the settings read stays
// available with the standard tenant/appgate/perms chain even as the
// package route group evolves).
func SettingsHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, d.Settings())
	}
}
