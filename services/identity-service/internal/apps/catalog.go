package apps

import (
	_ "embed"
	"fmt"

	"gopkg.in/yaml.v3"
)

// catalogYAML is the embedded app catalog (SPEC-W18 §3). The FILE CONTENT is
// owned by Wave-18 Agent C (all 16 contract apps); the loader here is
// content-agnostic — any number of rows loads without a code change.
//
// Yaml shape (contract §1 fields, one list entry per app):
//
//	apps:
//	  - app_id: receptionist
//	    name: AI Receptionist
//	    version: "1.0.0"
//	    description: ...
//	    category: ...
//	    icon: ...
//	    nav_route: /app/{org}/receptionist
//	    required_perms: [manage_bookings]
//	    default_plan_tier: starter|growth|scale
//	    backend_note: ...
//
//go:embed catalog.yaml
var catalogYAML []byte

type catalogFile struct {
	Apps []PlatformApp `yaml:"apps"`
}

// LoadCatalog parses and validates the embedded catalog.yaml. A malformed
// file is a boot-fatal programming error, so this returns an error rather
// than degrading (packs.Load idiom).
func LoadCatalog() ([]PlatformApp, error) {
	var f catalogFile
	if err := yaml.Unmarshal(catalogYAML, &f); err != nil {
		return nil, fmt.Errorf("parse embedded catalog.yaml: %w", err)
	}
	if len(f.Apps) == 0 {
		return nil, fmt.Errorf("embedded catalog.yaml contains no apps")
	}
	seen := map[string]bool{}
	for i, a := range f.Apps {
		if a.AppID == "" {
			return nil, fmt.Errorf("catalog app #%d: app_id is required", i)
		}
		if a.Name == "" {
			return nil, fmt.Errorf("catalog app %q: name is required", a.AppID)
		}
		if seen[a.AppID] {
			return nil, fmt.Errorf("catalog app %q: duplicate app_id", a.AppID)
		}
		seen[a.AppID] = true
		// Plan tiers: billing-engine's real tiers free|standard|pro are the
		// shipped values (Agent C's catalog; the SPEC contract names map
		// starter→free, growth→standard, scale→pro). Both spellings are
		// accepted so either catalog vintage loads without a code change.
		switch a.DefaultPlanTier {
		case "", "free", "standard", "pro", "starter", "growth", "scale":
		default:
			return nil, fmt.Errorf("catalog app %q: default_plan_tier must be free|standard|pro", a.AppID)
		}
	}
	return f.Apps, nil
}
