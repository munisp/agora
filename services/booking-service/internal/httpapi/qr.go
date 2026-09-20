package httpapi

// QR scan ingest (SPEC-W45 CODER-A item 9): POST /v1/qr/scan is the PUBLIC
// ping target for printed QR codes (the admin-web /l/{slug} redirect fires
// it fire-and-forget per scan — see QR_FUNNEL_PING_URL there). The site
// slug binds the scan to a tenant server-side (unknown/unpublished slugs
// 404); the endpoint is IP rate-limited like the other public paths.
// GET /v1/qr/scans (tenant, view_analytics) is the analytics read path.

import (
	"net/http"
	"strings"
	"time"

	"github.com/opendesk/booking-service/internal/store"
)

// qrScanRateLimit caps public scan pings per source IP.
const (
	qrScanRateLimit  = 30
	qrScanRateWindow = time.Minute
)

// qrScanRequest is the POST /v1/qr/scan body.
type qrScanRequest struct {
	Slug string `json:"slug"`
	Ref  string `json:"ref,omitempty"`
}

// ingestQRScan handles POST /v1/qr/scan (public, rate-limited, slug-bound).
// 202 Accepted — the caller (the QR redirect) never blocks on the response.
func (s *server) ingestQRScan(w http.ResponseWriter, r *http.Request) {
	if !s.qrLimiter.Allow(r.RemoteAddr, qrScanRateLimit, qrScanRateWindow) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	var req qrScanRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	slug := strings.TrimSpace(req.Slug)
	if slug == "" || len(slug) > 64 {
		writeError(w, http.StatusBadRequest, "slug is required (max 64 chars)")
		return
	}
	site, err := s.d.Store.GetSiteBySlug(r.Context(), slug)
	if err != nil {
		// Unknown OR unpublished slug — no tenant existence oracle beyond
		// what the public booking page itself exposes (it 404s too).
		writeError(w, http.StatusNotFound, "unknown site slug")
		return
	}
	ref := strings.TrimSpace(req.Ref)
	if len(ref) > 128 {
		ref = ref[:128]
	}
	ua := r.UserAgent()
	if len(ua) > 256 {
		ua = ua[:256]
	}
	scan := store.QRScan{
		TenantID:  site.TenantID,
		SiteSlug:  site.Slug,
		Ref:       ref,
		UserAgent: ua,
	}
	if err := s.d.Store.InsertQRScan(r.Context(), &scan); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"recorded": true, "scan_id": scan.ID})
}

// listQRScans handles GET /v1/qr/scans?from&to (view_analytics) — the QR
// analytics read path: total scans + the newest scan rows of the tenant.
func (s *server) listQRScans(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	q := r.URL.Query()
	var from, to *time.Time
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid from (RFC3339)")
			return
		}
		from = &t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid to (RFC3339)")
			return
		}
		to = &t
	}
	sum, err := s.d.Store.ListQRScans(r.Context(), tenant.ID, from, to)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}
