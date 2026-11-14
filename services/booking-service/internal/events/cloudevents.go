// Package events builds CloudEvents 1.0 envelopes per SPEC §4:
// {specversion, id, source, type, subject, time, tenantid (ext), data}.
package events

import (
	"time"

	"github.com/google/uuid"
)

// ReceiptReconFindingEventType (SPEC-W56 Scope A) is the CloudEvent type
// emitted on opendesk.booking.events for each NEW receipt ↔ payment
// reconciliation finding (same transaction as the finding insert; deduped
// findings emit nothing). Consumed by the notification-worker signals
// bridge → ops-alerts pipeline.
const ReceiptReconFindingEventType = "com.opendesk.booking.ReceiptReconFinding"

// ReceiptIngestedEventType (SPEC-W64) is the CloudEvent type emitted on
// opendesk.booking.events when a receipt document is freshly ingested via
// either channel (field PWA capture attach / WhatsApp media inbox). Never
// emitted on the idempotent-replay branch. The type string is pinned free
// of the fraud-engine substring needles (G-C). Consumed by the
// analytics-pipeline router → bronze.receipt_ingest.
const ReceiptIngestedEventType = "com.opendesk.booking.ReceiptIngested"

// ReceiptReconResolvedEventType (SPEC-W64) is the CloudEvent type emitted
// on opendesk.booking.events when an operator resolves one recon finding
// (POST /v1/recon/receipts/{id}/resolve) — same transaction as the
// resolved_at/resolved_by UPDATE; an already-resolved (or unknown) finding
// emits nothing. Consumed by the analytics-pipeline router →
// bronze.recon_resolutions.
const ReceiptReconResolvedEventType = "com.opendesk.booking.ReceiptReconResolved"

// CloudEvent is the canonical envelope used on every Kafka topic.
type CloudEvent struct {
	SpecVersion string         `json:"specversion"`
	ID          string         `json:"id"`
	Source      string         `json:"source"`
	Type        string         `json:"type"`
	Subject     string         `json:"subject,omitempty"`
	Time        time.Time      `json:"time"`
	TenantID    string         `json:"tenantid,omitempty"`
	Data        map[string]any `json:"data"`
}

// New builds a CloudEvent envelope.
func New(source, eventType, subject, tenantID string, data map[string]any) CloudEvent {
	return CloudEvent{
		SpecVersion: "1.0",
		ID:          uuid.NewString(),
		Source:      source,
		Type:        eventType,
		Subject:     subject,
		Time:        time.Now().UTC(),
		TenantID:    tenantID,
		Data:        data,
	}
}
