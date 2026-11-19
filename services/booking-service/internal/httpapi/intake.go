package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"github.com/opendesk/booking-service/internal/store"
)

// SPEC-W95 C5-B: public intake completion endpoints. The link in the
// reminder (worker SendIntakeReminder) carries a stateless capability
// token — hex HMAC-SHA256(INTAKE_TOKEN_SECRET, "intake:"+bookingUUID) —
// so completing intake needs no portal session and no token table. Every
// failure mode (intake disabled, malformed uuid, bad token, unknown
// booking) answers the SAME 404 so the endpoint cannot enumerate
// bookings. No booking-status change: the intake state is the workflow's
// queryable state plus the intake_submissions row written here.

const (
	intakeMaxFields    = 64
	intakeMaxKeyLen    = 64
	intakeMaxValueLen  = 2000
	intakeMaxBodyBytes = 32 << 10

	// eventIntakeCompleted is the CloudEvent type the signals bridge maps
	// to workflows.SignalIntakeCompleted for the pack-{bookingId} workflow.
	eventIntakeCompleted = "com.opendesk.booking.IntakeCompleted"
)

// intakeToken computes the capability token for a booking.
func intakeToken(secret string, bookingID uuid.UUID) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("intake:" + bookingID.String()))
	return hex.EncodeToString(mac.Sum(nil))
}

// resolveIntakeBooking applies the uniform-404 discipline shared by both
// intake handlers: any failure answers "intake not found" and ok=false.
func (s *server) resolveIntakeBooking(w http.ResponseWriter, r *http.Request, site store.Site, bookingRaw, token string) (uuid.UUID, bool) {
	notFound := func() {
		writeError(w, http.StatusNotFound, "intake not found")
	}
	if s.d.IntakeTokenSecret == "" {
		notFound()
		return uuid.Nil, false
	}
	bookingID, err := uuid.Parse(bookingRaw)
	if err != nil {
		notFound()
		return uuid.Nil, false
	}
	want := intakeToken(s.d.IntakeTokenSecret, bookingID)
	if !hmac.Equal([]byte(token), []byte(want)) {
		notFound()
		return uuid.Nil, false
	}
	if _, err := s.d.Store.GetBooking(r.Context(), site.TenantID, bookingID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound()
			return uuid.Nil, false
		}
		s.internal(w, err)
		return uuid.Nil, false
	}
	return bookingID, true
}

// intakeInfoPublic serves GET /public/sites/{slug}/intake?booking=<uuid>&token=<hex>.
// Answers the intake state for the contact's own booking behind their
// capability token: pending, or submitted with their own payload back.
func (s *server) intakeInfoPublic(w http.ResponseWriter, r *http.Request) {
	site, ok := s.resolveSite(w, r)
	if !ok {
		return
	}
	bookingID, ok := s.resolveIntakeBooking(w, r, site, r.URL.Query().Get("booking"), r.URL.Query().Get("token"))
	if !ok {
		return
	}
	sub, err := s.d.Store.GetIntakeSubmission(r.Context(), site.TenantID, bookingID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	var fields map[string]string
	if err := json.Unmarshal(sub.Payload, &fields); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "submitted",
		"submitted_at": sub.SubmittedAt,
		"fields":       fields,
	})
}

type intakeSubmitBody struct {
	Booking string            `json:"booking"`
	Token   string            `json:"token"`
	Fields  map[string]string `json:"fields"`
}

// intakeSubmitPublic serves POST /public/sites/{slug}/intake. Validates the
// capability token, caps the payload, writes the submission + IntakeCompleted
// outbox event in one transaction (SubmitIntakeTx). Resubmission is
// idempotent-by-replace (no per-token limiter — documented SPEC-W95
// non-goal).
func (s *server) intakeSubmitPublic(w http.ResponseWriter, r *http.Request) {
	site, ok := s.resolveSite(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, intakeMaxBodyBytes)
	var req intakeSubmitBody
	if !decodeJSON(w, r, &req) {
		return
	}
	bookingID, ok := s.resolveIntakeBooking(w, r, site, req.Booking, req.Token)
	if !ok {
		return
	}
	if len(req.Fields) == 0 {
		writeError(w, http.StatusBadRequest, "fields is required")
		return
	}
	if len(req.Fields) > intakeMaxFields {
		writeError(w, http.StatusBadRequest, "too many fields")
		return
	}
	for k, v := range req.Fields {
		if len(k) == 0 || len(k) > intakeMaxKeyLen {
			writeError(w, http.StatusBadRequest, "field keys must be 1-64 characters")
			return
		}
		if len(v) > intakeMaxValueLen {
			writeError(w, http.StatusBadRequest, "field values must be at most 2000 characters")
			return
		}
	}
	payload, err := json.Marshal(req.Fields)
	if err != nil {
		s.internal(w, err)
		return
	}
	evt := events.New("booking-service", eventIntakeCompleted, site.TenantSlug, site.TenantID.String(), map[string]any{
		"booking_id": bookingID.String(),
	})
	evtPayload, err := json.Marshal(evt)
	if err != nil {
		s.internal(w, err)
		return
	}
	resub, err := s.d.Store.SubmitIntakeTx(r.Context(), site.TenantID, bookingID, payload, s.d.Ops.EventsTopic, evtPayload)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "submitted",
		"resubmitted": resub,
	})
}
