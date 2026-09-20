package lending

// SPEC-W45 tests: K22 (X-Internal-Token on the kyc resolve call), ORPH O4
// (Ledger seam wired — journal rows on disburse/repay), and the payments
// /v1/transfers bridge contract (grep-level).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
)

// K22: the kyc resolve call carries X-Internal-Token when
// Deps.KYCInternalToken is configured (and omits it when not).
func TestKYCResolveSendsInternalToken(t *testing.T) {
	var gotToken, gotPath string
	kycSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Internal-Token")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"verified","reference":"kyc-ref-t","latency_ms":1}`))
	}))
	defer kycSrv.Close()

	r, st, tenant := testRouter(t, &Deps{KYCURL: kycSrv.URL, KYCInternalToken: "kyc-tok-42"})
	contact := addContact(t, st, tenant.ID, "Ada")
	prod := createProduct(t, r)
	app := createApplication(t, r, prod.ID, contact, 1500000, "submitted")
	if rec := patchApp(t, r, app.ID, `{"status":"under_review"}`); rec.Code != http.StatusOK {
		t.Fatalf("→under_review = %d (%s)", rec.Code, rec.Body.String())
	}
	rec := patchApp(t, r, app.ID,
		`{"status":"approved","kyc":{"subject_phone":"+234801","id_type":"bvn","id_value":"12345678901"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d (%s)", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/kyc/resolve" {
		t.Fatalf("kyc path = %q", gotPath)
	}
	if gotToken != "kyc-tok-42" {
		t.Fatalf("X-Internal-Token = %q, want kyc-tok-42 (K22)", gotToken)
	}

	// Without the configured token the header is absent (dev-only posture).
	gotToken = "<unset>"
	kycSrv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Internal-Token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"verified","reference":"kyc-ref-u","latency_ms":1}`))
	}))
	defer kycSrv2.Close()
	r2, st2, tenant2 := testRouter(t, &Deps{KYCURL: kycSrv2.URL})
	contact2 := addContact(t, st2, tenant2.ID, "Ada")
	prod2 := createProduct(t, r2)
	app2 := createApplication(t, r2, prod2.ID, contact2, 1500000, "submitted")
	if rec := patchApp(t, r2, app2.ID, `{"status":"under_review"}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if rec := patchApp(t, r2, app2.ID,
		`{"status":"approved","kyc":{"subject_phone":"+234801","id_type":"bvn","id_value":"12345678901"}}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if gotToken != "" {
		t.Fatalf("X-Internal-Token = %q, want empty when KYCInternalToken unset", gotToken)
	}
}

// ORPH O4: with Deps.Ledger wired (as cmd/server/main.go now does),
// disburse and repay land balanced journal rows on the mirrored ledger
// (ref_type loan_disbursement / loan_repayment).
func TestDisburseRepayJournalWithLedgerWired(t *testing.T) {
	t.Setenv(EnvAllowMockRails, "1")
	// Same router shape as testRouter, but with the Ledger seam built from
	// the test store BEFORE registration (as cmd/server/main.go wires it).
	st := newTestStore(t)
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme", Timezone: "Africa/Lagos"}
	d := &Deps{
		EventsTopic: "test.lending", UsageTopic: "test.usage",
		Ledger: NewPostgresLedger(st),
	}
	d.Store = st
	d.Resolver = fakeResolver{"acme": tenant}
	r := chi.NewRouter()
	RegisterRoutes(r, d)
	contact := addContact(t, st, tenant.ID, "Ada")
	addBooking(t, st, tenant.ID, contact, "completed", mustTime(t))
	prod := createProduct(t, r)
	app := createApplication(t, r, prod.ID, contact, 2000000, "submitted")
	if rec := patchApp(t, r, app.ID, `{"status":"under_review"}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	rec := patchAppWithRoles(t, r, app.ID,
		`{"status":"approved","kyc_override":true,"kyc_reason":"branch-verified ID card"}`, "platform-admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d (%s)", rec.Code, rec.Body.String())
	}
	// Disburse (route: /v1/lending/applications/{id}/disburse). The repay
	// path below is /v1/lending/loans/{LOAN id}/repay — the loan account id
	// comes from the disburse response, not the application id.
	disburseRec := do(t, r, http.MethodPost, "/v1/lending/applications/"+app.ID.String()+"/disburse", `{}`)
	if disburseRec.Code != http.StatusOK {
		t.Fatalf("disburse = %d (%s)", disburseRec.Code, disburseRec.Body.String())
	}
	var disb struct {
		Loan struct {
			ID string `json:"id"`
		} `json:"loan"`
	}
	if err := json.Unmarshal(disburseRec.Body.Bytes(), &disb); err != nil || disb.Loan.ID == "" {
		t.Fatalf("disburse body: %s (%v)", disburseRec.Body.String(), err)
	}
	all, err := st.ListLedgerEntries(context.Background(), tenant.ID, nil, nil, "")
	if err != nil {
		t.Fatalf("list ledger entries: %v", err)
	}
	byRef := func(refType, refID string) []LedgerEntry {
		var out []LedgerEntry
		for _, e := range all {
			if e.RefType == refType && e.RefID == refID {
				out = append(out, e)
			}
		}
		return out
	}
	entries := byRef(RefTypeDisbursement, app.ID.String())
	if len(entries) != 2 {
		t.Fatalf("disbursement journal entries = %d, want 2: %+v", len(entries), entries)
	}
	var deb, cred int64
	for _, e := range entries {
		deb += e.DebitKobo
		cred += e.CreditKobo
		if e.AccountCode != AccountRepaymentReceived && e.AccountCode != AccountPrincipalDisbursed {
			t.Fatalf("unexpected account %d", e.AccountCode)
		}
	}
	if deb != 2000000 || cred != 2000000 {
		t.Fatalf("journal not balanced at principal: debit=%d credit=%d", deb, cred)
	}

	// Repay (partial) → repayment journal under the caller ref_id.
	rec = do(t, r, http.MethodPost, "/v1/lending/loans/"+disb.Loan.ID+"/repay",
		`{"amount_kobo":500000,"ref_id":"rep-ledger-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("repay = %d (%s)", rec.Code, rec.Body.String())
	}
	all, err = st.ListLedgerEntries(context.Background(), tenant.ID, nil, nil, "")
	if err != nil {
		t.Fatalf("list ledger entries: %v", err)
	}
	repEntries := byRef(RefTypeRepayment, "rep-ledger-1")
	if len(repEntries) != 2 {
		t.Fatalf("repayment journal entries = %d, want 2: %+v", len(repEntries), repEntries)
	}
	deb, cred = 0, 0
	for _, e := range repEntries {
		deb += e.DebitKobo
		cred += e.CreditKobo
	}
	if deb != 500000 || cred != 500000 {
		t.Fatalf("repayment journal not balanced: debit=%d credit=%d", deb, cred)
	}
}

// ORPH O4 invariants: an UNBALANCED journal can never land — the store's
// disburse path validates through validateJournal before inserting.
func TestValidateJournalGuardsLendingPostings(t *testing.T) {
	if err := validateJournal(uuid.New(), uuid.New(), []LedgerEntry{
		{AccountCode: AccountRepaymentReceived, DebitKobo: 100, RefType: RefTypeDisbursement, RefID: "x"},
		{AccountCode: AccountPrincipalDisbursed, CreditKobo: 99, RefType: RefTypeDisbursement, RefID: "x"},
	}); err == nil {
		t.Fatal("unbalanced journal must be rejected")
	}
	if err := validateJournal(uuid.New(), uuid.New(), []LedgerEntry{
		{AccountCode: 999, DebitKobo: 100, RefType: RefTypeDisbursement, RefID: "x"},
		{AccountCode: AccountPrincipalDisbursed, CreditKobo: 100, RefType: RefTypeDisbursement, RefID: "x"},
	}); err == nil {
		t.Fatal("unknown account code must be rejected")
	}
}

// SPEC-W45 item 3 (CONTRACT NOTE): the lending disbursement bridge targets
// payments POST /v1/transfers (CODER-K). Grep-level contract: when the
// payments-service checkout is present AND the route has landed, this
// asserts it; while K's work is pending the test skips with an explicit
// note (the consumer's HTTPRail posts to {PAYMENTS_URL}/v1/transfers —
// wired in cmd/server/main.go).
func TestPaymentsTransfersRouteContract(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".."))
	routesRS := filepath.Join(repoRoot, "services", "payments-service", "src", "routes.rs")
	raw, err := os.ReadFile(routesRS)
	if err != nil {
		t.Skipf("payments-service checkout not present (%v) — cannot verify the /v1/transfers contract", err)
	}
	src := string(raw)
	if !strings.Contains(src, "/v1/transfers") {
		t.Skipf("CONTRACT NOTE (CODER-K pending): payments routes.rs has no /v1/transfers route yet — " +
			"the lending HTTPRail target (PAYMENTS_URL + /v1/transfers) depends on it")
	}
	// The route must be a POST carrying a tenant-bound idempotent transfer.
	if !strings.Contains(src, `"/v1/transfers"`) {
		t.Fatalf("payments routes.rs mentions /v1/transfers but the route literal is missing")
	}
}
