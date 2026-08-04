package lending

// CloudEvents lifecycle events + the disbursement INTENT for SPEC-W20
// Agent C. All ride the transactional outbox (Store.EnqueueOutbox) and are
// best-effort post-commit — the same posture as internal/referrals and
// W19: eventing must never block a decision/disbursement/repayment;
// failures are logged for reconciliation.
//
// Topic: LENDING_EVENTS_TOPIC (Deps.EventsTopic, default
// opendesk.lending.events.v1; empty disables emission — graceful no-op,
// SPEC-W20 contract §5).

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// Lifecycle event types published to opendesk.lending.events.v1 (SPEC-W20
// contract §5: application decided / disbursed / repaid).
const (
	// EventTypeApplicationDecided fires at the →approved / →declined
	// transitions. The KYC gate outcome rides in data.kyc
	// ({mode:"service",reference,status} or {mode:"override",reason}).
	EventTypeApplicationDecided = "com.opendesk.lending.ApplicationDecided"
	// EventTypeLoanDisbursed fires once per non-idempotent disbursement.
	EventTypeLoanDisbursed = "com.opendesk.lending.LoanDisbursed"
	// EventTypeLoanRepaid fires when a repayment zeroes the outstanding
	// balance (loan + application flip to repaid).
	EventTypeLoanRepaid = "com.opendesk.lending.LoanRepaid"
	// EventTypeDisbursementIntent is the INTEGRATION POINT for the
	// payments/TigerBeetle rail: real money movement is OUT of scope for
	// this package — the rail's consumer subscribes to this intent,
	// performs the actual payout and owns settlement/reconciliation.
	EventTypeDisbursementIntent = "com.opendesk.lending.DisbursementIntent"
)

// KYCDecision records how the approve-gate KYC check was satisfied; it is
// embedded in the ApplicationDecided event payload (SPEC-W20: the
// kyc_override + reason must be recorded in the event payload).
type KYCDecision struct {
	// Mode is "service" (kyc-service verified), "override" (explicit
	// operator override — only path when LENDING_KYC_URL is unset) or
	// "none" (declined applications carry no KYC decision).
	Mode      string `json:"mode"`
	Reference string `json:"reference,omitempty"` // kyc-service reference (service mode)
	Status    string `json:"status,omitempty"`    // kyc-service status (service mode)
	Reason    string `json:"reason,omitempty"`    // operator justification (override mode)
}

// MarshalDecidedEvent builds the envelope for one approve/decline
// decision. kyc may be nil for declines (data.kyc = {mode:"none"}).
func MarshalDecidedEvent(tenantSlug string, a Application, decision string, kyc *KYCDecision) ([]byte, error) {
	data := map[string]any{
		"tenant_id":      a.TenantID.String(),
		"application_id": a.ID.String(),
		"contact_id":     a.ContactID.String(),
		"product_id":     a.ProductID.String(),
		"principal_kobo": a.PrincipalKobo,
		"decision":       decision, // approved | declined
		"ts":             time.Now().UTC(),
	}
	if a.Score != nil {
		data["score"] = *a.Score
	}
	if a.DecidedBy != nil {
		data["decided_by"] = *a.DecidedBy
	}
	if a.DeclineReason != nil {
		data["decline_reason"] = *a.DeclineReason
	}
	if kyc != nil {
		data["kyc"] = kyc
	} else {
		data["kyc"] = KYCDecision{Mode: "none"}
	}
	return json.Marshal(events.New("booking-service", EventTypeApplicationDecided, tenantSlug, a.TenantID.String(), data))
}

// MarshalDisbursedEvent builds the envelope for one disbursement.
func MarshalDisbursedEvent(tenantSlug string, res DisburseResult) ([]byte, error) {
	return json.Marshal(events.New("booking-service", EventTypeLoanDisbursed, tenantSlug, res.Loan.TenantID.String(), map[string]any{
		"tenant_id":        res.Loan.TenantID.String(),
		"application_id":   res.Application.ID.String(),
		"loan_id":          res.Loan.ID.String(),
		"contact_id":       res.Loan.ContactID.String(),
		"principal_kobo":   res.Loan.PrincipalKobo,
		"interest_kobo":    res.Loan.InterestKobo,
		"fee_kobo":         res.Loan.FeeKobo,
		"outstanding_kobo": res.Loan.OutstandingKobo,
		"disbursed_at":     res.Loan.DisbursedAt,
		"due_at":           res.Loan.DueAt,
		"ts":               time.Now().UTC(),
	}))
}

// MarshalDisbursementIntent builds the INTENT envelope for the payments /
// TigerBeetle rail (see EventTypeDisbursementIntent). Published on the
// lending events topic; the rail consumer performs the real payout.
func MarshalDisbursementIntent(tenantSlug string, res DisburseResult) ([]byte, error) {
	return json.Marshal(events.New("booking-service", EventTypeDisbursementIntent, tenantSlug, res.Loan.TenantID.String(), map[string]any{
		"tenant_id":      res.Loan.TenantID.String(),
		"intent":         "loan_disbursement_payout",
		"application_id": res.Application.ID.String(),
		"loan_id":        res.Loan.ID.String(),
		"contact_id":     res.Loan.ContactID.String(),
		"amount_kobo":    res.Loan.PrincipalKobo,
		"currency":       "NGN",
		"ref_id":         res.Application.ID.String(), // rail-side idempotency anchor
		"ts":             time.Now().UTC(),
	}))
}

// MarshalRepaidEvent builds the envelope for one fully-repaid loan.
func MarshalRepaidEvent(tenantSlug string, res RepayResult) ([]byte, error) {
	return json.Marshal(events.New("booking-service", EventTypeLoanRepaid, tenantSlug, res.Loan.TenantID.String(), map[string]any{
		"tenant_id":      res.Loan.TenantID.String(),
		"loan_id":        res.Loan.ID.String(),
		"application_id": res.Loan.ApplicationID.String(),
		"contact_id":     res.Loan.ContactID.String(),
		"total_kobo":     res.Loan.TotalKobo(),
		"last_ref_id":    res.Repayment.RefID,
		"ts":             time.Now().UTC(),
	}))
}

// ---------------------------------------------------------------------------
// best-effort publishers (post-commit; never block the mutation)
// ---------------------------------------------------------------------------

func (h *Handlers) publish(ctx context.Context, aggregateID uuid.UUID, payload []byte, what string) {
	if h.EventsTopic == "" {
		return
	}
	if err := h.Store.EnqueueOutbox(ctx, aggregateID, h.EventsTopic, payload); err != nil {
		h.log().Warn("lending event enqueue failed; skipping emission",
			zap.String("event", what), zap.Error(err))
	}
}

// publishDecided emits the ApplicationDecided event.
func (h *Handlers) publishDecided(ctx context.Context, tenantSlug string, a Application, decision string, kyc *KYCDecision) {
	if h.EventsTopic == "" {
		return
	}
	payload, err := MarshalDecidedEvent(tenantSlug, a, decision, kyc)
	if err != nil {
		h.log().Warn("lending decided event marshal failed; skipping emission", zap.Error(err))
		return
	}
	h.publish(ctx, a.ID, payload, EventTypeApplicationDecided)
}

// publishDisbursed emits LoanDisbursed + the DisbursementIntent for the
// payments rail (non-idempotent disbursements only — a replay never
// re-intents money movement).
func (h *Handlers) publishDisbursed(ctx context.Context, tenantSlug string, res DisburseResult) {
	if h.EventsTopic == "" {
		return
	}
	payload, err := MarshalDisbursedEvent(tenantSlug, res)
	if err != nil {
		h.log().Warn("lending disbursed event marshal failed; skipping emission", zap.Error(err))
	} else {
		h.publish(ctx, res.Loan.ID, payload, EventTypeLoanDisbursed)
	}
	intent, err := MarshalDisbursementIntent(tenantSlug, res)
	if err != nil {
		h.log().Warn("lending disbursement intent marshal failed; skipping emission", zap.Error(err))
		return
	}
	h.publish(ctx, res.Loan.ID, intent, EventTypeDisbursementIntent)
}

// publishRepaid emits LoanRepaid (only on the outstanding==0 transition).
func (h *Handlers) publishRepaid(ctx context.Context, tenantSlug string, res RepayResult) {
	if h.EventsTopic == "" {
		return
	}
	payload, err := MarshalRepaidEvent(tenantSlug, res)
	if err != nil {
		h.log().Warn("lending repaid event marshal failed; skipping emission", zap.Error(err))
		return
	}
	h.publish(ctx, res.Loan.ID, payload, EventTypeLoanRepaid)
}
