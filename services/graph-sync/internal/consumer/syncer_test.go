package consumer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opendesk/graph-sync/internal/events"
	"github.com/opendesk/graph-sync/internal/graph"
)

// Compile-time seam check: the fake satisfies the GraphClient interface.
var _ graph.Client = (*fakeGraph)(nil)

func newTestSyncer() (*Syncer, *fakeGraph, *fakeAudit) {
	fg := newFakeGraph()
	fa := &fakeAudit{}
	now := func() time.Time { return time.Date(2026, 2, 20, 12, 0, 0, 0, time.UTC) }
	return &Syncer{
		Graph:            fg,
		Embed:            nil, // embeddings off by default in handler tests
		Audit:            fa,
		Salt:             "test-salt",
		ErasureDoneTopic: "opendesk.graph.erasure.done.v1",
		Now:              now,
	}, fg, fa
}

func bookingEvent(t *testing.T, eventType string, data map[string]any) events.CloudEvent {
	t.Helper()
	return events.CloudEvent{
		SpecVersion: "1.0",
		ID:          "evt-" + eventType + "-1",
		Source:      "booking-service",
		Type:        eventType,
		Subject:     "acme-salon",
		Time:        time.Date(2026, 2, 20, 11, 0, 0, 0, time.UTC),
		TenantID:    "tenant-1",
		Data:        data,
	}
}

// ---------------------------------------------------------------------------
// bookings
// ---------------------------------------------------------------------------

func TestHandleBookingCreatedUpsertsGraph(t *testing.T) {
	s, fg, _ := newTestSyncer()
	evt := bookingEvent(t, events.TypeBookingCreated, map[string]any{
		"booking_id":     "bk-1",
		"starts_at":      "2026-03-01T10:00:00Z",
		"status":         "confirmed",
		"source":         "voice",
		"offering_id":    "off-1",
		"offering_name":  "Haircut",
		"contact_id":     "ct-1",
		"contact_name":   "Jane Doe",
		"phone":          "+234 803 000 1111",
		"team_member_id": "tm-1",
	})
	if err := s.HandleBooking(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	if slug := fg.tenants["tenant-1"]; slug != "acme-salon" {
		t.Errorf("tenant slug = %q", slug)
	}
	p := fg.persons["tenant-1|ct-1"]
	if p == nil {
		t.Fatalf("person not upserted; persons=%v", fg.persons)
	}
	wantHash := graph.PhoneHash("test-salt", "tenant-1", "+234 803 000 1111")
	if p.PhoneHash != wantHash {
		t.Errorf("phone_hash = %q, want %q", p.PhoneHash, wantHash)
	}
	if strings.Contains(p.PhoneHash, "2348030001111") {
		t.Error("raw phone must never be stored")
	}
	if p.Name != "Jane Doe" {
		t.Errorf("name = %q", p.Name)
	}
	if len(p.Channels) != 1 || p.Channels[0] != "voice" {
		t.Errorf("channels = %v", p.Channels)
	}
	b := fg.bookings["tenant-1|bk-1"]
	if b == nil || b.Status != "confirmed" {
		t.Fatalf("booking = %+v", b)
	}
	if !fg.booked["tenant-1|ct-1"]["bk-1"] {
		t.Error("BOOKED edge missing")
	}
}

func TestHandleBookingDuplicateEventSkipped(t *testing.T) {
	s, fg, _ := newTestSyncer()
	evt := bookingEvent(t, events.TypeBookingCreated, map[string]any{
		"booking_id": "bk-1", "phone": "+1555", "status": "pending",
	})
	if err := s.HandleBooking(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	before := len(fg.bookings)
	if err := s.HandleBooking(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	if len(fg.bookings) != before {
		t.Error("duplicate event mutated the graph")
	}
}

func TestHandleBookingWithoutTenantIsPoison(t *testing.T) {
	s, _, _ := newTestSyncer()
	evt := bookingEvent(t, events.TypeBookingCreated, map[string]any{"booking_id": "bk-1"})
	evt.TenantID = ""
	err := s.HandleBooking(context.Background(), evt)
	if err == nil || !IsPermanent(err) {
		t.Fatalf("err = %v, want permanent tenant-missing error", err)
	}
}

func TestHandleBookingUnknownTypeAcked(t *testing.T) {
	s, fg, _ := newTestSyncer()
	evt := bookingEvent(t, "com.opendesk.booking.SomethingNew", map[string]any{"booking_id": "bk-9"})
	if err := s.HandleBooking(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	if len(fg.bookings) != 0 {
		t.Error("unknown type must not write")
	}
}

// ---------------------------------------------------------------------------
// identity: contacts + consent
// ---------------------------------------------------------------------------

func TestContactCapturedWithGeoAndQuarantine(t *testing.T) {
	s, fg, _ := newTestSyncer()
	evt := events.CloudEvent{
		SpecVersion: "1.0", ID: "evt-cc-1", Source: "identity-service",
		Type: events.TypeContactCaptured, Subject: "acme-salon",
		Time: time.Now(), TenantID: "tenant-1",
		Data: map[string]any{
			"lead_id": "lead-1", "contact_id": "ct-9", "name": "Amina Bello",
			"phone": "+2349012345678", "channel": "field", "source": "agent-app",
			"lga": "ikeja", "ward": "ward-3", "lat": 6.6, "lon": 3.35,
			"quarantine": true, "captured_at": "2026-02-20T09:00:00Z",
			"consent_purposes": []any{"marketing"},
			"referred_by_person_id": "ct-ref", "referral_program": "launch",
		},
	}
	if err := s.HandleIdentity(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	c := fg.contacts["tenant-1|lead-1"]
	if c == nil {
		t.Fatal("contact not upserted")
	}
	if c.ChannelOfFirstTouch != "field" || c.LGA != "ikeja" || c.Ward != "ward-3" {
		t.Errorf("contact = %+v", c)
	}
	p := fg.persons["tenant-1|ct-9"]
	if p == nil || !p.Quarantine {
		t.Errorf("quarantine flag lost: %+v", p)
	}
	if p.ConsentSummary != "marketing" {
		t.Errorf("consent_summary = %q", p.ConsentSummary)
	}
	if len(fg.referred) != 1 || fg.referred[0] != [2]string{"tenant-1|ct-ref", "tenant-1|ct-9"} {
		t.Errorf("referral edge = %v", fg.referred)
	}
}

func TestConsentGrantAndRevoke(t *testing.T) {
	s, fg, _ := newTestSyncer()
	grant := events.CloudEvent{
		SpecVersion: "1.0", ID: "evt-cg-1", Type: events.TypeConsentGranted,
		Time: time.Now(), TenantID: "tenant-1",
		Data: map[string]any{
			"consent_id": "cs-1", "person_id": "ct-1", "phone": "+15550001",
			"purpose": "reminders", "granted_at": "2026-02-20T08:00:00Z",
			"proof_ref": "rec-123",
		},
	}
	if err := s.HandleIdentity(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	cs := fg.consents["tenant-1|cs-1"]
	if cs == nil || cs.Purpose != "reminders" || cs.RevokedAt != nil {
		t.Fatalf("consent = %+v", cs)
	}
	if !fg.consented["tenant-1|ct-1"]["cs-1"] {
		t.Error("CONSENTED edge missing")
	}
	revoke := grant
	revoke.ID = "evt-cr-1"
	revoke.Type = events.TypeConsentRevoked
	revoke.Data["revoked_at"] = "2026-02-20T10:00:00Z"
	if err := s.HandleIdentity(context.Background(), revoke); err != nil {
		t.Fatal(err)
	}
	if fg.consents["tenant-1|cs-1"].RevokedAt == nil {
		t.Error("revoked_at not stamped")
	}
}

// ---------------------------------------------------------------------------
// transcripts
// ---------------------------------------------------------------------------

func TestTranscriptPersonFromPhoneOnly(t *testing.T) {
	s, fg, _ := newTestSyncer()
	evt := events.CloudEvent{
		SpecVersion: "1.0", ID: "evt-tr-1", Type: events.TypeSessionEnded,
		Time: time.Now(), TenantID: "tenant-1",
		Data: map[string]any{"phone": "+15551234", "channel": "voice", "name": "Caller"},
	}
	if err := s.HandleTranscript(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	hash := graph.PhoneHash("test-salt", "tenant-1", "+15551234")
	p := fg.persons["tenant-1|ph-"+hash[:16]]
	if p == nil {
		t.Fatalf("hash-derived person missing; persons=%v", fg.persons)
	}
	if p.PhoneHash != hash || p.Channels[0] != "voice" {
		t.Errorf("person = %+v", p)
	}
}

// ---------------------------------------------------------------------------
// erasure
// ---------------------------------------------------------------------------

func TestErasureDetachesPersonAndEmitsAudit(t *testing.T) {
	s, fg, fa := newTestSyncer()
	// Seed a person + contact + consent via the normal handlers.
	cc := events.CloudEvent{
		SpecVersion: "1.0", ID: "evt-seed", Type: events.TypeContactCaptured,
		Time: time.Now(), TenantID: "tenant-1",
		Data: map[string]any{
			"lead_id": "lead-1", "contact_id": "ct-1", "phone": "+15550001",
			"name": "Jane", "channel": "web",
		},
	}
	if err := s.HandleIdentity(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
	grant := events.CloudEvent{
		SpecVersion: "1.0", ID: "evt-seed-2", Type: events.TypeConsentGranted,
		Time: time.Now(), TenantID: "tenant-1",
		Data: map[string]any{
			"consent_id": "cs-1", "person_id": "ct-1", "phone": "+15550001",
			"purpose": "marketing", "granted_at": "2026-02-20T08:00:00Z",
		},
	}
	if err := s.HandleIdentity(context.Background(), grant); err != nil {
		t.Fatal(err)
	}

	erasure := events.CloudEvent{
		SpecVersion: "1.0", ID: "evt-erase-1", Type: events.TypeErasureRequested,
		Time: time.Now(), TenantID: "tenant-1", Subject: "acme-salon",
		Data: map[string]any{"person_id": "ct-1"},
	}
	if err := s.HandleErasure(context.Background(), erasure); err != nil {
		t.Fatal(err)
	}
	if _, ok := fg.persons["tenant-1|ct-1"]; ok {
		t.Error("person still present after erasure")
	}
	if len(fg.consents) != 0 || len(fg.contacts) != 0 {
		t.Error("person subgraph not fully erased")
	}
	if len(fa.events) != 1 {
		t.Fatalf("audit events = %d", len(fa.events))
	}
	audit := fa.events[0]
	if audit.Topic != "opendesk.graph.erasure.done.v1" {
		t.Errorf("audit topic = %q", audit.Topic)
	}
	if audit.Evt.Type != "com.opendesk.graph.ErasureDone" {
		t.Errorf("audit type = %q", audit.Evt.Type)
	}
	if audit.Evt.Data["found"] != true || audit.Evt.Data["person_id"] != "ct-1" {
		t.Errorf("audit data = %v", audit.Evt.Data)
	}

	// Idempotent: second erasure finds nothing and still audits (found=false).
	erasure.ID = "evt-erase-2"
	if err := s.HandleErasure(context.Background(), erasure); err != nil {
		t.Fatal(err)
	}
	if fa.events[1].Evt.Data["found"] != false {
		t.Errorf("second erasure should report found=false: %v", fa.events[1].Evt.Data)
	}
}

func TestErasureByPhoneOnly(t *testing.T) {
	s, fg, _ := newTestSyncer()
	cc := events.CloudEvent{
		SpecVersion: "1.0", ID: "evt-seed", Type: events.TypeContactCaptured,
		Time: time.Now(), TenantID: "tenant-1",
		Data: map[string]any{"lead_id": "l1", "contact_id": "ct-1", "phone": "+15550001", "channel": "web"},
	}
	if err := s.HandleIdentity(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
	erasure := events.CloudEvent{
		SpecVersion: "1.0", ID: "evt-erase-p", Type: events.TypeErasureRequested,
		Time: time.Now(), TenantID: "tenant-1",
		Data: map[string]any{"phone": "+15550001"},
	}
	if err := s.HandleErasure(context.Background(), erasure); err != nil {
		t.Fatal(err)
	}
	if len(fg.persons) != 0 {
		t.Errorf("phone-only erasure failed; persons=%v", fg.persons)
	}
}

// ---------------------------------------------------------------------------
// SPEC-W45 K9: TenantDeleted → DETACH DELETE the tenant subgraph
// ---------------------------------------------------------------------------

func TestTenantDeletedPurgesSubgraph(t *testing.T) {
	s, fg, _ := newTestSyncer()
	seed := func(tenantID string) {
		t.Helper()
		cc := events.CloudEvent{
			SpecVersion: "1.0", ID: "evt-seed-" + tenantID, Type: events.TypeContactCaptured,
			Time: time.Now(), TenantID: tenantID, Subject: tenantID + "-slug",
			Data: map[string]any{
				"lead_id": "lead-1", "contact_id": "ct-1", "phone": "+15550001",
				"name": "Jane", "channel": "web",
			},
		}
		if err := s.HandleIdentity(context.Background(), cc); err != nil {
			t.Fatal(err)
		}
		bk := events.CloudEvent{
			SpecVersion: "1.0", ID: "evt-bk-" + tenantID, Type: events.TypeBookingCreated,
			Time: time.Now(), TenantID: tenantID,
			Data: map[string]any{
				"booking_id": "bk-1", "status": "confirmed", "phone": "+15550001",
				"contact_id": "ct-1", "starts_at": "2026-03-01T10:00:00Z",
			},
		}
		if err := s.HandleBooking(context.Background(), bk); err != nil {
			t.Fatal(err)
		}
	}
	seed("tenant-a")
	seed("tenant-b")

	del := events.CloudEvent{
		SpecVersion: "1.0", ID: "evt-td-1", Source: "identity-service",
		Type: events.TypeTenantDeleted, Subject: "tenant-a-slug",
		Time: time.Now(), TenantID: "tenant-a",
		Data: map[string]any{
			"tenant_slug": "tenant-a-slug", "tenant_id": "tenant-a",
			"deleted_at": "2026-02-20T12:00:00Z", "actor": "owner@a",
		},
	}
	if err := s.HandleIdentity(context.Background(), del); err != nil {
		t.Fatal(err)
	}
	// tenant-a subgraph is gone...
	if _, ok := fg.tenants["tenant-a"]; ok {
		t.Error("tenant-a anchor survives")
	}
	for k := range fg.persons {
		if strings.HasPrefix(k, "tenant-a|") {
			t.Errorf("tenant-a person survives: %s", k)
		}
	}
	for k := range fg.contacts {
		if strings.HasPrefix(k, "tenant-a|") {
			t.Errorf("tenant-a contact survives: %s", k)
		}
	}
	for k := range fg.bookings {
		if strings.HasPrefix(k, "tenant-a|") {
			t.Errorf("tenant-a booking survives: %s", k)
		}
	}
	// ...tenant-b untouched.
	if _, ok := fg.tenants["tenant-b"]; !ok {
		t.Error("tenant-b anchor lost")
	}
	if _, ok := fg.persons["tenant-b|ct-1"]; !ok {
		t.Error("tenant-b person lost")
	}
	// Idempotent redelivery: the processed marker for evt-td-1 was deleted
	// with the subgraph, so the event is reprocessed — but the delete is a
	// no-op (pre-check reports found=false) and must not error.
	if err := s.HandleIdentity(context.Background(), del); err != nil {
		t.Errorf("TenantDeleted redelivery must be idempotent: %v", err)
	}
}

// ---------------------------------------------------------------------------
// entity resolution (embedding branch)
// ---------------------------------------------------------------------------

func TestAutoMergeOnExactPhoneHash(t *testing.T) {
	s, fg, _ := newTestSyncer()
	phone := "+15550001"
	first := events.CloudEvent{
		SpecVersion: "1.0", ID: "e1", Type: events.TypeContactCaptured,
		Time: time.Now(), TenantID: "tenant-1",
		Data: map[string]any{"lead_id": "l1", "contact_id": "ct-A", "phone": phone, "channel": "web"},
	}
	if err := s.HandleIdentity(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	// Same phone, DIFFERENT person id (cross-channel) → auto-merge into ct-A.
	second := events.CloudEvent{
		SpecVersion: "1.0", ID: "e2", Type: events.TypeSessionEnded,
		Time: time.Now(), TenantID: "tenant-1",
		Data: map[string]any{"phone": phone, "channel": "voice", "name": "Jane"},
	}
	if err := s.HandleTranscript(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if len(fg.persons) != 1 {
		t.Fatalf("persons = %d, want 1 after auto-merge: %v", len(fg.persons), fg.persons)
	}
	p := fg.persons["tenant-1|ct-A"]
	if p == nil {
		t.Fatalf("merge target missing: %v", fg.persons)
	}
	if p.Name != "Jane" {
		t.Errorf("folded name = %q", p.Name)
	}
	if len(p.Channels) != 2 {
		t.Errorf("channels union = %v", p.Channels)
	}
}

func TestMergeProposalAboveThreshold(t *testing.T) {
	s, fg, _ := newTestSyncer()
	s.Embed = &fakeEmbedder{vectors: map[string][]float32{
		"Jane Doe | web":   {1, 0},
		"Janet Doe | voice": {0.999, 0.044}, // cosine ≈ 0.999 ≥ 0.92
	}}
	seed := events.CloudEvent{
		SpecVersion: "1.0", ID: "e1", Type: events.TypeContactCaptured,
		Time: time.Now(), TenantID: "tenant-1",
		Data: map[string]any{"lead_id": "l1", "contact_id": "ct-1", "phone": "+111", "name": "Jane Doe", "channel": "web"},
	}
	if err := s.HandleIdentity(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	incoming := events.CloudEvent{
		SpecVersion: "1.0", ID: "e2", Type: events.TypeSessionEnded,
		Time: time.Now(), TenantID: "tenant-1",
		Data: map[string]any{"phone": "+222", "channel": "voice", "name": "Janet Doe"},
	}
	if err := s.HandleTranscript(context.Background(), incoming); err != nil {
		t.Fatal(err)
	}
	if len(fg.mergeCandidates) != 1 {
		t.Fatalf("merge candidates = %v", fg.mergeCandidates)
	}
	mc := fg.mergeCandidates[0]
	if mc.TenantID != "tenant-1" || mc.Score < 0.92 {
		t.Errorf("candidate = %+v", mc)
	}
	if len(fg.persons) != 2 {
		t.Error("merge proposal must NOT merge the nodes")
	}
}

func TestEmbeddingUnavailableDegrades(t *testing.T) {
	s, _, _ := newTestSyncer()
	s.Embed = &fakeEmbedder{err: errors.New("ollama down")}
	evt := events.CloudEvent{
		SpecVersion: "1.0", ID: "e1", Type: events.TypeSessionEnded,
		Time: time.Now(), TenantID: "tenant-1",
		Data: map[string]any{"phone": "+111", "name": "Jane", "channel": "voice"},
	}
	// Must not poison: graceful degrade.
	if err := s.HandleTranscript(context.Background(), evt); err != nil {
		t.Fatalf("degraded embedding must not fail the event: %v", err)
	}
}

// ---------------------------------------------------------------------------
// dual-TZ normalization
// ---------------------------------------------------------------------------

func TestOffsetsNormalizedToUTC(t *testing.T) {
	s, fg, _ := newTestSyncer()
	evt := events.CloudEvent{
		SpecVersion: "1.0", ID: "e1", Type: events.TypeContactCaptured,
		Time: time.Now(), TenantID: "tenant-1",
		Data: map[string]any{
			"lead_id": "l1", "contact_id": "ct-1", "phone": "+1",
			"channel": "web", "captured_at": "2026-02-20T14:00:00+05:30",
		},
	}
	if err := s.HandleIdentity(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	got := fg.contacts["tenant-1|l1"].CapturedAt
	want := time.Date(2026, 2, 20, 8, 30, 0, 0, time.UTC)
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("captured_at = %v (%v), want %v UTC", got, got.Location(), want)
	}
}

// ---------------------------------------------------------------------------
// Permanent() marking
// ---------------------------------------------------------------------------

func TestPermanentWrapper(t *testing.T) {
	err := Permanent(errors.New("bad"))
	if !IsPermanent(err) {
		t.Fatal("IsPermanent should unwrap")
	}
	if IsPermanent(errors.New("transient")) {
		t.Fatal("plain errors are transient")
	}
}
