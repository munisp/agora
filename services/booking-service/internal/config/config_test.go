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

// SPEC-W49 B1/B2 additive envs: payments-events consumer topic/group
// defaults and the pay-redirect allow-list csv parse (fail-closed empty).
func TestW49PaymentEnvs(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x:x@localhost/x")
	t.Setenv("PORTAL_SECRET", "prod-secret-9f2c")
	t.Setenv("PAYMENTS_EVENTS_TOPIC", "")
	t.Setenv("PAYMENTS_EVENTS_GROUP", "")
	t.Setenv("PUBLIC_PAY_REDIRECT_ALLOW_HOSTS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PaymentsEventsTopic != "opendesk.payments.events" || cfg.PaymentsEventsGroup != "booking-service-payments-events" {
		t.Fatalf("payments consumer defaults = %q/%q", cfg.PaymentsEventsTopic, cfg.PaymentsEventsGroup)
	}
	if len(cfg.PayRedirectAllowHosts) != 0 {
		t.Fatalf("unset allow-list must stay empty (fail closed): %v", cfg.PayRedirectAllowHosts)
	}

	t.Setenv("PAYMENTS_EVENTS_TOPIC", "custom.payments")
	t.Setenv("PAYMENTS_EVENTS_GROUP", "g1")
	t.Setenv("PUBLIC_PAY_REDIRECT_ALLOW_HOSTS", " Pay.Acme.NG , *.opendesk.ng ,, ")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PaymentsEventsTopic != "custom.payments" || cfg.PaymentsEventsGroup != "g1" {
		t.Fatalf("payments consumer envs not loaded: %q/%q", cfg.PaymentsEventsTopic, cfg.PaymentsEventsGroup)
	}
	want := []string{"pay.acme.ng", "*.opendesk.ng"}
	if len(cfg.PayRedirectAllowHosts) != len(want) {
		t.Fatalf("allow-list = %v, want %v", cfg.PayRedirectAllowHosts, want)
	}
	for i := range want {
		if cfg.PayRedirectAllowHosts[i] != want[i] {
			t.Fatalf("allow-list[%d] = %q, want %q (trimmed/lowercased)", i, cfg.PayRedirectAllowHosts[i], want[i])
		}
	}
}

// SPEC-W53 Scope B: OCR envs default EMPTY (fail-closed — the receipt
// endpoint then answers 503) and pass through when set; the W52 residual
// GEO fields pass through for the main.go Deps wiring (previously read as
// env fallbacks inside httpapi.NewRouter — same values, behavior-identical).
func TestW53OCRAndGeoEnvs(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x:x@localhost/x")
	t.Setenv("PORTAL_SECRET", "prod-secret-9f2c")
	t.Setenv("OCR_SERVICE_URL", "")
	t.Setenv("OCR_INTERNAL_TOKEN", "")
	t.Setenv("GEO_SERVICE_URL", "")
	t.Setenv("BOOKING_GEO_ENFORCE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OCRServiceURL != "" || cfg.OCRInternalToken != "" {
		t.Fatalf("OCR envs must default empty (fail closed): %+v", cfg)
	}
	if cfg.GeoServiceURL != "" || cfg.BookingGeoEnforce {
		t.Fatalf("GEO defaults = %q/%v, want empty/false", cfg.GeoServiceURL, cfg.BookingGeoEnforce)
	}

	t.Setenv("OCR_SERVICE_URL", "http://ocr:7020")
	t.Setenv("OCR_INTERNAL_TOKEN", "ocr-tok")
	t.Setenv("GEO_SERVICE_URL", "http://geo-service:7010")
	t.Setenv("BOOKING_GEO_ENFORCE", "true")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OCRServiceURL != "http://ocr:7020" || cfg.OCRInternalToken != "ocr-tok" {
		t.Fatalf("OCR envs not loaded: %+v", cfg)
	}
	if cfg.GeoServiceURL != "http://geo-service:7010" || !cfg.BookingGeoEnforce {
		t.Fatalf("GEO pass-through not loaded: %q/%v", cfg.GeoServiceURL, cfg.BookingGeoEnforce)
	}
}
