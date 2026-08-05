package consumer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opendesk/graph-sync/internal/embed"
	"github.com/opendesk/graph-sync/internal/events"
	"github.com/opendesk/graph-sync/internal/graph"
	"github.com/opendesk/graph-sync/internal/metrics"
	"github.com/stretchr/testify/require"
)

func newTestSyncer(g *fakeGraph) (*Syncer, *fakeAudit, *fakeEmbedder) {
	audit := &fakeAudit{}
	emb := &fakeEmbedder{vectors: map[string][]float32{}}
	s := &Syncer{
		Graph:            g,
		Embed:            emb,
		Audit:            audit,
		Metrics:          metrics.New(),
		Salt:             "test-salt",
		MergeThreshold:   0.92,
		ErasureDoneTopic: "opendesk.graph.erasure.done.v1",
		Now: func() time.Time {
			return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
		},
	}
	return s, audit, emb
}

func bookingEvent(id, tenantID string, data map[string]any) events.CloudEvent {
	return events.CloudEvent{
		SpecVersion: "1.0",
		ID:          id,
		Source:      "booking-service",
		Type:        events.TypeBookingCreated,
		Subject:     "acme",
		Time:        time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
		TenantID:    tenantID,
		Data:        data,
	}
}

func TestBookingUpsert_PersonHashedPhone_TenantTagged(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()

	err := s.HandleBooking(ctx, bookingEvent("evt-1", "tenant-1", map[string]any{
		"booking_id":    "bk-1",
		"contact_id":    "ct-1",
		"contact_name":  "Adaeze Okafor",
		"phone":         "+234 803 555 0101",
		"status":        "pending",
		"offering_id":   "of-1",
		"offering_name": "Braids",
		"starts_at":     "2026-08-06T10:00:00+01:00",
		"source":        "voice",
	}))
	require.NoError(t, err)

	p := g.persons[key("tenant-1", "ct-1")]
	require.NotNil(t, p, "person upserted")
	require.Equal(t, "tenant-1", p.TenantID, "tenant_id mandatory on node")
	require.Equal(t, graph.PhoneHash("test-salt", "tenant-1", "+234 803 555 0101"), p.PhoneHash)
	require.NotContains(t, p.PhoneHash, "8035550101", "phone must be hashed, never raw")
	require.Equal(t, "Adaeze Okafor", p.Name, "name is the only plaintext PII")
	require.Equal(t, []string{"voice"}, p.Channels)

	b := g.bookings[key("tenant-1", "bk-1")]
	require.NotNil(t, b)
	require.Equal(t, "tenant-1", b.TenantID)
	// dual-TZ: +01:00 offset normalized to UTC
	require.Equal(t, time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC), b.CreatedAt)
	require.True(t, g.booked[key("tenant-1", "ct-1")]["bk-1"])
	// Tenant anchor node exists.
	_, ok := g.tenants["tenant-1"]
	require.True(t, ok)
}

func TestIdempotency_DuplicateEventSkipped(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	evt := bookingEvent("evt-dup", "tenant-1", map[string]any{
		"booking_id": "bk-1", "contact_id": "ct-1", "phone": "+2348030000000", "status": "pending",
	})
	require.NoError(t, s.HandleBooking(ctx, evt))
	require.NoError(t, s.HandleBooking(ctx, evt)) // redelivery
	require.Len(t, g.persons, 1)
	require.Len(t, g.bookings, 1)
	require.Contains(t, s.Metrics.Render(), `graph_sync_counter{name="events_duplicate"} 1`)
}

func TestAutoMerge_ExactPhoneHash(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()

	// Same phone, two different contact ids (web lead + voice booking) →
	// the ONLY auto-merge rule (SPEC §4).
	require.NoError(t, s.HandleIdentity(ctx, events.CloudEvent{
		ID: "evt-cap", Type: events.TypeContactCaptured, TenantID: "tenant-1", Subject: "acme",
		Time: time.Now().UTC(),
		Data: map[string]any{
			"lead_id": "lead-1", "contact_id": "ct-web", "name": "Adaeze Okafor",
			"phone": "+234 803 555 0101", "channel": "web",
		},
	}))
	require.NoError(t, s.HandleBooking(ctx, bookingEvent("evt-bk", "tenant-1", map[string]any{
		"booking_id": "bk-9", "contact_id": "ct-voice", "contact_name": "Adaeze Okafor",
		"phone": "+2348035550101", "status": "confirmed", "source": "voice",
	})))

	require.Len(t, g.persons, 1, "exact phone_hash match auto-merges into one node")
	p := g.persons[key("tenant-1", "ct-web")]
	require.NotNil(t, p, "first node wins as the merge target")
	require.ElementsMatch(t, []string{"web", "voice"}, p.Channels, "channel union folded")
	require.True(t, g.booked[key("tenant-1", "ct-web")]["bk-9"], "booking attaches to the merged person")
}

func TestMergeCandidate_EmbeddingSimilarity(t *testing.T) {
	g := newFakeGraph()
	s, _, emb := newTestSyncer(g)
	ctx := context.Background()

	// Existing person with a stored embedding.
	g.persons[key("tenant-1", "ct-old")] = &graph.Person{
		PersonID: "ct-old", TenantID: "tenant-1", Name: "A. Okafor",
	}
	g.embeddings[key("tenant-1", "ct-old")] = []float32{0.98, 0.02}
	emb.vectors["Adaeze Okafor | web"] = []float32{0.99, 0.01} // cosine ≈ 0.9999 ≥ 0.92

	require.NoError(t, s.HandleIdentity(ctx, events.CloudEvent{
		ID: "evt-cap2", Type: events.TypeContactCaptured, TenantID: "tenant-1",
		Time: time.Now().UTC(),
		Data: map[string]any{
			"lead_id": "lead-2", "contact_id": "ct-new", "name": "Adaeze Okafor",
			"phone": "+2348090000000", "channel": "web",
		},
	}))

	require.Len(t, g.persons, 2, "similarity NEVER auto-merges (SPEC §4)")
	require.Len(t, g.mergeCandidates, 1)
	mc := g.mergeCandidates[0]
	require.Equal(t, "tenant-1", mc.TenantID, "tenant_id mandatory on edge")
	require.ElementsMatch(t, []string{"ct-new", "ct-old"}, []string{mc.A, mc.B})
	require.GreaterOrEqual(t, mc.Score, 0.92)
	require.NotEmpty(t, g.embeddings[key("tenant-1", "ct-new")], "embedding stored")
}

func TestMergeCandidate_BelowThreshold_NoEdge(t *testing.T) {
	g := newFakeGraph()
	s, _, emb := newTestSyncer(g)
	ctx := context.Background()
	g.persons[key("tenant-1", "ct-old")] = &graph.Person{PersonID: "ct-old", TenantID: "tenant-1", Name: "Someone Else"}
	g.embeddings[key("tenant-1", "ct-old")] = []float32{0, 1}
	emb.vectors["Adaeze Okafor | web"] = []float32{0.9, 0.1} // cosine ≈ 0.11

	require.NoError(t, s.HandleIdentity(ctx, events.CloudEvent{
		ID: "evt-cap3", Type: events.TypeContactCaptured, TenantID: "tenant-1",
		Time: time.Now().UTC(),
		Data: map[string]any{"lead_id": "l", "contact_id": "ct-new", "name": "Adaeze Okafor", "phone": "+1", "channel": "web"},
	}))
	require.Empty(t, g.mergeCandidates)
}

func TestOllamaDegraded_SkipsEmbeddings_EventStillProcessed(t *testing.T) {
	g := newFakeGraph()
	s, _, emb := newTestSyncer(g)
	emb.err = embed.ErrDegraded
	ctx := context.Background()

	require.NoError(t, s.HandleIdentity(ctx, events.CloudEvent{
		ID: "evt-deg", Type: events.TypeContactCaptured, TenantID: "tenant-1",
		Time: time.Now().UTC(),
		Data: map[string]any{
			"lead_id": "lead-d", "contact_id": "ct-d", "name": "Adaeze",
			"phone": "+2348010000000", "channel": "field",
		},
	}))
	require.Equal(t, 1, emb.calls)
	require.NotNil(t, g.persons[key("tenant-1", "ct-d")], "person upserted despite degraded embeddings")
	require.Empty(t, g.mergeCandidates)
	require.Empty(t, g.embeddings, "no embedding stored while degraded")
	require.Contains(t, s.Metrics.Render(), "embeddings_skipped")
}

func TestQuarantineFlag_PropagatesAndIsMonotonic(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()

	// Imported, consent-unverified lead → quarantine=true (SPEC §5 gate 4).
	require.NoError(t, s.HandleCAC(ctx, events.CloudEvent{
		ID: "evt-imp", Type: events.TypeLeadCreated, TenantID: "tenant-1",
		Time: time.Now().UTC(),
		Data: map[string]any{
			"lead_id": "lead-q", "contact_id": "ct-q", "name": "Imported Person",
			"phone": "+2348020000000", "channel": "import", "quarantine": true,
		},
	}))
	p := g.persons[key("tenant-1", "ct-q")]
	require.NotNil(t, p)
	require.True(t, p.Quarantine, "quarantine flag propagated to the Person node")

	// A later non-quarantine event for the same person must NOT clear it.
	require.NoError(t, s.HandleBooking(ctx, bookingEvent("evt-bkq", "tenant-1", map[string]any{
		"booking_id": "bk-q", "contact_id": "ct-q", "phone": "+2348020000000", "status": "confirmed",
	})))
	require.True(t, g.persons[key("tenant-1", "ct-q")].Quarantine, "quarantine is monotonic")
}

func TestErasure_DetachDeletesSubgraph_EmitsAudit(t *testing.T) {
	g := newFakeGraph()
	s, audit, _ := newTestSyncer(g)
	ctx := context.Background()

	// Build a small subgraph: person + consent + contact + booking.
	require.NoError(t, s.HandleIdentity(ctx, events.CloudEvent{
		ID: "evt-c1", Type: events.TypeConsentGranted, TenantID: "tenant-1", Subject: "acme",
		Time: time.Now().UTC(),
		Data: map[string]any{
			"consent_id": "cons-1", "person_id": "ct-e", "phone": "+2348030000099",
			"name": "Erase Me", "purpose": "marketing", "granted_at": "2026-08-01T09:00:00Z",
		},
	}))
	require.NoError(t, s.HandleIdentity(ctx, events.CloudEvent{
		ID: "evt-c2", Type: events.TypeContactCaptured, TenantID: "tenant-1",
		Time: time.Now().UTC(),
		Data: map[string]any{
			"lead_id": "lead-e", "person_id": "ct-e", "phone": "+2348030000099",
			"channel": "field", "lga": "Ikeja",
		},
	}))
	require.NoError(t, s.HandleBooking(ctx, bookingEvent("evt-c3", "tenant-1", map[string]any{
		"booking_id": "bk-e", "contact_id": "ct-e", "phone": "+2348030000099", "status": "completed",
	})))
	require.Len(t, g.persons, 1)
	require.Len(t, g.consents, 1)
	require.Len(t, g.contacts, 1)
	require.Len(t, g.bookings, 1)

	// Erasure tombstone.
	require.NoError(t, s.HandleErasure(ctx, events.CloudEvent{
		ID: "evt-er1", Type: events.TypeErasureRequested, TenantID: "tenant-1", Subject: "acme",
		Time: time.Now().UTC(),
		Data: map[string]any{"person_id": "ct-e", "reason": "subject_request"},
	}))

	require.Empty(t, g.persons, "person node gone")
	require.Empty(t, g.consents, "consent nodes gone (subgraph)")
	require.Empty(t, g.contacts, "contact nodes gone (subgraph)")
	require.Len(t, g.bookings, 1, "booking kept as transactional record")
	require.Empty(t, g.booked, "BOOKED edge detached")

	// Audit event on opendesk.graph.erasure.done.v1.
	require.Len(t, audit.events, 1)
	ae := audit.events[0]
	require.Equal(t, "opendesk.graph.erasure.done.v1", ae.Topic)
	require.Equal(t, "com.opendesk.graph.ErasureDone", ae.Evt.Type)
	require.Equal(t, "tenant-1", ae.Evt.TenantID)
	require.Equal(t, "ct-e", ae.Evt.Data["person_id"])
	require.Equal(t, true, ae.Evt.Data["found"])

	// Erasure is idempotent: replaying the same tombstone is a no-op (the
	// processed marker skips it before any graph write).
	require.NoError(t, s.HandleErasure(ctx, events.CloudEvent{
		ID: "evt-er1", Type: events.TypeErasureRequested, TenantID: "tenant-1",
		Time: time.Now().UTC(), Data: map[string]any{"person_id": "ct-e"},
	}))
	require.Len(t, audit.events, 1, "duplicate erasure skipped by idempotency marker")

	// Erasing an unknown person succeeds with found=false (idempotent SLA).
	require.NoError(t, s.HandleErasure(ctx, events.CloudEvent{
		ID: "evt-er2", Type: events.TypeErasureRequested, TenantID: "tenant-1",
		Time: time.Now().UTC(), Data: map[string]any{"person_id": "ct-ghost"},
	}))
	require.Len(t, audit.events, 2)
	require.Equal(t, false, audit.events[1].Evt.Data["found"])
}

func TestErasure_PhoneOnlyTombstone_ResolvesViaHash(t *testing.T) {
	g := newFakeGraph()
	s, audit, _ := newTestSyncer(g)
	ctx := context.Background()

	require.NoError(t, s.HandleIdentity(ctx, events.CloudEvent{
		ID: "evt-p1", Type: events.TypeContactCaptured, TenantID: "tenant-1",
		Time: time.Now().UTC(),
		Data: map[string]any{
			"lead_id": "lead-p", "person_id": "ct-ph", "phone": "+2348055551234", "channel": "web",
		},
	}))
	require.NoError(t, s.HandleErasure(ctx, events.CloudEvent{
		ID: "evt-er3", Type: events.TypeErasureRequested, TenantID: "tenant-1",
		Time: time.Now().UTC(),
		Data: map[string]any{"phone": "+234 805 555 1234"},
	}))
	require.Empty(t, g.persons)
	require.Equal(t, "ct-ph", audit.events[0].Evt.Data["person_id"])
}

func TestMissingTenant_IsPoison(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	err := s.HandleBooking(context.Background(), bookingEvent("evt-x", "", map[string]any{
		"booking_id": "bk-x",
	}))
	require.Error(t, err)
	require.True(t, errors.Is(err, errPermanent), "tenant-less events dead-letter immediately")
	require.Empty(t, g.persons, "nothing written without tenant_id")
}

func TestConsentGrantAndRevoke(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()

	require.NoError(t, s.HandleIdentity(ctx, events.CloudEvent{
		ID: "evt-g", Type: events.TypeConsentGranted, TenantID: "tenant-1",
		Time: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC),
		Data: map[string]any{
			"consent_id": "cons-9", "person_id": "ct-c", "purpose": "marketing",
			"granted_at": "2026-08-01T10:00:00+01:00", "proof_ref": "rec-123",
		},
	}))
	c := g.consents[key("tenant-1", "cons-9")]
	require.NotNil(t, c)
	require.Equal(t, "tenant-1", c.TenantID)
	require.Equal(t, "marketing", c.Purpose)
	// dual-TZ: +01:00 grant normalized to UTC
	require.Equal(t, time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC), c.GrantedAt)
	require.True(t, g.consented[key("tenant-1", "ct-c")]["cons-9"])

	require.NoError(t, s.HandleIdentity(ctx, events.CloudEvent{
		ID: "evt-r", Type: events.TypeConsentRevoked, TenantID: "tenant-1",
		Time: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
		Data: map[string]any{"consent_id": "cons-9", "person_id": "ct-c", "purpose": "marketing"},
	}))
	require.NotNil(t, g.consents[key("tenant-1", "cons-9")].RevokedAt, "revocation stamped")
}

func TestTranscriptUpsert_VoiceCaller(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	require.NoError(t, s.HandleTranscript(ctx, events.CloudEvent{
		ID: "evt-t", Type: events.TypeSessionEnded, TenantID: "tenant-1",
		Time: time.Now().UTC(),
		Data: map[string]any{"conversation_id": "cv-1", "phone": "+2348070000000", "channel": "voice"},
	}))
	require.Len(t, g.persons, 1)
	for _, p := range g.persons {
		require.Equal(t, []string{"voice"}, p.Channels)
		require.Equal(t, graph.PhoneHash("test-salt", "tenant-1", "+2348070000000"), p.PhoneHash)
	}
}

func TestUnknownTypeOnTopic_Acked(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	err := s.HandleBooking(context.Background(), events.CloudEvent{
		ID: "evt-u", Type: "com.opendesk.booking.SomeFutureEvent", TenantID: "tenant-1",
		Data: map[string]any{},
	})
	require.NoError(t, err, "unknown types are forward-compatible acks")
	require.Empty(t, g.persons)
}
