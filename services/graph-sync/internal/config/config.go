// Package config loads graph-sync configuration from environment variables
// (envconfig style, no external dependency — notification-worker pattern).
package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration for graph-sync (SPEC-W28 §4 WS-A).
// Topic names are NEVER hardcoded in the consumer — they are read from env
// here with the SPEC-W28 §2 documented defaults.
type Config struct {
	Port         int    // HTTP listen port for /healthz + /metrics (PORT, default 7015)
	KafkaBrokers string // comma-separated broker list (KAFKA_BROKERS, default kafka:9092)
	// ConsumerGroup is the Kafka consumer group (GRAPH_SYNC_GROUP, default
	// "graph-sync").
	ConsumerGroup string
	// Input topics (SPEC-W28 §2). CACTopic empty = the funnel/CAC consumer
	// is skipped (GRAPH_SYNC_CAC_TOPIC, default "").
	BookingTopic     string // GRAPH_SYNC_BOOKING_TOPIC, default opendesk.booking.events
	IdentityTopic    string // GRAPH_SYNC_IDENTITY_TOPIC, default opendesk.identity.events
	TranscriptsTopic string // GRAPH_SYNC_TRANSCRIPTS_TOPIC, default opendesk.conversation.transcripts
	ErasureTopic     string // GRAPH_SYNC_ERASURE_TOPIC, default opendesk.consent.erasure.v1
	CACTopic         string // GRAPH_SYNC_CAC_TOPIC, default "" (skip)
	// EnrichmentTopic carries the nightly gold→graph Person property rows
	// from spark graph_enrichment.py (GRAPH_SYNC_ENRICHMENT_TOPIC, default
	// opendesk.graph.enrichment.v1; empty = skip, logged — same pattern as
	// the CAC topic).
	EnrichmentTopic string
	// DLQTopic receives poison messages after 3 attempts (GRAPH_SYNC_DLQ_TOPIC,
	// default opendesk.dlq).
	DLQTopic string
	// ErasureDoneTopic is the audit topic emitted after a Person subgraph is
	// erased (GRAPH_ERASURE_DONE_TOPIC, default opendesk.graph.erasure.done.v1).
	ErasureDoneTopic string
	// FalkorDBAddr is the Redis-protocol address of FalkorDB (FALKORDB_ADDR,
	// default graph-db:6379); FalkorDBGraph is the graph name
	// (FALKORDB_GRAPH, default opendesk).
	FalkorDBAddr  string
	FalkorDBGraph string
	// PhoneHashSalt is the SHA-256 salt for phone_hash (PHONE_HASH_SALT) —
	// the same salted-hash posture as the leads dedupe scheme (SPEC-W28 §3).
	// Empty is allowed for dev but logged loudly (mirrors crm-sync's
	// TWENTY_WEBHOOK_SECRET posture).
	PhoneHashSalt string
	// Ollama embeddings (SPEC-W28 §1): OpenAI-compatible endpoint
	// (OLLAMA_BASE_URL, default http://localhost:11434/v1), embed model
	// (OLLAMA_EMBED_MODEL, default nomic-embed-text). Unreachable Ollama
	// degrades gracefully: embeddings are skipped, merges still happen on
	// exact phone_hash.
	OllamaBaseURL    string
	OllamaEmbedModel string
	// MergeThreshold is the cosine-similarity threshold for MERGE_CANDIDATE
	// edges (GRAPH_SYNC_MERGE_THRESHOLD, default 0.92).
	MergeThreshold float64
	// ConsumerEnabled gates all Kafka consumers (CONSUMER_ENABLED, default true).
	ConsumerEnabled bool
	ShutdownTimeout time.Duration
}

// Load reads configuration from the environment.
func Load() Config {
	return Config{
		Port:             envInt("PORT", 7015),
		KafkaBrokers:     envStr("KAFKA_BROKERS", "kafka:9092"),
		ConsumerGroup:    envStr("GRAPH_SYNC_GROUP", "graph-sync"),
		BookingTopic:     envStr("GRAPH_SYNC_BOOKING_TOPIC", "opendesk.booking.events"),
		IdentityTopic:    envStr("GRAPH_SYNC_IDENTITY_TOPIC", "opendesk.identity.events"),
		TranscriptsTopic: envStr("GRAPH_SYNC_TRANSCRIPTS_TOPIC", "opendesk.conversation.transcripts"),
		ErasureTopic:     envStr("GRAPH_SYNC_ERASURE_TOPIC", "opendesk.consent.erasure.v1"),
		CACTopic:         envStr("GRAPH_SYNC_CAC_TOPIC", ""),
		EnrichmentTopic:  envStr("GRAPH_SYNC_ENRICHMENT_TOPIC", "opendesk.graph.enrichment.v1"),
		DLQTopic:         envStr("GRAPH_SYNC_DLQ_TOPIC", "opendesk.dlq"),
		ErasureDoneTopic: envStr("GRAPH_ERASURE_DONE_TOPIC", "opendesk.graph.erasure.done.v1"),
		FalkorDBAddr:     envStr("FALKORDB_ADDR", "graph-db:6379"),
		FalkorDBGraph:    envStr("FALKORDB_GRAPH", "opendesk"),
		PhoneHashSalt:    os.Getenv("PHONE_HASH_SALT"),
		OllamaBaseURL:    envStr("OLLAMA_BASE_URL", "http://localhost:11434/v1"),
		OllamaEmbedModel: envStr("OLLAMA_EMBED_MODEL", "nomic-embed-text"),
		MergeThreshold:   envFloat("GRAPH_SYNC_MERGE_THRESHOLD", 0.92),
		ConsumerEnabled:  envStr("CONSUMER_ENABLED", "true") == "true",
		ShutdownTimeout:  time.Duration(envInt("SHUTDOWN_TIMEOUT_SECONDS", 20)) * time.Second,
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
