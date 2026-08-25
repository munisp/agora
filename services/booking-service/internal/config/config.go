// Package config loads booking-service configuration from environment
// variables (envconfig style, no external dependency).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all booking-service configuration.
type Config struct {
	// HTTP listen address, e.g. ":8080".
	HTTPAddr string
	// GRPCAddr is reserved for the future gRPC health/reflection endpoint.
	GRPCAddr string

	// Postgres DSN, e.g. postgres://user:pass@host:5432/booking?sslmode=disable
	PostgresDSN string
	// Max open connections to Postgres.
	DBMaxOpenConns int
	DBMaxIdleConns int
	DBConnMaxLife  time.Duration

	// Kafka bootstrap servers (comma separated) and topics.
	KafkaBrokers  []string
	TopicCommands string // opendesk.booking.commands
	TopicEvents   string // opendesk.booking.events
	TopicDLQ      string // opendesk.dlq
	ConsumerGroup string

	// Dapr sidecar HTTP endpoint, e.g. http://localhost:3500
	DaprHTTPAddr string
	// Dapr pub/sub component and state store names.
	DaprPubSubName  string
	DaprStateStore  string
	DaprSecretStore string

	// OIDC / JWT validation (Keycloak realm).
	OIDCIssuer   string
	OIDCAudience string
	// If true, JWT signature validation is skipped (local dev only).
	AuthDevMode bool

	// Lending / credit-score integration.
	LendingEnabled  bool
	ScoreServiceURL string // e.g. http://localhost:8081 (mock) or internal DNS
	ScoreMaxAmountCents int64

	// Referral self-verify: requires booking.analytics.events projections.
	ReferralSelfVerify bool

	// Idempotency: dedup window for consumer keys.
	IdempotencyTTL time.Duration

	// Graceful shutdown timeout.
	ShutdownTimeout time.Duration

	// Observability.
	LogLevel  string
	LogFormat string // "json" or "text"
}

// Load reads configuration from environment variables, applying defaults
// that match deploy/k3s/booking-deployment.yaml and docker-compose.yml.
func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:            getEnv("HTTP_ADDR", ":8080"),
		GRPCAddr:            getEnv("GRPC_ADDR", ":9090"),
		PostgresDSN:         getEnv("POSTGRES_DSN", "postgres://booking:booking@localhost:5432/booking?sslmode=disable"),
		DBMaxOpenConns:      getEnvInt("DB_MAX_OPEN_CONNS", 25),
		DBMaxIdleConns:      getEnvInt("DB_MAX_IDLE_CONNS", 5),
		DBConnMaxLife:       getEnvDur("DB_CONN_MAX_LIFE", 30*time.Minute),
		KafkaBrokers:        splitCSV(getEnv("KAFKA_BROKERS", "localhost:9092")),
		TopicCommands:       getEnv("TOPIC_COMMANDS", "opendesk.booking.commands"),
		TopicEvents:         getEnv("TOPIC_EVENTS", "opendesk.booking.events"),
		TopicDLQ:            getEnv("TOPIC_DLQ", "opendesk.dlq"),
		ConsumerGroup:       getEnv("CONSUMER_GROUP", "booking-service"),
		DaprHTTPAddr:        getEnv("DAPR_HTTP_ADDR", "http://localhost:3500"),
		DaprPubSubName:      getEnv("DAPR_PUBSUB_NAME", "kafka-pubsub"),
		DaprStateStore:      getEnv("DAPR_STATE_STORE", "booking-state"),
		DaprSecretStore:     getEnv("DAPR_SECRET_STORE", "opendesk-secrets"),
		OIDCIssuer:          getEnv("OIDC_ISSUER", "http://localhost:8180/realms/opendesk"),
		OIDCAudience:        getEnv("OIDC_AUDIENCE", "opendesk"),
		AuthDevMode:         getEnvBool("AUTH_DEV_MODE", false),
		LendingEnabled:      getEnvBool("LENDING_ENABLED", false),
		ScoreServiceURL:     getEnv("SCORE_SERVICE_URL", "http://localhost:8081"),
		ScoreMaxAmountCents: getEnvInt64("SCORE_MAX_AMOUNT_CENTS", 5000000),
		ReferralSelfVerify:  getEnvBool("REFERRAL_SELF_VERIFY", false),
		IdempotencyTTL:      getEnvDur("IDEMPOTENCY_TTL", 24*time.Hour),
		ShutdownTimeout:     getEnvDur("SHUTDOWN_TIMEOUT", 15*time.Second),
		LogLevel:            getEnv("LOG_LEVEL", "info"),
		LogFormat:           getEnv("LOG_FORMAT", "json"),
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate enforces invariants that would otherwise fail at runtime deep
// inside the service. Called by Load and by tests.
func (c *Config) Validate() error {
	if c.HTTPAddr == "" {
		return fmt.Errorf("config: HTTP_ADDR must not be empty")
	}
	if !strings.HasPrefix(c.HTTPAddr, ":") && !strings.Contains(c.HTTPAddr, ":") {
		return fmt.Errorf("config: HTTP_ADDR %q must be host:port or :port", c.HTTPAddr)
	}
	if c.PostgresDSN == "" {
		return fmt.Errorf("config: POSTGRES_DSN must not be empty")
	}
	if !strings.HasPrefix(c.PostgresDSN, "postgres://") && !strings.HasPrefix(c.PostgresDSN, "postgresql://") {
		return fmt.Errorf("config: POSTGRES_DSN must be a postgres:// URL")
	}
	if len(c.KafkaBrokers) == 0 {
		return fmt.Errorf("config: KAFKA_BROKERS must not be empty")
	}
	for _, b := range c.KafkaBrokers {
		if !strings.Contains(b, ":") {
			return fmt.Errorf("config: KAFKA_BROKERS entry %q must be host:port", b)
		}
	}
	if c.TopicCommands == "" || c.TopicEvents == "" || c.TopicDLQ == "" {
		return fmt.Errorf("config: topic names must not be empty")
	}
	if c.TopicCommands == c.TopicEvents || c.TopicCommands == c.TopicDLQ || c.TopicEvents == c.TopicDLQ {
		return fmt.Errorf("config: topics must be distinct")
	}
	if c.ConsumerGroup == "" {
		return fmt.Errorf("config: CONSUMER_GROUP must not be empty")
	}
	if c.DaprHTTPAddr == "" {
		return fmt.Errorf("config: DAPR_HTTP_ADDR must not be empty")
	}
	if c.DBMaxOpenConns < 1 {
		return fmt.Errorf("config: DB_MAX_OPEN_CONNS must be >= 1")
	}
	if c.DBMaxIdleConns > c.DBMaxOpenConns {
		return fmt.Errorf("config: DB_MAX_IDLE_CONNS (%d) must be <= DB_MAX_OPEN_CONNS (%d)", c.DBMaxIdleConns, c.DBMaxOpenConns)
	}
	if c.IdempotencyTTL < time.Minute {
		return fmt.Errorf("config: IDEMPOTENCY_TTL must be >= 1m, got %s", c.IdempotencyTTL)
	}
	if c.ShutdownTimeout < time.Second {
		return fmt.Errorf("config: SHUTDOWN_TIMEOUT must be >= 1s, got %s", c.ShutdownTimeout)
	}
	if c.LendingEnabled {
		if !strings.HasPrefix(c.ScoreServiceURL, "http://") && !strings.HasPrefix(c.ScoreServiceURL, "https://") {
			return fmt.Errorf("config: SCORE_SERVICE_URL must be an http(s) URL when LENDING_ENABLED")
		}
		if c.ScoreMaxAmountCents <= 0 {
			return fmt.Errorf("config: SCORE_MAX_AMOUNT_CENTS must be > 0 when LENDING_ENABLED")
		}
	}
	if !c.AuthDevMode && c.OIDCIssuer == "" {
		return fmt.Errorf("config: OIDC_ISSUER must not be empty unless AUTH_DEV_MODE")
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: LOG_LEVEL %q invalid", c.LogLevel)
	}
	switch strings.ToLower(c.LogFormat) {
	case "json", "text":
	default:
		return fmt.Errorf("config: LOG_FORMAT %q invalid", c.LogFormat)
	}
	return nil
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getEnvInt64(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func getEnvDur(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if dd, err := time.ParseDuration(v); err == nil {
			return dd
		}
	}
	return def
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
