package consumer

// Civic case projection tests (SPEC-W32 §3 WS-D): Case nodes are PII-free,
// the reporter resolves through the EXISTING salted-hash personFromPhone
// path, every node/edge carries tenant_id, and the ProcessedEvent marker
// makes redelivery a no-op.

import (
	"context"
	"testing"
	"time"

	"github.com/opendesk/graph-sync/internal/events"
	"github.com/opendesk/graph-sync/internal/graph"
	"github.com/stretchr/testify/require"
)

func civicEvent(id, tenantID, typ string, data map[string]any) events.CloudEvent {
	return events.CloudEvent{
		SpecVersion: "1.0",
		ID:          id,
		Source:      "booking-service",
		Type:        typ,
		Subject:     "lagos-lga",
		Time:        time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
		TenantID:    tenantID,
		Data:        data,
	}
}

func reportData() map[string]any {
	return map[string]any{
		"ref":            "GOV-IKEJA-W3-2026-000042",
		"category":       "roads",
		"status":         "new",
		"ward":           "Ward 3",
		"lga":            "Ikeja",
		"channel":        "web",
		"reporter_phone": "+234 803 555 0101",
		"reporter_name":  "Adaeze Okafor",
		"created_at":     "2026-08-05T10:00:00+01:00",
	}
}

func TestCivicReportReceived_CaseProjected_TenantTagged(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()

	require.NoError(t, s.HandleCivic(ctx, civicEvent("civ-1", "tenant-1", events.TypeCivicReportReceived, reportData())))

	cs := g.cases[key("tenant-1", "GOV-IKEJA-W3-2026-000042")]
	require.NotNil(t, cs, "case node projected")
	require.Equal(t, "tenant-1", cs.TenantID, "tenant_id mandatory on the Case node")
	require.Equal(t, "roads", cs.Category)
	require.Equal(t, "new", cs.Status)
	require.Equal(t, "Ward 3", cs.Ward)
	// dual-TZ: +01:00 offset normalized to UTC.
	require.Equal(t, time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC), cs.CreatedAt)
	_, ok := g.tenants["tenant-1"]
	require.True(t, ok, "tenant anchor exists")
}

func TestCivicReportReceived_ReporterResolved_ReportedEdge(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()

	require.NoError(t, s.HandleCivic(ctx, civicEvent("civ-1", "tenant-1", events.TypeCivicReportReceived, reportData())))

	wantHash := graph.PhoneHash("test-salt", "tenant-1", "+234 803 555 0101")
	pid, err := g.FindPersonByPhoneHash(ctx, "tenant-1", wantHash)
	require.NoError(t, err)
	require.NotEmpty(t, pid, "reporter person resolved via the salted-hash path")
	p := g.persons[key("tenant-1", pid)]
	require.NotContains(t, p.PhoneHash, "8035550101", "raw phone never touches the graph")
	require.Equal(t, []string{"web"}, p.Channels, "civic channel recorded on the person")
	require.True(t, g.reported[key("tenant-1", pid)]["GOV-IKEJA-W3-2026-000042"],
		"(p)-[:REPORTED]->(cs) edge wired")
}

func TestCivicReportReceived_NoPhone_NoPersonNoReportedEdge(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	data := reportData()
	delete(data, "reporter_phone")
	delete(data, "reporter_name")

	require.NoError(t, s.HandleCivic(ctx, civicEvent("civ-1", "tenant-1", events.TypeCivicReportReceived, data)))
	require.NotNil(t, g.cases[key("tenant-1", "GOV-IKEJA-W3-2026-000042")], "case still projected")
	require.Empty(t, g.persons, "anonymous report creates no Person")
	require.Empty(t, g.reported, "no REPORTED edge without a reporter")
}

func TestCivicReportReceived_Geo_LocationAndAtEdge(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	data := reportData()
	data["lat"] = 6.6018
	data["lon"] = 3.3515

	require.NoError(t, s.HandleCivic(ctx, civicEvent("civ-1", "tenant-1", events.TypeCivicReportReceived, data)))
	k := key("tenant-1", "GOV-IKEJA-W3-2026-000042")
	require.True(t, g.cases[k].HasGeo)
	require.NotEmpty(t, g.caseAt[k], "(cs)-[:AT]->(Location) wired")
	require.Contains(t, g.caseAt[k], key("tenant-1", "Ikeja|Ward 3"))
}

func TestCivicReportReceived_NoGeo_NoLocation(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()

	require.NoError(t, s.HandleCivic(ctx, civicEvent("civ-1", "tenant-1", events.TypeCivicReportReceived, reportData())))
	require.Empty(t, g.caseAt, "no AT edge without lat/lon")
}

func TestCivicStatusChanged_MirrorsStatus_StampsAckedResolved(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	require.NoError(t, s.HandleCivic(ctx, civicEvent("civ-1", "tenant-1", events.TypeCivicReportReceived, reportData())))

	require.NoError(t, s.HandleCivic(ctx, civicEvent("civ-2", "tenant-1", events.TypeCivicStatusChanged, map[string]any{
		"ref":       "GOV-IKEJA-W3-2026-000042",
		"status":    "in_progress",
		"acked_at":  "2026-08-05T14:30:00+01:00",
		"tenant_id": "tenant-1",
	})))
	k := key("tenant-1", "GOV-IKEJA-W3-2026-000042")
	require.Equal(t, "in_progress", g.cases[k].Status, "status mirrored")
	require.Equal(t, "2026-08-05T13:30:00Z", g.caseExtra[k]["acked_at"], "acked_at stamped, UTC-normalized")

	require.NoError(t, s.HandleCivic(ctx, civicEvent("civ-3", "tenant-1", events.TypeCivicStatusChanged, map[string]any{
		"ref":         "GOV-IKEJA-W3-2026-000042",
		"status":      "resolved",
		"resolved_at": "2026-08-06T09:00:00Z",
	})))
	require.Equal(t, "resolved", g.cases[k].Status)
	require.Equal(t, "2026-08-06T09:00:00Z", g.caseExtra[k]["resolved_at"])
	require.Equal(t, "2026-08-05T13:30:00Z", g.caseExtra[k]["acked_at"],
		"earlier acked_at survives a later status event that omits it")
}

func TestCivicMerged_MergedIntoCanonical(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	require.NoError(t, s.HandleCivic(ctx, civicEvent("civ-1", "tenant-1", events.TypeCivicReportReceived, reportData())))
	require.NoError(t, s.HandleCivic(ctx, civicEvent("civ-2", "tenant-1", events.TypeCivicMerged, map[string]any{
		"ref":           "GOV-IKEJA-W3-2026-000042",
		"canonical_ref": "GOV-IKEJA-W3-2026-000017",
	})))

	k := key("tenant-1", "GOV-IKEJA-W3-2026-000042")
	require.Equal(t, key("tenant-1", "GOV-IKEJA-W3-2026-000017"), g.caseMerged[k],
		"(cs)-[:MERGED_INTO]->(canonical)")
	require.NotNil(t, g.cases[key("tenant-1", "GOV-IKEJA-W3-2026-000017")],
		"canonical stub is safe to MERGE even before its ReportReceived arrives")
}

func TestCivicMerged_AcceptsMergedIntoAlias(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	require.NoError(t, s.HandleCivic(ctx, civicEvent("civ-1", "tenant-1", events.TypeCivicMerged, map[string]any{
		"ref":         "GOV-IKEJA-W3-2026-000042",
		"merged_into": "GOV-IKEJA-W3-2026-000017",
	})))
	require.Equal(t, key("tenant-1", "GOV-IKEJA-W3-2026-000017"),
		g.caseMerged[key("tenant-1", "GOV-IKEJA-W3-2026-000042")])
}

func TestCivic_IdempotentReplay(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	evt := civicEvent("civ-dup", "tenant-1", events.TypeCivicReportReceived, reportData())
	require.NoError(t, s.HandleCivic(ctx, evt))
	require.NoError(t, s.HandleCivic(ctx, evt)) // redelivery
	require.Len(t, g.cases, 1)
	require.Len(t, g.persons, 1)
	require.Contains(t, s.Metrics.Render(), `graph_sync_counter{name="events_duplicate"} 1`)
}

func TestCivic_MissingTenant_IsPoison(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	err := s.HandleCivic(context.Background(), civicEvent("civ-1", "", events.TypeCivicReportReceived, reportData()))
	require.Error(t, err)
	require.ErrorIs(t, err, errPermanent, "tenant-less civic events are poison (gate 1)")
	require.Empty(t, g.cases)
}

func TestCivic_MissingRef_IsPoison(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	data := reportData()
	delete(data, "ref")
	err := s.HandleCivic(context.Background(), civicEvent("civ-1", "tenant-1", events.TypeCivicReportReceived, data))
	require.Error(t, err)
	require.ErrorIs(t, err, errPermanent)
}

func TestCivic_UnknownType_Acked(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	require.NoError(t, s.HandleCivic(context.Background(),
		civicEvent("civ-x", "tenant-1", "com.opendesk.civic.FutureThing", reportData())))
	require.Empty(t, g.cases, "unknown types are acknowledged and skipped")
	require.Empty(t, g.processed, "unknown types never consume the idempotency marker")
}

func TestCivic_TenantIsolation_SameRefDifferentTenants(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	require.NoError(t, s.HandleCivic(ctx, civicEvent("civ-a", "tenant-1", events.TypeCivicReportReceived, reportData())))
	require.NoError(t, s.HandleCivic(ctx, civicEvent("civ-b", "tenant-2", events.TypeCivicReportReceived, reportData())))
	require.NotNil(t, g.cases[key("tenant-1", "GOV-IKEJA-W3-2026-000042")])
	require.NotNil(t, g.cases[key("tenant-2", "GOV-IKEJA-W3-2026-000042")])
	// Same reporter phone in two tenants -> two distinct persons (the hash
	// input includes the tenant).
	require.Len(t, g.persons, 2, "phone hashes are not joinable across tenants")
}

func TestCivic_ErasureDropsReportedEdges_CaseKeepsNoPII(t *testing.T) {
	g := newFakeGraph()
	s, _, _ := newTestSyncer(g)
	ctx := context.Background()
	require.NoError(t, s.HandleCivic(ctx, civicEvent("civ-1", "tenant-1", events.TypeCivicReportReceived, reportData())))
	pid, err := g.FindPersonByPhoneHash(ctx, "tenant-1", graph.PhoneHash("test-salt", "tenant-1", "+234 803 555 0101"))
	require.NoError(t, err)
	require.NotEmpty(t, pid)

	found, err := g.ErasePerson(ctx, "tenant-1", pid)
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, g.reported, "REPORTED edges die with the Person (W28 erasure)")

	cs := g.cases[key("tenant-1", "GOV-IKEJA-W3-2026-000042")]
	require.NotNil(t, cs, "the Case itself survives erasure")
	// PII-free contract (SPEC-W32 §5 gate 5): ref/category/ward/status only.
	require.Equal(t, "GOV-IKEJA-W3-2026-000042", cs.Ref)
	require.Equal(t, "roads", cs.Category)
	require.Equal(t, "Ward 3", cs.Ward)
	require.NotContains(t, cs.Ref+cs.Category+cs.Status+cs.Ward, "8035550101")
}
