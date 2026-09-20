// Package httpapi wires the chi router and REST handlers for kyc-service
// (SPEC-W12 §5): consent-gated BVN/NIN resolution with audit + events.
package httpapi

import (
	"context"
	"crypto/subtle"
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
	// InternalToken (KYC_INTERNAL_TOKEN, SPEC-W45 K22): X-Internal-Token
	// gate on /v1/kyc/* — 503 fail-closed when unset, 401 missing/wrong
	// (identity-service K2 pattern).
	InternalToken string
	// HashSecret (KYC_HASH_SECRET, SPEC-W45 K22): HMAC-SHA256 key for
	// id_value_hash. Config fails closed when unset outside dev, so this
	// is never empty in a wired deployment.
	HashSecret string
	Logger     *zap.Logger
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
		r.Use(s.internauth)
		r.Post("/resolve", s.resolve)
	})
	return r
}

// internauth gates /v1/kyc/* (SPEC-W45 K22, OOS-07): X-Internal-Token must
// match KYC_INTERNAL_TOKEN via constant-time compare. Fail-closed: 503 when
// the env token is unset, 401 on missing/wrong — the identity-service K2
// pattern (identity-service/internal/httpapi/auth.go internauth).
func (s *server) internauth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.d.InternalToken == "" {
			s.d.Logger.Error("KYC_INTERNAL_TOKEN unset — refusing request (fail-closed, K22)",
				zap.String("path", r.URL.Path))
			writeError(w, http.StatusServiceUnavailable, "internal token not configured")
			return
		}
		got := r.Header.Get("X-Internal-Token")
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.d.InternalToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid internal token")
			return
		}
		next.ServeHTTP(w, r)
	})
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
// endpoint (SPEC-W12 §5), gated by X-Internal-Token (SPEC-W45 K22):
//  1. validate the request;
//  2. consent gate — identity GET /internal/consents/check?purpose=kyc;
//     no consent → 403; gate unreachable → 502;
//  3. consent↔ID binding (SPEC-W45 K22) — the ID must be unbound or bound
//     to this subject_phone, else 403;
//  4. resolve via the mock (deterministic) or live provider;
//  5. write exactly one kyc_audit row (who/what/when/result — raw id_value
//     is HMAC-hashed, never stored);
//  6. publish com.opendesk.kyc.Resolved (best-effort outbox, identity-service
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

	// SPEC-W45 K22: the id_value hash is computed once, up front — the
	// consent check, the binding rule, the audit row and the event all use
	// the SAME keyed digest (HMAC-SHA256 with KYC_HASH_SECRET).
	idHash := hashIDValue(s.d.HashSecret, req.IDValue)

	// Consent gate (contract §5: no consent → 403). SPEC-W45 K22 binding
	// rule, part 1: the consent record must match the PHONE — the check
	// runs on (tenant, subject_phone, purpose=kyc) and the id_type/id_hash
	// being resolved are forwarded so the gate can enforce ID-level consent
	// scoping; the request MUST carry subject_phone (validated above) and
	// that phone — not any other identifier — is the consented subject.
	tenantID, err := s.d.Consent.CheckConsent(r.Context(), req.TenantID, req.SubjectPhone, "kyc", req.IDType, idHash)
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

	// SPEC-W45 K22 binding rule, part 2 (OOS-17): an ID is bound to the
	// phone of its FIRST consented resolution in this tenant (recorded in
	// the append-only audit trail). Resolving the same (id_type, id_hash)
	// under a DIFFERENT subject_phone means the ID is not associated with
	// the consented phone → 403, before any provider call or audit write.
	owner, err := s.d.Store.IDHashOwner(r.Context(), tenantID, req.IDType, idHash)
	if err != nil {
		s.internal(w, err)
		return
	}
	if owner != "" && owner != req.SubjectPhone {
		s.d.Logger.Warn("kyc id binding violation",
			zap.String("tenant_id", tenantID.String()), zap.String("id_type", req.IDType))
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":  "id_binding_violation",
			"detail": "this id is not associated with the consented subject_phone",
		})
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
