// Package httpapi wires the chi router and REST handlers for kyc-service
// (SPEC-W12 §5): consent-gated BVN/NIN resolution with audit + events.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/opendesk/kyc-service/internal/events"
	"github.com/opendesk/kyc-service/internal/store"
	"go.uber.org/zap"
)

// ResolvedEventType is the CloudEvent type for resolution outcomes
// (SPEC-W12 §5: com.opendesk.kyc.Resolved on opendesk.kyc.resolved.v1).
const ResolvedEventType = "com.opendesk.kyc.Resolved"

// EventPublisher publishes a CloudEvent via Dapr pubsub (daprc.Client
// satisfies it; tests substitute a fake).
type EventPublisher interface {
	PublishEvent(ctx context.Context, pubsub, topic string, data any) error
}

// Deps bundles server dependencies.
type Deps struct {
	Store       store.Repository
	Consent     ConsentChecker
	Resolver    Resolver
	Events      EventPublisher
	PubSub      string
	EventsTopic string
	Logger      *zap.Logger
}

// NewRouter builds the chi router with all routes (booking-service shape).
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	s := &server{d: d}

	r.Get("/healthz", s.healthz)
	r.Route("/v1/kyc", func(r chi.Router) {
		r.Post("/resolve", s.resolve)
	})
	return r
}

type server struct{ d Deps }

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.d.Store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "db unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type resolveRequest struct {
	TenantID     string `json:"tenant_id"` // uuid or slug (identity resolves both)
	SubjectPhone string `json:"subject_phone"`
	IDType       string `json:"id_type"`  // bvn|nin
	IDValue      string `json:"id_value"` // raw BVN/NIN — never stored, only hashed
}

type resolveResponse struct {
	Status    string `json:"status"` // verified|mismatch|pending
	Reference string `json:"reference"`
	LatencyMS int64  `json:"latency_ms"`
}

// resolve (POST /v1/kyc/resolve) is the consent-gated KYC resolution
// endpoint (SPEC-W12 §5):
//  1. validate the request;
//  2. consent gate — identity GET /internal/consents/check?purpose=kyc;
//     no consent → 403; gate unreachable → 502;
//  3. resolve via the mock (deterministic) or live provider;
//  4. write exactly one kyc_audit row (who/what/when/result — raw id_value
//     is hashed, never stored);
//  5. publish com.opendesk.kyc.Resolved (best-effort outbox, identity-service
//     pattern; the audit row is the durable record for reconciliation).
func (s *server) resolve(w http.ResponseWriter, r *http.Request) {
	var req resolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.TenantID = strings.TrimSpace(req.TenantID)
	req.SubjectPhone = strings.TrimSpace(req.SubjectPhone)
	req.IDType = strings.ToLower(strings.TrimSpace(req.IDType))
	req.IDValue = strings.TrimSpace(req.IDValue)
	if req.TenantID == "" || req.SubjectPhone == "" || req.IDValue == "" {
		writeError(w, http.StatusBadRequest, "tenant_id, subject_phone and id_value are required")
		return
	}
	if !validIDTypes[req.IDType] {
		writeError(w, http.StatusBadRequest, "id_type must be bvn|nin")
		return
	}

	// Consent gate (contract §5: no consent → 403).
	tenantID, err := s.d.Consent.CheckConsent(r.Context(), req.TenantID, req.SubjectPhone, "kyc")
	if errors.Is(err, ErrConsentDenied) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":  "consent_required",
			"detail": "no active consent for subject_phone with purpose kyc",
		})
		return
	}
	if err != nil {
		s.d.Logger.Error("consent gate failure", zap.Error(err))
		writeError(w, http.StatusBadGateway, "consent gate unavailable")
		return
	}

	start := time.Now()
	status, resolveErr := s.d.Resolver.Resolve(r.Context(), req.IDType, req.IDValue)
	latencyMS := time.Since(start).Milliseconds()
	if resolveErr != nil {
		// Resolver contract: errors come paired with StatusPending; log for
		// the operator, answer per the response contract.
		s.d.Logger.Warn("kyc resolution degraded to pending", zap.Error(resolveErr))
		if status == "" {
			status = StatusPending
		}
	}
	idHash := hashIDValue(req.IDValue)
	reference := referenceFor(tenantID, req.SubjectPhone, req.IDType, idHash)

	// Audit (who/what/when/result). The actor header is optional metadata —
	// no PII beyond the subject phone is stored.
	actor := strings.TrimSpace(r.Header.Get("X-Actor"))
	audit := store.Audit{
		TenantID:     tenantID,
		Actor:        actor,
		SubjectPhone: req.SubjectPhone,
		IDType:       req.IDType,
		IDValueHash:  idHash,
		Status:       status,
		Reference:    reference,
		LatencyMS:    latencyMS,
	}
	if err := s.d.Store.InsertAudit(r.Context(), &audit); err != nil {
		s.internal(w, err)
		return
	}

	evt := events.New("kyc-service", ResolvedEventType, reference, tenantID.String(), map[string]any{
		"tenant_id":     tenantID.String(),
		"subject_phone": req.SubjectPhone,
		"id_type":       req.IDType,
		"id_value_hash": idHash,
		"status":        status,
		"reference":     reference,
		"latency_ms":    latencyMS,
	})
	if err := s.d.Events.PublishEvent(r.Context(), s.d.PubSub, s.d.EventsTopic, evt); err != nil {
		// Best-effort outbox: the kyc_audit row is the durable record; a
		// reconciler can republish from it.
		s.d.Logger.Error("failed to publish kyc Resolved", zap.String("reference", reference), zap.Error(err))
	}

	writeJSON(w, http.StatusOK, resolveResponse{
		Status:    status,
		Reference: reference,
		LatencyMS: latencyMS,
	})
}

func (s *server) internal(w http.ResponseWriter, err error) {
	s.d.Logger.Error("internal error", zap.Error(err))
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
