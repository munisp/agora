package httpapi

// Commission payouts API (SPEC-W14 Agent B): the tenant payout queue read
// by Agent C's admin payouts page. Handler style mirrors referrals.go
// (Agent A); the store is Agent B's referrals.PayoutStore.

import (
	"net/http"
	"strconv"

	"github.com/opendesk/booking-service/internal/referrals"
)

// listPayouts handles GET /v1/payouts?status=&limit= (view_analytics).
// status filters queued|processing|paid|failed; limit defaults to 100
// (max 500, enforced by the store).
func (s *server) listPayouts(w http.ResponseWriter, r *http.Request) {
	if s.d.Payouts == nil {
		writeError(w, http.StatusServiceUnavailable, "commission payouts unavailable")
		return
	}
	tenant := tenantFrom(r.Context())
	status := r.URL.Query().Get("status")
	if status != "" {
		switch status {
		case referrals.PayoutStatusQueued, referrals.PayoutStatusProcessing,
			referrals.PayoutStatusPaid, referrals.PayoutStatusFailed:
		default:
			writeError(w, http.StatusBadRequest, "invalid status filter")
			return
		}
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	rows, err := s.d.Payouts.ListPayouts(r.Context(), tenant.ID, status, limit)
	if err != nil {
		s.internal(w, err)
		return
	}
	if rows == nil {
		rows = []referrals.Payout{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"payouts": rows})
}
