package loyalty

// SPEC-W80 B10: RegisterRoutes honors the documented contract "empty
// EventsTopic disables emission" — the old default-override
// (handlers.go:63-66) silently re-enabled it. With EventsTopic "" an
// accrual writes NO lifecycle outbox row; with the topic set it does.

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
)

func TestB10EmptyEventsTopicDisablesEmission(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	st := newTestStore(t)
	r := chi.NewRouter()
	RegisterRoutes(r, &Deps{
		Store: st,
		TenantFromContext: func(ctx context.Context) bookingops.TenantInfo {
			return tenant
		},
		Require: func(perm string) func(http.Handler) http.Handler {
			return func(n http.Handler) http.Handler { return n }
		},
		EventsTopic: "", // B10: empty MUST disable lifecycle emission
		UsageTopic:  "",
	})
	ctx := context.Background()

	if rec := do(t, r, http.MethodPost, "/loyalty/programs",
		`{"name":"Club","earn_rules":[{"event":"first_txn","points":100}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("create program = %d (%s)", rec.Code, rec.Body.String())
	}
	contactID := uuid.New()
	rec := do(t, r, http.MethodPost, "/loyalty/accrue",
		`{"contact_id":"`+contactID.String()+`","event":"first_txn","ref_id":"b10-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("accrue = %d (%s), want 200", rec.Code, rec.Body.String())
	}

	// The accrual applied (points moved) but NOTHING was emitted.
	w, err := st.GetWallet(ctx, tenant.ID, contactID)
	if err != nil || w.Balance != 100 {
		t.Fatalf("wallet = %+v, %v — accrual must still apply with emission disabled", w, err)
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox`).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if n != 0 {
		t.Fatalf("outbox rows = %d, want 0 — empty EventsTopic must disable emission (B10)", n)
	}
}
