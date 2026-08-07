// Package config loads kyc-service configuration from environment variables
// (envconfig style, no external dependency — identity-service pattern).
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration for kyc-service.
type Config struct {
	Port            int           // HTTP listen port (PORT, default 7013, SPEC-W12 §5)
	DatabaseURL     string        // postgres DSN for the kyc DB (DATABASE_URL)
	DaprHost        string        // daprd host (default daprd-kyc)
	DaprHTTPPort    int           // daprd HTTP port (default 3500)
	PubSubName      string        // Dapr pubsub component (default pubsub-kafka)
	KYCEventsTopic  string        // Kafka topic for kyc Resolved CloudEvents (SPEC-W12 §5)
	IdentityAppID   string        // Dapr app-id of identity-service (consent gate)
	IdentityBaseURL string        // optional direct base URL of identity-service (tests / no-Dapr dev); when set, Dapr invoke is bypassed
	Mock            bool          // KYC_MOCK (default false, SPEC-W34 GF8): deterministic mock resolver — auto-verifies fabricated IDs, dev only
	ProviderURL     string        // live provider base URL (KYC_PROVIDER_URL, ASSUMPTION — see docs/kyc.md)
	ProviderAPIKey  string        // live provider API key (KYC_PROVIDER_API_KEY, ASSUMPTION)
	ResolveTimeout  time.Duration // per-resolution budget (p95 target <= 8s, docs/kyc.md)
	ShutdownTimeout time.Duration // graceful shutdown budget
}

// Load reads configuration from the environment, applying defaults and
// returning an error when a required variable is missing.
func Load() (Config, error) {
	cfg := Config{
		Port:            envInt("PORT", 7013),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		DaprHost:        envStr("DAPR_HOST", "daprd-kyc"),
		DaprHTTPPort:    envInt("DAPR_HTTP_PORT", 3500),
		PubSubName:      envStr("DAPR_PUBSUB_NAME", "pubsub-kafka"),
		KYCEventsTopic:  envStr("KYC_EVENTS_TOPIC", "opendesk.kyc.resolved.v1"),
		IdentityAppID:   envStr("IDENTITY_APP_ID", "identity"),
		IdentityBaseURL: os.Getenv("IDENTITY_BASE_URL"),
		// SPEC-W34 GF8: mock defaults OFF. The MockResolver auto-verifies any
		// all-digits id with length >= 10, so a default deployment must never
		// silently verify fabricated BVN/NIN — mock is explicit opt-in only.
		Mock:            envBool("KYC_MOCK", false),
		ProviderURL:     os.Getenv("KYC_PROVIDER_URL"),
		ProviderAPIKey:  os.Getenv("KYC_PROVIDER_API_KEY"),
		ResolveTimeout:  time.Duration(envInt("KYC_RESOLVE_TIMEOUT_SECONDS", 8)) * time.Second,
		ShutdownTimeout: time.Duration(envInt("SHUTDOWN_TIMEOUT_SECONDS", 15)) * time.Second,
	}
	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("DATABASE_URL is required")
	}
	return cfg, nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
