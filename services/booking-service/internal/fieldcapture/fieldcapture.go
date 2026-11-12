// Package fieldcapture implements SPEC-W16 Agent B (contract §4): the
// server half of the field PWA / mobile offline queue. Clients batch
// queued items to POST /v1/field/capture; every item carries a
// client-generated idempotency key (client_id, a UUID — logical dedupe key
// "field_capture:{client_id}") so flushing the queue after connectivity
// loss, and any retry of the same flush, applies each item exactly once.
//
// Kinds:
//   - lead_capture → creates a lead via the W13 leads service (channel
//     "field", honoring its 24h first-touch dedupe on top of the
//     client_id anchor);
//   - checkin → appends a geo check-in row. The W8 location store
//     (store/geolocations.go contact_locations) is a last-known-position
//     UPSERT keyed (tenant_id, contact_id) — it exposes NO history — so
//     check-ins persist in the new field_checkins table per the spec's
//     fallback clause.
//
// Persistence mirrors the W13 leads idiom (idempotent bootstrap DDL, RLS
// enabled + forced, tenant_isolation policy) packaged like the W14
// PayoutStore (dedicated small pool).
package fieldcapture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// Capture kinds (contract §4 enum; SPEC-W32 WS-A adds civic_report).
const (
	KindLeadCapture = "lead_capture"
	KindCheckin     = "checkin"
	// KindCivicReport (SPEC-W32 WS-A): a field agent files a civic report
	// on behalf of a citizen — payload = the public report body; the civic
	// module creates a case with channel=pwa.
	KindCivicReport = "civic_report"
)

// Item result statuses.
const (
	StatusApplied = "applied" // first application (or leads 24h dedupe hit — still exactly-once)
	StatusDeduped = "deduped" // client_id replay: original outcome returned, nothing re-applied
	StatusError   = "error"   // deterministic validation failure (recorded; replays dedupe to it)
)

// ErrInvalidInput marks deterministic validation failures.
var ErrInvalidInput = errors.New("invalid field capture input")

// maxClientIDLen bounds client_id (a UUID string is 36 chars; headroom for
// prefixed variants).
const maxClientIDLen = 128

// GPS is the optional fix attached to a capture item (contract §4:
// gps:{lat,lng,accuracy} null).
type GPS struct {
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
	Accuracy float64 `json:"accuracy"` // meters
}

// Validate enforces sane coordinate ranges.
func (g *GPS) Validate() error {
	if g.Lat < -90 || g.Lat > 90 {
		return fmt.Errorf("%w: gps.lat out of range", ErrInvalidInput)
	}
	if g.Lng < -180 || g.Lng > 180 {
		return fmt.Errorf("%w: gps.lng out of range", ErrInvalidInput)
	}
	if g.Accuracy < 0 {
		return fmt.Errorf("%w: gps.accuracy must be >= 0", ErrInvalidInput)
	}
	return nil
}

// CaptureItem is one offline-queue entry (contract §4). Payload is
// kind-specific and kept as raw JSON: it is persisted verbatim on the
// field_captures anchor row (audit trail / no data loss for fields the
// server does not structure yet).
type CaptureItem struct {
	ClientID   string          `json:"client_id"`
	Kind       string          `json:"kind"`
	Payload    json.RawMessage `json:"payload"`
	CapturedAt *time.Time      `json:"captured_at"`
	GPS        *GPS            `json:"gps"`
}

// Validate checks the kind-agnostic fields. Kind-specific payload
// validation happens in the service (it owns the payload schemas).
func (it *CaptureItem) Validate() error {
	it.ClientID = strings.TrimSpace(it.ClientID)
	if it.ClientID == "" {
		return fmt.Errorf("%w: client_id is required", ErrInvalidInput)
	}
	if len(it.ClientID) > maxClientIDLen {
		return fmt.Errorf("%w: client_id exceeds %d bytes", ErrInvalidInput, maxClientIDLen)
	}
	switch it.Kind {
	case KindLeadCapture, KindCheckin, KindCivicReport:
	default:
		return fmt.Errorf("%w: kind %q (want lead_capture|checkin|civic_report)", ErrInvalidInput, it.Kind)
	}
	if len(it.Payload) == 0 {
		it.Payload = json.RawMessage(`{}`)
	}
	if !json.Valid(it.Payload) {
		return fmt.Errorf("%w: payload is not valid JSON", ErrInvalidInput)
	}
	if it.GPS != nil {
		if err := it.GPS.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// LeadCapturePayload is the kind=lead_capture payload schema. PhoneE164 is
// the only required field (mirrors POST /v1/leads); name/notes are
// preserved verbatim on the anchor row's payload but have no leads column
// today (documented in docs/field-capture.md).
type LeadCapturePayload struct {
	PhoneE164  string         `json:"phone_e164"`
	Name       string         `json:"name,omitempty"`
	Notes      string         `json:"notes,omitempty"`
	UTM        map[string]any `json:"utm,omitempty"`
	CampaignID *uuid.UUID     `json:"campaign_id,omitempty"`
	LgaID      *int           `json:"lga_id,omitempty"`
	ConsentID  *uuid.UUID     `json:"consent_id,omitempty"`
}

// CheckinPayload is the kind=checkin payload schema: an optional contact
// being visited plus a free-text note. The GPS fix rides on the item, not
// the payload.
type CheckinPayload struct {
	ContactID *uuid.UUID `json:"contact_id,omitempty"`
	Note      string     `json:"note,omitempty"`
}

// CivicReportPayload is the kind=civic_report payload schema (SPEC-W32
// WS-A): the same shape as the public report body — it flows through the
// existing capture pipe and creates a civic case with channel=pwa.
type CivicReportPayload struct {
	CategorySlug      string `json:"category_slug"`
	Description       string `json:"description"`
	Ward              string `json:"ward,omitempty"`
	LGA               string `json:"lga,omitempty"`
	LocationText      string `json:"location_text,omitempty"`
	ReporterPhoneE164 string `json:"reporter_phone_e164,omitempty"`
	ReporterName      string `json:"reporter_name,omitempty"`
	Anonymous         bool   `json:"anonymous,omitempty"`
	PhotoURL          string `json:"photo_url,omitempty"`
}

// ItemResult is the per-item outcome of a batch. On a deduped replay the
// ORIGINAL result (lead_id / checkin_id / case_ref / error) is returned
// unchanged so the client can reconcile its outbox without side effects.
type ItemResult struct {
	ClientID  string     `json:"client_id"`
	Kind      string     `json:"kind"`
	Status    string     `json:"status"` // applied | deduped | error
	LeadID    *uuid.UUID `json:"lead_id,omitempty"`
	CheckinID *uuid.UUID `json:"checkin_id,omitempty"`
	CaseID    *uuid.UUID `json:"case_id,omitempty"`  // SPEC-W32: civic_report outcome
	CaseRef   string     `json:"case_ref,omitempty"` // SPEC-W32: GOV-... reference
	Error     string     `json:"error,omitempty"`
}

// ClockSkewThreshold (SPEC-W45 OOS-21): when the client-claimed captured_at
// and the server's receipt timestamp differ by more than this, the read API
// flags the row (the device clock is untrustworthy — e.g. an offline PWA
// queued for hours, or a mis-set handset).
const ClockSkewThreshold = 5 * time.Minute

// Checkin mirrors booking.field_checkins: one geo check-in event (the W8
// contact_locations store has no history, so the history lives here).
type Checkin struct {
	ID         uuid.UUID       `json:"checkin_id"`
	TenantID   uuid.UUID       `json:"tenant_id"`
	ContactID  *uuid.UUID      `json:"contact_id"`
	Lat        *float64        `json:"lat"`
	Lng        *float64        `json:"lng"`
	AccuracyM  *float64        `json:"accuracy_m"`
	Note       string          `json:"note"`
	Payload    json.RawMessage `json:"payload"`
	CapturedAt *time.Time      `json:"captured_at"`
	CreatedAt  time.Time       `json:"created_at"`
	// SPEC-W45 OOS-21: server-side receipt timestamp stored alongside the
	// client-claimed captured_at, plus the derived skew flag surfaced in
	// the read API.
	ServerReceivedAt time.Time `json:"server_received_at"`
	ClockSkew        bool      `json:"clock_skew"`
}

// ComputeClockSkew sets ClockSkew from captured_at vs server_received_at
// (false when the client sent no captured_at — nothing to distrust).
func (c *Checkin) ComputeClockSkew() {
	c.ClockSkew = false
	if c.CapturedAt == nil || c.ServerReceivedAt.IsZero() {
		return
	}
	diff := c.ServerReceivedAt.Sub(*c.CapturedAt)
	if diff < 0 {
		diff = -diff
	}
	c.ClockSkew = diff > ClockSkewThreshold
}

// ---------------------------------------------------------------------------
// Geo enrichment (SPEC-W52 Scope B)
// ---------------------------------------------------------------------------
//
// Freshly applied capture items carrying a GPS fix are enriched via the
// geo-service AFTER the underlying row commits (fail-open — a failed/timed-out
// enrich never fails the capture item, which is already applied and anchored):
//
//   - checkin       → the field_checkins row's lga/ward are filled when empty
//                     (SetCheckinGeoEnrichment) and stamped with
//                     geo_enriched_at / geo_enrichment_source='geo-service'.
//   - civic_report  → the created civic_cases row is enriched inside
//                     store.InsertCivicCase (the registered civic enricher);
//                     the capture layer deliberately does not double-enrich.
//   - lead_capture  → leads carry lga_id (INTEGER into the synthetic cac
//                     gazetteer), not lga/ward text; mapping a boundary NAME
//                     onto a synthetic integer id is a data-model decision
//                     deferred to a later wave — documented gap, not a stub.
//
// The enrichment HTTP call is synchronous but bounded by the client's 3s
// timeout (geo.EnrichTimeout); per-item latency on a batch stays additive
// only for freshly applied GPS items.

// CaptureEnriched serves POST /v1/field/capture with SPEC-W52 geo
// enrichment on top of Handlers.Capture: the batch applies with identical
// exactly-once semantics, then freshly applied GPS items are enriched
// (fail-open). httpapi wires this variant only when a geo enricher is
// configured; otherwise the plain Capture path runs unchanged. The decode /
// batch-limit prologue mirrors Handlers.Capture (handlers.go) — keep in
// sync.
func (h *Handlers) CaptureEnriched(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo, enr store.GeoEnricher) {
	if enr == nil {
		h.Capture(w, r, tenant)
		return
	}
	var req captureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "items is required (non-empty array)")
		return
	}
	limit := h.BatchLimit
	if limit <= 0 {
		limit = 100
	}
	if len(req.Items) > limit {
		writeError(w, http.StatusBadRequest, "too many items (max per batch)")
		return
	}
	results := h.Svc.Capture(r.Context(), tenant, req.Items)
	h.enrichApplied(r.Context(), tenant, req.Items, results, enr)
	writeJSON(w, http.StatusOK, captureResponse{Results: results})
}

// enrichApplied post-enriches freshly applied GPS items (see section
// comment). Fail-open per item: errors are logged, never surfaced — the
// item result the client reconciles against is already final.
func (h *Handlers) enrichApplied(ctx context.Context, tenant bookingops.TenantInfo, items []CaptureItem, results []ItemResult, enr store.GeoEnricher) {
	for i := range items {
		if i >= len(results) || results[i].Status != StatusApplied || items[i].GPS == nil {
			continue
		}
		if items[i].Kind != KindCheckin || results[i].CheckinID == nil {
			continue // civic_report: enriched at the civic insert; lead_capture: no lga/ward text columns
		}
		// The geo-service parses X-Tenant-Slug as the tenant uuid (RLS GUC
		// app.tenant_id) — forward tenant.ID, never the slug.
		res := enr.Enrich(ctx, tenant.ID.String(), items[i].GPS.Lat, items[i].GPS.Lng)
		if res == nil || (res.LGA == "" && res.Ward == "") {
			continue
		}
		if err := h.Svc.Store.SetCheckinGeoEnrichment(ctx, tenant.ID, *results[i].CheckinID, res.LGA, res.Ward); err != nil {
			h.log().Warn("field check-in geo enrichment failed (fail-open)",
				zap.String("tenant_id", tenant.ID.String()), zap.Error(err))
		}
	}
}

// fcGeoEnsured tracks the per-store lazy migration state for the
// field_checkins enrichment columns (Store's struct lives in store.go,
// outside the W52 file scope, so the once-state rides a registry keyed by
// *Store — same idiom as the civic enricher registry).
var fcGeoEnsured sync.Map // *Store → struct{}

// ensureGeoEnrichmentColumns applies the SPEC-W52 provenance columns to
// field_checkins idempotently (ALTER … IF NOT EXISTS, same guards as the
// boot-time ensure* DDL). Boot-time coverage: store.ensureGeoTables already
// carries these ALTERs for existing deployments; this lazy path covers the
// fresh-cluster ordering gap (field_checkins is created by THIS package's
// ensureSchema, dialled after store.New) and the package's own tests.
//
// NOTE (RLS): bootstrap DDL is a superuser migration path, not a tenant
// query — it intentionally runs outside withTenant.
func (s *Store) ensureGeoEnrichmentColumns(ctx context.Context) error {
	const ddl = `
ALTER TABLE IF EXISTS field_checkins ADD COLUMN IF NOT EXISTS lga TEXT NOT NULL DEFAULT '';
ALTER TABLE IF EXISTS field_checkins ADD COLUMN IF NOT EXISTS ward TEXT NOT NULL DEFAULT '';
ALTER TABLE IF EXISTS field_checkins ADD COLUMN IF NOT EXISTS geo_enriched_at TIMESTAMPTZ;
ALTER TABLE IF EXISTS field_checkins ADD COLUMN IF NOT EXISTS geo_enrichment_source TEXT;
ALTER TABLE IF EXISTS field_checkins DROP CONSTRAINT IF EXISTS field_checkins_geo_enrichment_source_check;
ALTER TABLE IF EXISTS field_checkins ADD CONSTRAINT field_checkins_geo_enrichment_source_check
    CHECK (geo_enrichment_source IS NULL OR geo_enrichment_source IN ('geo-service'));`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("ensure field_checkins enrichment columns: %w", err)
	}
	return nil
}

// SetCheckinGeoEnrichment fills a check-in row's lga/ward ONLY where empty
// (the SQL CASE re-checks under the writer so a concurrent human/agent edit
// is never clobbered) and stamps geo_enriched_at/geo_enrichment_source. The
// WHERE clause skips the write entirely when nothing empty can be filled
// (no provenance stamp without machine-filled data). Tenant-scoped via
// withTenant (RLS intact). Fail-open is the caller's posture; this method
// reports its error honestly.
func (s *Store) SetCheckinGeoEnrichment(ctx context.Context, tenantID, checkinID uuid.UUID, lga, ward string) error {
	if _, done := fcGeoEnsured.Load(s); !done {
		if err := s.ensureGeoEnrichmentColumns(ctx); err != nil {
			return err
		}
		fcGeoEnsured.Store(s, struct{}{})
	}
	const q = `UPDATE field_checkins SET
		ward = CASE WHEN ward = '' AND $4 <> '' THEN $4 ELSE ward END,
		lga  = CASE WHEN lga  = '' AND $3 <> '' THEN $3 ELSE lga  END,
		geo_enriched_at = now(),
		geo_enrichment_source = $5
		WHERE tenant_id = $1 AND id = $2
		  AND ((ward = '' AND $4 <> '') OR (lga = '' AND $3 <> ''))`
	return s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, q, tenantID, checkinID, lga, ward, store.GeoEnrichmentSource)
		return err
	})
}
