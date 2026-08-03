// Package config loads notification-worker configuration from environment
// variables (envconfig style, no external dependency).
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for notification-worker.
type Config struct {
	Port               int    // HTTP listen port for /healthz + /dev endpoints (7003)
	TemporalHostPort   string // temporal:7233
	TemporalNamespace  string // opendesk
	TemporalTaskQueue  string // opendesk-main
	DaprHost           string // daprd-notification
	DaprHTTPPort       int    // 3500
	BookingAppID       string // Dapr app-id of booking-service
	PaymentsAppID      string // Dapr app-id of payments-service
	IdentityAppID      string // Dapr app-id of identity-service
	KnowledgeAppID     string // Dapr app-id of knowledge-service (pack knowledge seed)
	CRMSyncAppID       string // Dapr app-id of crm-sync-service (CRM task helper)
	PubSubName         string // Dapr pubsub component for CRM events
	CRMEventsTopic     string // topic for escalation priority flags
	IndustriesDir      string // mounted industry packs dir (INDUSTRIES_DIR, default /industries)
	SMTPBinding        string // Dapr output binding for email (bindings-smtp)
	TwilioBinding      string // Dapr output binding for SMS (bindings-twilio)
	// Messaging channel routing (docs/integrations/messaging-channels.md).
	MessagingChannels string // MESSAGING_CHANNELS: "email:smtp,sms:twilio"
	TenantChannelMap  string // TENANT_CHANNEL_MAP: per-tenant provider JSON
	SMTPFrom           string // sender address
	TwilioFrom         string // sender phone number
	OpenSearchURL      string // used by the tenant onboarding search-alias activity
	PublicBaseURL      string // user-facing base URL for waitlist claim links (PUBLIC_BASE_URL)
	KafkaBrokers       string // comma-separated broker list for the booking-events signal bridge
	BookingEventsTopic string // topic consumed by the signal bridge
	SignalGroup        string // consumer group of the signal bridge
	// Outbound webhook platform (Wave 5 #10) + notifications outbox (#7)
	DatabaseURL              string // notifications DB DSN (empty = webhook platform disabled)
	ConversationEventsTopic  string // opendesk.conversation.events (webhook dispatcher source)
	WebhookGroup             string // consumer group of the webhook dispatcher
	NotificationsOutboxTopic string // opendesk.notifications.outbox (SendPortalCode etc.)
	NotificationsOutboxGroup string // consumer group of the outbox consumer
	WebhookSigningRequired   bool   // require a signing secret on subscription create
	// GDPR (SPEC-W3 §2 innovation 13)
	ConversationAppID  string // Dapr app-id of conversation-service (export collector)
	PrivacyEventsTopic string // opendesk.privacy.events (erase tombstones)
	S3Endpoint         string // MinIO endpoint for GDPR exports (http://minio:9000)
	S3Region           string // SigV4 region (us-east-1)
	S3AccessKey        string // MinIO access key (S3_ACCESS_KEY)
	S3SecretKey        string // MinIO secret key (S3_SECRET_KEY)
	S3ExportsBucket    string // exports
	// Outbound CPS pacing + sender rotation (VOICE-SCALING §4 telephony)
	OutboundCPS         float64  // OUTBOUND_CPS: outbound starts/sec (1.0)
	OutboundBurst       int      // OUTBOUND_BURST: token bucket capacity (3)
	PacerBackend        string   // PACER_BACKEND: redis|local (redis)
	OutboundFromNumbers []string // OUTBOUND_FROM_NUMBERS: sender rotation pool
	RedisAddr           string   // REDIS_ADDR: shared pacer state (redis:6379)
	// SPEC-W12 §8: DND/quiet-hours compliance guards.
	DNDEnforcement      bool   // DND_ENFORCEMENT: suppress marketing sends on the DND registry (true)
	QuietHoursDefault   string // QUIET_HOURS_DEFAULT: "HH:MM-HH:MM" ("20:00-08:00")
	QuietHoursOverrides string // QUIET_HOURS_OVERRIDES: per-channel JSON {"sms":"21:00-07:00"}
	ShutdownTimeout     time.Duration
}

// Load reads configuration from the environment.
func Load() Config {
	return Config{
		Port:                     envInt("PORT", 7003),
		TemporalHostPort:         envStr("TEMPORAL_HOST_PORT", "temporal:7233"),
		TemporalNamespace:        envStr("TEMPORAL_NAMESPACE", "opendesk"),
		TemporalTaskQueue:        envStr("TEMPORAL_TASK_QUEUE", "opendesk-main"),
		DaprHost:                 envStr("DAPR_HOST", "daprd-notification"),
		DaprHTTPPort:             envInt("DAPR_HTTP_PORT", 3500),
		BookingAppID:             envStr("BOOKING_APP_ID", "booking"),
		PaymentsAppID:            envStr("PAYMENTS_APP_ID", "payments"),
		IdentityAppID:            envStr("IDENTITY_APP_ID", "identity"),
		KnowledgeAppID:           envStr("KNOWLEDGE_APP_ID", "knowledge"),
		CRMSyncAppID:             envStr("CRM_SYNC_APP_ID", "crm-sync"),
		PubSubName:               envStr("DAPR_PUBSUB_NAME", "pubsub-kafka"),
		CRMEventsTopic:           envStr("CRM_EVENTS_TOPIC", "opendesk.crm.events"),
		IndustriesDir:            envStr("INDUSTRIES_DIR", "/industries"),
		SMTPBinding:              envStr("SMTP_BINDING", "bindings-smtp"),
		TwilioBinding:            envStr("TWILIO_BINDING", "bindings-twilio"),
		MessagingChannels:        envStr("MESSAGING_CHANNELS", "email:smtp,sms:twilio"),
		TenantChannelMap:         os.Getenv("TENANT_CHANNEL_MAP"),
		SMTPFrom:                 envStr("SMTP_FROM", "no-reply@opendesk.local"),
		TwilioFrom:               envStr("TWILIO_FROM", "+10000000000"),
		OpenSearchURL:            envStr("OPENSEARCH_URL", "http://opensearch:9200"),
		PublicBaseURL:            envStr("PUBLIC_BASE_URL", "http://localhost:9080"),
		KafkaBrokers:             envStr("KAFKA_BROKERS", "kafka:9092"),
		BookingEventsTopic:       envStr("BOOKING_EVENTS_TOPIC", "opendesk.booking.events"),
		SignalGroup:              envStr("SIGNAL_GROUP", "notification-signals"),
		DatabaseURL:              os.Getenv("DATABASE_URL"),
		ConversationEventsTopic:  envStr("CONVERSATION_EVENTS_TOPIC", "opendesk.conversation.events"),
		WebhookGroup:             envStr("WEBHOOK_GROUP", "notification-webhooks"),
		NotificationsOutboxTopic: envStr("NOTIFICATIONS_OUTBOX_TOPIC", "opendesk.notifications.outbox"),
		NotificationsOutboxGroup: envStr("NOTIFICATIONS_OUTBOX_GROUP", "notification-outbox"),
		WebhookSigningRequired:   envStr("WEBHOOK_SIGNING_REQUIRED", "false") == "true",
		ConversationAppID:        envStr("CONVERSATION_APP_ID", "conversation"),
		PrivacyEventsTopic:       envStr("PRIVACY_EVENTS_TOPIC", "opendesk.privacy.events"),
		S3Endpoint:               envStr("S3_ENDPOINT", "http://minio:9000"),
		S3Region:                 envStr("S3_REGION", "us-east-1"),
		S3AccessKey:              envStr("S3_ACCESS_KEY", "minioadmin"),
		S3SecretKey:              envStr("S3_SECRET_KEY", "minioadmin"),
		S3ExportsBucket:          envStr("S3_EXPORTS_BUCKET", "exports"),
		OutboundCPS:              envFloat("OUTBOUND_CPS", 1.0),
		OutboundBurst:            envInt("OUTBOUND_BURST", 3),
		PacerBackend:             envStr("PACER_BACKEND", "redis"),
		OutboundFromNumbers:      envList("OUTBOUND_FROM_NUMBERS"),
		RedisAddr:                envStr("REDIS_ADDR", "redis:6379"),
		DNDEnforcement:           envStr("DND_ENFORCEMENT", "true") == "true",
		QuietHoursDefault:        envStr("QUIET_HOURS_DEFAULT", "20:00-08:00"),
		QuietHoursOverrides:      os.Getenv("QUIET_HOURS_OVERRIDES"),
		ShutdownTimeout:          time.Duration(envInt("SHUTDOWN_TIMEOUT_SECONDS", 20)) * time.Second,
	}
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

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// envList reads a comma-separated list; empty entries are dropped.
func envList(key string) []string {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
