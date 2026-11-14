package httpapi

// SPEC-W61 Scope C: the WhatsApp media inbox surface — machine ingest plus
// the staff review routes.
//
// Routes (wired in server.go):
//
//	POST /v1/media/inbox/ingest            internauth-gated machine ingest
//	GET  /v1/media/inbox?state=&limit=&offset=   staff list (paginated)
//	POST /v1/media/inbox/{id}/link         {booking_id} staff link
//	POST /v1/media/inbox/{id}/discard      staff discard
//
// Ingest auth (K2 fail-closed, bookings.go internauth idiom): the route is
// wrapped in s.internAuth — X-Internal-Token must match
// BOOKING_INTERNAL_TOKEN (constant-time); unset token => 503, missing/wrong
// => 401. APISIX edge-denies the route (D's block rule), so the token is
// the second line, not the only one. The tenant is resolved in-handler
// (incidents ingest precedent, NOT the tenant middleware): the gateway
// knows the tenant from the channel site map and addresses it via the
// optional tenant_id/tenant_slug body fields or the X-Tenant-Slug header.
//
// Staff routes (KYC P5 posture, kyc_handlers.go precedent):
// require(manage_bookings) + requireOwnerAdmin — manage_bookings includes
// field agents holding the member role, so the realm-role gate rejects
// members with 403 on ALL three routes.
//
// Store boundary: ALL persistence lives in B's store/media_inbox.go
// (IngestMediaInbox / ListMediaInbox / LinkMediaInbox / DiscardMediaInbox);
// this file is validation + transport mapping only. Deterministic
// validation failures are 400; store errors are 500 (ErrNotFound => 404 on
// the staff actions); a nil store fails closed with 503 (never a panic).

import (
	"errors"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// mediaInboxEventsTopic resolves the ReceiptIngested outbox topic
// (SPEC-W64 Scope 3) for the ingest path: BOOKING_EVENTS_TOPIC, default
// opendesk.booking.events — mirrors internal/config (config.go:206). Read
// here instead of via Deps because server.go is single-owner this wave
// (A: resolve route line only); a later wave may thread cfg through Deps.
func mediaInboxEventsTopic() string {
	if v := strings.TrimSpace(os.Getenv("BOOKING_EVENTS_TOPIC")); v != "" {
		return v
	}
	return "opendesk.booking.events"
}

// mediaInboxIngestRequest is the POST /v1/media/inbox/ingest body
// (SPEC-W61 pinned contract). tenant_id/tenant_slug are ADDITIVE tenant
// addressing (the pinned body carries none; the incidents ingest precedent
// resolves the tenant in-handler exactly this way).
type mediaInboxIngestRequest struct {
	TenantID          string `json:"tenant_id,omitempty"`
	TenantSlug        string `json:"tenant_slug,omitempty"`
	Channel           string `json:"channel"`
	MessageID         string `json:"message_id"`
	DocumentID        string `json:"document_id"`
	SenderPhone       string `json:"sender_phone"`
	SenderPhoneDigits string `json:"sender_phone_digits"`
	Mime              string `json:"mime"`
	Bytes             int64  `json:"bytes"`
	SHA256Prefix      string `json:"sha256_prefix"`
	// BookingRefHint (SPEC-W66 WS2) is the optional caption-extracted
	// BK-XXXXXXXX token. Best-effort: a malformed hint is treated as
	// ABSENT, never a 400. The caption itself never crosses the wire (P8).
	BookingRefHint string `json:"booking_ref_hint,omitempty"`
}

// mediaInboxItemJSON is the staff-facing/media response projection of a
// store.MediaInboxItem (snake_case; AnchorMethod/LinkedBy are plain strings
// in the pinned store contract — empty string = absent, omitted from JSON).
type mediaInboxItemJSON struct {
	ID                uuid.UUID  `json:"id"`
	Channel           string     `json:"channel"`
	ProviderMessageID string     `json:"provider_message_id"`
	DocumentID        uuid.UUID  `json:"document_id"`
	SenderPhone       string     `json:"sender_phone"`
	SenderPhoneDigits string     `json:"sender_phone_digits"`
	Mime              string     `json:"mime"`
	Bytes             int64      `json:"bytes"`
	SHA256Prefix      string     `json:"sha256_prefix"`
	State             string     `json:"state"`
	AnchorMethod      string     `json:"anchor_method,omitempty"`
	BookingID         *uuid.UUID `json:"booking_id,omitempty"`
	ContactID         *uuid.UUID `json:"contact_id,omitempty"`
	LinkedBy          string     `json:"linked_by,omitempty"`
	BookingRefHint    string     `json:"booking_ref_hint,omitempty"` // SPEC-W66 WS2: token only
	CreatedAt         string     `json:"created_at"`
	UpdatedAt         string     `json:"updated_at"`
}

func mediaInboxItemToJSON(it store.MediaInboxItem) mediaInboxItemJSON {
	return mediaInboxItemJSON{
		ID:                it.ID,
		Channel:           it.Channel,
		ProviderMessageID: it.ProviderMessageID,
		DocumentID:        it.DocumentID,
		SenderPhone:       it.SenderPhone,
		SenderPhoneDigits: it.SenderPhoneDigits,
		Mime:              it.Mime,
		Bytes:             it.Bytes,
		SHA256Prefix:      it.SHA256Prefix,
		State:             it.State,
		AnchorMethod:      it.AnchorMethod,
		BookingID:         it.BookingID,
		ContactID:         it.ContactID,
		LinkedBy:          it.LinkedBy,
		BookingRefHint:    it.BookingRefHint,
		CreatedAt:         it.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999Z"),
		UpdatedAt:         it.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999Z"),
	}
}

var (
	mediaInboxDigitsRe = regexp.MustCompile(`^[0-9]+$`)
	mediaInboxHexRe    = regexp.MustCompile(`^[0-9a-f]+$`)
)

// mediaInboxStoreOr503 fails closed when the store is unwired (the route
// exists and answers 503 rather than panicking — KYC routes-wired idiom).
func (s *server) mediaInboxStoreOr503(w http.ResponseWriter) *store.Store {
	if s.d.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "media inbox not configured (store unavailable)")
		return nil
	}
	return s.d.Store
}

// mediaInboxIngest serves POST /v1/media/inbox/ingest (internauth-gated,
// edge-blocked by APISIX). Idempotent on (tenant, channel, message_id) via
// the store's UNIQUE key: a replayed ingest returns the EXISTING row.
func (s *server) mediaInboxIngest(w http.ResponseWriter, r *http.Request) {
	st := s.mediaInboxStoreOr503(w)
	if st == nil {
		return
	}
	var req mediaInboxIngestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tenantID, ok := s.mediaInboxTenant(w, r, req.TenantID, req.TenantSlug)
	if !ok {
		return
	}
	// Deterministic validation (400) BEFORE any store call.
	req.Channel = strings.TrimSpace(req.Channel)
	req.MessageID = strings.TrimSpace(req.MessageID)
	req.SenderPhone = strings.TrimSpace(req.SenderPhone)
	req.SenderPhoneDigits = strings.TrimSpace(req.SenderPhoneDigits)
	req.Mime = strings.TrimSpace(req.Mime)
	req.SHA256Prefix = strings.TrimSpace(req.SHA256Prefix)
	if req.Channel == "" || len(req.Channel) > 32 {
		writeError(w, http.StatusBadRequest, "channel is required (max 32 chars)")
		return
	}
	if req.MessageID == "" || len(req.MessageID) > 128 {
		writeError(w, http.StatusBadRequest, "message_id is required (max 128 chars)")
		return
	}
	documentID, err := uuid.Parse(strings.TrimSpace(req.DocumentID))
	if err != nil {
		writeError(w, http.StatusBadRequest, "document_id must be a uuid")
		return
	}
	if req.SenderPhone == "" || len(req.SenderPhone) > 32 {
		writeError(w, http.StatusBadRequest, "sender_phone is required (max 32 chars)")
		return
	}
	// sender_phone_digits is the gateway-computed canonical form (G3); the
	// store re-derives it SQL-side (trust-but-verify). When supplied it
	// must be digits-only — anything else is a contract violation.
	if req.SenderPhoneDigits != "" && (len(req.SenderPhoneDigits) > 32 || !mediaInboxDigitsRe.MatchString(req.SenderPhoneDigits)) {
		writeError(w, http.StatusBadRequest, "sender_phone_digits must be digits-only")
		return
	}
	if req.Mime == "" || len(req.Mime) > 128 {
		writeError(w, http.StatusBadRequest, "mime is required (max 128 chars)")
		return
	}
	if req.Bytes < 0 {
		writeError(w, http.StatusBadRequest, "bytes must be >= 0")
		return
	}
	// sha256_prefix = first 16 hex chars of the content sha256 (PII-light
	// anchor payload); bounded + hex to keep the column deterministic.
	if req.SHA256Prefix == "" || len(req.SHA256Prefix) > 64 || !mediaInboxHexRe.MatchString(req.SHA256Prefix) {
		writeError(w, http.StatusBadRequest, "sha256_prefix must be 1..64 lowercase hex chars")
		return
	}

	// SPEC-W66 WS2: booking_ref_hint is best-effort — bounded here and
	// normalized/validated store-side (BK- + 8 Crockford chars); a
	// malformed hint is treated as ABSENT, never a 400 (binding ruling).
	req.BookingRefHint = strings.TrimSpace(req.BookingRefHint)
	if len(req.BookingRefHint) > 64 {
		req.BookingRefHint = ""
	}

	item, err := st.IngestMediaInbox(r.Context(), store.MediaInboxIngest{
		TenantID:          tenantID,
		Channel:           req.Channel,
		ProviderMessageID: req.MessageID,
		DocumentID:        documentID,
		SenderPhone:       req.SenderPhone,
		SenderPhoneDigits: req.SenderPhoneDigits,
		Mime:              req.Mime,
		Bytes:             req.Bytes,
		SHA256Prefix:      req.SHA256Prefix,
		BookingRefHint:    req.BookingRefHint,
	}, mediaInboxEventsTopic())
	if err != nil {
		s.d.Logger.Error("media inbox ingest failed", zap.Error(err),
			zap.String("tenant_id", tenantID.String()), zap.String("channel", req.Channel))
		writeError(w, http.StatusInternalServerError, "media inbox ingest failed")
		return
	}
	// 200 {id, state, booking_id?} — the pinned response shape.
	resp := map[string]any{"id": item.ID, "state": item.State}
	if item.BookingID != nil {
		resp["booking_id"] = *item.BookingID
	}
	writeJSON(w, http.StatusOK, resp)
}

// mediaInboxTenant resolves the ingest tenant (incidents ingest idiom):
// body tenant_id wins; else body tenant_slug or the X-Tenant-Slug header
// resolved via the identity resolver. Missing addressing is a 400, never a
// silent no-tenant row.
func (s *server) mediaInboxTenant(w http.ResponseWriter, r *http.Request, tenantIDRaw, tenantSlugRaw string) (uuid.UUID, bool) {
	if raw := strings.TrimSpace(tenantIDRaw); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "tenant_id must be a uuid")
			return uuid.Nil, false
		}
		return id, true
	}
	slug := strings.TrimSpace(tenantSlugRaw)
	if slug == "" {
		slug = strings.TrimSpace(r.Header.Get("X-Tenant-Slug"))
	}
	if slug == "" {
		writeError(w, http.StatusBadRequest, "tenant_id or tenant_slug is required")
		return uuid.Nil, false
	}
	if s.d.Resolver == nil {
		writeError(w, http.StatusServiceUnavailable, "tenant resolver not configured")
		return uuid.Nil, false
	}
	info, err := s.d.Resolver.BySlug(r.Context(), slug)
	if err != nil {
		s.d.Logger.Warn("media ingest tenant resolution failed", zap.String("slug", slug), zap.Error(err))
		writeError(w, http.StatusNotFound, "tenant not found")
		return uuid.Nil, false
	}
	return info.ID, true
}

// validMediaInboxState mirrors the CHECK constraint (SPEC-W61 pinned
// states); the empty string means "no filter".
func validMediaInboxState(state string) bool {
	switch state {
	case "parked", "anchored", "linked", "discarded":
		return true
	}
	return false
}

// mediaInboxList serves GET /v1/media/inbox?state=&limit=&offset= — the
// staff review queue, newest first, paginated (limit default 50, max 200).
func (s *server) mediaInboxList(w http.ResponseWriter, r *http.Request) {
	st := s.mediaInboxStoreOr503(w)
	if st == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	q := r.URL.Query()
	state := strings.TrimSpace(q.Get("state"))
	if state != "" && !validMediaInboxState(state) {
		writeError(w, http.StatusBadRequest, "state must be one of parked|anchored|linked|discarded")
		return
	}
	limit := 50
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "limit must be an integer in 1..200")
			return
		}
		limit = n
	}
	offset := 0
	if v := strings.TrimSpace(q.Get("offset")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		offset = n
	}
	items, err := st.ListMediaInbox(r.Context(), tenant.ID, state, limit, offset)
	if err != nil {
		s.d.Logger.Error("media inbox list failed", zap.Error(err),
			zap.String("tenant_id", tenant.ID.String()))
		writeError(w, http.StatusInternalServerError, "media inbox list failed")
		return
	}
	out := make([]mediaInboxItemJSON, 0, len(items))
	for _, it := range items {
		out = append(out, mediaInboxItemToJSON(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": out, "limit": limit, "offset": offset,
	})
}

// mediaInboxLinkRequest is the POST /v1/media/inbox/{id}/link body.
type mediaInboxLinkRequest struct {
	BookingID string `json:"booking_id"`
}

// mediaInboxLink serves POST /v1/media/inbox/{id}/link — the staff anchor
// override: validates the booking exists in the tenant (store-side), sets
// state='linked', anchor_method='staff', linked_by=caller sub and fills
// contact_id from the booking.
func (s *server) mediaInboxLink(w http.ResponseWriter, r *http.Request) {
	st := s.mediaInboxStoreOr503(w)
	if st == nil {
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(chi.URLParam(r, "id")))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid media inbox id")
		return
	}
	var req mediaInboxLinkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	bookingID, err := uuid.Parse(strings.TrimSpace(req.BookingID))
	if err != nil {
		writeError(w, http.StatusBadRequest, "booking_id must be a uuid")
		return
	}
	tenant := tenantFrom(r.Context())
	actor := userFrom(r.Context())
	item, err := st.LinkMediaInbox(r.Context(), tenant.ID, id, bookingID, actor)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "media inbox row or booking not found in tenant")
		return
	}
	if err != nil {
		s.d.Logger.Error("media inbox link failed", zap.Error(err),
			zap.String("tenant_id", tenant.ID.String()))
		writeError(w, http.StatusInternalServerError, "media inbox link failed")
		return
	}
	// Staff-action audit (P5 view-audit precedent): actor + row + booking.
	s.d.Logger.Info("media inbox link",
		zap.String("audit", "media_inbox_link"),
		zap.String("actor", actor),
		zap.String("media_inbox_id", id.String()),
		zap.String("booking_id", bookingID.String()),
		zap.String("tenant_id", tenant.ID.String()))
	writeJSON(w, http.StatusOK, mediaInboxItemToJSON(item))
}

// mediaInboxDiscard serves POST /v1/media/inbox/{id}/discard — the staff
// terminal reject: state='discarded', linked_by=caller sub.
func (s *server) mediaInboxDiscard(w http.ResponseWriter, r *http.Request) {
	st := s.mediaInboxStoreOr503(w)
	if st == nil {
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(chi.URLParam(r, "id")))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid media inbox id")
		return
	}
	tenant := tenantFrom(r.Context())
	actor := userFrom(r.Context())
	if err := st.DiscardMediaInbox(r.Context(), tenant.ID, id, actor); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "media inbox row not found")
			return
		}
		s.d.Logger.Error("media inbox discard failed", zap.Error(err),
			zap.String("tenant_id", tenant.ID.String()))
		writeError(w, http.StatusInternalServerError, "media inbox discard failed")
		return
	}
	s.d.Logger.Info("media inbox discard",
		zap.String("audit", "media_inbox_discard"),
		zap.String("actor", actor),
		zap.String("media_inbox_id", id.String()),
		zap.String("tenant_id", tenant.ID.String()))
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "state": "discarded"})
}
