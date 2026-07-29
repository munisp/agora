// Package incidents implements SPEC-W11 Part B: the Incident Data Packet
// (IDP) domain model, Kafka ingestion (topic opendesk.incidents, consumer
// group booking-incidents), signed dispatch to tenant webhook endpoints and
// critical/high-severity auto-outreach via the notification-worker paced
// fast-lane (kind incident_alert, priority).
//
// The canonical IDP JSON shape is owned by docs/schemas/incident-data-packet.json
// (SPEC-W11); the structs below mirror it field-for-field.
package incidents

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SchemaVersion is the IDP schema version this service emits/accepts.
const SchemaVersion = "1.0"

// EventTypeIDPCreated is the CloudEvents type on topic opendesk.incidents.
const EventTypeIDPCreated = "com.opendesk.incidents.IDPCreated"

// Channel values of the IDP.
const (
	ChannelVoice    = "voice"
	ChannelWhatsApp = "whatsapp"
	ChannelTelegram = "telegram"
	ChannelWeb      = "web"
	ChannelSMS      = "sms"
	ChannelWebhook  = "webhook"
)

// Severity values of the IDP.
const (
	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"
)

// Incident row statuses (CHECK constraint of the incidents table).
const (
	StatusNew          = "new"
	StatusDispatched   = "dispatched"
	StatusAcknowledged = "acknowledged"
	StatusClosed       = "closed"
)

// Delivery row statuses (mirrors the Wave-5 webhook delivery workflow:
// pending → retrying → delivered | dlq).
const (
	DeliveryPending   = "pending"
	DeliveryRetrying  = "retrying"
	DeliveryDelivered = "delivered"
	DeliveryDLQ       = "dlq"
)

// ErrInvalidInput marks deterministic validation failures (no retry).
var ErrInvalidInput = errors.New("invalid incident input")

// Location is the IDP location block (nullable).
type Location struct {
	Lat         float64 `json:"lat"`
	Lng         float64 `json:"lng"`
	AccuracyM   float64 `json:"accuracy_m"`
	Source      string  `json:"source"` // gps | address | caller_id | manual
	AddressText string  `json:"address_text"`
}

// IDP is the canonical Incident Data Packet (SPEC-W11).
type IDP struct {
	IncidentID       uuid.UUID  `json:"incident_id"`
	SchemaVersion    string     `json:"schema_version"`
	TenantID         uuid.UUID  `json:"tenant_id"`
	CapturedAt       time.Time  `json:"captured_at"`
	Channel          string     `json:"channel"`
	Location         *Location  `json:"location"`
	CallbackNumber   *string    `json:"callback_number"`
	IncidentType     string     `json:"incident_type"`
	Severity         string     `json:"severity"`
	PeopleInvolved   int        `json:"people_involved"`
	Hazards          []string   `json:"hazards"`
	NarrativeSummary string     `json:"narrative_summary"`
	ReferenceNumber  string     `json:"reference_number"`
	ContactID        *uuid.UUID `json:"contact_id"`
	ConversationID   *uuid.UUID `json:"conversation_id"`
}

// validSeverities is the IDP severity enum.
var validSeverities = map[string]bool{
	SeverityCritical: true, SeverityHigh: true, SeverityMedium: true, SeverityLow: true,
}

// Validate checks the minimal field set required for persistence. Partial
// IDPs (e.g. IoT webhook payloads) are normalized by Complete first.
func (p *IDP) Validate() error {
	if p.IncidentID == uuid.Nil {
		return fmt.Errorf("%w: incident_id is required", ErrInvalidInput)
	}
	if p.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if p.IncidentType == "" {
		return fmt.Errorf("%w: incident_type is required", ErrInvalidInput)
	}
	if !validSeverities[p.Severity] {
		return fmt.Errorf("%w: severity %q (want critical|high|medium|low)", ErrInvalidInput, p.Severity)
	}
	if len(p.NarrativeSummary) > 500 {
		return fmt.Errorf("%w: narrative_summary exceeds 500 chars", ErrInvalidInput)
	}
	return nil
}

// Complete fills IDP defaults for partial payloads (IoT webhook ingest):
// incident id, schema version, capture time, channel, severity and the
// tenant-facing reference number. It mutates p and returns it.
func (p *IDP) Complete() *IDP {
	if p.IncidentID == uuid.Nil {
		p.IncidentID = uuid.New()
	}
	if p.SchemaVersion == "" {
		p.SchemaVersion = SchemaVersion
	}
	if p.CapturedAt.IsZero() {
		p.CapturedAt = time.Now().UTC()
	}
	if p.Channel == "" {
		p.Channel = ChannelWebhook
	}
	if p.Severity == "" {
		p.Severity = SeverityMedium
	}
	if p.IncidentType == "" {
		p.IncidentType = "other"
	}
	if p.ReferenceNumber == "" {
		p.ReferenceNumber = ReferenceNumber(p.IncidentID, p.CapturedAt)
	}
	if p.Hazards == nil {
		p.Hazards = []string{}
	}
	return p
}

// ReferenceNumber derives a tenant-facing reference INC-{YYYY}-{seq:06d}
// deterministically from the incident id (webhook-ingest path — the
// conversation-service emitter owns the per-tenant sequence, SPEC-W11 Part A).
func ReferenceNumber(id uuid.UUID, at time.Time) string {
	var sum uint32
	for _, b := range id {
		sum = sum*31 + uint32(b)
	}
	return fmt.Sprintf("INC-%d-%06d", at.UTC().Year(), sum%1000000)
}

// NeedsOutreach reports whether the incident warrants an automatic alert to
// the reporter: critical/high severity AND a reachable contact (callback
// number or CRM contact id), per SPEC-W11 Part B §5.
func (p *IDP) NeedsOutreach() bool {
	if p.Severity != SeverityCritical && p.Severity != SeverityHigh {
		return false
	}
	return (p.CallbackNumber != nil && *p.CallbackNumber != "") || p.ContactID != nil
}

// OutreachChannel picks the alert channel: when the incident arrived over a
// messaging channel we answer on the same channel; anything else falls back
// to sms (per-tenant provider routing happens in notification-worker).
func (p *IDP) OutreachChannel() string {
	switch p.Channel {
	case ChannelWhatsApp, ChannelTelegram:
		return p.Channel
	default:
		return ChannelSMS
	}
}

// OutreachText renders the incident_alert message template
// (SPEC-W11 Part B §5): always reference number + incident type, plus the
// location address when the IDP carries one.
func (p *IDP) OutreachText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "EMERGENCY ALERT %s: %s incident reported", p.ReferenceNumber, p.IncidentType)
	if p.Location != nil && p.Location.AddressText != "" {
		fmt.Fprintf(&b, " near %s", p.Location.AddressText)
	}
	fmt.Fprintf(&b, ". Severity: %s. Help has been notified; keep this line open.", p.Severity)
	return b.String()
}

// Header names of the signed incident dispatch delivery (SPEC-W11 Part B §4).
const (
	// HeaderSignature carries the hex HMAC-SHA256 of the raw body with the
	// endpoint secret (no "sha256=" prefix — plain hex per spec).
	HeaderSignature = "X-OpenDesk-Signature"
	// HeaderIncident carries the incident id.
	HeaderIncident = "X-OpenDesk-Incident"
)

// SignatureHex computes the X-OpenDesk-Signature value for an incident
// delivery: lowercase hex HMAC-SHA256(secret, body). An empty secret yields
// an empty signature (unsigned delivery).
func SignatureHex(secret string, body []byte) string {
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
