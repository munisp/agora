package loyalty

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
)

// Handler tests boot the same embedded-Postgres harness as the store tests
// and exercise the real HTTP request/response cycle through RegisterRoutes
// (the integrator's wiring surface) with a stub TenantFromContext.

func testRouter(t *testing.T, tenant bookingops.TenantInfo) http.Handler {
	t.Helper()
	d := &Deps{
		Store: newTestStore(t),
		TenantFromContext: func(ctx context.Context) bookingops.TenantInfo {
			return tenant
		},
		Require: func(perm string) func(http.Handler) http.Handler {
			return func(n http.Handler) http.Handler { return n }
		},
		EventsTopic: "opendesk.loyalty.events.v1",
		UsageTopic:  "opendesk.usage.events",
	}
	r := chi.NewRouter()
	RegisterRoutes(r, d)
	return r
}

func do(t *testing.T, r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

// End-to-end through RegisterRoutes: program create → accrue (idempotent)
// → wallet view with ledger entries → redeem (409 then 200) → leaderboard.
func TestLoyaltyEndpoints(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r := testRouter(t, tenant)
	contactID := uuid.New()

	// Create program (201).
	rec := do(t, r, http.MethodPost, "/loyalty/programs",
		`{"name":"Club","earn_rules":[{"event":"booking_completed","points":50},{"event":"first_txn","points":100}],"tiers":[{"name":"silver","min_points":100}],"cap_per_day":0}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create program = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	var created struct {
		Program Program `json:"program"`
	}
	decode(t, rec, &created)
	progID := created.Program.ID

	// Invalid program → 400.
	if rec := do(t, r, http.MethodPost, "/loyalty/programs",
		`{"name":"Bad","earn_rules":[{"event":"nonsense","points":5}]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad program = %d (%s), want 400", rec.Code, rec.Body.String())
	}

	// List programs.
	rec = do(t, r, http.MethodGet, "/loyalty/programs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list programs = %d", rec.Code)
	}
	var listed struct {
		Programs []Program `json:"programs"`
	}
	decode(t, rec, &listed)
	if len(listed.Programs) != 1 || listed.Programs[0].ID != progID {
		t.Fatalf("programs: %+v", listed)
	}

	// Patch cap.
	rec = do(t, r, http.MethodPatch, "/loyalty/programs/"+progID.String(), `{"cap_per_day":500}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d (%s)", rec.Code, rec.Body.String())
	}

	// Accrue (200, applied) → replay (applied=false).
	body := `{"contact_id":"` + contactID.String() + `","event":"first_txn","ref_id":"tx-9"}`
	rec = do(t, r, http.MethodPost, "/loyalty/accrue", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("accrue = %d (%s)", rec.Code, rec.Body.String())
	}
	var ac struct {
		Wallet  Wallet `json:"wallet"`
		Awarded int64  `json:"awarded"`
		Applied bool   `json:"applied"`
		Capped  bool   `json:"capped"`
	}
	decode(t, rec, &ac)
	if !ac.Applied || ac.Awarded != 100 || ac.Wallet.Balance != 100 || ac.Wallet.Tier != "silver" {
		t.Fatalf("accrue body: %+v", ac)
	}
	rec = do(t, r, http.MethodPost, "/loyalty/accrue", body)
	decode(t, rec, &ac)
	if rec.Code != http.StatusOK || ac.Applied || ac.Awarded != 0 || ac.Wallet.Balance != 100 {
		t.Fatalf("accrue replay = %d %+v", rec.Code, ac)
	}

	// Wallet view: wallet + ledger entries + ledger balance cross-check.
	rec = do(t, r, http.MethodGet, "/loyalty/wallets/"+contactID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("wallet = %d (%s)", rec.Code, rec.Body.String())
	}
	var wv struct {
		Wallet        Wallet        `json:"wallet"`
		Entries       []LedgerEntry `json:"entries"`
		LedgerBalance int64         `json:"ledger_balance"`
	}
	decode(t, rec, &wv)
	if wv.Wallet.Balance != 100 || wv.LedgerBalance != 100 || len(wv.Entries) != 1 {
		t.Fatalf("wallet view: %+v", wv)
	}

	// Redeem over balance → 409 with balance.
	rec = do(t, r, http.MethodPost, "/loyalty/redeem",
		`{"contact_id":"`+contactID.String()+`","points":150,"reason":"voucher","ref_id":"rd-1"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("redeem = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	var conflict struct {
		Error   string `json:"error"`
		Balance int64  `json:"balance"`
	}
	decode(t, rec, &conflict)
	if conflict.Error != "insufficient_points" || conflict.Balance != 100 {
		t.Fatalf("409 body: %+v", conflict)
	}

	// Redeem within balance → 200; replay on ref_id → applied=false.
	body = `{"contact_id":"` + contactID.String() + `","points":30,"reason":"voucher","ref_id":"rd-2"}`
	rec = do(t, r, http.MethodPost, "/loyalty/redeem", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("redeem = %d (%s)", rec.Code, rec.Body.String())
	}
	var rd struct {
		Wallet   Wallet `json:"wallet"`
		Redeemed int64  `json:"redeemed"`
		Applied  bool   `json:"applied"`
		RefID    string `json:"ref_id"`
	}
	decode(t, rec, &rd)
	if !rd.Applied || rd.Redeemed != 30 || rd.Wallet.Balance != 70 || rd.RefID != "rd-2" {
		t.Fatalf("redeem body: %+v", rd)
	}
	rec = do(t, r, http.MethodPost, "/loyalty/redeem", body)
	decode(t, rec, &rd)
	if rd.Applied || rd.Wallet.Balance != 70 {
		t.Fatalf("redeem replay: %+v", rd)
	}

	// Leaderboard.
	rec = do(t, r, http.MethodGet, "/loyalty/leaderboard?metric=lifetime_earned&limit=5", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("leaderboard = %d", rec.Code)
	}
	var lb struct {
		Entries []LeaderboardEntry `json:"entries"`
	}
	decode(t, rec, &lb)
	if len(lb.Entries) != 1 || lb.Entries[0].ContactID != contactID || lb.Entries[0].LifetimeEarned != 100 {
		t.Fatalf("leaderboard: %+v", lb)
	}

	// Unknown wallet → 404; unknown event → 400.
	if rec := do(t, r, http.MethodGet, "/loyalty/wallets/"+uuid.NewString(), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown wallet = %d, want 404", rec.Code)
	}
	if rec := do(t, r, http.MethodPost, "/loyalty/accrue",
		`{"contact_id":"`+contactID.String()+`","event":"nonsense","ref_id":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown event = %d (%s), want 400", rec.Code, rec.Body.String())
	}

	// Lifecycle + metering emission is asserted against the real outbox in
	// TestEmission below.
}

// Emission: lifecycle CloudEvents (opendesk.loyalty.events.v1) + the
// points_redeemed usage record land in the outbox with the contract
// shapes; replays do NOT re-emit.
func TestEmission(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	st := newTestStore(t)
	svc := &Service{
		Store:       st,
		Ledger:      NewPostgresLedger(st),
		EventsTopic: "opendesk.loyalty.events.v1",
		UsageTopic:  "opendesk.usage.events",
	}
	ctx := context.Background()
	contactID := uuid.New()
	prog := mkProgram(tenant.ID)
	if err := st.CreateProgram(ctx, &prog); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.Accrue(ctx, tenant.ID, contactID, EventFirstTxn, "em-1"); err != nil {
		t.Fatalf("accrue: %v", err)
	}
	if _, err := svc.Accrue(ctx, tenant.ID, contactID, EventFirstTxn, "em-1"); err != nil { // replay
		t.Fatalf("replay: %v", err)
	}
	if _, err := svc.Redeem(ctx, tenant.ID, contactID, 10, "voucher", "em-r1"); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if _, err := svc.Redeem(ctx, tenant.ID, contactID, 10, "voucher", "em-r1"); err != nil { // replay
		t.Fatalf("redeem replay: %v", err)
	}

	rows, err := st.pool.Query(ctx, `SELECT topic, payload FROM outbox ORDER BY topic`)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	type row struct {
		topic   string
		payload map[string]any
	}
	var got []row
	for rows.Next() {
		var topic string
		var payload []byte
		if err := rows.Scan(&topic, &payload); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(payload, &m); err != nil {
			t.Fatalf("payload JSON: %v", err)
		}
		got = append(got, row{topic, m})
	}
	if len(got) != 3 { // 1 PointsIssued + 1 PointsRedeemed + 1 usage record
		t.Fatalf("outbox rows = %d, want 3 (replays must not re-emit): %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, g := range got {
		if g.payload["specversion"] != "1.0" || g.payload["source"] != "booking-service" {
			t.Fatalf("not a CloudEvent from booking-service: %+v", g.payload)
		}
		seen[g.topic+"|"+g.payload["type"].(string)] = true
	}
	for _, want := range []string{
		"opendesk.loyalty.events.v1|" + EventTypePointsIssued,
		"opendesk.loyalty.events.v1|" + EventTypePointsRedeemed,
		"opendesk.usage.events|com.opendesk.usage.UsageRecord",
	} {
		if !seen[want] {
			t.Fatalf("missing emission %s in %+v", want, seen)
		}
	}
}

// RegisterRoutes honours the nil-Require passthrough and the 503 posture
// when TenantFromContext is not wired.
func TestRegisterRoutesUnwired(t *testing.T) {
	d := &Deps{Store: newTestStore(t)}
	r := chi.NewRouter()
	RegisterRoutes(r, d)
	rec := do(t, r, http.MethodGet, "/loyalty/programs", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired tenant = %d, want 503", rec.Code)
	}
}
