package lending

// SPEC-W20 contract §4: usage metering for the lending app. Mirrors the
// W14 referrals metering idiom (internal/referrals/metering.go): one
// CloudEvents usage record on the shared usage topic
// (USAGE_EVENTS_TOPIC, default opendesk.usage.events):
//
//	{type: com.opendesk.usage.UsageRecord,
//	 data: {tenant_id, metric, value: 1, ts, meta: {...}}}
//
// Metric ownership: loan_disbursed — THIS FILE, emitted once per
// non-idempotent disbursement (a replayed disburse can never
// double-meter). Value is ALWAYS 1 per event; the principal lives in meta
// so billing can price tiers without re-parsing ledgers.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// UsageMetricLoanDisbursed is the metered unit emitted once per loan
// disbursement (SPEC-W20 contract §4: lending bills on loan_disbursed).
const UsageMetricLoanDisbursed = "loan_disbursed"

// MarshalLoanDisbursedUsageRecord builds the usage-record payload for one
// disbursement.
func MarshalLoanDisbursedUsageRecord(tenantSlug string, res DisburseResult) ([]byte, error) {
	tenantID := res.Loan.TenantID
	evt := events.New("booking-service", "com.opendesk.usage.UsageRecord", tenantSlug, tenantID.String(), map[string]any{
		"tenant_id": tenantID.String(),
		"metric":    UsageMetricLoanDisbursed,
		"value":     1,
		"ts":        time.Now().UTC(),
		"meta": map[string]any{
			"loan_id":        res.Loan.ID.String(),
			"application_id": res.Application.ID.String(),
			"contact_id":     res.Loan.ContactID.String(),
			"principal_kobo": res.Loan.PrincipalKobo,
		},
	})
	return json.Marshal(evt)
}

// meterLoanDisbursed writes one loan_disbursed usage record to the outbox
// after a successful Disburse (best-effort, post-commit — metering must
// never block a disbursement, failures are logged for reconciliation).
// Called only on the NON-idempotent path.
func (h *Handlers) meterLoanDisbursed(ctx context.Context, tenantSlug string, res DisburseResult) {
	if h.UsageTopic == "" {
		return
	}
	payload, err := MarshalLoanDisbursedUsageRecord(tenantSlug, res)
	if err != nil {
		h.log().Warn("lending usage record marshal failed; skipping metering", zap.Error(err))
		return
	}
	if err := h.Store.EnqueueOutbox(ctx, res.Loan.ID, h.UsageTopic, payload); err != nil {
		h.log().Warn("lending usage record enqueue failed; skipping metering", zap.Error(err))
	}
}
