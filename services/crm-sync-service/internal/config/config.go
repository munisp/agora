// Package config loads crm-sync-service configuration from environment
// variables (envconfig style, no external dependency) — SPEC-CRM §B.
package config

import (
	"encoding/json"
	"fmt"
	"github.com/opendesk/crm-sync-service/internal/twentyc"
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
	// Call-summary note sentences (empty URL = facts-only notes).
	ConversationURL string        // CONVERSATION_SERVICE_URL, e.g. http://conversation:7007
	SummaryLLMURL   string        // SUMMARY_LLM_BASE_URL, OpenAI-compatible (e.g. Ollama .../v1)
	SummaryLLMModel string        // SUMMARY_LLM_MODEL
	SummaryTimeout  time.Duration // SUMMARY_TIMEOUT_SECONDS (default 90)
	PrivacyTopic    string        // opendesk.privacy.events (GDPR erase tombstones)
	ConsumerGroup   string        // crm-sync
	ReverseGroup    string        // crm-sync-reverse (Twenty -> OpenDesk worker)
	BookingAppID    string        // Dapr app-id of booking-service (default booking)
	EchoWindow      time.Duration // reverse echo suppression window (default 10s)
	DLQTopic        string        // opendesk.dlq
	ShutdownTimeout time.Duration
	ConsumerEnabled bool   // run Kafka consumers (default true)
	InternalToken   string // CRM_SYNC_INTERNAL_TOKEN (SPEC-W45 K21): X-Internal-Token gate on /v1/tasks + /v1/people/lookup — 503 fail-closed when unset, 401 missing/wrong
	// BookingInternalToken (BOOKING_INTERNAL_TOKEN, SPEC-W80 A11/B7): the
	// reverse worker's invokes of booking-service's internAuth-guarded
	// internal routes (/internal/contacts/upsert,
	// /internal/bookings/{id}/crm-note) carry X-Internal-Token with this
	// value. Reuses booking-service's EXISTING env name verbatim. Empty =>
	// header omitted, peer fails closed (warned at boot).
	BookingInternalToken string
	// TwentyRoutes (TWENTY_ROUTES, 2026-11-23): businesses with their own Twenty instance, as a JSON list
	// [{"name","tenants":[id or slug...],"url","api_key_env","webhook_secret_env","workspace_id"}]. The key and
	// webhook secret are read from the named env vars (never written into the JSON). Unrouted businesses keep
	// the default instance above.
	TwentyRoutes []TwentyRoute

	// Human inbox (Chatwoot, 2026-11-23). INBOX_ROUTES is a JSON list of inbox.Route (parsed in main); empty = off.
	InboxRoutesJSON    string // INBOX_ROUTES
	TranscriptsTopic   string // TRANSCRIPTS_TOPIC (ConversationTurn events)
	InboxGroup         string // INBOX_CONSUMER_GROUP
	GatewayURL         string // MESSAGING_GATEWAY_URL (WhatsApp/Telegram sends)
	InboxWebhookSecret string // INBOX_WEBHOOK_SECRET (path secret of Chatwoot's webhook)
	ConversationToken  string // CONVERSATION_INTERNAL_TOKEN (conversation-service internal routes)
	BookingURL         string // BOOKING_URL (booking-service base, advisor names for person fields)
}

// TwentyRoute is one business-specific Twenty instance (see Config.TwentyRoutes).
type TwentyRoute struct {
	Name             string   `json:"name"`
	Tenants          []string `json:"tenants"`
	URL              string   `json:"url"`
	APIKeyEnv        string   `json:"api_key_env"`
	WebhookSecretEnv string   `json:"webhook_secret_env"`
	WorkspaceID      string   `json:"workspace_id"`
	// PersonFields (2026-11-23): this business's own contact fields filled from bookings and calls.
	PersonFields  *twentyc.PersonFields `json:"person_fields,omitempty"`
	APIKey        string                `json:"-"`
	WebhookSecret string                `json:"-"`
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
		ConversationURL:     envStr("CONVERSATION_SERVICE_URL", ""),
		SummaryLLMURL:       envStr("SUMMARY_LLM_BASE_URL", ""),
		SummaryLLMModel:     envStr("SUMMARY_LLM_MODEL", ""),
		SummaryTimeout:      time.Duration(envInt("SUMMARY_TIMEOUT_SECONDS", 90)) * time.Second,
		PrivacyTopic:        envStr("PRIVACY_EVENTS_TOPIC", "opendesk.privacy.events"),
		ConsumerGroup:       envStr("CONSUMER_GROUP", "crm-sync"),
		ReverseGroup:        envStr("REVERSE_CONSUMER_GROUP", "crm-sync-reverse"),
		BookingAppID:        envStr("BOOKING_APP_ID", "booking"),
		EchoWindow:          time.Duration(envInt("REVERSE_ECHO_WINDOW_SECONDS", 10)) * time.Second,
		DLQTopic:            envStr("DLQ_TOPIC", "opendesk.dlq"),
		ShutdownTimeout:     time.Duration(envInt("SHUTDOWN_TIMEOUT_SECONDS", 20)) * time.Second,
		ConsumerEnabled:     envStr("CONSUMER_ENABLED", "true") == "true",
		// SPEC-W45 K21 (OOS-09): service-to-service gate for the /v1 surface.
		InternalToken: os.Getenv("CRM_SYNC_INTERNAL_TOKEN"),
		// SPEC-W80 A11 (B7 caller side): X-Internal-Token for the reverse
		// worker's booking-service internal invokes. Same shared secret as
		// the booking block's BOOKING_INTERNAL_TOKEN.
		BookingInternalToken: os.Getenv("BOOKING_INTERNAL_TOKEN"),
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
	cfg.InboxRoutesJSON = strings.TrimSpace(os.Getenv("INBOX_ROUTES"))
	cfg.TranscriptsTopic = envStr("TRANSCRIPTS_TOPIC", "opendesk.conversation.transcripts")
	cfg.InboxGroup = envStr("INBOX_CONSUMER_GROUP", "crm-sync-inbox")
	cfg.GatewayURL = envStr("MESSAGING_GATEWAY_URL", "")
	cfg.InboxWebhookSecret = envStr("INBOX_WEBHOOK_SECRET", "")
	cfg.ConversationToken = envStr("CONVERSATION_INTERNAL_TOKEN", "")
	cfg.BookingURL = envStr("BOOKING_URL", "")
	if raw := strings.TrimSpace(os.Getenv("TWENTY_ROUTES")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.TwentyRoutes); err != nil {
			return cfg, fmt.Errorf("TWENTY_ROUTES is not valid JSON: %w", err)
		}
		for i := range cfg.TwentyRoutes {
			r := &cfg.TwentyRoutes[i]
			r.URL = strings.TrimRight(r.URL, "/")
			if r.URL == "" || len(r.Tenants) == 0 || r.APIKeyEnv == "" {
				return cfg, fmt.Errorf("TWENTY_ROUTES[%d] (%s) needs url, tenants and api_key_env", i, r.Name)
			}
			r.APIKey = os.Getenv(r.APIKeyEnv)
			if r.APIKey == "" {
				return cfg, fmt.Errorf("TWENTY_ROUTES[%d] (%s): env %s is empty", i, r.Name, r.APIKeyEnv)
			}
			if r.WebhookSecretEnv != "" {
				r.WebhookSecret = os.Getenv(r.WebhookSecretEnv)
			}
		}
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
