// Package config loads messaging-gateway configuration from environment
// variables (envconfig style, no external dependency).
package config

import (
	"os"
	"strconv"
)

// Config holds all runtime configuration for messaging-gateway.
type Config struct {
	Port int // HTTP listen port (7011)

	// Termii (https://developers.termii.com)
	TermiiAPIKey   string // TERMII_API_KEY
	TermiiSenderID string // TERMII_SENDER_ID, default "OpenDesk"
	TermiiBaseURL  string // TERMII_BASE_URL override (tests), default https://v2.api.termii.com

	// Africa's Talking (https://developers.africastalking.com)
	ATAPIKey   string // AT_API_KEY
	ATUsername string // AT_USERNAME (use "sandbox" on the sandbox)
	ATBaseURL  string // AT_BASE_URL override (tests), default https://api.africastalking.com
	ATFrom     string // AT_FROM default sender id / shortcode (optional)

	// WhatsApp Cloud API (https://developers.facebook.com/docs/whatsapp/cloud-api)
	WhatsAppToken         string // WHATSAPP_TOKEN (permanent access token)
	WhatsAppPhoneNumberID string // WHATSAPP_PHONE_NUMBER_ID
	WhatsAppBaseURL       string // WHATSAPP_BASE_URL override (tests), default https://graph.facebook.com/v21.0

	// Omnichannel inbound (SPEC-W6 Part A)
	WhatsAppVerifyToken   string // WHATSAPP_VERIFY_TOKEN (Meta webhook GET handshake)
	TelegramBotToken      string // TELEGRAM_BOT_TOKEN (BotFather)
	TelegramBotUsername   string // TELEGRAM_BOT_USERNAME (CHANNEL_SITE_MAP route key)
	TelegramWebhookSecret string // TELEGRAM_WEBHOOK_SECRET (optional setWebhook secret_token)
	TelegramBaseURL       string // TELEGRAM_BASE_URL override (tests), default https://api.telegram.org
	ChannelSiteMap        string // CHANNEL_SITE_MAP raw JSON (channel identity → site/tenant)

	// Internal upstreams for the inbound bridge. Direct-base overrides win;
	// empty means "via Dapr sidecar invoke on DAPR_HTTP_PORT".
	ConversationURL string // CONVERSATION_URL override (tests, no-Dapr dev)
	VoiceRuntimeURL string // VOICE_RUNTIME_URL override (tests, no-Dapr dev)
	DaprHTTPPort    int    // DAPR_HTTP_PORT, default 3500

	// IoT incident ingest (SPEC-W11 Part B §6): POST /webhooks/incidents.
	IncidentWebhookSecrets string // INCIDENT_WEBHOOK_SECRETS raw JSON {"<tenant_slug|tenant_id>": "<secret>"}
	BookingURL             string // BOOKING_URL override (tests, no-Dapr dev); empty = Dapr invoke booking

	// eBulksSMS (SPEC-W12 Agent A; https://api.ebulksms.com — request shape
	// is an ASSUMPTION, no live keys this wave).
	EBulkAPIKey   string // EBULK_API_KEY
	EBulkUsername string // EBULK_USERNAME
	EBulkSender   string // EBULK_SENDER default sender id (optional)
	EBulkBaseURL  string // EBULK_BASE_URL override (tests), default https://api.ebulksms.com

	// SMS failover chain (SPEC-W12 Agent A).
	SMSProviderChain string // SMS_PROVIDER_CHAIN csv, default "africastalking,termii,ebulksms"

	// USSD inbound (SPEC-W12 Agent A): POST /webhooks/ussd.
	IdentityURL        string // IDENTITY_URL override (tests, no-Dapr dev); empty = Dapr invoke identity
	USSDSessionBackend string // USSD_SESSION_BACKEND: "memory" (default) | "dapr"
	USSDStateStore     string // USSD_STATE_STORE Dapr component name (backend=dapr), default "statestore"
	USSDSessionTTL     int    // USSD_SESSION_TTL_SECONDS, default 180 (contract §1)
}

// Load reads configuration from the environment.
func Load() Config {
	return Config{
		Port:                   envInt("PORT", 7011),
		TermiiAPIKey:           os.Getenv("TERMII_API_KEY"),
		TermiiSenderID:         envStr("TERMII_SENDER_ID", "OpenDesk"),
		TermiiBaseURL:          envStr("TERMII_BASE_URL", "https://v2.api.termii.com"),
		ATAPIKey:               os.Getenv("AT_API_KEY"),
		ATUsername:             os.Getenv("AT_USERNAME"),
		ATBaseURL:              envStr("AT_BASE_URL", "https://api.africastalking.com"),
		ATFrom:                 os.Getenv("AT_FROM"),
		WhatsAppToken:          os.Getenv("WHATSAPP_TOKEN"),
		WhatsAppPhoneNumberID:  os.Getenv("WHATSAPP_PHONE_NUMBER_ID"),
		WhatsAppBaseURL:        envStr("WHATSAPP_BASE_URL", "https://graph.facebook.com/v21.0"),
		WhatsAppVerifyToken:    os.Getenv("WHATSAPP_VERIFY_TOKEN"),
		TelegramBotToken:       os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramBotUsername:    os.Getenv("TELEGRAM_BOT_USERNAME"),
		TelegramWebhookSecret:  os.Getenv("TELEGRAM_WEBHOOK_SECRET"),
		TelegramBaseURL:        envStr("TELEGRAM_BASE_URL", "https://api.telegram.org"),
		ChannelSiteMap:         os.Getenv("CHANNEL_SITE_MAP"),
		ConversationURL:        os.Getenv("CONVERSATION_URL"),
		VoiceRuntimeURL:        os.Getenv("VOICE_RUNTIME_URL"),
		DaprHTTPPort:           envInt("DAPR_HTTP_PORT", 3500),
		IncidentWebhookSecrets: os.Getenv("INCIDENT_WEBHOOK_SECRETS"),
		BookingURL:             os.Getenv("BOOKING_URL"),
		EBulkAPIKey:            os.Getenv("EBULK_API_KEY"),
		EBulkUsername:          os.Getenv("EBULK_USERNAME"),
		EBulkSender:            os.Getenv("EBULK_SENDER"),
		EBulkBaseURL:           envStr("EBULK_BASE_URL", "https://api.ebulksms.com"),
		SMSProviderChain:       envStr("SMS_PROVIDER_CHAIN", "africastalking,termii,ebulksms"),
		IdentityURL:            os.Getenv("IDENTITY_URL"),
		USSDSessionBackend:     envStr("USSD_SESSION_BACKEND", "memory"),
		USSDStateStore:         envStr("USSD_STATE_STORE", "statestore"),
		USSDSessionTTL:         envInt("USSD_SESSION_TTL_SECONDS", 180),
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
