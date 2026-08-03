package geo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// Geo HTTP API (SPEC-W8 A2). Routes are wired in httpapi/server.go under
// /v1 (the BFF exposes them as /api/bookings/v1/...); the tenant context
// is injected by httpapi's tenant middleware and passed explicitly, so
// this package stays free of httpapi internals. Same authz pattern as the
// sibling resources: reads open to authenticated tenant users, writes
// behind manage_bookings.

// CampaignStarter abstracts the Temporal starter (temporalclient.Client
// satisfies it) so handlers are testable without a Temporal server.
type CampaignStarter interface {
	StartGeoCampaign(ctx context.Context, in GeoCampaignInput) (string, error)
}

// Handlers bundles the geo endpoint dependencies.
type Handlers struct {
	Store     *store.Store
	Starter   CampaignStarter // nil → campaign launch returns 503
	Geocoder  *Geocoder       // nil/disabled → address geocoding off
	BatchSize int             // GEO_CAMPAIGN_BATCH
	Log       *zap.Logger
}

// Campaign channels (Part C offers whatsapp/telegram/sms).
var validChannels = map[string]bool{"whatsapp": true, "telegram": true, "sms": true}

var validLocationSources = map[string]bool{
	store.LocationSourceBookingAddress: true,
	store.LocationSourceChannelShare:   true,
	store.LocationSourceManual:         true,
	store.LocationSourceGeocode:        true,
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handlers) mapErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrPostGISUnavailable):
		writeError(w, http.StatusServiceUnavailable, "geo features unavailable (postgis extension missing)")
	default:
		h.Log.Error("geo handler error", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func urlUUID(w http.ResponseWriter, r *http.Request, param string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid "+param)
		return uuid.Nil, false
	}
	return id, true
}

// MaskPhone masks a phone number for the audience preview sample (PII
// minimization): keep the country-code prefix and the last two digits.
func MaskPhone(phone string) string {
	if len(phone) <= 6 {
		return "***"
	}
	return phone[:3] + strings.Repeat("*", len(phone)-5) + phone[len(phone)-2:]
}

// ---------------------------------------------------------------------------
// PUT /v1/contacts/{id}/location
// ---------------------------------------------------------------------------

type putLocationRequest struct {
	Lat     *float64 `json:"lat"`
	Lng     *float64 `json:"lng"`
	Source  string   `json:"source"`
	Address string   `json:"address"` // optional: geocoded when GEOCODE_ENABLED
}

// PutContactLocation upserts a contact's position from an explicit
// lat/lng pair, or — when the geocoding hook is enabled — from an address
// string (source=geocode).
func (h *Handlers) PutContactLocation(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req putLocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if _, err := h.Store.GetContact(r.Context(), tenant.ID, id); err != nil {
		h.mapErr(w, err)
		return
	}

	loc := store.ContactLocation{TenantID: tenant.ID, ContactID: id}
	switch {
	case req.Lat != nil || req.Lng != nil:
		if req.Lat == nil || req.Lng == nil {
			writeError(w, http.StatusBadRequest, "lat and lng are required together")
			return
		}
		if err := ValidateLatLng(*req.Lat, *req.Lng); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		loc.Lat, loc.Lng = *req.Lat, *req.Lng
		loc.Source = req.Source
		if loc.Source == "" {
			loc.Source = store.LocationSourceManual
		}
		if !validLocationSources[loc.Source] {
			writeError(w, http.StatusBadRequest, "source must be one of booking_address, channel_share, manual, geocode")
			return
		}
	case req.Address != "":
		if !h.Geocoder.Enabled() {
			writeError(w, http.StatusUnprocessableEntity, "address geocoding is disabled (GEOCODE_ENABLED=false); pass lat/lng instead")
			return
		}
		res, err := h.Geocoder.Geocode(r.Context(), req.Address)
		if err != nil {
			h.mapErr(w, err)
			return
		}
		if !res.Found {
			writeError(w, http.StatusUnprocessableEntity, "address could not be geocoded")
			return
		}
		loc.Lat, loc.Lng = res.Lat, res.Lng
		loc.Source = store.LocationSourceGeocode
	default:
		writeError(w, http.StatusBadRequest, "lat+lng (or address with geocoding enabled) is required")
		return
	}

	if err := h.Store.UpsertContactLocation(r.Context(), &loc); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, loc)
}

// ---------------------------------------------------------------------------
// GET /v1/locations/summary?from=&to=&offering_id=
// ---------------------------------------------------------------------------

// LocationsSummary returns booking-joined contact points (cap 5000); when
// the set exceeds 500 points, ST_SnapToGrid clusters are included.
func (h *Handlers) LocationsSummary(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	q := r.URL.Query()
	var f SummaryFilter
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid from (RFC3339)")
			return
		}
		f.From = &t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid to (RFC3339)")
			return
		}
		f.To = &t
	}
	if v := q.Get("offering_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid offering_id")
			return
		}
		f.OfferingID = &id
	}

	pq, pargs := BuildLocationPointsQuery(tenant.ID, f)
	points, err := h.Store.ListLocationPoints(r.Context(), tenant.ID, pq, pargs)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if points == nil {
		points = []store.LocationPoint{}
	}
	clusters := []store.LocationCluster{}
	if len(points) > LocationClusterThreshold {
		cq, cargs := BuildLocationClustersQuery(tenant.ID, f)
		clusters, err = h.Store.ListLocationClusters(r.Context(), tenant.ID, cq, cargs)
		if err != nil {
			h.mapErr(w, err)
			return
		}
		if clusters == nil {
			clusters = []store.LocationCluster{}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"points": points, "clusters": clusters})
}

// ---------------------------------------------------------------------------
// Service areas: GET/POST /v1/service-areas, DELETE /v1/service-areas/{id}
// ---------------------------------------------------------------------------

type createServiceAreaRequest struct {
	Name    string          `json:"name"`
	GeoJSON json.RawMessage `json:"geojson"`
	Meta    json.RawMessage `json:"meta"`
}

// ListServiceAreas handles GET /v1/service-areas.
func (h *Handlers) ListServiceAreas(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	items, err := h.Store.ListServiceAreas(r.Context(), tenant.ID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if items == nil {
		items = []store.ServiceArea{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"service_areas": items})
}

// CreateServiceArea handles POST /v1/service-areas (GeoJSON Polygon or
// MultiPolygon; Polygon is promoted to MultiPolygon server-side).
func (h *Handlers) CreateServiceArea(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	var req createServiceAreaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	norm, err := ValidatePolygonGeoJSON(req.GeoJSON)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Meta) > 0 {
		var m map[string]any
		if err := json.Unmarshal(req.Meta, &m); err != nil || m == nil {
			writeError(w, http.StatusBadRequest, "meta must be a JSON object")
			return
		}
	}
	area := store.ServiceArea{TenantID: tenant.ID, Name: req.Name, GeoJSON: string(norm), Meta: req.Meta}
	if err := h.Store.CreateServiceArea(r.Context(), &area); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, area)
}

// DeleteServiceArea handles DELETE /v1/service-areas/{id}.
func (h *Handlers) DeleteServiceArea(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.Store.DeleteServiceArea(r.Context(), tenant.ID, id); err != nil {
		h.mapErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// POST /v1/geo/audience/preview
// ---------------------------------------------------------------------------

type latLng struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

type audiencePreviewRequest struct {
	Polygon json.RawMessage `json:"polygon"`
	Center  *latLng         `json:"center"`
	RadiusM float64         `json:"radius_m"`
}

// targetFromRequest normalizes the polygon XOR center+radius_m selector.
func targetFromRequest(polygon json.RawMessage, center *latLng, radiusM float64) (Target, error) {
	var t Target
	if len(polygon) > 0 {
		norm, err := ValidatePolygonGeoJSON(polygon)
		if err != nil {
			return t, err
		}
		t.Polygon = norm
	}
	if center != nil {
		t.HasCenter = true
		t.CenterLat, t.CenterLng, t.RadiusM = center.Lat, center.Lng, radiusM
	}
	return t, nil
}

// AudiencePreview handles POST /v1/geo/audience/preview: count + masked
// phone sample of the contacts inside the target (ST_Within for polygons,
// ST_DWithin for center+radius_m).
func (h *Handlers) AudiencePreview(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	var req audiencePreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	target, err := targetFromRequest(req.Polygon, req.Center, req.RadiusM)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cq, cargs, err := BuildAudienceCountQuery(tenant.ID, target)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sq, sargs, err := BuildAudienceSampleQuery(tenant.ID, target, AudienceSampleLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	count, err := h.Store.AudienceCount(r.Context(), tenant.ID, cq, cargs)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	contacts, err := h.Store.AudienceSample(r.Context(), tenant.ID, sq, sargs)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	type sampleEntry struct {
		ContactID   uuid.UUID `json:"contact_id"`
		PhoneMasked string    `json:"phone_masked"`
	}
	sample := make([]sampleEntry, 0, len(contacts))
	for _, c := range contacts {
		sample = append(sample, sampleEntry{ContactID: c.ContactID, PhoneMasked: MaskPhone(c.Phone)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": count, "sample": sample})
}

// ---------------------------------------------------------------------------
// Geo campaigns: POST/GET /v1/geo/campaigns, GET /v1/geo/campaigns/{id}
// ---------------------------------------------------------------------------

type createCampaignRequest struct {
	Name    string `json:"name"`
	Channel string `json:"channel"`
	Message string `json:"message"`
	Target  struct {
		Polygon json.RawMessage `json:"polygon"`
		Center  *latLng         `json:"center"`
		RadiusM float64         `json:"radius_m"`
	} `json:"target"`
}

// CreateGeoCampaign handles POST /v1/geo/campaigns: validates the target,
// creates the row in `running` status and starts the GeoCampaignWorkflow
// (workflow id geo-campaign-{id}, so a duplicate launch is a no-op).
func (h *Handlers) CreateGeoCampaign(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	var req createCampaignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if !validChannels[req.Channel] {
		writeError(w, http.StatusBadRequest, "channel must be one of whatsapp, telegram, sms")
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "message is required ({name} token supported)")
		return
	}
	target, err := targetFromRequest(req.Target.Polygon, req.Target.Center, req.Target.RadiusM)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	campaign := store.GeoCampaign{
		TenantID: tenant.ID,
		Name:     req.Name,
		Channel:  req.Channel,
		Message:  req.Message,
		Status:   store.GeoCampaignRunning,
	}
	var circle *store.CircleTarget
	if target.HasCenter {
		circle = &store.CircleTarget{Lat: target.CenterLat, Lng: target.CenterLng, RadiusM: target.RadiusM}
	} else {
		campaign.TargetGeoJSON = string(target.Polygon)
	}
	if err := h.Store.CreateGeoCampaign(r.Context(), &campaign, circle); err != nil {
		h.mapErr(w, err)
		return
	}

	if h.Starter == nil {
		// Temporal unavailable at boot: the row is durable; flip to failed
		// so it never hangs in `running` without a workflow behind it.
		h.Log.Error("temporal starter unavailable; geo campaign cannot launch",
			zap.String("campaign_id", campaign.ID.String()))
		_ = h.Store.SetGeoCampaignStatus(r.Context(), tenant.ID, campaign.ID, store.GeoCampaignFailed)
		campaign.Status = store.GeoCampaignFailed
		writeError(w, http.StatusServiceUnavailable, "campaign created but workflow engine unavailable")
		return
	}
	// SPEC-W14 Agent D: capture the quiet-hours configuration at schedule
	// time (QUIET_HOURS_* env, SPEC-W12 §8) so the workflow's deferral
	// replay stays deterministic if the env changes mid-campaign. A
	// malformed overrides value is ignored with a warn (defaults apply) —
	// the workflow validates the resolved window when it runs.
	quietOverrides, err := ParseQuietHoursOverrides(os.Getenv("QUIET_HOURS_OVERRIDES"))
	if err != nil {
		h.Log.Warn("ignoring malformed QUIET_HOURS_OVERRIDES; default quiet-hours window applies",
			zap.Error(err))
		quietOverrides = nil
	}
	if _, err := h.Starter.StartGeoCampaign(r.Context(), GeoCampaignInput{
		CampaignID:          campaign.ID.String(),
		TenantID:            tenant.ID.String(),
		TenantSlug:          tenant.Slug,
		Channel:             campaign.Channel,
		Message:             campaign.Message,
		BatchSize:           h.BatchSize,
		QuietHoursWindow:    os.Getenv("QUIET_HOURS_DEFAULT"),
		QuietHoursOverrides: quietOverrides,
	}); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, campaign)
}

// ListGeoCampaigns handles GET /v1/geo/campaigns.
func (h *Handlers) ListGeoCampaigns(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	items, err := h.Store.ListGeoCampaigns(r.Context(), tenant.ID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	if items == nil {
		items = []store.GeoCampaign{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"campaigns": items})
}

// GetGeoCampaign handles GET /v1/geo/campaigns/{id}.
func (h *Handlers) GetGeoCampaign(w http.ResponseWriter, r *http.Request, tenant bookingops.TenantInfo) {
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	campaign, err := h.Store.GetGeoCampaign(r.Context(), tenant.ID, id)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, campaign)
}
