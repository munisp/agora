// SPEC-W34 GF16: CRITICAL ops alerts for saga compensation exhaustion.
//
// A booking-saga compensation that exhausts its retries (e.g. VoidHold
// failing after HoldDeposit succeeded) leaves an orphaned deposit hold —
// holds have no timeout on the payments side, so without an alert the
// customer's funds stay held silently. EmitOpsAlert (1) emits a CRITICAL
// structured log and (2) publishes a
// com.opendesk.ops.SagaCompensationExhausted CloudEvent to the ops alerts
// topic (OPS_ALERTS_TOPIC, default opendesk.ops.alerts) via the service's
// Kafka producer plumbing (TrajectoryProducer signature, like the civic
// SLA-breach escalation path). Nil producer / "off" topic degrades to
// log-only — the service has no metrics plumbing, so the CRITICAL log is
// the last-resort signal (documented deviation from the "metric" option).
package activities

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.uber.org/zap"
)

const (
	// EventTypeSagaCompensationExhausted is emitted once per exhausted
	// saga compensation.
	EventTypeSagaCompensationExhausted = "com.opendesk.ops.SagaCompensationExhausted"
	// DefaultOpsAlertsTopic receives ops alert events (OPS_ALERTS_TOPIC
	// overrides; "off" disables emission).
	DefaultOpsAlertsTopic = "opendesk.ops.alerts"
)

// OpsAlertDeps bundles the GF16 activity dependencies; set by main after
// New. Zero value degrades to CRITICAL-log-only alerting.
type OpsAlertDeps struct {
	// Producer publishes alert CloudEvents (TrajectoryProducer's Produce
	// signature; nil-safe → log-only).
	Producer TrajectoryProducer
	// Topic is OPS_ALERTS_TOPIC (default opendesk.ops.alerts; ""/"off"
	// disables emission).
	Topic string
}

// EmitOpsAlert handles one exhausted saga compensation: CRITICAL structured
// log, then the CloudEvent publish. A publish failure is returned as an
// activity error so Temporal's retry schedule applies — this alert is the
// ONLY signal for an orphaned deposit hold, so it must not be dropped
// silently.
func (a *Activities) EmitOpsAlert(ctx context.Context, in workflows.OpsAlertInput) error {
	a.Log.Error("CRITICAL: saga compensation exhausted retries — orphaned resource needs manual ops intervention",
		zap.String("booking_id", in.BookingID),
		zap.String("tenant_id", in.TenantID),
		zap.String("tenant_slug", in.TenantSlug),
		zap.String("compensation_activity", in.Activity),
		zap.String("error", in.Error))

	if a.Ops.Producer == nil || a.Ops.Topic == "" || a.Ops.Topic == "off" {
		return nil // log-only degradation (no brokers / disabled)
	}
	now := time.Now().UTC()
	evt := map[string]any{
		"specversion": "1.0",
		"id":          uuid.NewString(),
		"source":      "notification-worker",
		"type":        EventTypeSagaCompensationExhausted,
		"subject":     in.BookingID,
		"time":        now.Format(time.RFC3339),
		"tenantid":    in.TenantID,
		"data": map[string]any{
			"tenant_id":             in.TenantID,
			"tenant_slug":           in.TenantSlug,
			"booking_id":            in.BookingID,
			"compensation_activity": in.Activity,
			"error":                 in.Error,
			"severity":              "critical",
			"ts":                    now.Format(time.RFC3339),
		},
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal ops alert: %w", err)
	}
	// Keyed by booking id so one booking's alerts stay ordered.
	if err := a.Ops.Producer.Produce(ctx, a.Ops.Topic, []byte(in.BookingID), payload); err != nil {
		return fmt.Errorf("publish ops alert to %s: %w", a.Ops.Topic, err)
	}
	return nil
}
