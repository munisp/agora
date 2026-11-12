package fieldcapture

// SPEC-W52 Scope B field-capture enrichment tests — embedded Postgres
// (newTestRig, port 5562, -short skips): CaptureEnriched fills
// field_checkins lga/ward for freshly applied GPS check-ins (fail-open,
// fill-only-when-empty at the SQL layer), stamps provenance, skips
// deduped/lead_capture items, and keeps RLS tenant isolation. The lazy
// ensureGeoEnrichmentColumns path is exercised implicitly: the rig's
// database gets the columns on first enrichment.

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/store"
)

// w52FakeEnricher is the store.GeoEnricher seam with call recording.
type w52FakeEnricher struct {
	mu    sync.Mutex
	res   *store.GeoEnrichment
	calls []string // tenant slugs seen
}

func (f *w52FakeEnricher) Enrich(_ context.Context, tenantSlug string, _, _ float64) *store.GeoEnrichment {
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

// w52CheckinGeo reads the enrichment columns of one check-in row directly
// (same-package test: the pool is package-private by design).
func w52CheckinGeo(t *testing.T, fs *Store, tenantID, checkinID uuid.UUID) (lga, ward string, enrichedAt *time.Time, source *string) {
	t.Helper()
	err := fs.withTenant(context.Background(), tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT lga, ward, geo_enriched_at, geo_enrichment_source FROM field_checkins WHERE tenant_id=$1 AND id=$2`,
			tenantID, checkinID).Scan(&lga, &ward, &enrichedAt, &source)
	})
	if err != nil {
		t.Fatalf("read checkin geo: %v", err)
	}
	return lga, ward, enrichedAt, source
}

func w52CaptureHTTP(t *testing.T, h *Handlers, tenant bookingops.TenantInfo, enr store.GeoEnricher, body string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/field/capture", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.CaptureEnriched(rec, req, tenant, enr)
	return rec.Code
}

func TestW52CaptureEnrichedCheckin(t *testing.T) {
	rig := newTestRig(t)
	h := &Handlers{Svc: rig.svc, BatchLimit: 10}
	fake := &w52FakeEnricher{res: &store.GeoEnrichment{State: "Lagos", LGA: "Eti-Osa", Ward: "VI I"}}

	// Fresh GPS check-in → enriched + stamped.
	body := `{"items":[{"client_id":"` + uuid.NewString() + `","kind":"checkin","payload":{"note":"site visit"},` +
		`"gps":{"lat":6.5244,"lng":3.3792,"accuracy":12}}]}`
	if code := w52CaptureHTTP(t, h, rig.tenant, fake, body); code != 200 {
		t.Fatalf("capture = %d", code)
	}
	if fake.callCount() != 1 {
		t.Fatalf("enricher calls = %d, want 1", fake.callCount())
	}
	checkins, err := rig.fstore.ListCheckins(context.Background(), rig.tenant.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkins) != 1 {
		t.Fatalf("checkins = %d, want 1", len(checkins))
	}
	lga, ward, at, src := w52CheckinGeo(t, rig.fstore, rig.tenant.ID, checkins[0].ID)
	if lga != "Eti-Osa" || ward != "VI I" {
		t.Fatalf("checkin lga/ward = %q/%q, want enriched", lga, ward)
	}
	if at == nil || src == nil || *src != store.GeoEnrichmentSource {
		t.Fatalf("provenance = %v/%v, want stamped %q", at, src, store.GeoEnrichmentSource)
	}

	// SQL-level fill-only-when-empty: a second enrichment attempt with
	// different values must not overwrite.
	if err := rig.fstore.SetCheckinGeoEnrichment(context.Background(), rig.tenant.ID, checkins[0].ID, "Other LGA", "Other Ward"); err != nil {
		t.Fatal(err)
	}
	lga, ward, _, _ = w52CheckinGeo(t, rig.fstore, rig.tenant.ID, checkins[0].ID)
	if lga != "Eti-Osa" || ward != "VI I" {
		t.Fatalf("re-enrich overwrote values: %q/%q", lga, ward)
	}

	// Deduped replay of the same client_id → no second enrich call.
	if code := w52CaptureHTTP(t, h, rig.tenant, fake, body); code != 200 {
		t.Fatalf("replay = %d", code)
	}
	if fake.callCount() != 1 {
		t.Fatalf("replay triggered enrich: calls = %d, want 1", fake.callCount())
	}

	// lead_capture carries no lga/ward text columns → not enriched.
	leadBody := `{"items":[{"client_id":"` + uuid.NewString() + `","kind":"lead_capture",` +
		`"payload":{"phone_e164":"+2348012345678"},"gps":{"lat":6.5,"lng":3.3,"accuracy":5}}]}`
	if code := w52CaptureHTTP(t, h, rig.tenant, fake, leadBody); code != 200 {
		t.Fatalf("lead capture = %d", code)
	}
	if fake.callCount() != 1 {
		t.Fatalf("lead_capture triggered enrich: calls = %d, want 1", fake.callCount())
	}

	// RLS: another tenant sees no check-ins.
	other := uuid.New()
	rows, err := rig.fstore.ListCheckins(context.Background(), other, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("tenant %s sees %d check-ins — RLS isolation broken", other, len(rows))
	}
}

func TestW52CaptureEnrichedFailOpen(t *testing.T) {
	rig := newTestRig(t)
	h := &Handlers{Svc: rig.svc, BatchLimit: 10}
	// geo-service down/erroring → client contract returns nil.
	fake := &w52FakeEnricher{res: nil}
	body := `{"items":[{"client_id":"` + uuid.NewString() + `","kind":"checkin","payload":{"note":"x"},` +
		`"gps":{"lat":6.5244,"lng":3.3792,"accuracy":12}}]}`
	if code := w52CaptureHTTP(t, h, rig.tenant, fake, body); code != 200 {
		t.Fatalf("capture = %d — enrichment failure must never fail the capture", code)
	}
	checkins, err := rig.fstore.ListCheckins(context.Background(), rig.tenant.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkins) != 1 {
		t.Fatalf("checkins = %d, want 1 (row stands)", len(checkins))
	}
	// Apply the lazy column migration (it normally runs on the first actual
	// enrichment — which the fail-open path correctly skipped) so the
	// provenance columns can be read.
	if err := rig.fstore.ensureGeoEnrichmentColumns(context.Background()); err != nil {
		t.Fatalf("lazy ensure: %v", err)
	}
	lga, ward, at, src := w52CheckinGeo(t, rig.fstore, rig.tenant.ID, checkins[0].ID)
	if lga != "" || ward != "" || at != nil || src != nil {
		t.Fatalf("fail-open row mutated: %q/%q/%v/%v", lga, ward, at, src)
	}
}

// TestW52CaptureNilEnricherFallsBack proves CaptureEnriched with a nil
// enricher is exactly the pre-W52 Capture path.
func TestW52CaptureNilEnricherFallsBack(t *testing.T) {
	rig := newTestRig(t)
	h := &Handlers{Svc: rig.svc, BatchLimit: 10}
	body := `{"items":[{"client_id":"` + uuid.NewString() + `","kind":"checkin","payload":{"note":"x"},` +
		`"gps":{"lat":6.5244,"lng":3.3792,"accuracy":12}}]}`
	if code := w52CaptureHTTP(t, h, rig.tenant, nil, body); code != 200 {
		t.Fatalf("capture = %d", code)
	}
	checkins, err := rig.fstore.ListCheckins(context.Background(), rig.tenant.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkins) != 1 {
		t.Fatalf("checkins = %d, want 1", len(checkins))
	}
}
