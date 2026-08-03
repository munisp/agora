// Package events builds CloudEvents 1.0 envelopes per SPEC §4:
// {specversion, id, source, type, subject, time, tenantid (ext), data}.
package events

import (
	"time"

	"github.com/google/uuid"
)

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
