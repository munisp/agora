package consumer

import (
	"context"
	"testing"
	"time"

	"github.com/opendesk/graph-sync/internal/events"
	"github.com/stretchr/testify/require"
)

// enrichmentEvent mirrors graph_enrichment.py build_enrichment_event:
// id "<tenant>:<person>:<snapshot_day>", tenantid extension, data
// {tenant_id, person_id, snapshot_day, properties{...}}.
func enrichmentEvent(id, tenantID, personID, snapshotDay string, props map[string]any) events.CloudEvent {
	return events.CloudEvent{
		SpecVersion: "1.0",
		ID:          id,
		Source:      "spark/graph_enrichment",
		Type:        events.TypePersonEnrichment,
		Subject:     "acme",
		Time:        time.Date(2026, 8, 6, 2, 0, 0, 0, time.UTC),
		TenantID:    tenantID,
		Data: map[string]any{
			"tenant_id":    tenantID,
			"person_id":    personID,
			"snapshot_day": snapshotDay,
			"properties":   props,
		},
	}
}

// seedPerson creates one event-sourced Person via a booking event.
func seedPerson(t *testing.T, s *Syncer, tenantID, contactID string) {
	t.Helper()
	require.NoError(t, s.HandleBooking(context.Background(), bookingEvent(
		"seed-"+tenantID+"-"+contactID, tenantID, map[string]any{
			"booking_id": "seed-bk-" + contactID, "contact_id": contactID,
			"phone": "+2348000000000", "status": "completed",
		})))
}

func TestEnrichment_AppliesToKnownPerson_TenantScoped(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	seedPerson(t, s, "tenant-1", "ct-1")

	err := s.HandleEnrichment(ctx, enrichmentEvent("tenant-1:ct-1:2026-08-06", "tenant-1", "ct-1", "2026-08-06", map[string]any{
		"bookings_total":         float64(7),
		"bookings_showed":        float64(6),
		"bookings_no_show":       float64(1),
		"ltv_cents":              float64(420000),
		"no_show_rate":           0.142857,
		"channel_of_first_touch": "web",
		"funnel_stage_max":       "booked",
		"cac_channel_ngn_30d":    1850.5,
		"propensity_repeat":      0.81,
	}))
	require.NoError(t, err)

	rec, ok := g.enrichment[key("tenant-1", "ct-1")]
	require.True(t, ok, "properties applied to the existing node")
	require.Equal(t, "2026-08-06", rec.SnapshotDay)
	require.Equal(t, float64(7), rec.Props["bookings_total"])
	require.Equal(t, float64(420000), rec.Props["ltv_cents"])
	require.Equal(t, 0.81, rec.Props["propensity_repeat"])
	require.Contains(t, s.Metrics.Render(), `graph_sync_counter{name="enrichment_applied"} 1`)

	// Cross-tenant: same person_id in another tenant has no node → dropped.
	require.NoError(t, s.HandleEnrichment(ctx, enrichmentEvent(
		"tenant-2:ct-1:2026-08-06", "tenant-2", "ct-1", "2026-08-06",
		map[string]any{"bookings_total": float64(99)})))
	_, ok = g.enrichment[key("tenant-2", "ct-1")]
	require.False(t, ok, "no cross-tenant write")
	require.Empty(t, g.enrichment[key("tenant-2", "ct-1")].Props)
	require.Contains(t, s.Metrics.Render(), `graph_sync_counter{name="enrichment_dropped_unknown_person"} 1`)
}

func TestEnrichment_UnknownPerson_Dropped(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()

	// No event-sourced node for tenant+person — dropped, never created
	// (docs/graph.md §4: enrichment must not resurrect).
	require.NoError(t, s.HandleEnrichment(ctx, enrichmentEvent(
		"tenant-1:ghost:2026-08-06", "tenant-1", "ghost", "2026-08-06",
		map[string]any{"bookings_total": float64(3)})))
	require.Empty(t, g.persons, "enrichment never creates a Person node")
	require.Empty(t, g.enrichment)
	require.Contains(t, s.Metrics.Render(), `graph_sync_counter{name="enrichment_dropped_unknown_person"} 1`)
}

func TestEnrichment_ErasedPerson_Dropped(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	seedPerson(t, s, "tenant-1", "ct-e")

	// Erase, then the nightly enrichment row for the same person arrives.
	require.NoError(t, s.HandleErasure(ctx, events.CloudEvent{
		ID: "er-1", Type: events.TypeErasureRequested, TenantID: "tenant-1",
		Time: time.Now().UTC(), Data: map[string]any{"person_id": "ct-e"},
	}))
	require.Empty(t, g.persons)

	require.NoError(t, s.HandleEnrichment(ctx, enrichmentEvent(
		"tenant-1:ct-e:2026-08-06", "tenant-1", "ct-e", "2026-08-06",
		map[string]any{"ltv_cents": float64(1000)})))
	require.Empty(t, g.persons, "erased person is not resurrected by enrichment")
	require.Empty(t, g.enrichment)
	require.Contains(t, s.Metrics.Render(), `graph_sync_counter{name="enrichment_dropped_unknown_person"} 1`)
}

func TestEnrichment_IdempotentReplay(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	seedPerson(t, s, "tenant-1", "ct-1")

	evt := enrichmentEvent("tenant-1:ct-1:2026-08-06", "tenant-1", "ct-1", "2026-08-06",
		map[string]any{"bookings_total": float64(7)})
	require.NoError(t, s.HandleEnrichment(ctx, evt))
	// Same snapshot-day re-run emits the same CloudEvent id → processed
	// marker skips it (W24 pattern, per graph_enrichment.py contract).
	require.NoError(t, s.HandleEnrichment(ctx, evt))
	require.Contains(t, s.Metrics.Render(), `graph_sync_counter{name="enrichment_applied"} 1`)
	require.Contains(t, s.Metrics.Render(), `graph_sync_counter{name="events_duplicate"} 1`)
}

func TestEnrichment_DualTZ_TimestampPropsNormalized(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	seedPerson(t, s, "tenant-1", "ct-1")

	require.NoError(t, s.HandleEnrichment(ctx, enrichmentEvent(
		"tenant-1:ct-1:2026-08-06", "tenant-1", "ct-1", "2026-08-06", map[string]any{
			"last_booking_at": "2026-08-05T10:00:00+01:00", // WAT offset
		})))
	rec := g.enrichment[key("tenant-1", "ct-1")]
	require.Equal(t, "2026-08-05T09:00:00Z", rec.Props["last_booking_at"],
		"RFC3339 property values are normalized to UTC (dual-TZ safety)")
}

func TestEnrichment_Poison_DeadLetters(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()

	// Missing person_id → permanent error → DLQ after the consumer's retry
	// policy (permanent = no retries).
	err := s.HandleEnrichment(ctx, enrichmentEvent("bad-1", "tenant-1", "", "2026-08-06",
		map[string]any{"bookings_total": float64(1)}))
	require.ErrorIs(t, err, errPermanent)

	// Empty properties map → permanent too (nothing to apply).
	err = s.HandleEnrichment(ctx, enrichmentEvent("bad-2", "tenant-1", "ct-1", "2026-08-06",
		map[string]any{}))
	require.ErrorIs(t, err, errPermanent)

	// Missing tenant → permanent (tenant_id mandatory on every write).
	err = s.HandleEnrichment(ctx, enrichmentEvent("bad-3", "", "ct-1", "2026-08-06",
		map[string]any{"bookings_total": float64(1)}))
	require.ErrorIs(t, err, errPermanent)

	require.Empty(t, g.enrichment, "poison rows never reach the graph")
}

func TestEnrichment_UnknownType_Acked(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	err := s.HandleEnrichment(context.Background(), events.CloudEvent{
		ID: "u1", Type: "com.opendesk.graph.SomeFutureEnrichment", TenantID: "tenant-1",
		Data: map[string]any{},
	})
	require.NoError(t, err, "unknown types on the enrichment topic are forward-compatible acks")
	require.Empty(t, g.enrichment)
}
