package lending

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
)

// Handler tests boot the same embedded-Postgres harness as the store tests
// and exercise the real HTTP request/response cycle through RegisterRoutes
// (the integrator-facing entry point) with a fake tenant resolver.

type fakeResolver map[string]bookingops.TenantInfo

func (f fakeResolver) BySlug(_ context.Context, slug string) (bookingops.TenantInfo, error) {
	t, ok := f[slug]
	if !ok {
		return bookingops.TenantInfo{}, fmt.Errorf("unknown tenant %q", slug)
	}
	return t, nil
}

func testRouter(t *testing.T, d *Deps) (http.Handler, *Store, bookingops.TenantInfo) {
	t.Helper()
	st := newTestStore(t)
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme", Timezone: "Africa/Lagos"}
	d.Store = st
	d.Resolver = fakeResolver{"acme": tenant}
	r := chi.NewRouter()
	RegisterRoutes(r, d)
	return r, st, tenant
}

func do(t *testing.T, r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Tenant-Slug", "acme")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func qstr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func createProduct(t *testing.T, r http.Handler) Product {
	t.Helper()
	rec := do(t, r, http.MethodPost, "/v1/lending/products",
		`{"name":"Trader Cash","principal_min_kobo":100000,"principal_max_kobo":5000000,"term_days":30,"interest_bps":1500,"fee_flat_kobo":50000}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create product = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Product Product `json:"product"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("product body: %v", err)
	}
	return resp.Product
}

func createApplication(t *testing.T, r http.Handler, productID, contactID uuid.UUID, principal int64, status string) Application {
	t.Helper()
	body := fmt.Sprintf(`{"contact_id":%q,"product_id":%q,"principal_kobo":%d,"status":%q}`,
		contactID.String(), productID.String(), principal, status)
	rec := do(t, r, http.MethodPost, "/v1/lending/applications", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create application = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Application Application `json:"application"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("application body: %v", err)
	}
	return resp.Application
}

func patchApp(t *testing.T, r http.Handler, id uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, r, http.MethodPatch, "/v1/lending/applications/"+id.String(), body)
}

// Full lifecycle through the API with the OVERRIDE KYC path (no KYC URL
// configured): product → application (submitted, scored) → under_review →
// approve (override) → disburse (idempotent) → repay (clamped, repaid) →
// portfolio — plus the outbox emissions (decided/disbursed/intent/repaid +
// one loan_disbursed usage record).
func TestLendingLifecycle(t *testing.T) {
	// W39 SIM-001: disbursement requires a rail — the tests exercise the
	// simulated-rail posture via the explicit ALLOW_MOCK_RAILS opt-in.
	t.Setenv(EnvAllowMockRails, "1")
	r, st, tenant := testRouter(t, &Deps{EventsTopic: "test.lending", UsageTopic: "test.usage"})
	contact := addContact(t, st, tenant.ID, "Ada")
	addBooking(t, st, tenant.ID, contact, "completed", mustTime(t))

	prod := createProduct(t, r)
	app := createApplication(t, r, prod.ID, contact, 2000000, "submitted")
	if app.Status != StatusSubmitted || app.Score == nil {
		t.Fatalf("submitted application not scored: %+v", app)
	}

	// Principal outside the band → 400.
	rec := do(t, r, http.MethodPost, "/v1/lending/applications",
		fmt.Sprintf(`{"contact_id":%q,"product_id":%q,"principal_kobo":50}`,
			contact.String(), prod.ID.String()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("below-band principal = %d, want 400", rec.Code)
	}

	// Illegal: submitted → approved directly.
	if rec := patchApp(t, r, app.ID, `{"status":"approved"}`); rec.Code != http.StatusConflict {
		t.Fatalf("submitted→approved = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	// Walk to under_review.
	if rec := patchApp(t, r, app.ID, `{"status":"under_review"}`); rec.Code != http.StatusOK {
		t.Fatalf("→under_review = %d (%s)", rec.Code, rec.Body.String())
	}
	// Approve WITHOUT the override (no KYC URL configured) → 409.
	if rec := patchApp(t, r, app.ID, `{"status":"approved"}`); rec.Code != http.StatusConflict {
		t.Fatalf("approve without override = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	// Approve with override but no reason → 409.
	if rec := patchApp(t, r, app.ID, `{"status":"approved","kyc_override":true}`); rec.Code != http.StatusConflict {
		t.Fatalf("approve without reason = %d, want 409", rec.Code)
	}
	// Approve with override + reason.
	rec = patchApp(t, r, app.ID, `{"status":"approved","kyc_override":true,"kyc_reason":"branch-verified ID card","decided_by":"ops-ada"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d (%s)", rec.Code, rec.Body.String())
	}
	var decided struct {
		Application Application `json:"application"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decided); err != nil {
		t.Fatalf("approve body: %v", err)
	}
	if decided.Application.Status != StatusApproved || decided.Application.DecidedAt == nil {
		t.Fatalf("approved application: %+v", decided.Application)
	}

	// Disburse.
	rec = do(t, r, http.MethodPost, "/v1/lending/applications/"+app.ID.String()+"/disburse", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disburse = %d (%s)", rec.Code, rec.Body.String())
	}
	var dis DisburseResult
	if err := json.Unmarshal(rec.Body.Bytes(), &dis); err != nil {
		t.Fatalf("disburse body: %v", err)
	}
	if dis.Replayed || dis.Loan.OutstandingKobo != 2350000 || dis.Application.Status != StatusDisbursed {
		t.Fatalf("disburse result: %+v", dis)
	}
	// Disburse replay → 200 same loan, replayed.
	rec = do(t, r, http.MethodPost, "/v1/lending/applications/"+app.ID.String()+"/disburse", "")
	var disRe DisburseResult
	if err := json.Unmarshal(rec.Body.Bytes(), &disRe); err != nil || !disRe.Replayed || disRe.Loan.ID != dis.Loan.ID {
		t.Fatalf("disburse replay = %d %+v (%v)", rec.Code, disRe, err)
	}

	// Loan view: schedule + empty repayments.
	rec = do(t, r, http.MethodGet, "/v1/lending/loans/"+dis.Loan.ID.String(), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"outstanding_kobo":2350000`) {
		t.Fatalf("loan view = %d %s", rec.Code, rec.Body.String())
	}

	// Repay missing ref_id → 400.
	rec = do(t, r, http.MethodPost, "/v1/lending/loans/"+dis.Loan.ID.String()+"/repay", `{"amount_kobo":1000}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("repay without ref_id = %d, want 400", rec.Code)
	}
	// Overpay → clamped + loan repaid.
	rec = do(t, r, http.MethodPost, "/v1/lending/loans/"+dis.Loan.ID.String()+"/repay",
		`{"amount_kobo":3000000,"ref_id":"opay-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("repay = %d (%s)", rec.Code, rec.Body.String())
	}
	var rep RepayResult
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("repay body: %v", err)
	}
	if !rep.Clamped || rep.Repayment.AmountKobo != 2350000 || !rep.LoanRepaid || rep.Loan.Status != LoanRepaid {
		t.Fatalf("clamped repay result: %+v", rep)
	}
	// Replay → 200 same body.
	rec = do(t, r, http.MethodPost, "/v1/lending/loans/"+dis.Loan.ID.String()+"/repay",
		`{"amount_kobo":3000000,"ref_id":"opay-1"}`)
	var repRe RepayResult
	if err := json.Unmarshal(rec.Body.Bytes(), &repRe); err != nil || !repRe.Replayed || repRe.Repayment.ID != rep.Repayment.ID {
		t.Fatalf("repay replay = %d %+v (%v)", rec.Code, repRe, err)
	}

	// Portfolio reflects the repaid book.
	rec = do(t, r, http.MethodGet, "/v1/lending/portfolio", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"repaid_count":1`) {
		t.Fatalf("portfolio = %d %s", rec.Code, rec.Body.String())
	}

	// Outbox emissions: decided + disbursed + intent + repaid on
	// test.lending, ONE loan_disbursed usage record on test.usage.
	rows, err := st.pool.Query(context.Background(),
		`SELECT topic, payload::text FROM outbox ORDER BY created_at`)
	if err != nil {
		t.Fatalf("outbox query: %v", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	typeEnv := map[string]string{}
	for rows.Next() {
		var tp, pl string
		if err := rows.Scan(&tp, &pl); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		counts[tp]++
		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(pl), &env); err == nil {
			typeEnv[env.Type] = pl
		}
	}
	if counts["test.lending"] != 4 || counts["test.usage"] != 1 {
		t.Fatalf("outbox topics = %v, want lending×4 usage×1", counts)
	}
	for _, want := range []string{
		EventTypeApplicationDecided, EventTypeLoanDisbursed,
		EventTypeDisbursementIntent, EventTypeLoanRepaid,
	} {
		if _, ok := typeEnv[want]; !ok {
			t.Fatalf("missing event type %q in outbox (have %v)", want, typeEnv)
		}
	}
	// The decided event records the kyc override + reason (SPEC-W20).
	var decidedEnv struct {
		Data struct {
			Decision string `json:"decision"`
			KYC      struct {
				Mode   string `json:"mode"`
				Reason string `json:"reason"`
			} `json:"kyc"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(typeEnv[EventTypeApplicationDecided]), &decidedEnv); err != nil {
		t.Fatalf("decided event: %v", err)
	}
	if decidedEnv.Data.Decision != "approved" || decidedEnv.Data.KYC.Mode != "override" ||
		decidedEnv.Data.KYC.Reason != "branch-verified ID card" {
		t.Fatalf("decided event kyc payload: %+v", decidedEnv.Data)
	}
	// The intent carries the principal + the rail idempotency anchor.
	var intentEnv struct {
		Data struct {
			AmountKobo int64  `json:"amount_kobo"`
			RefID      string `json:"ref_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(typeEnv[EventTypeDisbursementIntent]), &intentEnv); err != nil {
		t.Fatalf("intent event: %v", err)
	}
	if intentEnv.Data.AmountKobo != 2000000 || intentEnv.Data.RefID != app.ID.String() {
		t.Fatalf("intent payload: %+v", intentEnv.Data)
	}
	// The usage record carries the contracted metric.
	var usageEnv struct {
		Data struct {
			Metric string `json:"metric"`
			Value  int    `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(typeEnv["com.opendesk.usage.UsageRecord"]), &usageEnv); err != nil ||
		usageEnv.Data.Metric != UsageMetricLoanDisbursed || usageEnv.Data.Value != 1 {
		t.Fatalf("usage record: %v", err)
	}
}

// The SERVICE KYC path: LENDING_KYC_URL configured → the approve gate
// calls kyc-service (POST {url}/v1/kyc/resolve); only "verified" passes.
func TestKYCServiceGate(t *testing.T) {
	var gotBody map[string]string
	kycSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/kyc/resolve" || r.Method != http.MethodPost {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"verified","reference":"kyc-ref-1","latency_ms":3}`))
	}))
	defer kycSrv.Close()

	r, st, tenant := testRouter(t, &Deps{KYCURL: kycSrv.URL})
	contact := addContact(t, st, tenant.ID, "Ada")
	prod := createProduct(t, r)
	app := createApplication(t, r, prod.ID, contact, 1500000, "submitted")
	if rec := patchApp(t, r, app.ID, `{"status":"under_review"}`); rec.Code != http.StatusOK {
		t.Fatalf("→under_review = %d (%s)", rec.Code, rec.Body.String())
	}

	// Missing kyc block → 409.
	if rec := patchApp(t, r, app.ID, `{"status":"approved"}`); rec.Code != http.StatusConflict {
		t.Fatalf("approve without kyc block = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	// With the service configured the override path is closed (SPEC-W20:
	// kyc_override is the fallback for the UNCONFIGURED deployment) — the
	// operator passes the subject identifiers and the gate calls the
	// service.
	rec := patchApp(t, r, app.ID,
		`{"status":"approved","kyc":{"subject_phone":"+234801","id_type":"bvn","id_value":"12345678901"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve with kyc = %d (%s)", rec.Code, rec.Body.String())
	}
	if gotBody["tenant_id"] != "acme" || gotBody["subject_phone"] != "+234801" ||
		gotBody["id_type"] != "bvn" || gotBody["id_value"] != "12345678901" {
		t.Fatalf("kyc-service request body: %+v", gotBody)
	}

	// Non-verified status → 409.
	kycSrv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"mismatch","reference":"kyc-ref-2"}`))
	}))
	defer kycSrv2.Close()
	r2, st2, tenant2 := testRouter(t, &Deps{KYCURL: kycSrv2.URL})
	contact2 := addContact(t, st2, tenant2.ID, "Bola")
	prod2 := createProduct(t, r2)
	app2 := createApplication(t, r2, prod2.ID, contact2, 1500000, "submitted")
	if rec := patchApp(t, r2, app2.ID, `{"status":"under_review"}`); rec.Code != http.StatusOK {
		t.Fatalf("→under_review = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = patchApp(t, r2, app2.ID,
		`{"status":"approved","kyc":{"subject_phone":"+234802","id_type":"nin","id_value":"98765432109"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve with mismatch kyc = %d (%s), want 409", rec.Code, rec.Body.String())
	}
}

// Decline requires a reason; the decided event for a decline carries
// mode "none" KYC.
func TestDeclineFlow(t *testing.T) {
	r, st, tenant := testRouter(t, &Deps{EventsTopic: "test.lending"})
	contact := addContact(t, st, tenant.ID, "Ada")
	prod := createProduct(t, r)
	app := createApplication(t, r, prod.ID, contact, 1000000, "draft")

	// Draft → submitted (score computed on submit).
	rec := patchApp(t, r, app.ID, `{"status":"submitted"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("draft→submitted = %d (%s)", rec.Code, rec.Body.String())
	}
	var sub struct {
		Application Application `json:"application"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sub); err != nil || sub.Application.Score == nil {
		t.Fatalf("submit must compute the score: %+v (%v)", sub, err)
	}

	if rec := patchApp(t, r, app.ID, `{"status":"under_review"}`); rec.Code != http.StatusOK {
		t.Fatalf("→under_review = %d", rec.Code)
	}
	// Decline without reason → 400.
	if rec := patchApp(t, r, app.ID, `{"status":"declined"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("decline without reason = %d, want 400", rec.Code)
	}
	rec = patchApp(t, r, app.ID, `{"status":"declined","decline_reason":"thin file","decided_by":"ops-ada"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("decline = %d (%s)", rec.Code, rec.Body.String())
	}
	// Declined is terminal.
	if rec := patchApp(t, r, app.ID, `{"status":"under_review"}`); rec.Code != http.StatusConflict {
		t.Fatalf("declined→under_review = %d, want 409", rec.Code)
	}

	// The decided event: decision declined + kyc mode none + reason.
	var payload string
	err := st.pool.QueryRow(context.Background(),
		`SELECT payload::text FROM outbox WHERE topic='test.lending' LIMIT 1`).Scan(&payload)
	if err != nil {
		t.Fatalf("outbox: %v", err)
	}
	var env struct {
		Type string `json:"type"`
		Data struct {
			Decision      string `json:"decision"`
			DeclineReason string `json:"decline_reason"`
			KYC           struct {
				Mode string `json:"mode"`
			} `json:"kyc"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		t.Fatalf("decided event: %v", err)
	}
	if env.Type != EventTypeApplicationDecided || env.Data.Decision != "declined" ||
		env.Data.DeclineReason != "thin file" || env.Data.KYC.Mode != "none" {
		t.Fatalf("decline event payload: %+v", env)
	}
}

// Tenant middleware: no header → 400; unknown slug → 404; cross-tenant
// reads through the API are empty (end-to-end RLS).
func TestTenantHandling(t *testing.T) {
	r, st, tenant := testRouter(t, &Deps{})
	contact := addContact(t, st, tenant.ID, "Ada")
	prod := createProduct(t, r)
	app := createApplication(t, r, prod.ID, contact, 1000000, "submitted")

	req := httptest.NewRequest(http.MethodGet, "/v1/lending/products", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no tenant header = %d, want 400", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/lending/products", nil)
	req.Header.Set("X-Tenant-Slug", "ghost")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown tenant = %d, want 404", rec.Code)
	}

	// Filters + list endpoints work.
	rec = do(t, r, http.MethodGet, "/v1/lending/applications?status=submitted", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), app.ID.String()) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, r, http.MethodGet, "/v1/lending/applications?status=bogus", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad status filter = %d, want 400", rec.Code)
	}
	rec = do(t, r, http.MethodGet, "/v1/lending/products?all=true", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), prod.ID.String()) {
		t.Fatalf("products list = %d %s", rec.Code, rec.Body.String())
	}
	// 404s for unknown ids.
	rec = do(t, r, http.MethodGet, "/v1/lending/loans/"+uuid.NewString(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown loan = %d, want 404", rec.Code)
	}
}

// decided_by wiring (SPEC-W24 WS-A2): the decision stamp carries the
// authenticated operator identity, following the workforce leave-decision
// convention (the caller identity wins over client input).
//
//	(a) JWT sub resolvable (integrator-wired UserFromContext) → decided_by
//	    = JWT identity; a body-supplied decided_by CANNOT spoof it.
//	(b) no UserFromContext subject → X-User-Id header fallback (mirrors
//	    workforce.callerSub); still wins over the body.
//	(c) no identity at all → body decided_by fallback (pre-W24 behavior).
func TestDecidedByIdentity(t *testing.T) {
	var jwtUser string
	r, st, tenant := testRouter(t, &Deps{
		UserFromContext: func(context.Context) string { return jwtUser },
	})
	contact := addContact(t, st, tenant.ID, "Ada")

	underReview := func(t *testing.T) Application {
		t.Helper()
		prod := createProduct(t, r)
		app := createApplication(t, r, prod.ID, contact, 1000000, "submitted")
		if rec := patchApp(t, r, app.ID, `{"status":"under_review"}`); rec.Code != http.StatusOK {
			t.Fatalf("→under_review = %d (%s)", rec.Code, rec.Body.String())
		}
		return app
	}
	patchWithUser := func(t *testing.T, id uuid.UUID, body, xUserID string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, "/v1/lending/applications/"+id.String(), strings.NewReader(body))
		req.Header.Set("X-Tenant-Slug", "acme")
		if xUserID != "" {
			req.Header.Set("X-User-Id", xUserID)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	decidedByOf := func(t *testing.T, rec *httptest.ResponseRecorder) string {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("decision = %d (%s)", rec.Code, rec.Body.String())
		}
		var resp struct {
			Application Application `json:"application"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decision body: %v", err)
		}
		if resp.Application.DecidedBy == nil {
			t.Fatalf("decided_by not stamped: %+v", resp.Application)
		}
		return *resp.Application.DecidedBy
	}

	// (a) JWT identity wins on approve AND decline; the body cannot spoof.
	jwtUser = "ops-jwt-7"
	app := underReview(t)
	if got := decidedByOf(t, patchWithUser(t, app.ID,
		`{"status":"approved","kyc_override":true,"kyc_reason":"branch visit","decided_by":"spoofed-body"}`, "")); got != "ops-jwt-7" {
		t.Fatalf("approve decided_by = %q, want JWT identity ops-jwt-7", got)
	}
	app = underReview(t)
	if got := decidedByOf(t, patchWithUser(t, app.ID,
		`{"status":"declined","decline_reason":"thin file","decided_by":"spoofed-body"}`, "")); got != "ops-jwt-7" {
		t.Fatalf("decline decided_by = %q, want JWT identity ops-jwt-7", got)
	}

	// (b) No JWT subject → the X-User-Id header is the fallback identity
	// (mirrors workforce.callerSub) and still wins over the body.
	jwtUser = ""
	app = underReview(t)
	if got := decidedByOf(t, patchWithUser(t, app.ID,
		`{"status":"declined","decline_reason":"thin file","decided_by":"spoofed-body"}`, "ops-header-3")); got != "ops-header-3" {
		t.Fatalf("header fallback decided_by = %q, want ops-header-3", got)
	}

	// (c) No identity at all → the body-supplied decided_by remains the
	// fallback (pre-W24 behavior preserved).
	app = underReview(t)
	if got := decidedByOf(t, patchWithUser(t, app.ID,
		`{"status":"declined","decline_reason":"thin file","decided_by":"ops-body-9"}`, "")); got != "ops-body-9" {
		t.Fatalf("body fallback decided_by = %q, want ops-body-9", got)
	}
}

func mustTime(t *testing.T) (tm time.Time) {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, "2025-06-01T10:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	return tm.Add(-24 * time.Hour)
}

// W39 SIM-001 regression: with NO rail configured (LENDING_TB_BRIDGE_URL
// unset, RealRailConfigured false) and NO ALLOW_MOCK_RAILS opt-in,
// Disburse FAILS CLOSED — 503 with an explicit error, no state mutation
// (application stays approved, no loan account), no LoanDisbursed /
// DisbursementIntent events, no metering. Opting into the simulation
// (ALLOW_MOCK_RAILS=1) or wiring the real rail restores disbursement.
func TestDisburseFailsClosedWithoutRail(t *testing.T) {
	t.Setenv(EnvAllowMockRails, "")
	r, st, tenant := testRouter(t, &Deps{EventsTopic: "test.lending", UsageTopic: "test.usage"})
	contact := addContact(t, st, tenant.ID, "Bola")
	addBooking(t, st, tenant.ID, contact, "completed", mustTime(t))

	prod := createProduct(t, r)
	app := createApplication(t, r, prod.ID, contact, 2000000, "submitted")
	if rec := patchApp(t, r, app.ID, `{"status":"under_review"}`); rec.Code != http.StatusOK {
		t.Fatalf("→under_review = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := patchApp(t, r, app.ID, `{"status":"approved","kyc_override":true,"kyc_reason":"branch-verified ID card"}`); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d (%s)", rec.Code, rec.Body.String())
	}

	// Fail closed: no mock opt-in, no real rail.
	rec := do(t, r, http.MethodPost, "/v1/lending/applications/"+app.ID.String()+"/disburse", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("fail-closed disburse = %d (%s), want 503", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rail not configured") {
		t.Fatalf("error must name the missing rail: %s", rec.Body.String())
	}

	// No state mutation: no loan account exists for the application.
	var loans int
	if err := st.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM loan_accounts WHERE application_id=$1`, app.ID).Scan(&loans); err != nil {
		t.Fatalf("count loans: %v", err)
	}
	if loans != 0 {
		t.Fatalf("fail-closed disburse must not create a loan account, found %d", loans)
	}
	// No disbursed/intent events, no metering (only ApplicationDecided
	// from the approve step may exist).
	rows, err := st.pool.Query(context.Background(),
		`SELECT payload->>'type' FROM outbox`)
	if err != nil {
		t.Fatalf("outbox query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if typ == EventTypeLoanDisbursed || typ == EventTypeDisbursementIntent {
			t.Fatalf("fail-closed disburse emitted %s — no money moved, no event allowed", typ)
		}
		if typ == "com.opendesk.usage.UsageRecord" {
			t.Fatal("fail-closed disburse must not meter loan_disbursed")
		}
	}

	// Dev opt-in restores the simulated disbursement.
	t.Setenv(EnvAllowMockRails, "1")
	rec = do(t, r, http.MethodPost, "/v1/lending/applications/"+app.ID.String()+"/disburse", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disburse with mock opt-in = %d (%s)", rec.Code, rec.Body.String())
	}
}

// W39 SIM-001 regression: a configured REAL rail (RealRailConfigured,
// wired by main.go from LENDING_TB_BRIDGE_URL) disburses without the
// mock opt-in.
func TestDisburseAllowedWithRealRail(t *testing.T) {
	t.Setenv(EnvAllowMockRails, "")
	r, st, tenant := testRouter(t, &Deps{
		EventsTopic: "test.lending", UsageTopic: "test.usage",
		RealRailConfigured: true,
	})
	contact := addContact(t, st, tenant.ID, "Chidi")
	addBooking(t, st, tenant.ID, contact, "completed", mustTime(t))

	prod := createProduct(t, r)
	app := createApplication(t, r, prod.ID, contact, 2000000, "submitted")
	if rec := patchApp(t, r, app.ID, `{"status":"under_review"}`); rec.Code != http.StatusOK {
		t.Fatalf("→under_review = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := patchApp(t, r, app.ID, `{"status":"approved","kyc_override":true,"kyc_reason":"branch-verified ID card"}`); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d (%s)", rec.Code, rec.Body.String())
	}
	rec := do(t, r, http.MethodPost, "/v1/lending/applications/"+app.ID.String()+"/disburse", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disburse with real rail = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestMockRailsAllowed(t *testing.T) {
	t.Setenv(EnvAllowMockRails, "")
	if MockRailsAllowed() {
		t.Fatal("unset ALLOW_MOCK_RAILS must be OFF (fail closed)")
	}
	t.Setenv(EnvAllowMockRails, "0")
	if MockRailsAllowed() {
		t.Fatal("ALLOW_MOCK_RAILS=0 must be OFF")
	}
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv(EnvAllowMockRails, v)
		if !MockRailsAllowed() {
			t.Fatalf("ALLOW_MOCK_RAILS=%q must be ON", v)
		}
	}
}
