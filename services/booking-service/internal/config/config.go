// Package config loads booking-service configuration from environment
// variables (envconfig style, no external dependency).
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for booking-service.
type Config struct {
	Port               int           // HTTP listen port (PORT, default 7002)
	DatabaseURL        string        // postgres DSN for the booking DB (DATABASE_URL)
	PGMaxConns         int32         // pgxpool MaxConns (PG_MAX_CONNS, default 20 — peak-sized per capacity runbook)
	PermifyURL         string        // Permify HTTP API base (http://permify:3476)
	DaprHost           string        // daprd host (default daprd-booking)
	DaprHTTPPort       int           // daprd HTTP port (default 3500)
	PubSubName         string        // Dapr pubsub component (pubsub-kafka)
	BookingEventsTopic string        // opendesk.booking.events
	IdentityAppID      string        // Dapr app-id of identity-service
	TemporalHostPort   string        // temporal:7233
	TemporalNamespace  string        // opendesk
	TemporalTaskQueue  string        // opendesk-main
	KafkaBrokers       []string      // direct broker list for the command consumer
	CommandsTopic      string        // opendesk.booking.commands
	CommandsGroup      string        // consumer group id
	DLQTopic           string        // opendesk.dlq
	PrivacyEventsTopic string        // opendesk.privacy.events (GDPR erase tombstones)
	PrivacyGroup       string        // consumer group of the privacy erase consumer
	RedisAddr          string        // REDIS_ADDR for the availability cache (empty = cache disabled)
	CacheTTL           time.Duration // availability cache entry TTL (CACHE_TTL_SECONDS, default 120s)
	CacheStaleTTL      time.Duration // serve-stale window after CacheTTL (CACHE_STALE_TTL_SECONDS, default 15min)
	UsageEventsTopic   string        // opendesk.usage.events (usage metering, Wave 5 #9; empty disables)
	IdentityCacheTTL   time.Duration // tenant-context resolver cache TTL (TENANT_CACHE_TTL_SECONDS, default 5min)
	OutboxPollInterval time.Duration // outbox dispatcher poll cadence
	ShutdownTimeout    time.Duration
	AuthzDisabled      bool   // dev escape hatch: skip Permify checks (AUTHZ_DISABLED=true)
	AuthzOutagePolicy  string // fail_closed (default) | fail_open — behavior when Permify itself errors
	ConsumerEnabled    bool   // run the Kafka command consumer (default true)
	// Customer portal (Wave 5 #7)
	PortalSecret       string // HMAC secret signing 15-min portal JWTs (PORTAL_SECRET)
	NotificationsTopic string // opendesk.notifications.outbox (SendPortalCode delivery)
	// Geospatial (SPEC-W8 Part A)
	GeocodeEnabled   bool   // GEOCODE_ENABLED: Nominatim address geocoding hook (default false)
	GeocodeBaseURL   string // GEOCODE_BASE_URL (default https://nominatim.openstreetmap.org)
	GeoCampaignBatch int    // GEO_CAMPAIGN_BATCH: recipients per campaign batch (default 50)
	// Incidents (SPEC-W11 Part B)
	IncidentsTopic       string // opendesk.incidents (IDP ingestion)
	IncidentsGroup       string // consumer group booking-incidents
	IncidentAutoDispatch bool   // INCIDENT_AUTO_DISPATCH: auto-deliver new incidents to active endpoints (default true)
	// Leads / CAC (SPEC-W13 Agent A)
	CACEventsTopic                string // cac.events funnel topic (CAC_EVENTS_TOPIC; empty disables emission)
	LeadAttributionFirstTouchOnly bool   // LEAD_ATTRIBUTION_FIRST_TOUCH_ONLY (default true)
	// Referrals + commissions (SPEC-W14, contract §7)
	CommissionsEnabled bool   // COMMISSIONS_ENABLED (default true; false → referral/commission endpoints 503)
	PayoutProvider     string // PAYOUT_PROVIDER (default paystack; Agent B payout execution)
	PayoutMinNGN       int64  // PAYOUT_MIN_NGN (default 100 — minimum payout, naira)
	ReconCron          string // RECON_CRON (default "30 2 * * *" — commission-recon-nightly, Africa/Lagos 02:30)
	// Field capture (SPEC-W16 Agent B, contract §4)
	FieldCaptureBatchLimit int // FIELD_CAPTURE_BATCH_LIMIT: max offline-queue items per POST /v1/field/capture (default 100)
	// App entitlement gate (SPEC-W18 Agent D, contract §4)
	AppGateEnabled  bool          // APP_GATE_ENABLED: DEFAULT false → gate is a pure pass-through; production behavior UNCHANGED unless opted in
	AppGateCacheTTL time.Duration // APP_GATE_CACHE_TTL_SECONDS: entitlement decision cache TTL (default 60s)
	// Helpdesk app (SPEC-W19 Agent A; integrator-wired)
	HelpdeskEventsTopic string // HELPDESK_EVENTS_TOPIC (default opendesk.helpdesk.events.v1; empty disables emission)
	HelpdeskUsageTopic  string // HELPDESK_USAGE_TOPIC (default opendesk.usage.events; empty disables metering)
	HelpdeskDBMaxConns  int32  // HELPDESK_DB_MAX_CONNS: dedicated pool size (default 4, devices idiom)
	// Field-service app (SPEC-W19 Agent B; integrator-wired)
	WorkordersNotificationsTopic string // WORKORDERS_NOTIFICATIONS_TOPIC (default opendesk.notifications.outbox; empty disables dispatch push)
	WorkordersUsageTopic         string // WORKORDERS_USAGE_TOPIC (default opendesk.usage.events; empty disables metering)
	WorkordersFSMEventsTopic     string // WORKORDERS_FSM_EVENTS_TOPIC (default opendesk.fsm.events.v1; empty disables events)
	// Loyalty-wallet app (SPEC-W19 Agent C; integrator-wired). Metering rides
	// the existing UsageEventsTopic (USAGE_EVENTS_TOPIC).
	LoyaltyEventsTopic string // LOYALTY_EVENTS_TOPIC (default opendesk.loyalty.events.v1; empty disables events)
	// Campaign-studio app (SPEC-W19 Agent D; integrator-wired)
	StudioDatabaseURL string // STUDIO_DATABASE_URL (default "" → falls back to DATABASE_URL)
	StudioStepBatch   int    // STUDIO_STEP_BATCH: enrollments advanced per step call (default 200)
	StudioEventsTopic string // STUDIO_EVENTS_TOPIC (default opendesk.studio.events.v1; empty disables events)
	// CRM-360 app (SPEC-W20 Agent A; integrator-wired). No metering
	// (internal-ops app, contract §4).
	CRM360EventsTopic string // CRM360_EVENTS_TOPIC (default opendesk.crm.events.v1; empty disables events)
	// Surveys/VoC app (SPEC-W20 Agent B; integrator-wired). Metering
	// (survey_response_received) rides the existing UsageEventsTopic
	// (USAGE_EVENTS_TOPIC) — same posture as loyalty (SPEC-W19 Agent C).
	SurveysEventsTopic        string // SURVEYS_EVENTS_TOPIC (default opendesk.surveys.events.v1; empty disables events)
	SurveysNotificationsTopic string // SURVEYS_NOTIFICATIONS_TOPIC (default opendesk.notifications.outbox; empty disables invite sends)
	SurveysPublicBaseURL      string // SURVEYS_PUBLIC_BASE_URL (default https://app.opendesk.ng/s — invite link base)
	SurveysDatabaseURL        string // SURVEYS_DATABASE_URL (default "" → falls back to DATABASE_URL)
	// Lending app (SPEC-W20 Agent C; integrator-wired). Metering
	// (loan_disbursed) rides UsageEventsTopic.
	LendingEventsTopic string // LENDING_EVENTS_TOPIC (default opendesk.lending.events.v1; empty disables events)
	LendingKYCURL      string // LENDING_KYC_URL (default "" → kyc-service not wired; approvals require explicit kyc_override)
	// Workforce app (SPEC-W20 Agent D; integrator-wired). No metering
	// (internal-ops app, contract §4).
	WorkforceEventsTopic string // WORKFORCE_EVENTS_TOPIC (default opendesk.workforce.events.v1; empty disables events)
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		Port:        envInt("PORT", 7002),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		// Voice turns fan out into availability lookups; default 20 covers
		// ~10 peak concurrent calls at 2 mid-call turns each (runbook §DB).
		PGMaxConns:         int32(envInt("PG_MAX_CONNS", 20)),
		PermifyURL:         envStr("PERMIFY_URL", "http://permify:3476"),
		DaprHost:           envStr("DAPR_HOST", "daprd-booking"),
		DaprHTTPPort:       envInt("DAPR_HTTP_PORT", 3500),
		PubSubName:         envStr("DAPR_PUBSUB_NAME", "pubsub-kafka"),
		BookingEventsTopic: envStr("BOOKING_EVENTS_TOPIC", "opendesk.booking.events"),
		IdentityAppID:      envStr("IDENTITY_APP_ID", "identity"),
		TemporalHostPort:   envStr("TEMPORAL_HOST_PORT", "temporal:7233"),
		TemporalNamespace:  envStr("TEMPORAL_NAMESPACE", "opendesk"),
		TemporalTaskQueue:  envStr("TEMPORAL_TASK_QUEUE", "opendesk-main"),
		KafkaBrokers:       strings.Split(envStr("KAFKA_BROKERS", "kafka:9092"), ","),
		CommandsTopic:      envStr("BOOKING_COMMANDS_TOPIC", "opendesk.booking.commands"),
		CommandsGroup:      envStr("BOOKING_COMMANDS_GROUP", "booking-service-commands"),
		DLQTopic:           envStr("DLQ_TOPIC", "opendesk.dlq"),
		PrivacyEventsTopic: envStr("PRIVACY_EVENTS_TOPIC", "opendesk.privacy.events"),
		PrivacyGroup:       envStr("PRIVACY_EVENTS_GROUP", "booking-service-privacy"),
		RedisAddr:          os.Getenv("REDIS_ADDR"),
		CacheTTL:           time.Duration(envInt("CACHE_TTL_SECONDS", 120)) * time.Second,
		CacheStaleTTL:      time.Duration(envInt("CACHE_STALE_TTL_SECONDS", 900)) * time.Second,
		UsageEventsTopic:   envStr("USAGE_EVENTS_TOPIC", "opendesk.usage.events"),
		IdentityCacheTTL:   time.Duration(envInt("TENANT_CACHE_TTL_SECONDS", 300)) * time.Second,
		OutboxPollInterval: time.Duration(envInt("OUTBOX_POLL_INTERVAL_SECONDS", 2)) * time.Second,
		ShutdownTimeout:    time.Duration(envInt("SHUTDOWN_TIMEOUT_SECONDS", 20)) * time.Second,
		AuthzDisabled:      envStr("AUTHZ_DISABLED", "false") == "true",
		AuthzOutagePolicy:  envStr("AUTHZ_OUTAGE_POLICY", "fail_closed"),
		ConsumerEnabled:    envStr("CONSUMER_ENABLED", "true") == "true",
		// Dev fallback keeps the portal bootable locally; prod MUST override.
		PortalSecret:                  envStr("PORTAL_SECRET", "opendesk-dev-portal-secret-change-in-prod"),
		NotificationsTopic:            envStr("NOTIFICATIONS_TOPIC", "opendesk.notifications.outbox"),
		GeocodeEnabled:                envStr("GEOCODE_ENABLED", "false") == "true",
		GeocodeBaseURL:                envStr("GEOCODE_BASE_URL", "https://nominatim.openstreetmap.org"),
		GeoCampaignBatch:              envInt("GEO_CAMPAIGN_BATCH", 50),
		IncidentsTopic:                envStr("INCIDENTS_TOPIC", "opendesk.incidents"),
		IncidentsGroup:                envStr("INCIDENTS_GROUP", "booking-incidents"),
		IncidentAutoDispatch:          envStr("INCIDENT_AUTO_DISPATCH", "true") == "true",
		CACEventsTopic:                envStr("CAC_EVENTS_TOPIC", "cac.events"),
		LeadAttributionFirstTouchOnly: envStr("LEAD_ATTRIBUTION_FIRST_TOUCH_ONLY", "true") == "true",
		CommissionsEnabled:            envStr("COMMISSIONS_ENABLED", "true") == "true",
		PayoutProvider:                envStr("PAYOUT_PROVIDER", "paystack"),
		PayoutMinNGN:                  int64(envInt("PAYOUT_MIN_NGN", 100)),
		ReconCron:                     envStr("RECON_CRON", "30 2 * * *"),
		FieldCaptureBatchLimit:        envInt("FIELD_CAPTURE_BATCH_LIMIT", 100),
		// SPEC-W18 Agent D (additive): off by default — opt-in only.
		AppGateEnabled:  envStr("APP_GATE_ENABLED", "false") == "true",
		AppGateCacheTTL: time.Duration(envInt("APP_GATE_CACHE_TTL_SECONDS", 60)) * time.Second,
		// SPEC-W19 integrator (additive): the four enterprise apps are
		// functional with zero config — every default matches the package
		// doc contracts; empty string disables the corresponding emission.
		HelpdeskEventsTopic:          envStr("HELPDESK_EVENTS_TOPIC", "opendesk.helpdesk.events.v1"),
		HelpdeskUsageTopic:           envStr("HELPDESK_USAGE_TOPIC", "opendesk.usage.events"),
		HelpdeskDBMaxConns:           int32(envInt("HELPDESK_DB_MAX_CONNS", 4)),
		WorkordersNotificationsTopic: envStr("WORKORDERS_NOTIFICATIONS_TOPIC", "opendesk.notifications.outbox"),
		WorkordersUsageTopic:         envStr("WORKORDERS_USAGE_TOPIC", "opendesk.usage.events"),
		WorkordersFSMEventsTopic:     envStr("WORKORDERS_FSM_EVENTS_TOPIC", "opendesk.fsm.events.v1"),
		LoyaltyEventsTopic:           envStr("LOYALTY_EVENTS_TOPIC", "opendesk.loyalty.events.v1"),
		StudioDatabaseURL:            os.Getenv("STUDIO_DATABASE_URL"),
		StudioStepBatch:              envInt("STUDIO_STEP_BATCH", 200),
		StudioEventsTopic:            envStr("STUDIO_EVENTS_TOPIC", "opendesk.studio.events.v1"),
		// SPEC-W20 integrator (additive): the four batch-2 enterprise apps
		// are functional with zero config — every default matches the
		// package doc contracts; empty string disables the corresponding
		// emission (or, for LENDING_KYC_URL, switches approvals to
		// override-only mode).
		CRM360EventsTopic:         envStr("CRM360_EVENTS_TOPIC", "opendesk.crm.events.v1"),
		SurveysEventsTopic:        envStr("SURVEYS_EVENTS_TOPIC", "opendesk.surveys.events.v1"),
		SurveysNotificationsTopic: envStr("SURVEYS_NOTIFICATIONS_TOPIC", "opendesk.notifications.outbox"),
		SurveysPublicBaseURL:      envStr("SURVEYS_PUBLIC_BASE_URL", "https://app.opendesk.ng/s"),
		SurveysDatabaseURL:        os.Getenv("SURVEYS_DATABASE_URL"),
		LendingEventsTopic:        envStr("LENDING_EVENTS_TOPIC", "opendesk.lending.events.v1"),
		LendingKYCURL:             os.Getenv("LENDING_KYC_URL"),
		WorkforceEventsTopic:      envStr("WORKFORCE_EVENTS_TOPIC", "opendesk.workforce.events.v1"),
	}
	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("DATABASE_URL is required")
	}
	return cfg, nil
}

// databaseURL resolves the booking DB DSN. DATABASE_URL wins; otherwise the
// DSN is constructed from PG_DSN (base) + PG_DATABASE with an optional
// PG_USER/PG_PASS credential override (per-service DB roles, SPEC-W3 §2).
// The default credentials stay opendesk/opendesk for local dev.
func databaseURL() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	base := envStr("PG_DSN", "postgres://opendesk:opendesk@postgres:5432")
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	if user := os.Getenv("PG_USER"); user != "" {
		u.User = url.UserPassword(user, os.Getenv("PG_PASS"))
	}
	u.Path = "/" + strings.TrimPrefix(envStr("PG_DATABASE", "booking"), "/")
	return u.String()
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
