// Syncer: the graph-sync event handlers (SPEC-W28 §4 WS-A). Every handler
// is idempotent by event_id (processed marker in the graph, W24 pattern),
// requires tenant_id on every write (SPEC §5 gate 1), stores phones only as
// salted SHA-256 hashes, and degrades gracefully when Ollama is unreachable
// (embedding merge proposals are skipped; exact phone_hash merges still
// work).
package consumer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opendesk/graph-sync/internal/embed"
	"github.com/opendesk/graph-sync/internal/events"
	"github.com/opendesk/graph-sync/internal/graph"
	"github.com/opendesk/graph-sync/internal/metrics"
	"go.uber.org/zap"
)

// AuditProducer emits the erasure-done audit CloudEvent
// (opendesk.graph.erasure.done.v1). A nil producer disables emission
// (logged, never fatal).
type AuditProducer interface {
	Publish(ctx context.Context, topic, key string, evt events.CloudEvent) error
}

// Syncer wires the graph client, embedder and audit producer into the
// per-topic Handlers.
type Syncer struct {
	Graph   graph.Client
	Embed   embed.Embedder // nil → embeddings skipped entirely (documented degrade)
	Audit   AuditProducer  // nil → erasure-done audit emission skipped (logged)
	Metrics *metrics.Registry
	Log     *zap.Logger

	// Salt is PHONE_HASH_SALT for the leads-dedupe-style phone hash.
	Salt string
	// MergeThreshold is the cosine floor for MERGE_CANDIDATE edges
	// (SPEC §4: 0.92).
	MergeThreshold float64
	// ErasureDoneTopic is the audit topic (default
	// opendesk.graph.erasure.done.v1).
	ErasureDoneTopic string
	// Now is the clock (tests pin it for dual-TZ assertions).
	Now func() time.Time
}

func (s *Syncer) log() *zap.Logger {
	if s.Log != nil {
		return s.Log
	}
	return zap.NewNop()
}

func (s *Syncer) metrics() *metrics.Registry {
	if s.Metrics != nil {
		return s.Metrics
	}
	return metrics.New()
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Syncer) threshold() float64 {
	if s.MergeThreshold > 0 {
		return s.MergeThreshold
	}
	return 0.92
}

// tenantOf extracts the mandatory tenant id (CloudEvent `tenantid`
// extension wins, data.tenant_id fallback). An event without a tenant can
// NEVER be written (SPEC §5 gate 1) — it is a poison message.
func (s *Syncer) tenantOf(evt events.CloudEvent) (string, error) {
	t := strings.TrimSpace(evt.TenantID)
	if t == "" {
		if v, ok := evt.Data["tenant_id"].(string); ok {
			t = strings.TrimSpace(v)
		}
	}
	if t == "" {
		return "", Permanent(fmt.Errorf("event %s carries no tenant_id (tenant_id is mandatory on every graph node)", evt.ID))
	}
	return t, nil
}

// processed checks the idempotency marker; already-processed events are
// skipped (returns true). Marker failures are retried (transient).
func (s *Syncer) processed(ctx context.Context, evt events.CloudEvent, tenantID string) (bool, error) {
	if evt.ID == "" {
		// Producers are contract-bound to set the CloudEvent id; without one
		// we cannot dedupe, so we process (at-least-once beats dropping —
		// same posture as the notification outbox consumer).
		return false, nil
	}
	already, err := s.Graph.MarkProcessed(ctx, evt.ID, tenantID, s.now())
	if err != nil {
		return false, fmt.Errorf("processed marker: %w", err)
	}
	if already {
		s.metrics().Inc("events_duplicate")
		s.log().Info("duplicate event skipped (idempotency marker)",
			zap.String("event_id", evt.ID), zap.String("type", evt.Type))
	}
	return already, nil
}

// ---------------------------------------------------------------------------
// booking events (opendesk.booking.events)
// ---------------------------------------------------------------------------

// HandleBooking consumes opendesk.booking.events: upserts the Person
// (phone hashed, entity-resolved) and the Booking/Offering subgraph.
func (s *Syncer) HandleBooking(ctx context.Context, evt events.CloudEvent) error {
	switch evt.Type {
	case events.TypeBookingCreated, events.TypeBookingConfirmed,
		events.TypeBookingRescheduled, events.TypeBookingCancelled,
		events.TypeBookingCompleted, events.TypeBookingNoShow:
	default:
		return nil // forward-compatible: ack unknown types on the topic
	}
	tenantID, err := s.tenantOf(evt)
	if err != nil {
		return err
	}
	if dup, err := s.processed(ctx, evt, tenantID); err != nil {
		return err
	} else if dup {
		return nil
	}
	var d events.BookingData
	if err := evt.DecodeData(&d); err != nil {
		return Permanent(fmt.Errorf("malformed booking data: %v", err))
	}
	if d.BookingID == "" {
		return Permanent(errors.New("booking event carries no booking_id"))
	}
	if err := s.Graph.UpsertTenant(ctx, tenantID, evt.Subject, s.now()); err != nil {
		return err
	}
	person := s.personFromPhone(tenantID, firstNonEmpty(d.ContactID, d.BookingID+"-contact"),
		d.ContactName, d.Phone, channelOr(d.Source, "web"), false, "")
	personID, err := s.upsertResolvedPerson(ctx, person)
	if err != nil {
		return err
	}
	status := d.Status
	if evt.Type == events.TypeBookingNoShow {
		status = "no_show"
	}
	showed := d.Showed
	if evt.Type == events.TypeBookingNoShow && showed == nil {
		f := false
		showed = &f
	}
	// SPEC-W30: optional staff attribution (D6 ghost bookings) and the
	// cancellation timestamp (flash create->cancel detection).
	var createdBy string
	if v, ok := evt.Data["created_by"].(string); ok {
		createdBy = strings.TrimSpace(v)
	}
	var cancelledAt *time.Time
	if evt.Type == events.TypeBookingCancelled {
		t := orEventTime(time.Time{}, evt, s.now())
		cancelledAt = &t
	}
	return s.Graph.UpsertBooking(ctx, graph.Booking{
		BookingID:    d.BookingID,
		TenantID:     tenantID,
		Status:       status,
		OfferingID:   d.OfferingID,
		OfferingName: d.OfferingName,
		CreatedAt:    orEventTime(d.StartsAt, evt, s.now()),
		Showed:       showed,
		CreatedBy:    createdBy,
		CancelledAt:  cancelledAt,
	}, personID)
}

// ---------------------------------------------------------------------------
// identity events (opendesk.identity.events)
// ---------------------------------------------------------------------------

// HandleIdentity consumes opendesk.identity.events: tenant provisioning,
// contact capture (field PWA / web / import) and consent grants/revocations.
func (s *Syncer) HandleIdentity(ctx context.Context, evt events.CloudEvent) error {
	switch evt.Type {
	case events.TypeTenantProvisioned, events.TypeContactCaptured,
		events.TypeConsentGranted, events.TypeConsentRevoked:
	default:
		return nil
	}
	tenantID, err := s.tenantOf(evt)
	if err != nil {
		return err
	}
	if dup, err := s.processed(ctx, evt, tenantID); err != nil {
		return err
	} else if dup {
		return nil
	}
	switch evt.Type {
	case events.TypeTenantProvisioned:
		var d struct {
			Slug string `json:"slug"`
		}
		_ = evt.DecodeData(&d)
		return s.Graph.UpsertTenant(ctx, tenantID, firstNonEmpty(d.Slug, evt.Subject), s.now())
	case events.TypeContactCaptured:
		return s.contactCaptured(ctx, evt, tenantID)
	default: // consent granted / revoked
		return s.consentChanged(ctx, evt, tenantID)
	}
}

// contactCaptured handles ContactCaptured (identity topic) and LeadCreated
// (CAC topic) — both describe a lead entering the graph with first-touch
// channel, geo and the quarantine flag (imported, consent-unverified;
// SPEC §5 gate 4).
func (s *Syncer) contactCaptured(ctx context.Context, evt events.CloudEvent, tenantID string) error {
	var d events.ContactCapturedData
	if err := evt.DecodeData(&d); err != nil {
		return Permanent(fmt.Errorf("malformed contact-capture data: %v", err))
	}
	leadID := firstNonEmpty(d.LeadID, d.ContactID, d.PersonID)
	if leadID == "" {
		return Permanent(errors.New("contact-capture event carries no lead_id/contact_id"))
	}
	if err := s.Graph.UpsertTenant(ctx, tenantID, evt.Subject, s.now()); err != nil {
		return err
	}
	person := s.personFromPhone(tenantID,
		firstNonEmpty(d.PersonID, d.ContactID, "lead-"+leadID),
		d.Name, d.Phone, channelOr(d.Channel, "field"), d.Quarantine,
		strings.Join(d.ConsentPurposes, ","))
	personID, err := s.upsertResolvedPerson(ctx, person)
	if err != nil {
		return err
	}
	capturedAt := parseTimeOr(d.CapturedAt, evt.Time, s.now())
	// SPEC-W30: optional staff/agent attribution rides the raw event map
	// (same pattern as referred_by_person_id below) — the typed struct
	// predates it. Detectors D2/D3/D4 stay silent when upstream omits it.
	var capturedBy string
	for _, k := range []string{"agent_id", "staff_id", "captured_by"} {
		if v, ok := evt.Data[k].(string); ok && strings.TrimSpace(v) != "" {
			capturedBy = strings.TrimSpace(v)
			break
		}
	}
	if err := s.Graph.UpsertContact(ctx, graph.Contact{
		LeadID:              leadID,
		TenantID:            tenantID,
		ChannelOfFirstTouch: channelOr(d.Channel, "field"),
		Source:              d.Source,
		CapturedAt:          capturedAt,
		CapturedBy:          capturedBy,
		LGA:                 d.LGA,
		Ward:                d.Ward,
		Lat:                 d.Lat,
		Lon:                 d.Lon,
		HasGeo:              d.LGA != "" || d.Ward != "" || d.Lat != 0 || d.Lon != 0,
	}, personID); err != nil {
		return err
	}
	// Referral tree seam (W14-A): an optional referrer on the capture
	// payload wires (referrer)-[:REFERRED]->(new person).
	if ref, _ := evt.Data["referred_by_person_id"].(string); strings.TrimSpace(ref) != "" {
		program, _ := evt.Data["referral_program"].(string)
		if err := s.Graph.LinkReferral(ctx, tenantID, strings.TrimSpace(ref), personID, program, capturedAt); err != nil {
			return err
		}
	}
	return nil
}

// HandleCAC consumes the optional funnel/CAC topic (GRAPH_SYNC_CAC_TOPIC):
// LeadCreated events enter the graph exactly like identity contact captures.
func (s *Syncer) HandleCAC(ctx context.Context, evt events.CloudEvent) error {
	if evt.Type != events.TypeLeadCreated && evt.Type != events.TypeContactCaptured {
		return nil
	}
	tenantID, err := s.tenantOf(evt)
	if err != nil {
		return err
	}
	if dup, err := s.processed(ctx, evt, tenantID); err != nil {
		return err
	} else if dup {
		return nil
	}
	return s.contactCaptured(ctx, evt, tenantID)
}

// consentChanged upserts the Consent node and the CONSENTED edge
// (revocations stamp revoked_at).
func (s *Syncer) consentChanged(ctx context.Context, evt events.CloudEvent, tenantID string) error {
	var d events.ConsentData
	if err := evt.DecodeData(&d); err != nil {
		return Permanent(fmt.Errorf("malformed consent data: %v", err))
	}
	if d.ConsentID == "" || d.Purpose == "" {
		return Permanent(errors.New("consent event requires consent_id and purpose"))
	}
	if err := s.Graph.UpsertTenant(ctx, tenantID, evt.Subject, s.now()); err != nil {
		return err
	}
	person := s.personFromPhone(tenantID,
		firstNonEmpty(d.PersonID, d.ContactID, "consent-"+d.ConsentID),
		d.Name, d.Phone, "consent", false, d.Purpose)
	personID, err := s.upsertResolvedPerson(ctx, person)
	if err != nil {
		return err
	}
	grantedAt := parseTimeOr(d.GrantedAt, evt.Time, s.now())
	var revokedAt *time.Time
	if evt.Type == events.TypeConsentRevoked {
		t := parseTimeOr(d.RevokedAt, evt.Time, s.now())
		revokedAt = &t
	}
	return s.Graph.LinkConsent(ctx, graph.Consent{
		ConsentID: d.ConsentID,
		TenantID:  tenantID,
		Purpose:   d.Purpose,
		GrantedAt: grantedAt,
		RevokedAt: revokedAt,
		ProofRef:  d.ProofRef,
	}, personID)
}

// ---------------------------------------------------------------------------
// conversation transcripts (opendesk.conversation.transcripts)
// ---------------------------------------------------------------------------

// HandleTranscript consumes opendesk.conversation.transcripts: the caller
// appears as a Person (phone hashed, voice channel). Transcripts carry no
// bookings/consents — this is identity resolution only.
func (s *Syncer) HandleTranscript(ctx context.Context, evt events.CloudEvent) error {
	if evt.Type != events.TypeSessionEnded {
		return nil
	}
	tenantID, err := s.tenantOf(evt)
	if err != nil {
		return err
	}
	if dup, err := s.processed(ctx, evt, tenantID); err != nil {
		return err
	} else if dup {
		return nil
	}
	var d events.TranscriptData
	if err := evt.DecodeData(&d); err != nil {
		return Permanent(fmt.Errorf("malformed transcript data: %v", err))
	}
	if d.Phone == "" {
		return Permanent(errors.New("transcript event carries no phone"))
	}
	if err := s.Graph.UpsertTenant(ctx, tenantID, evt.Subject, s.now()); err != nil {
		return err
	}
	person := s.personFromPhone(tenantID, "", d.Name, d.Phone, channelOr(d.Channel, "voice"), false, "")
	_, err = s.upsertResolvedPerson(ctx, person)
	return err
}

// ---------------------------------------------------------------------------
// consent erasure (opendesk.consent.erasure.v1)
// ---------------------------------------------------------------------------

// HandleErasure consumes opendesk.consent.erasure.v1 (SPEC §4): DETACH
// DELETE the Person subgraph for tenant+person, then emit the
// opendesk.graph.erasure.done.v1 audit event.
func (s *Syncer) HandleErasure(ctx context.Context, evt events.CloudEvent) error {
	if evt.Type != events.TypeErasureRequested {
		return nil
	}
	tenantID, err := s.tenantOf(evt)
	if err != nil {
		return err
	}
	if dup, err := s.processed(ctx, evt, tenantID); err != nil {
		return err
	} else if dup {
		return nil
	}
	var d events.ErasureData
	if err := evt.DecodeData(&d); err != nil {
		return Permanent(fmt.Errorf("malformed erasure data: %v", err))
	}
	personID := firstNonEmpty(d.PersonID, d.ContactID)
	if personID == "" && d.Phone != "" {
		// Phone-only tombstone: resolve via the salted hash (same tenant).
		id, err := s.Graph.FindPersonByPhoneHash(ctx, tenantID, graph.PhoneHash(s.Salt, tenantID, d.Phone))
		if err != nil {
			return err
		}
		personID = id
	}
	if personID == "" {
		return Permanent(errors.New("erasure event carries neither person_id nor phone"))
	}
	found, err := s.Graph.ErasePerson(ctx, tenantID, personID)
	if err != nil {
		return err
	}
	s.metrics().Inc("persons_erased")
	s.log().Info("person subgraph erased",
		zap.String("tenant_id", tenantID), zap.String("person_id", personID),
		zap.Bool("found", found), zap.String("erasure_event_id", evt.ID))
	return s.emitErasureDone(ctx, evt, tenantID, personID, found)
}

// emitErasureDone publishes the SPEC §4 audit event (best-effort contract:
// a publish failure retries the whole erasure event — DETACH DELETE is
// idempotent so redelivery is safe).
func (s *Syncer) emitErasureDone(ctx context.Context, src events.CloudEvent, tenantID, personID string, found bool) error {
	if s.Audit == nil || s.ErasureDoneTopic == "" {
		s.log().Warn("erasure audit producer not wired; skipping erasure-done emission",
			zap.String("person_id", personID))
		return nil
	}
	audit := events.CloudEvent{
		SpecVersion: "1.0",
		ID:          "graph-erasure-done-" + src.ID,
		Source:      "graph-sync",
		Type:        "com.opendesk.graph.ErasureDone",
		Subject:     src.Subject,
		Time:        s.now(),
		TenantID:    tenantID,
		Data: map[string]any{
			"person_id":        personID,
			"found":            found,
			"erasure_event_id": src.ID,
			"tenant_id":        tenantID,
		},
	}
	if err := s.Audit.Publish(ctx, s.ErasureDoneTopic, personID, audit); err != nil {
		return fmt.Errorf("publish erasure-done audit: %w", err)
	}
	s.metrics().Inc("erasure_done_emitted")
	return nil
}

// ---------------------------------------------------------------------------
// civic case events (opendesk.civic.events.v1, SPEC-W32 §3 WS-D)
// ---------------------------------------------------------------------------

// HandleCivic consumes opendesk.civic.events.v1: the civic Case projection.
// Case nodes are PII-free (ref/category/ward/status only, SPEC-W32 §5 gate
// 5); the reporter's phone is used ONLY to resolve/create the Person via
// the existing salted-hash path (raw phone never touches the graph), and
// the REPORTED edge dies with the Person on W28 erasure.
func (s *Syncer) HandleCivic(ctx context.Context, evt events.CloudEvent) error {
	switch evt.Type {
	case events.TypeCivicReportReceived, events.TypeCivicStatusChanged, events.TypeCivicMerged:
	default:
		return nil // forward-compatible: ack unknown types on the topic
	}
	tenantID, err := s.tenantOf(evt)
	if err != nil {
		return err
	}
	if dup, err := s.processed(ctx, evt, tenantID); err != nil {
		return err
	} else if dup {
		return nil
	}
	switch evt.Type {
	case events.TypeCivicReportReceived:
		return s.civicReportReceived(ctx, evt, tenantID)
	case events.TypeCivicStatusChanged:
		return s.civicStatusChanged(ctx, evt, tenantID)
	default:
		return s.civicMerged(ctx, evt, tenantID)
	}
}

// civicReportReceived MERGEs the Case (+REPORTED edge when the reporter
// phone is present, +AT Location edge when geo is present).
func (s *Syncer) civicReportReceived(ctx context.Context, evt events.CloudEvent, tenantID string) error {
	var d events.CivicReportData
	if err := evt.DecodeData(&d); err != nil {
		return Permanent(fmt.Errorf("malformed civic report data: %v", err))
	}
	if strings.TrimSpace(d.Ref) == "" {
		return Permanent(errors.New("civic report event carries no ref"))
	}
	if err := s.Graph.UpsertTenant(ctx, tenantID, evt.Subject, s.now()); err != nil {
		return err
	}
	// Reporter resolution: only when the phone is present (SPEC-W32 §3
	// WS-D). The existing personFromPhone path hashes the phone BEFORE the
	// graph and auto-merges on exact phone_hash (W28 §4).
	personID := ""
	if strings.TrimSpace(d.ReporterPhone) != "" {
		person := s.personFromPhone(tenantID, "", d.ReporterName, d.ReporterPhone,
			channelOr(d.Channel, "web"), false, "")
		id, err := s.upsertResolvedPerson(ctx, person)
		if err != nil {
			return err
		}
		personID = id
	}
	c := graph.Case{
		Ref:       strings.TrimSpace(d.Ref),
		TenantID:  tenantID,
		Category:  strings.TrimSpace(d.Category),
		Status:    channelOr(d.Status, "new"),
		Ward:      strings.TrimSpace(d.Ward),
		CreatedAt: parseTimeOr(d.CreatedAt, evt.Time, s.now()),
		LGA:       strings.TrimSpace(d.LGA),
		Lat:       d.Lat,
		Lon:       d.Lon,
		HasGeo:    d.Lat != 0 || d.Lon != 0,
	}
	if err := s.Graph.UpsertCase(ctx, c, personID); err != nil {
		return err
	}
	s.metrics().Inc("civic_cases_projected")
	return nil
}

// civicStatusChanged mirrors the case status (+ acked_at/resolved_at when
// the event carries them).
func (s *Syncer) civicStatusChanged(ctx context.Context, evt events.CloudEvent, tenantID string) error {
	var d events.CivicStatusData
	if err := evt.DecodeData(&d); err != nil {
		return Permanent(fmt.Errorf("malformed civic status data: %v", err))
	}
	if strings.TrimSpace(d.Ref) == "" || strings.TrimSpace(d.Status) == "" {
		return Permanent(errors.New("civic status event requires ref and status"))
	}
	var ackedAt, resolvedAt *time.Time
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(d.AckedAt)); err == nil && !t.IsZero() {
		u := t.UTC()
		ackedAt = &u
	}
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(d.ResolvedAt)); err == nil && !t.IsZero() {
		u := t.UTC()
		resolvedAt = &u
	}
	if err := s.Graph.SetCaseStatus(ctx, tenantID, strings.TrimSpace(d.Ref),
		strings.TrimSpace(d.Status), ackedAt, resolvedAt); err != nil {
		return err
	}
	s.metrics().Inc("civic_status_mirrored")
	return nil
}

// civicMerged wires (cs)-[:MERGED_INTO]->(canonical). The canonical ref is
// accepted as canonical_ref / merged_into / canonical_id (producer-contract
// drift tolerance, same posture as captured_by above).
func (s *Syncer) civicMerged(ctx context.Context, evt events.CloudEvent, tenantID string) error {
	var d events.CivicMergedData
	if err := evt.DecodeData(&d); err != nil {
		return Permanent(fmt.Errorf("malformed civic merged data: %v", err))
	}
	canonical := strings.TrimSpace(d.CanonicalRef)
	if canonical == "" {
		for _, k := range []string{"merged_into", "canonical_id"} {
			if v, ok := evt.Data[k].(string); ok && strings.TrimSpace(v) != "" {
				canonical = strings.TrimSpace(v)
				break
			}
		}
	}
	if strings.TrimSpace(d.Ref) == "" || canonical == "" {
		return Permanent(errors.New("civic merged event requires ref and canonical_ref"))
	}
	if err := s.Graph.LinkCaseMerged(ctx, tenantID, strings.TrimSpace(d.Ref), canonical); err != nil {
		return err
	}
	s.metrics().Inc("civic_cases_merged")
	return nil
}

// ---------------------------------------------------------------------------
// graph enrichment (opendesk.graph.enrichment.v1, nightly spark job)
// ---------------------------------------------------------------------------

// HandleEnrichment consumes opendesk.graph.enrichment.v1 (SPEC-W28 §2
// lakehouse bi-direction; event shape mirrors graph_enrichment.py
// build_enrichment_event): tenant-scoped SET of the property map onto the
// matching Person. Enrichment NEVER creates a Person — rows for unknown or
// erased persons are dropped with a metric + debug log (docs/graph.md §4:
// "graph-sync drops enrichment for unknown/erased persons — no
// event-sourced node -> nothing to enrich").
func (s *Syncer) HandleEnrichment(ctx context.Context, evt events.CloudEvent) error {
	if evt.Type != events.TypePersonEnrichment {
		return nil
	}
	tenantID, err := s.tenantOf(evt)
	if err != nil {
		return err
	}
	if dup, err := s.processed(ctx, evt, tenantID); err != nil {
		return err
	} else if dup {
		return nil
	}
	var d events.EnrichmentData
	if err := evt.DecodeData(&d); err != nil {
		return Permanent(fmt.Errorf("malformed enrichment data: %v", err))
	}
	if strings.TrimSpace(d.PersonID) == "" {
		return Permanent(errors.New("enrichment event carries no person_id"))
	}
	if len(d.Properties) == 0 {
		return Permanent(errors.New("enrichment event carries an empty properties map"))
	}
	applied, err := s.Graph.ApplyEnrichment(ctx, tenantID, strings.TrimSpace(d.PersonID),
		normalizeEnrichmentProps(d.Properties), d.SnapshotDay, s.now())
	if err != nil {
		return err
	}
	if !applied {
		s.metrics().Inc("enrichment_dropped_unknown_person")
		s.log().Debug("enrichment dropped: unknown/erased person",
			zap.String("tenant_id", tenantID), zap.String("person_id", d.PersonID),
			zap.String("event_id", evt.ID))
		return nil
	}
	s.metrics().Inc("enrichment_applied")
	return nil
}

// normalizeEnrichmentProps normalizes RFC3339 timestamp VALUES inside the
// opaque property map to UTC (dual-TZ safety: offsets emitted by the spark
// job never leak into the graph). Non-timestamp values pass through
// untouched.
func normalizeEnrichmentProps(props map[string]any) map[string]any {
	out := make(map[string]any, len(props))
	for k, v := range props {
		if sv, ok := v.(string); ok {
			if t, err := time.Parse(time.RFC3339, sv); err == nil {
				out[k] = graph.FormatTime(t)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// person upsert + entity resolution
// ---------------------------------------------------------------------------

// personFromPhone builds the Person model: the phone is hashed (salted
// SHA-256, leads-dedupe scheme) BEFORE it can touch the graph; a person id
// derived from the hash is used when the event carries no stable id.
func (s *Syncer) personFromPhone(tenantID, personID, name, phone, channel string, quarantine bool, consentSummary string) graph.Person {
	phoneHash := ""
	if strings.TrimSpace(phone) != "" {
		phoneHash = graph.PhoneHash(s.Salt, tenantID, phone)
	}
	if personID == "" {
		if phoneHash != "" {
			personID = "ph-" + phoneHash[:16]
		} else {
			personID = "anon-" + tenantID
		}
	}
	channels := []string{}
	if channel != "" {
		channels = []string{channel}
	}
	return graph.Person{
		PersonID:       personID,
		TenantID:       tenantID,
		PhoneHash:      phoneHash,
		Name:           name,
		Channels:       channels,
		ConsentSummary: consentSummary,
		Quarantine:     quarantine,
		UpdatedAt:      s.now(),
	}
}

// upsertResolvedPerson upserts the Person (auto-merge on exact phone_hash)
// and then runs the embedding-similarity branch (MERGE_CANDIDATE proposals
// only). Embedding failures degrade gracefully: skipped + logged, never a
// poison message.
func (s *Syncer) upsertResolvedPerson(ctx context.Context, p graph.Person) (string, error) {
	personID, merged, err := s.Graph.UpsertPerson(ctx, p)
	if err != nil {
		return "", err
	}
	if merged {
		s.metrics().Inc("persons_auto_merged")
	}
	s.proposeMerges(ctx, p, personID, merged)
	return personID, nil
}

// proposeMerges runs the SPEC §4 entity-resolution branch: nomic-embed-text
// embedding over name+channel context, cosine ≥ threshold (0.92) against
// same-tenant candidates → MERGE_CANDIDATE edge. Skipped (with log) when
// Ollama is unreachable, when the person has no name, or right after an
// auto-merge (the node is already resolved).
func (s *Syncer) proposeMerges(ctx context.Context, p graph.Person, personID string, merged bool) {
	if s.Embed == nil || merged || strings.TrimSpace(p.Name) == "" {
		return
	}
	text := p.Name
	if len(p.Channels) > 0 {
		text += " | " + strings.Join(p.Channels, ",")
	}
	embedding, err := s.Embed.Embed(ctx, text)
	if err != nil {
		// Graceful degradation (SPEC §4): unreachable Ollama skips
		// embeddings — exact phone_hash merges are unaffected.
		s.metrics().Inc("embeddings_skipped")
		s.log().Warn("embedding unavailable; skipping merge proposals",
			zap.String("person_id", personID), zap.Error(err))
		return
	}
	now := s.now()
	if err := s.Graph.SetPersonEmbedding(ctx, p.TenantID, personID, embedding, now); err != nil {
		s.log().Warn("store embedding failed; merge proposals skipped",
			zap.String("person_id", personID), zap.Error(err))
		return
	}
	cands, err := s.Graph.PersonCandidates(ctx, p.TenantID, personID, 500)
	if err != nil {
		s.log().Warn("candidate scan failed; merge proposals skipped",
			zap.String("person_id", personID), zap.Error(err))
		return
	}
	for _, cand := range cands {
		score := graph.Cosine(embedding, cand.Embedding)
		if score >= s.threshold() {
			if err := s.Graph.AddMergeCandidate(ctx, p.TenantID, personID, cand.PersonID, score, now); err != nil {
				s.log().Warn("merge-candidate edge failed",
					zap.String("person_id", personID), zap.String("candidate", cand.PersonID), zap.Error(err))
				continue
			}
			s.metrics().Inc("merge_candidates")
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func channelOr(channel, def string) string {
	if c := strings.ToLower(strings.TrimSpace(channel)); c != "" {
		return c
	}
	return def
}

// parseTimeOr parses an RFC3339 timestamp (any offset) and normalizes to
// UTC — dual-TZ safety: offsets in producer events never leak into the
// graph.
func parseTimeOr(raw string, fallback time.Time, def time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw)); err == nil && !t.IsZero() {
		return t.UTC()
	}
	if !fallback.IsZero() {
		return fallback.UTC()
	}
	return def.UTC()
}

func orEventTime(t time.Time, evt events.CloudEvent, def time.Time) time.Time {
	if !t.IsZero() {
		return t.UTC()
	}
	if !evt.Time.IsZero() {
		return evt.Time.UTC()
	}
	return def.UTC()
}
