package config

import "testing"

// setBaseEnv provisions the mandatory env for Load (SPEC-W45 K22 added
// KYC_HASH_SECRET, fail-closed unless OPENDESK_DEV_INSECURE=1).
func setBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://kyc:kyc@localhost:5432/kyc")
	t.Setenv("KYC_HASH_SECRET", "test-hash-secret")
}

// SPEC-W34 GF8: KYC_MOCK must default to false — the MockResolver
// auto-verifies any all-digits BVN/NIN (len >= 10), so a default
// deployment must never silently verify fabricated IDs.

func TestLoadMockDefaultsFalse(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("KYC_MOCK", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mock {
		t.Errorf("KYC_MOCK unset must default to false (safe default), got true")
	}
}

func TestLoadMockExplicitOptIn(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("KYC_MOCK", "1")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Mock {
		t.Errorf("KYC_MOCK=1 must enable the mock resolver")
	}
}

func TestLoadMockExplicitFalse(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("KYC_MOCK", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Mock {
		t.Errorf("KYC_MOCK=0 must disable the mock resolver")
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Errorf("Load without DATABASE_URL must fail")
	}
}

// SPEC-W45 K22 (OOS-17): KYC_HASH_SECRET is fail-closed — required unless
// the explicit OPENDESK_DEV_INSECURE=1 dev opt-in is set.

func TestLoadHashSecretFailClosedUnset(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://kyc:kyc@localhost:5432/kyc")
	t.Setenv("KYC_HASH_SECRET", "")
	t.Setenv("OPENDESK_DEV_INSECURE", "")
	if _, err := Load(); err == nil {
		t.Errorf("Load without KYC_HASH_SECRET (and no dev opt-in) must fail")
	}
}

func TestLoadHashSecretDevInsecureFallback(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://kyc:kyc@localhost:5432/kyc")
	t.Setenv("KYC_HASH_SECRET", "")
	t.Setenv("OPENDESK_DEV_INSECURE", "1")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with dev opt-in: %v", err)
	}
	if cfg.HashSecret != DevHashSecretDefault {
		t.Errorf("dev fallback secret = %q, want DevHashSecretDefault", cfg.HashSecret)
	}
}

func TestLoadHashSecretConfigured(t *testing.T) {
	setBaseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HashSecret != "test-hash-secret" {
		t.Errorf("HashSecret = %q", cfg.HashSecret)
	}
}

func TestLoadReadsInternalTokenAndEventsTopic(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("KYC_INTERNAL_TOKEN", "tok-1")
	t.Setenv("KYC_EVENTS_TOPIC", "opendesk.kyc.resolved.v9")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.InternalToken != "tok-1" {
		t.Errorf("InternalToken = %q (KYC_INTERNAL_TOKEN)", cfg.InternalToken)
	}
	if cfg.KYCEventsTopic != "opendesk.kyc.resolved.v9" {
		t.Errorf("KYCEventsTopic = %q (KYC_EVENTS_TOPIC, ORPH O12)", cfg.KYCEventsTopic)
	}
}
