// Package config loads identity-service configuration from environment
// variables (envconfig style, no external dependency).
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration for identity-service.
type Config struct {
	Port                 int           // HTTP listen port (PORT, default 7001)
	DatabaseURL          string        // postgres DSN for the identity DB (DATABASE_URL)
	KeycloakURL          string        // base URL of Keycloak, e.g. http://keycloak:8080
	KeycloakRealm        string        // realm name (default opendesk)
	KeycloakClientID     string        // admin client id for client_credentials
	KeycloakClientSecret string        // admin client secret
	PermifyURL           string        // Permify HTTP API base, e.g. http://permify:3476
	DaprHost             string        // daprd host (default daprd-identity)
	DaprHTTPPort         int           // daprd HTTP port (default 3500)
	PubSubName           string        // Dapr pubsub component (default pubsub-kafka)
	IdentityEventsTopic  string        // Kafka topic for identity events
	NotificationAppID    string        // Dapr app-id of notification-worker (onboarding trigger)
	ConsentErasureTopic  string        // Kafka topic for consent erasure CloudEvents (SPEC-W12 §4)
	AppsLifecycleTopic   string        // Kafka topic for app lifecycle CloudEvents (SPEC-W18 §1)
	IndustriesDir        string        // mounted industry packs dir (INDUSTRIES_DIR, default /industries)
	ShutdownTimeout      time.Duration // graceful shutdown budget
}

// Load reads configuration from the environment, applying defaults and
// returning an error when a required variable is missing.
func Load() (Config, error) {
	cfg := Config{
		Port:                 envInt("PORT", 7001),
		DatabaseURL:          os.Getenv("DATABASE_URL"),
		KeycloakURL:          envStr("KEYCLOAK_URL", "http://keycloak:8080"),
		KeycloakRealm:        envStr("KEYCLOAK_REALM", "opendesk"),
		KeycloakClientID:     os.Getenv("KEYCLOAK_ADMIN_CLIENT_ID"),
		KeycloakClientSecret: os.Getenv("KEYCLOAK_ADMIN_CLIENT_SECRET"),
		PermifyURL:           envStr("PERMIFY_URL", "http://permify:3476"),
		DaprHost:             envStr("DAPR_HOST", "daprd-identity"),
		DaprHTTPPort:         envInt("DAPR_HTTP_PORT", 3500),
		PubSubName:           envStr("DAPR_PUBSUB_NAME", "pubsub-kafka"),
		IdentityEventsTopic:  envStr("IDENTITY_EVENTS_TOPIC", "opendesk.identity.events"),
		NotificationAppID:    envStr("NOTIFICATION_APP_ID", "notification"),
		ConsentErasureTopic:  envStr("CONSENT_ERASURE_TOPIC", "opendesk.consent.erasure.v1"),
		AppsLifecycleTopic:   envStr("APPS_LIFECYCLE_TOPIC", "opendesk.apps.lifecycle.v1"),
		IndustriesDir:        envStr("INDUSTRIES_DIR", "/industries"),
		ShutdownTimeout:      time.Duration(envInt("SHUTDOWN_TIMEOUT_SECONDS", 15)) * time.Second,
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
