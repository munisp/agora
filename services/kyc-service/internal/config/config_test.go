package config

import "testing"

// SPEC-W34 GF8: KYC_MOCK must default to false — the MockResolver
// auto-verifies any all-digits BVN/NIN (len >= 10), so a default
// deployment must never silently verify fabricated IDs.

func TestLoadMockDefaultsFalse(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://kyc:kyc@localhost:5432/kyc")
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
	t.Setenv("DATABASE_URL", "postgres://kyc:kyc@localhost:5432/kyc")
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
	t.Setenv("DATABASE_URL", "postgres://kyc:kyc@localhost:5432/kyc")
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
