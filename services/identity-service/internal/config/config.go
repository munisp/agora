// Package config loads identity-service configuration from environment
// variables (envconfig style, no external dependency).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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
	PrivacyEventsTopic   string        // Kafka topic for K4 PrivacyEraseRequested tombstones (PRIVACY_EVENTS_TOPIC)
	InternalToken        string        // K2: X-Internal-Token gate for /internal/* (IDENTITY_INTERNAL_TOKEN; unset = fail-closed 503)
	InternalDatabaseURL  string        // optional INTERNAL_DATABASE_URL — app_identity_internal member (RLS escape)
	PlatformAdmins       []string      // OPENDESK_PLATFORM_ADMINS csv of platform-admin subjects (SPEC-W43 I-01)
	TrustDirectTenancy   bool          // OPENDESK_TRUST_DIRECT_TENANT=1 — logged dev escape for gateway-less runs (K1; SPEC-W44 F4 / V2-D3)
	ConsentRelayInterval time.Duration // consent erasure outbox relay sweep interval
	AppsLifecycleTopic   string        // Kafka topic for app lifecycle CloudEvents (SPEC-W18 §1)
	IndustriesDir        string        // mounted industry packs dir (INDUSTRIES_DIR, default /industries)
	// SPEC-W45 K10: realm SMTP bootstrap. When RealmSMTPHost is set the
	// bootstrap PATCHes the Keycloak realm smtpServer map (fail-soft warn
	// otherwise) so Keycloak's own credentials e-mails (K8
	// execute-actions-email) can fire.
	RealmSMTPHost     string // KC_REALM_SMTP_HOST
	RealmSMTPPort     string // KC_REALM_SMTP_PORT
	RealmSMTPFrom     string // KC_REALM_SMTP_FROM
	RealmSMTPUser     string // KC_REALM_SMTP_USER
	RealmSMTPPassword string // KC_REALM_SMTP_PASSWORD
	// SPEC-W45 STK O13: booking portal JWT HMAC secret (PORTAL_SECRET, shared
	// with booking-service). Lets data subjects self-serve consent
	// data-access/erasure with their portal session. Unset = portal path
	// unavailable (fail-closed).
	PortalSecret string
	// SPEC-W45 K contract note: billing plan push. BillingURL empty disables
	// the push (dev default); BillingInternalToken is forwarded as
	// X-Internal-Token (billing-engine RS-002 gate).
	BillingURL           string        // BILLING_URL
	BillingInternalToken string        // BILLING_INTERNAL_TOKEN
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
		PrivacyEventsTopic:   envStr("PRIVACY_EVENTS_TOPIC", "opendesk.privacy.events"),
		InternalToken:        os.Getenv("IDENTITY_INTERNAL_TOKEN"),
		InternalDatabaseURL:  os.Getenv("INTERNAL_DATABASE_URL"),
		PlatformAdmins:       envCSV("OPENDESK_PLATFORM_ADMINS"),
		TrustDirectTenancy:   envBool("OPENDESK_TRUST_DIRECT_TENANT"),
		ConsentRelayInterval: time.Duration(envInt("CONSENT_OUTBOX_RELAY_INTERVAL_SECONDS", 10)) * time.Second,
		AppsLifecycleTopic:   envStr("APPS_LIFECYCLE_TOPIC", "opendesk.apps.lifecycle.v1"),
		IndustriesDir:        envStr("INDUSTRIES_DIR", "/industries"),
		RealmSMTPHost:        os.Getenv("KC_REALM_SMTP_HOST"),
		RealmSMTPPort:        envStr("KC_REALM_SMTP_PORT", "587"),
		RealmSMTPFrom:        os.Getenv("KC_REALM_SMTP_FROM"),
		RealmSMTPUser:        os.Getenv("KC_REALM_SMTP_USER"),
		RealmSMTPPassword:    os.Getenv("KC_REALM_SMTP_PASSWORD"),
		PortalSecret:         os.Getenv("PORTAL_SECRET"),
		BillingURL:           os.Getenv("BILLING_URL"),
		BillingInternalToken: os.Getenv("BILLING_INTERNAL_TOKEN"),
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

func envCSV(key string) []string {
	var out []string
	for _, p := range strings.Split(os.Getenv(key), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envBool reports whether the variable is set to a truthy value ("1"/"true",
// case-insensitive) — the explicit dev-escape idiom (default false).
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true":
		return true
	}
	return false
}
