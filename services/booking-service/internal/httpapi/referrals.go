package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/referrals"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// Referrals & commissions API (SPEC-W14 Agent A): referral CRUD + verify,
// commission rules CRUD (manage_bookings), ledger list + beneficiary balance
// (view_analytics). Payout endpoints are Agent B's (payouts.go).

func (s *server) referralsSvc(w http.ResponseWriter) *referrals.Service {
	if s.d.Referrals == nil {
		writeError(w, http.StatusServiceUnavailable, "referrals/commissions unavailable")
		return nil
	}
	return s.d.Referrals
}

// callerIdentity resolves the caller subject for the self-verify guard:
// the JWT sub resolved by the tenant middleware, else the X-User-Id header
// (SPEC-W44 W-B/S1-F7-06).
func callerIdentity(r *http.Request) string {
	if sub := userFrom(r.Context()); strings.TrimSpace(sub) != "" {
		return strings.TrimSpace(sub)
	}
	return strings.TrimSpace(r.Header.Get("X-User-Id"))
}

// mapReferralError converts referrals/store sentinel errors to HTTP statuses.
func (s *server) mapReferralError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, referrals.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, referrals.ErrInvalidTransition):
		writeError(w, http.StatusConflict, err.Error())
	default:
		s.internal(w, err)
	}
}

// ---------------------------------------------------------------------------
// Referrals
// ---------------------------------------------------------------------------

// createReferralRequest is the POST /v1/referrals body (contract §1).
type createReferralRequest struct {
	ReferrerType string     `json:"referrer_type"` // contact | agent | staff
	ReferrerID   string     `json:"referrer_id"`
	RefereePhone string     `json:"referee_phone"`
	CampaignID   *uuid.UUID `json:"campaign_id,omitempty"`
	BountyRuleID *uuid.UUID `json:"bounty_rule_id,omitempty"`
}

func (s *server) createReferral(w http.ResponseWriter, r *http.Request) {
	svc := s.referralsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	var req createReferralRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ref, created, err := svc.Create(r.Context(), referrals.CreateInput{
		TenantID:     tenant.ID,
		ReferrerType: req.ReferrerType,
		ReferrerID:   req.ReferrerID,
		RefereePhone: req.RefereePhone,
		CampaignID:   req.CampaignID,
		BountyRuleID: req.BountyRuleID,
	})
	if err != nil {
		s.mapReferralError(w, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK // §1 dedupe hit: existing open referral returned
	}
	writeJSON(w, status, map[string]any{"referral": ref, "created": created})
}

// listReferrals handles GET /v1/referrals?status= (view_analytics).
func (s *server) listReferrals(w http.ResponseWriter, r *http.Request) {
	svc := s.referralsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	status := r.URL.Query().Get("status")
	if status != "" {
		switch status {
		case referrals.StatusPending, referrals.StatusVerified, referrals.StatusConverted,
			referrals.StatusPaid, referrals.StatusRejected:
		default:
			writeError(w, http.StatusBadRequest, "invalid status filter")
			return
		}
	}
	rows, err := svc.Store.ListReferrals(r.Context(), tenant.ID, status)
	if err != nil {
		s.internal(w, err)
		return
	}
	if rows == nil {
		rows = []store.Referral{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"referrals": rows})
}

func (s *server) getReferral(w http.ResponseWriter, r *http.Request) {
	svc := s.referralsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	ref, err := svc.Store.GetReferral(r.Context(), tenant.ID, id)
	if err != nil {
		s.mapReferralError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"referral": ref})
}

// verifyReferralRequest is the POST /v1/referrals/{id}/verify body.
type verifyReferralRequest struct {
	Trigger string `json:"trigger"` // signup_verified|first_booking|first_txn|sale
	// BaseAmountNGN is the revenue base of the verify (kobo, integer) that
	// percent/bps rules are computed against; 0 for signup_verified.
	BaseAmountNGN int64 `json:"base_amount_ngn"`
}

func (s *server) verifyReferral(w http.ResponseWriter, r *http.Request) {
	svc := s.referralsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req verifyReferralRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// SPEC-W44 W-B/S1-F7-06 self-verify guard: the referrer must not verify
	// their own referral (409 + audit Warn). The verifier is the caller
	// identity (JWT sub via the tenant middleware, X-User-Id header
	// fallback); when no caller identity is resolvable the guard is skipped
	// (unauthenticated callers can't be the referrer by construction).
	if verifier := callerIdentity(r); verifier != "" {
		ref, err := svc.Store.GetReferral(r.Context(), tenant.ID, id)
		if err != nil {
			s.mapReferralError(w, err)
			return
		}
		if strings.TrimSpace(ref.ReferrerID) != "" && ref.ReferrerID == verifier {
			s.d.Logger.Warn("referral self-verify rejected: referrer == verifier",
				zap.String("tenant_id", tenant.ID.String()),
				zap.String("referral_id", id.String()),
				zap.String("referrer_id", ref.ReferrerID))
			writeError(w, http.StatusConflict, "self-referral: the referrer cannot verify their own referral")
			return
		}
	}
	res, err := svc.Verify(r.Context(), tenant.ID, id, req.Trigger, req.BaseAmountNGN, tenant.Slug)
	if err != nil {
		s.mapReferralError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// rejectReferral handles POST /v1/referrals/{id}/reject — the
// audit-preserving "delete": rows are never hard-deleted.
func (s *server) rejectReferral(w http.ResponseWriter, r *http.Request) {
	svc := s.referralsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	ref, err := svc.Reject(r.Context(), tenant.ID, id)
	if err != nil {
		s.mapReferralError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"referral": ref})
}

// ---------------------------------------------------------------------------
// Commission rules (manage_bookings writes, view_analytics reads)
// ---------------------------------------------------------------------------

// commissionRuleRequest is the POST/PUT /v1/commissions/rules body (§2).
type commissionRuleRequest struct {
	Name        string `json:"name"`
	Trigger     string `json:"trigger"`
	Beneficiary string `json:"beneficiary"`
	AmountType  string `json:"amount_type"`
	AmountNGN   int64  `json:"amount_ngn"` // kobo (flat)
	Bps         int    `json:"bps"`        // basis points (percent)
	CapNGN      *int64 `json:"cap_ngn"`    // kobo, null = uncapped
	Active      *bool  `json:"active"`     // nil on create = true
	Priority    int    `json:"priority"`
}

func (req commissionRuleRequest) rule(tenantID uuid.UUID) referrals.CommissionRule {
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	return referrals.CommissionRule{
		TenantID:    tenantID,
		Name:        strings.TrimSpace(req.Name),
		Trigger:     req.Trigger,
		Beneficiary: req.Beneficiary,
		AmountType:  req.AmountType,
		AmountNGN:   req.AmountNGN,
		Bps:         req.Bps,
		CapNGN:      req.CapNGN,
		Active:      active,
		Priority:    req.Priority,
	}
}

func (s *server) createCommissionRule(w http.ResponseWriter, r *http.Request) {
	svc := s.referralsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	var req commissionRuleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	rule := req.rule(tenant.ID)
	if err := svc.CreateRule(r.Context(), &rule); err != nil {
		s.mapReferralError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"rule": rule})
}

func (s *server) listCommissionRules(w http.ResponseWriter, r *http.Request) {
	svc := s.referralsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	rows, err := svc.Store.ListRules(r.Context(), tenant.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if rows == nil {
		rows = []store.CommissionRule{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rows})
}

func (s *server) updateCommissionRule(w http.ResponseWriter, r *http.Request) {
	svc := s.referralsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	var req commissionRuleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	rule := req.rule(tenant.ID)
	rule.ID = id
	if err := svc.UpdateRule(r.Context(), &rule); err != nil {
		s.mapReferralError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rule": rule})
}

func (s *server) deleteCommissionRule(w http.ResponseWriter, r *http.Request) {
	svc := s.referralsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	id, ok := urlUUID(w, r, "id")
	if !ok {
		return
	}
	if err := svc.Store.DeleteRule(r.Context(), tenant.ID, id); err != nil {
		s.mapReferralError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// ---------------------------------------------------------------------------
// Commission ledger reads (view_analytics)
// ---------------------------------------------------------------------------

// listCommissionLedger handles GET /v1/commissions/ledger?from&to.
func (s *server) listCommissionLedger(w http.ResponseWriter, r *http.Request) {
	svc := s.referralsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	q := r.URL.Query()
	from, ok := parseTimeBound(w, q.Get("from"))
	if !ok {
		return
	}
	to, ok := parseTimeBound(w, q.Get("to"))
	if !ok {
		return
	}
	rows, err := svc.LedgerEntries(r.Context(), tenant.ID, from, to)
	if err != nil {
		s.internal(w, err)
		return
	}
	if rows == nil {
		rows = []store.LedgerEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows})
}

// commissionBalance handles GET /v1/commissions/balance/{beneficiary}: the
// beneficiary's payable balance in kobo (account 300 credits − debits).
func (s *server) commissionBalance(w http.ResponseWriter, r *http.Request) {
	svc := s.referralsSvc(w)
	if svc == nil {
		return
	}
	tenant := tenantFrom(r.Context())
	beneficiary := chi.URLParam(r, "beneficiary")
	bal, err := svc.CommissionBalance(r.Context(), tenant.ID, beneficiary)
	if err != nil {
		s.mapReferralError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"beneficiary_id": beneficiary,
		"account_code":   referrals.AccountCommissionPayable,
		"balance_ngn":    bal, // kobo (credits − debits)
	})
}
