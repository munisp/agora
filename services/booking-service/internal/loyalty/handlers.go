package loyalty

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"go.uber.org/zap"
)

// Loyalty HTTP API (SPEC-W19 Agent C). Routes are SELF-REGISTERED by
// RegisterRoutes (W19 anti-collision contract — the integrator mounts them
// under /v1 and wires Deps; NO server.go/main.go edits by the builder):
//
//   - GET   /v1/loyalty/programs                 list programs     (view_analytics)
//   - POST  /v1/loyalty/programs                 create program    (manage_bookings)
//   - PATCH /v1/loyalty/programs/{id}            patch program     (manage_bookings)
//   - GET   /v1/loyalty/wallets/{contact_id}     wallet + ledger   (view_analytics)
//   - POST  /v1/loyalty/accrue                   earn-rule accrual (manage_bookings)
//   - POST  /v1/loyalty/redeem                   redemption        (manage_bookings)
//   - GET   /v1/loyalty/leaderboard              wallet ranking    (view_analytics)
type Handlers struct {
	Svc *Service
	// TenantFromContext extracts the tenant injected by httpapi's tenant
	// middleware (integrator wires httpapi's tenantFrom — the ctx key is
	// private to httpapi). Nil → 503 loyalty unavailable.
	TenantFromContext func(ctx context.Context) bookingops.TenantInfo
	Log               *zap.Logger
}

func (h *Handlers) log() *zap.Logger {
	if h.Log != nil {
		return h.Log
	}
	return zap.NewNop()
}

// Deps is the integrator wiring surface (W19 anti-collision contract).
type Deps struct {
	Store *Store
	Log   *zap.Logger
	// TenantFromContext — httpapi's tenantFrom (required; nil → 503).
	TenantFromContext func(ctx context.Context) bookingops.TenantInfo
	// Require — httpapi's permission middleware factory (s.require). Nil
	// registers routes WITHOUT permission gating (dev only).
	Require func(perm string) func(http.Handler) http.Handler
	// EventsTopic — LOYALTY_EVENTS_TOPIC (default
	// "opendesk.loyalty.events.v1"; empty disables lifecycle events).
	EventsTopic string
	// UsageTopic — USAGE_EVENTS_TOPIC (default "opendesk.usage.events";
	// empty disables points_redeemed metering).
	UsageTopic string
}

// RegisterRoutes self-registers the loyalty route group (W19 contract).
// The variadic middlewares apply to the WHOLE group — the integrator
// passes the appgate entitlement gate for app_id "loyalty-wallet" there.
func RegisterRoutes(r chi.Router, d *Deps, mw ...func(http.Handler) http.Handler) {
	topic := d.EventsTopic
	if topic == "" {
		topic = "opendesk.loyalty.events.v1"
	}
	svc := &Service{
		Store:       d.Store,
		Ledger:      NewPostgresLedger(d.Store),
		EventsTopic: topic,
		UsageTopic:  d.UsageTopic,
		Log:         d.Log,
	}
	h := &Handlers{Svc: svc, TenantFromContext: d.TenantFromContext, Log: d.Log}
	require := func(perm string) func(http.Handler) http.Handler {
		if d.Require == nil {
			return func(next http.Handler) http.Handler { return next }
		}
		return d.Require(perm)
	}
	r.Route("/loyalty", func(r chi.Router) {
		r.Use(mw...)
		r.With(require("view_analytics")).Get("/programs", h.ListPrograms)
		r.With(require("manage_bookings")).Post("/programs", h.CreateProgram)
		r.With(require("manage_bookings")).Patch("/programs/{id}", h.UpdateProgram)
		r.With(require("view_analytics")).Get("/wallets/{contact_id}", h.GetWallet)
		r.With(require("manage_bookings")).Post("/accrue", h.Accrue)
		r.With(require("manage_bookings")).Post("/redeem", h.Redeem)
		r.With(require("view_analytics")).Get("/leaderboard", h.Leaderboard)
	})
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
	var insuff *InsufficientError
	switch {
	case errors.As(err, &insuff):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "insufficient_points",
			"balance": insuff.Balance,
		})
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrNoActiveProgram):
		writeError(w, http.StatusBadRequest, "no active loyalty program — create and activate one first")
	case errors.Is(err, ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		h.log().Error("loyalty handler error", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// tenant resolves the request tenant or fails the request (503 when the
// integrator has not wired TenantFromContext; 400 when the middleware
// injected no tenant — same posture as httpapi's adapters).
func (h *Handlers) tenant(w http.ResponseWriter, r *http.Request) (bookingops.TenantInfo, bool) {
	if h.TenantFromContext == nil {
		writeError(w, http.StatusServiceUnavailable, "loyalty unavailable")
		return bookingops.TenantInfo{}, false
	}
	t := h.TenantFromContext(r.Context())
	if t.ID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "tenant context required")
		return bookingops.TenantInfo{}, false
	}
	return t, true
}

// ListPrograms (GET /v1/loyalty/programs).
func (h *Handlers) ListPrograms(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenant(w, r)
	if !ok {
		return
	}
	programs, err := h.Svc.Store.ListPrograms(r.Context(), t.ID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"programs": programs})
}

// programRequest is the POST /v1/loyalty/programs body.
type programRequest struct {
	Name      string     `json:"name"`
	Active    *bool      `json:"active,omitempty"`
	EarnRules []EarnRule `json:"earn_rules"`
	Tiers     []Tier     `json:"tiers"`
	CapPerDay int64      `json:"cap_per_day"`
}

// CreateProgram (POST /v1/loyalty/programs) → 201.
func (h *Handlers) CreateProgram(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenant(w, r)
	if !ok {
		return
	}
	var req programRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	p := Program{
		TenantID:  t.ID,
		Name:      req.Name,
		Active:    active,
		EarnRules: req.EarnRules,
		Tiers:     req.Tiers,
		CapPerDay: req.CapPerDay,
	}
	if err := ValidateProgram(&p); err != nil {
		h.mapErr(w, err)
		return
	}
	if err := h.Svc.Store.CreateProgram(r.Context(), &p); err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"program": p})
}

// UpdateProgram (PATCH /v1/loyalty/programs/{id}) — partial update
// validated against the merged row.
func (h *Handlers) UpdateProgram(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenant(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid program id")
		return
	}
	var patch ProgramPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	p, err := h.Svc.Store.UpdateProgram(r.Context(), t.ID, id, patch)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"program": p})
}

// GetWallet (GET /v1/loyalty/wallets/{contact_id}?from=&to=) answers the
// wallet plus its ledger entries (400-account rows for this contact) and a
// ledger-derived balance cross-check. 404 when the contact has no wallet
// yet.
func (h *Handlers) GetWallet(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenant(w, r)
	if !ok {
		return
	}
	contactID, err := uuid.Parse(chi.URLParam(r, "contact_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid contact_id")
		return
	}
	var from, to *time.Time
	if raw := r.URL.Query().Get("from"); raw != "" {
		if ts, err := time.Parse("2006-01-02", raw); err == nil {
			from = &ts
		}
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		if ts, err := time.Parse("2006-01-02", raw); err == nil {
			end := ts.Add(24*time.Hour - time.Nanosecond)
			to = &end
		}
	}
	wallet, err := h.Svc.Store.GetWallet(r.Context(), t.ID, contactID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	entries, err := h.Svc.Store.ListLedgerEntries(r.Context(), t.ID, from, to, contactID.String())
	if err != nil {
		h.mapErr(w, err)
		return
	}
	ledgerBalance, err := h.Svc.Ledger.Balance(r.Context(), t.ID, AccountPointsIssued, contactID.String())
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"wallet":         wallet,
		"entries":        entries,
		"ledger_balance": ledgerBalance,
	})
}

// accrueRequest is the POST /v1/loyalty/accrue body. ref_id is the
// caller's idempotency key (booking id, transaction id, referral id) —
// replays of ref_id+event are no-ops.
type accrueRequest struct {
	ContactID uuid.UUID `json:"contact_id"`
	Event     string    `json:"event"`
	RefID     string    `json:"ref_id"`
}

// Accrue (POST /v1/loyalty/accrue).
func (h *Handlers) Accrue(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenant(w, r)
	if !ok {
		return
	}
	var req accrueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	res, err := h.Svc.Accrue(r.Context(), t.ID, req.ContactID, req.Event, req.RefID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"wallet":  res.Wallet,
		"awarded": res.Awarded,
		"applied": res.Applied,
		"capped":  res.Capped,
		"event":   req.Event,
	})
}

// redeemRequest is the POST /v1/loyalty/redeem body. ref_id is OPTIONAL
// but recommended: it anchors idempotent retries (a fresh redemption id is
// minted when omitted, so retried submissions without ref_id redeem
// twice — documented in docs/apps/loyalty-wallet.md).
type redeemRequest struct {
	ContactID uuid.UUID `json:"contact_id"`
	Points    int64     `json:"points"`
	Reason    string    `json:"reason"`
	RefID     string    `json:"ref_id,omitempty"`
}

// Redeem (POST /v1/loyalty/redeem) — insufficient balance → 409
// {error: "insufficient_points", balance}.
func (h *Handlers) Redeem(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenant(w, r)
	if !ok {
		return
	}
	var req redeemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	res, err := h.Svc.Redeem(r.Context(), t.ID, req.ContactID, req.Points, req.Reason, req.RefID)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"wallet":   res.Wallet,
		"redeemed": res.Redeemed,
		"applied":  res.Applied,
		"ref_id":   res.RedeemRef,
	})
}

// Leaderboard (GET /v1/loyalty/leaderboard?metric=&limit=).
func (h *Handlers) Leaderboard(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenant(w, r)
	if !ok {
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	entries, err := h.Svc.Store.Leaderboard(r.Context(), t.ID,
		LeaderboardMetric(r.URL.Query().Get("metric")), limit)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}
