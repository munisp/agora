package config

import (
	"strings"
	"testing"
)

// SPEC-W44 W-B (F16-5/S1-F6-02): PORTAL_SECRET fail-closed — booting with
// the checked-in dev default is a startup error unless OPENDESK_DEV_INSECURE=1
// (gateway-edge config.rs validate() pattern).

func TestPortalSecretFailClosed(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x:x@localhost/x")

	// Default (dev fallback) without the dev opt-in → startup error.
	t.Setenv("PORTAL_SECRET", "")
	t.Setenv(EnvDevInsecure, "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PORTAL_SECRET") {
		t.Fatalf("default PORTAL_SECRET without dev opt-in: err = %v, want fail-closed startup error", err)
	}

	// The explicit dev opt-in keeps the local boot working.
	t.Setenv(EnvDevInsecure, "1")
	if _, err := Load(); err != nil {
		t.Fatalf("OPENDESK_DEV_INSECURE=1 must keep the dev default bootable: %v", err)
	}

	// A real secret boots without the opt-in.
	t.Setenv("PORTAL_SECRET", "prod-secret-9f2c")
	t.Setenv(EnvDevInsecure, "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("real PORTAL_SECRET: %v", err)
	}
	if cfg.PortalSecret != "prod-secret-9f2c" {
		t.Fatalf("PortalSecret = %q", cfg.PortalSecret)
	}
}

// K2 wiring: the internal tokens have NO defaults (fail closed at the
// guarded route, never a silent checked-in token).
func TestInternalTokenDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x:x@localhost/x")
	t.Setenv("PORTAL_SECRET", "prod-secret-9f2c")
	t.Setenv("BOOKING_INTERNAL_TOKEN", "")
	t.Setenv("IDENTITY_INTERNAL_TOKEN", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.InternalToken != "" || cfg.IdentityInternalToken != "" {
		t.Fatalf("internal tokens must default empty (fail closed): %+v", cfg)
	}
	t.Setenv("BOOKING_INTERNAL_TOKEN", "booking-tok")
	t.Setenv("IDENTITY_INTERNAL_TOKEN", "identity-tok")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.InternalToken != "booking-tok" || cfg.IdentityInternalToken != "identity-tok" {
		t.Fatalf("internal tokens not loaded: %+v", cfg)
	}
}

// W-B additive envs: SoD role gate + commission maturity defaults.
func TestWBAdditiveEnvs(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x:x@localhost/x")
	t.Setenv("PORTAL_SECRET", "prod-secret-9f2c")
	t.Setenv("LENDING_KYC_OVERRIDE_ROLES", "")
	t.Setenv("COMMISSION_MATURITY_SECONDS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LendingKYCOverrideRoles != "platform-admin" {
		t.Fatalf("LendingKYCOverrideRoles default = %q, want platform-admin", cfg.LendingKYCOverrideRoles)
	}
	if cfg.CommissionMaturitySeconds != 0 {
		t.Fatalf("CommissionMaturitySeconds default = %d, want 0", cfg.CommissionMaturitySeconds)
	}
	t.Setenv("LENDING_KYC_OVERRIDE_ROLES", "platform-admin,risk-officer")
	t.Setenv("COMMISSION_MATURITY_SECONDS", "86400")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LendingKYCOverrideRoles != "platform-admin,risk-officer" || cfg.CommissionMaturitySeconds != 86400 {
		t.Fatalf("additive envs not loaded: %+v", cfg)
	}
}
