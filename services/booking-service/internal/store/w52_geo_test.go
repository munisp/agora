package store

// SPEC-W52 Scope B store tests — embedded Postgres (newTestStore, -short
// skips): civic_cases geo-enrichment provenance columns, fill-only-when-empty
// semantics (human/agent-entered ward/lga always win), fail-open behavior,
// tenant-uuid forwarding on X-Tenant-Slug, and RLS tenant isolation of the
// enrichment write. The embedded Postgres has NO PostGIS, so geoEnabled is
// false here — that is also exactly the "PostGIS absent" store posture the
// coverage gate maps to 503 (covered at the bookingops/httpapi levels).

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// w52FakeEnricher records calls and returns a canned enrichment (the
// store.GeoEnricher seam — the HTTP client itself is tested in
// internal/geo/w52_enrich_test.go).
type w52FakeEnricher struct {
	mu    sync.Mutex
	res   *GeoEnrichment
	calls []string // tenant UUID strings seen
}

func (f *w52FakeEnricher) Enrich(_ context.Context, tenantSlug string, _, _ float64) *GeoEnrichment {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, tenantSlug)
	return f.res
}

func (f *w52FakeEnricher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *w52FakeEnricher) lastSlug() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1]
}

// w52InsertCase inserts one civic case with the given ward/lga/coords.
func w52InsertCase(t *testing.T, st *Store, tenantID uuid.UUID, ward, lga string, lat, lon *float64) CivicCase {
	t.Helper()
	c := CivicCase{
		TenantID:   tenantID,
		Ref:        "GOV-TEST-W52-" + uuid.NewString()[:8],
		CategoryID: uuid.New(),
		Status:     CivicStatusNew,
		Description: "broken streetlight on the corner, reported by a resident " +
			"who asks for a fix", // > DescriptionMinLen; store does not validate
		Ward:    ward,
		LGA:     lga,
		Lat:     lat,
		Lon:     lon,
		Channel: ChannelWebForTest,
	}
	if err := st.InsertCivicCase(context.Background(), &c); err != nil {
		t.Fatalf("insert civic case: %v", err)
	}
	return c
}

// ChannelWebForTest mirrors civic.ChannelWeb without importing internal/civic
// (the store package must not depend on its consumers).
const ChannelWebForTest = "web"

// w52EnrichmentState reads the provenance columns directly.
func w52EnrichmentState(t *testing.T, st *Store, tenantID, id uuid.UUID) (enrichedAt *time.Time, source *string) {
	t.Helper()
	err := st.withTenant(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT geo_enriched_at, geo_enrichment_source FROM civic_cases WHERE tenant_id=$1 AND id=$2`,
			tenantID, id).Scan(&enrichedAt, &source)
	})
	if err != nil {
		t.Fatalf("read enrichment state: %v", err)
	}
	return enrichedAt, source
}

func TestW52CivicEnrichmentColumnsEnsured(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	// ensureCivicTables ran inside store.New; the W52 ALTERs must have
	// landed (and re-running them is a no-op — idempotency proof).
	for _, col := range []string{"geo_enriched_at", "geo_enrichment_source"} {
		var n int
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns
			 WHERE table_name='civic_cases' AND column_name=$1`, col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("civic_cases.%s missing after boot DDL", col)
		}
	}
	if err := st.ensureCivicTables(ctx); err != nil {
		t.Fatalf("ensureCivicTables re-run not idempotent: %v", err)
	}
}

func TestW52CivicEnrichmentFillOnlyWhenEmpty(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	fake := &w52FakeEnricher{res: &GeoEnrichment{State: "Lagos", LGA: "Eti-Osa", Ward: "Victoria Island I"}}
	RegisterCivicGeoEnricher(st, fake, nil)
	t.Cleanup(func() { RegisterCivicGeoEnricher(st, nil, nil) })

	lat, lon := 6.5244, 3.3792

	// Case 1: both empty → both filled + provenance stamped.
	c1 := w52InsertCase(t, st, tenantID, "", "", &lat, &lon)
	got, err := st.GetCivicCase(ctx, tenantID, c1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ward != "Victoria Island I" || got.LGA != "Eti-Osa" {
		t.Fatalf("case 1 ward/lga = %q/%q, want enriched", got.Ward, got.LGA)
	}
	at, src := w52EnrichmentState(t, st, tenantID, c1.ID)
	if at == nil || src == nil || *src != GeoEnrichmentSource {
		t.Fatalf("case 1 provenance = %v/%v, want stamped %q", at, src, GeoEnrichmentSource)
	}

	// Case 2: human-entered ward survives; only lga is filled.
	c2 := w52InsertCase(t, st, tenantID, "My Ward", "", &lat, &lon)
	got, err = st.GetCivicCase(ctx, tenantID, c2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ward != "My Ward" {
		t.Fatalf("case 2 ward = %q — human/agent text was overwritten", got.Ward)
	}
	if got.LGA != "Eti-Osa" {
		t.Fatalf("case 2 lga = %q, want enriched", got.LGA)
	}

	// Case 3: both entered → the enricher is not even called.
	before := fake.callCount()
	c3 := w52InsertCase(t, st, tenantID, "Ward 7", "Some LGA", &lat, &lon)
	if fake.callCount() != before {
		t.Fatal("enricher called although ward+lga were both set")
	}
	at, src = w52EnrichmentState(t, st, tenantID, c3.ID)
	if at != nil || src != nil {
		t.Fatalf("case 3 provenance = %v/%v, want unstamped", at, src)
	}

	// Case 4: no coordinates → no call, no enrichment.
	c4 := w52InsertCase(t, st, tenantID, "", "", nil, nil)
	if fake.callCount() != before {
		t.Fatal("enricher called for a case without coordinates")
	}
	got, err = st.GetCivicCase(ctx, tenantID, c4.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ward != "" || got.LGA != "" {
		t.Fatalf("case 4 enriched without coordinates: %q/%q", got.Ward, got.LGA)
	}
}

func TestW52CivicEnrichmentFailOpen(t *testing.T) {
	st := newTestStore(t)
	tenantID := uuid.New()
	// geo-service unreachable / erroring → client contract returns nil.
	fake := &w52FakeEnricher{res: nil}
	RegisterCivicGeoEnricher(st, fake, nil)
	t.Cleanup(func() { RegisterCivicGeoEnricher(st, nil, nil) })

	lat, lon := 6.5244, 3.3792
	c := w52InsertCase(t, st, tenantID, "", "", &lat, &lon) // must not fail
	if fake.callCount() != 1 {
		t.Fatalf("enricher calls = %d, want 1", fake.callCount())
	}
	got, err := st.GetCivicCase(context.Background(), tenantID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ward != "" || got.LGA != "" {
		t.Fatalf("fail-open row mutated: %q/%q", got.Ward, got.LGA)
	}
	at, src := w52EnrichmentState(t, st, tenantID, c.ID)
	if at != nil || src != nil {
		t.Fatal("fail-open row stamped provenance")
	}
}

func TestW52CivicEnrichmentTenantUUIDForwardingAndRLS(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	fake := &w52FakeEnricher{res: &GeoEnrichment{LGA: "Eti-Osa", Ward: "VI I"}}
	RegisterCivicGeoEnricher(st, fake, nil)
	t.Cleanup(func() { RegisterCivicGeoEnricher(st, nil, nil) })

	lat, lon := 6.5244, 3.3792
	c := w52InsertCase(t, st, tenantA, "", "", &lat, &lon)
	if got := fake.lastSlug(); got != tenantA.String() {
		t.Fatalf("forwarded tenant id = %q, want tenant uuid %s", got, tenantA)
	}

	// RLS: tenant B cannot read the enriched row through the tenant-scoped
	// paths, and its own list stays empty.
	if _, err := st.GetCivicCase(ctx, tenantB, c.ID); err == nil {
		t.Fatal("cross-tenant GetCivicCase succeeded — RLS isolation broken")
	}
	rows, err := st.ListCivicCases(ctx, tenantB, CivicCaseFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("tenant B sees %d civic cases — RLS isolation broken", len(rows))
	}
	// And tenant A reads its own enriched row normally.
	got, err := st.GetCivicCase(ctx, tenantA, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LGA != "Eti-Osa" || got.Ward != "VI I" {
		t.Fatalf("tenant A row: %q/%q", got.LGA, got.Ward)
	}
}
