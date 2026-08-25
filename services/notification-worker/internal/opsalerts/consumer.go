// Package opsalerts consumes the ops-alerts topic (K3 canonical:
// opendesk.ops.alerts) and persists every alert CloudEvent to the
// ops_alerts table for the role-gated GET /v1/ops-alerts read-back
// (F15-04). Reliability contract (N-03 idiom): the offset is committed
// ONLY after a durable insert — a Process error leaves the offset behind
// for redelivery, and the store dedupes on the CloudEvent id so redelivery
// never double-records.
package opsalerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/opendesk/notification-worker/internal/store"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Topic is the canonical ops-alerts topic (K3; OPS_ALERTS_TOPIC overrides,
// "off" disables the consumer).
const Topic = "opendesk.ops.alerts"

// TopicEnabled maps the env value onto the effective topic: ""/default →
// Topic, "off" → "" (disabled).
func TopicEnabled(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off":
		return ""
	case "":
		return Topic
	default:
		return v
	}
}

// AlertStore is the persistence slice (*store.Store satisfies it; tests use
// a fake).
type AlertStore interface {
	InsertOpsAlert(ctx context.Context, a *store.OpsAlert) (bool, error)
}

// Consumer reads the ops-alerts topic.
type Consumer struct {
	reader *kafka.Reader
	store  AlertStore
	log    *zap.Logger
}

// New builds the consumer (explicit commits — see package header).
func New(brokers []string, topic, group string, st AlertStore, log *zap.Logger) *Consumer {
	return &Consumer{
		reader: kafka.NewReader(kafka.ReaderConfig{
			Brokers:        brokers,
			Topic:          topic,
			GroupID:        group,
			MinBytes:       1,
			MaxBytes:       10 << 20,
			CommitInterval: 0,
			StartOffset:    kafka.FirstOffset,
		}),
		store: st,
		log:   log,
	}
}

// Run consumes until ctx is cancelled; a fatal reader error is returned.
func (c *Consumer) Run(ctx context.Context) error {
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("fetch %s: %w", c.reader.Config().Topic, err)
		}
		if err := c.Process(ctx, msg.Value); err != nil {
			// No-commit-on-error: the offset stays behind; the insert is
			// idempotent on the CloudEvent id so the redelivery converges.
			c.log.Error("ops alert persist failed; offset NOT committed (redelivery pending)",
				zap.String("key", string(msg.Key)), zap.Error(err))
			continue
		}
		if err := c.reader.CommitMessages(ctx, msg); err != nil {
			c.log.Error("commit failed", zap.Error(err))
		}
	}
}

// Close releases the reader.
func (c *Consumer) Close() error { return c.reader.Close() }

// envelope is the CloudEvents subset the consumer needs.
type envelope struct {
	ID       string          `json:"id"`
	Source   string          `json:"source"`
	Type     string          `json:"type"`
	TenantID string          `json:"tenantid"` // CloudEvents extension
	Data     json.RawMessage `json:"data"`
}

// Process persists one raw CloudEvent payload (exported for testing).
// Malformed payloads are logged and DROPPED (nil error — a poison message
// must not stall the topic); store failures are returned (no-commit →
// redelivery).
func (c *Consumer) Process(ctx context.Context, raw []byte) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		c.log.Warn("malformed ops alert; dropping", zap.Error(err))
		return nil
	}
	if env.ID == "" {
		// No id → no idempotency anchor; generate-free drop is the honest
		// choice for an alerting stream (never silently invent identity).
		c.log.Warn("ops alert without CloudEvent id; dropping",
			zap.String("type", env.Type), zap.String("source", env.Source))
		return nil
	}
	severity := "info"
	tenantID := env.TenantID
	if len(env.Data) > 0 && string(env.Data) != "null" {
		var data struct {
			Severity string `json:"severity"`
			TenantID string `json:"tenant_id"`
		}
		if err := json.Unmarshal(env.Data, &data); err == nil {
			if data.Severity != "" {
				severity = data.Severity
			}
			if tenantID == "" {
				tenantID = data.TenantID
			}
		}
	}
	a := &store.OpsAlert{
		EventID:  env.ID,
		TenantID: tenantID,
		Source:   env.Source,
		Type:     env.Type,
		Severity: severity,
		Payload:  append([]byte(nil), raw...),
	}
	inserted, err := c.store.InsertOpsAlert(ctx, a)
	if err != nil {
		return fmt.Errorf("persist ops alert %s: %w", env.ID, err)
	}
	if inserted {
		c.log.Info("ops alert recorded",
			zap.String("event_id", env.ID), zap.String("type", env.Type),
			zap.String("severity", severity), zap.String("tenant_id", tenantID))
	}
	return nil
}
