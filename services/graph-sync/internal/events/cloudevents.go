// Package events parses CloudEvents 1.0 envelopes per SPEC §4:
// {specversion, id, source, type, subject, time, tenantid (ext), data}.
// Mirrors crm-sync-service/internal/events; data field names mirror the
// producers (booking-service bookingops, identity-service, W12 consent
// registry, W13 funnel events).
package events

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CloudEvent is the canonical envelope used on every Kafka topic.
type CloudEvent struct {
	SpecVersion string         `json:"specversion"`
	ID          string         `json:"id"`
	Source      string         `json:"source"`
	Type        string         `json:"type"`
	Subject     string         `json:"subject,omitempty"` // tenant slug
	Time        time.Time      `json:"time"`
	TenantID    string         `json:"tenantid,omitempty"`
	Data        map[string]any `json:"data"`
}

// Parse decodes one CloudEvent envelope.
func Parse(raw []byte) (CloudEvent, error) {
	var evt CloudEvent
	if err := json.Unmarshal(raw, &evt); err != nil {
		return evt, fmt.Errorf("malformed CloudEvent envelope: %v", err)
	}
	if evt.Type == "" {
		return evt, fmt.Errorf("CloudEvent carries no type")
	}
	return evt, nil
}

// Event type constants consumed from the backbone (SPEC-W28 §2). Unknown
// types on consumed topics are acknowledged and skipped (forward-compatible).
const (
	// identity events (opendesk.identity.events)
	TypeTenantProvisioned = "com.opendesk.identity.TenantProvisioned"
	TypeContactCaptured   = "com.opendesk.identity.ContactCaptured"
	TypeConsentGranted    = "com.opendesk.identity.ConsentGranted"
	TypeConsentRevoked    = "com.opendesk.identity.ConsentRevoked"
	// SPEC-W45 K9: tenant teardown cascade (delete the tenant subgraph).
	TypeTenantDeleted = "com.opendesk.identity.TenantDeleted"

	// booking events (opendesk.booking.events)
	TypeBookingCreated     = "com.opendesk.booking.BookingCreated"
	TypeBookingConfirmed   = "com.opendesk.booking.BookingConfirmed"
	TypeBookingRescheduled = "com.opendesk.booking.BookingRescheduled"
	TypeBookingCancelled   = "com.opendesk.booking.BookingCancelled"
	TypeBookingCompleted   = "com.opendesk.booking.BookingCompleted"
	TypeBookingNoShow      = "com.opendesk.booking.BookingNoShow"

	// conversation transcripts (opendesk.conversation.transcripts)
	TypeSessionEnded = "com.opendesk.conversation.SessionEnded"

	// funnel / CAC events (GRAPH_SYNC_CAC_TOPIC, optional)
	TypeLeadCreated = "com.opendesk.leads.LeadCreated"

	// consent erasure (opendesk.consent.erasure.v1, SPEC-W12)
	TypeErasureRequested = "com.opendesk.consent.ErasureRequested"

	// graph enrichment (opendesk.graph.enrichment.v1, emitted nightly by
	// infra/lakehouse/spark/jobs/graph_enrichment.py — the event shape
	// mirrors build_enrichment_event there, kept in sync by contract)
	TypePersonEnrichment = "com.opendesk.graph.PersonEnrichment"

	// civic case events (opendesk.civic.events.v1, SPEC-W32 §0.3 / §3 WS-D).
	TypeCivicReportReceived = "com.opendesk.civic.ReportReceived"
	TypeCivicStatusChanged  = "com.opendesk.civic.StatusChanged"
	TypeCivicMerged         = "com.opendesk.civic.Merged"

	// OCR extraction events (opendesk.ocr.events, SPEC-W55 Scope A): the
	// W53 ocr-service emits DocumentExtracted after a successful extraction.
	TypeOCRDocumentExtracted = "com.opendesk.ocr.DocumentExtracted"

	// SPEC-W67 G6: ocr-service's cross-tenant document registry emits ONE
	// CrossTenantDocumentMatch per (later tenant, hash) on the same
	// opendesk.ocr.events topic. CloudEvents tenantid/subject address the
	// LATER tenant only; earlier tenants never learn anything.
	TypeOCRCrossTenantMatch = "com.opendesk.ocr.CrossTenantDocumentMatch"
)

// BookingData mirrors booking-service bookingops.marshalEvent payloads
// (same shape as crm-sync-service's consumer contract — duplicated, not
// shared).
type BookingData struct {
	BookingID    string    `json:"booking_id"`
	StartsAt     time.Time `json:"starts_at"`
	EndsAt       time.Time `json:"ends_at"`
	Status       string    `json:"status"`
	Source       string    `json:"source"`
	OfferingID   string    `json:"offering_id"`
	OfferingName string    `json:"offering_name"`
	ContactID    string    `json:"contact_id"`
	ContactName  string    `json:"contact_name"`
	Phone        string    `json:"phone"`
	Email        string    `json:"email"`
	Reason       string    `json:"reason"`
	Showed       *bool     `json:"showed"`
}

// ContactCapturedData mirrors the field-PWA/web lead capture payload
// (W13/W16): a lead appearing with its first-touch channel and geo.
type ContactCapturedData struct {
	LeadID     string  `json:"lead_id"`
	ContactID  string  `json:"contact_id"`
	PersonID   string  `json:"person_id"`
	Name       string  `json:"name"`
	Phone      string  `json:"phone"`
	Channel    string  `json:"channel"` // channel_of_first_touch (field|web|import|...)
	Source     string  `json:"source"`
	CapturedAt string  `json:"captured_at"`
	LGA        string  `json:"lga"`
	Ward       string  `json:"ward"`
	Lat        float64 `json:"lat"`
	Lon        float64 `json:"lon"`
	// Quarantine marks imported, consent-unverified persons (SPEC-W28 §5
	// gate 4): visible in the graph but audience-ineligible.
	Quarantine      bool     `json:"quarantine"`
	ConsentPurposes []string `json:"consent_purposes"`
}

// ConsentData mirrors the W12 consent registry events: one purpose grant or
// revocation for a person.
type ConsentData struct {
	ConsentID string `json:"consent_id"`
	PersonID  string `json:"person_id"`
	ContactID string `json:"contact_id"`
	Phone     string `json:"phone"`
	Name      string `json:"name"`
	Purpose   string `json:"purpose"` // marketing|reminders|kyc|...
	GrantedAt string `json:"granted_at"`
	RevokedAt string `json:"revoked_at"`
	ProofRef  string `json:"proof_ref"`
}

// TranscriptData mirrors the conversation transcripts topic (session-end
// payloads): a caller identified by phone, optional display name.
type TranscriptData struct {
	ConversationID string `json:"conversation_id"`
	Phone          string `json:"phone"`
	Name           string `json:"name"`
	Channel        string `json:"channel"` // voice|sms|whatsapp
	EndedAt        string `json:"ended_at"`
}

// ErasureData mirrors the W12 erasure tombstone (SPEC-W28 §4): tenant +
// person to erase from the graph.
type ErasureData struct {
	PersonID  string `json:"person_id"`
	ContactID string `json:"contact_id"`
	PhoneHash string `json:"phone_hash"`
	Phone     string `json:"phone"`
	Reason    string `json:"reason"`
}

// EnrichmentData mirrors graph_enrichment.py's build_enrichment_event
// data payload (duplicated, not shared): per-Person property rows keyed by
// tenant_id + person_id. The CloudEvent id is
// "<tenant_id>:<person_id>:<snapshot_day>" so same-day re-runs dedupe via
// the processed marker (W24 idempotency pattern).
type EnrichmentData struct {
	PersonID    string         `json:"person_id"`
	SnapshotDay string         `json:"snapshot_day"` // YYYY-MM-DD
	Properties  map[string]any `json:"properties"`
}

// CivicReportData mirrors the booking-service civic module ReportReceived
// payload (SPEC-W32 §3 WS-A/WS-D). The reporter phone is used ONLY to derive
// the salted phone_hash for Person resolution — raw PII never touches the
// graph; the Case node itself is PII-free (ref/category/ward/status only).
type CivicReportData struct {
	Ref           string  `json:"ref"`
	Category      string  `json:"category"` // category slug (roads|water|...)
	Status        string  `json:"status"`   // "new" at intake
	Ward          string  `json:"ward"`
	LGA           string  `json:"lga"`
	Lat           float64 `json:"lat"`
	Lon           float64 `json:"lon"`
	Channel       string  `json:"channel"` // web|pwa|whatsapp
	ReporterPhone string  `json:"reporter_phone"`
	ReporterName  string  `json:"reporter_name"`
	CreatedAt     string  `json:"created_at"`
}

// CivicStatusData mirrors the civic StatusChanged payload (SPEC-W32 §3:
// data carries ref, status, optional acked_at/resolved_at, reporter_phone
// when wants_updates — the phone is NOT used for graph writes here).
type CivicStatusData struct {
	Ref        string `json:"ref"`
	Status     string `json:"status"`
	AckedAt    string `json:"acked_at"`
	ResolvedAt string `json:"resolved_at"`
}

// CivicMergedData mirrors the civic Merged payload: ref points at the
// canonical case ref (duplicate merge, SPEC-W32 §2 merged_into).
type CivicMergedData struct {
	Ref          string `json:"ref"`
	CanonicalRef string `json:"canonical_ref"`
}

// OCRDocumentData mirrors the ocr-service DocumentExtracted data payload
// (ocr-service app/events.py document_extracted_event; SPEC-W55 Scope A).
// HASH-ONLY STUB POSTURE (SPEC-W55 sacred constraint): this struct
// deliberately declares ONLY the id/hash/metadata fields — the payload's
// extracted_fields, storage_bucket and storage_key are NEVER declared,
// decoded, persisted, or logged in the graph-sync Document path (PII
// minimization; hash-only stubs keep person erasure out of the Document
// path and leave tenant erasure to the tenant-scoped wipe, RECON §5.4).
type OCRDocumentData struct {
	DocumentID   string `json:"document_id"`
	TenantID     string `json:"tenant_id"`
	Kind         string `json:"kind"`   // receipt|document|knowledge_scan
	Status       string `json:"status"` // "extracted" (producer emits only on success)
	PageCount    int    `json:"page_count"`
	SHA256       string `json:"sha256"` // lowercase hex of the full bytes
	PHash        string `json:"phash"`  // 64-bit dHash, 16-char lowercase hex (may be empty)
	ModelVersion string `json:"model_version"`
}

// Data decodes the event's data payload into v.
func (evt CloudEvent) DecodeData(v any) error {
	raw, err := json.Marshal(evt.Data)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// OCRCrossTenantMatchData mirrors the PINNED SPEC-W67 match-event payload
// (the ONLY Coder A <-> Coder B interface; ocr-service emits it). It carries
// EXACTLY four fields — the LATER tenant's OWN document_id + sha256 + kind
// plus the cross-tenant tenant_count integer. NO other tenant ids, NO other
// document ids, NO phash (G2: phash is excluded from every cross-tenant
// artifact), NO storage pointers, NO extracted fields/text — the hash-only
// stub posture (SPEC-W55 sacred constraint) is unchanged.
type OCRCrossTenantMatchData struct {
	DocumentID  string `json:"document_id"` // the LATER tenant's own document id
	SHA256      string `json:"sha256"`      // lowercase hex of the tenant's own document bytes
	Kind        string `json:"kind"`        // receipt|document|kyc_*|...
	TenantCount int    `json:"tenant_count"`
}

// crossTenantMatchAllowlist is the PINNED payload key set (SPEC-W67: "data
// = EXACTLY {document_id, sha256, kind, tenant_count: int}").
var crossTenantMatchAllowlist = map[string]bool{
	"document_id":  true,
	"sha256":       true,
	"kind":         true,
	"tenant_count": true,
}

// DecodeCrossTenantMatch strictly decodes the pinned match payload: any key
// outside the allowlist REJECTS the event (dead-letter posture — a widened
// payload could carry cross-tenant disclosure and must never be projected),
// and every pinned key must be present with a usable value (document_id /
// sha256 non-empty, tenant_count a positive integer). Callers wrap the
// error as Permanent; the consumer never crashes on a malformed event.
func (evt CloudEvent) DecodeCrossTenantMatch() (OCRCrossTenantMatchData, error) {
	var d OCRCrossTenantMatchData
	if evt.Data == nil {
		return d, fmt.Errorf("cross-tenant match event carries no data payload")
	}
	for k := range evt.Data {
		if !crossTenantMatchAllowlist[k] {
			return d, fmt.Errorf("cross-tenant match payload carries disallowed key %q (pinned allowlist: document_id, sha256, kind, tenant_count)", k)
		}
	}
	for _, k := range []string{"document_id", "sha256", "kind", "tenant_count"} {
		if _, ok := evt.Data[k]; !ok {
			return d, fmt.Errorf("cross-tenant match payload misses required key %q", k)
		}
	}
	if err := evt.DecodeData(&d); err != nil {
		return d, fmt.Errorf("cross-tenant match payload decode: %v", err)
	}
	if strings.TrimSpace(d.DocumentID) == "" {
		return d, fmt.Errorf("cross-tenant match payload requires a non-empty document_id")
	}
	if strings.TrimSpace(d.SHA256) == "" {
		return d, fmt.Errorf("cross-tenant match payload requires a non-empty sha256")
	}
	if d.TenantCount < 1 {
		return d, fmt.Errorf("cross-tenant match payload requires tenant_count >= 1 (got %d)", d.TenantCount)
	}
	return d, nil
}
