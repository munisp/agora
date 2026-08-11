// Package config loads crm-sync-service configuration from environment
// variables (envconfig style, no external dependency) — SPEC-CRM §B.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for crm-sync-service.
type Config struct {
	Port                int    // HTTP listen port (PORT, default 7010)
	DatabaseURL         string // postgres DSN for the crm_sync DB (DATABASE_URL)
	DaprHost            string // daprd host (default daprd-crm-sync)
	DaprHTTPPort        int    // daprd HTTP port (default 3500)
	PubSubName          string // Dapr pubsub component (pubsub-kafka)
	TwentyAPIURL        string // Twenty REST base URL (http://twenty-api:3000)
	TwentyAPIKey        string // Bearer token created in Twenty Settings -> API & Webhooks
	TwentyWebhookSecret string // HMAC secret for X-Twenty-Webhook-Signature
	TwentyRatePerMin    int    // token-bucket rate for Twenty REST calls (default 90)
	KafkaBrokers        []string
	IdentityTopic       string // opendesk.identity.events
	BookingTopic        string // opendesk.booking.events
	ConversationTopic   string // opendesk.conversation.events
	QualityTopic        string // opendesk.conversation.quality (CallQualityEnriched, Wave 5 #2)
	CapturesTopic       string // opendesk.conversation.captures (CaptureExtracted, SPEC-W38 F3)
	CaptureGroup        string // crm-sync-capture (own group for the captures topic)
	CRMEventsTopic      string // opendesk.crm.events (reverse webhook intake)
	PrivacyTopic        string // opendesk.privacy.events (GDPR erase tombstones)
	ConsumerGroup       string // crm-sync
	ReverseGroup        string // crm-sync-reverse (Twenty -> OpenDesk worker)
	BookingAppID        string // Dapr app-id of booking-service (default booking)
	EchoWindow          time.Duration // reverse echo suppression window (default 10s)
	DLQTopic            string // opendesk.dlq
	ShutdownTimeout     time.Duration
	ConsumerEnabled     bool // run Kafka consumers (default true)
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		Port:                envInt("PORT", 7010),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		DaprHost:            envStr("DAPR_HOST", "daprd-crm-sync"),
		DaprHTTPPort:        envInt("DAPR_HTTP_PORT", 3500),
		PubSubName:          envStr("DAPR_PUBSUB_NAME", "pubsub-kafka"),
		TwentyAPIURL:        strings.TrimRight(envStr("TWENTY_API_URL", "http://twenty-api:3000"), "/"),
		TwentyAPIKey:        os.Getenv("TWENTY_API_KEY"),
		TwentyWebhookSecret: os.Getenv("TWENTY_WEBHOOK_SECRET"),
		TwentyRatePerMin:    envInt("TWENTY_RATE_PER_MIN", 90),
		KafkaBrokers:        strings.Split(envStr("KAFKA_BROKERS", "kafka:9092"), ","),
		IdentityTopic:       envStr("IDENTITY_EVENTS_TOPIC", "opendesk.identity.events"),
		BookingTopic:        envStr("BOOKING_EVENTS_TOPIC", "opendesk.booking.events"),
		ConversationTopic:   envStr("CONVERSATION_EVENTS_TOPIC", "opendesk.conversation.events"),
		QualityTopic:        envStr("QUALITY_EVENTS_TOPIC", "opendesk.conversation.quality"),
		CapturesTopic:       envStr("CAPTURES_TOPIC", "opendesk.conversation.captures"),
		CaptureGroup:        envStr("CAPTURE_CONSUMER_GROUP", "crm-sync-capture"),
		CRMEventsTopic:      envStr("CRM_EVENTS_TOPIC", "opendesk.crm.events"),
		PrivacyTopic:        envStr("PRIVACY_EVENTS_TOPIC", "opendesk.privacy.events"),
		ConsumerGroup:       envStr("CONSUMER_GROUP", "crm-sync"),
		ReverseGroup:        envStr("REVERSE_CONSUMER_GROUP", "crm-sync-reverse"),
		BookingAppID:        envStr("BOOKING_APP_ID", "booking"),
		EchoWindow:          time.Duration(envInt("REVERSE_ECHO_WINDOW_SECONDS", 10)) * time.Second,
		DLQTopic:            envStr("DLQ_TOPIC", "opendesk.dlq"),
		ShutdownTimeout:     time.Duration(envInt("SHUTDOWN_TIMEOUT_SECONDS", 20)) * time.Second,
		ConsumerEnabled:     envStr("CONSUMER_ENABLED", "true") == "true",
	}
	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.TwentyRatePerMin <= 0 {
		cfg.TwentyRatePerMin = 90
	}
	if cfg.TwentyAPIKey == "" {
		// Not fatal: the service starts (health/metrics work) but Twenty calls
		// will fail fast with a clear error until a real key is configured.
		cfg.TwentyAPIKey = "opendesk-dev-twenty-api-key"
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
