// Package consent implements the NDPA consent registry (SPEC-W12 §4 /
// Agent C): ConsentRecord capture, subject lookup, the service-to-service
// consent check consumed by kyc-service, and tombstone-only erasure with a
// CloudEvent on opendesk.consent.erasure.v1.
package consent

import (
	"time"

	"github.com/google/uuid"
)

// Event type + topic contract (SPEC-W12 §4). The topic is configurable via
// CONSENT_ERASURE_TOPIC; these are the defaults consumers subscribe to.
const (
	// ErasureEventType is the CloudEvent type published on erasure.
	ErasureEventType = "com.opendesk.consent.ErasureRequested"
	// DefaultErasureTopic is the default Kafka topic for erasure events.
	DefaultErasureTopic = "opendesk.consent.erasure.v1"
)

// Record mirrors the identity.consents table — the ConsentRecord of
// SPEC-W12 contract §4. DataSubjectID is a phone number in E.164 or a
// contact uuid (free-form text by design: the registry must not need to
// resolve subjects to store their consent).
type Record struct {
	ConsentID       uuid.UUID  `json:"consent_id"`
	TenantID        uuid.UUID  `json:"tenant_id"`
	DataSubjectID   string     `json:"data_subject_id"`
	Purpose         string     `json:"purpose"`
	CapturedTS      time.Time  `json:"captured_ts"`
	CapturedChannel string     `json:"captured_channel"`
	CapturedLocale  string     `json:"captured_locale"`
	ErasureTS       *time.Time `json:"erasure_ts"`
}
