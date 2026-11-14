package httpapi

// SPEC-W65 Scope 2 (D): route-level tests for the booking KYC document
// LIST proxy (GET /v1/kyc/documents — query passthrough to kyc-service).
// Covered: P5 permission matrix (member => 403 BEFORE any upstream call),
// param validation (review_state enum, limit 1..200, offset >= 0),
// faithful query pass-through to the upstream, verbatim envelope
// pass-through, fail-closed wiring, kyc-down => 502, the P5/R8 list
// view-audit log line, and route wiring. Rides the w58 fixture idiom
// (w58Router / w58Req from kyc_routes_test.go; no DB is touched).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opendesk/booking-service/internal/kycclient"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// w65ListBody is the pinned C-side contract envelope (hash-only Document
// projections incl. subject_phone; limit/offset echoed).
const w65ListBody = `{"items":[{"document_id":"11111111-1111-1111-1111-111111111111","subject_phone":"+2348012345678","doc_type":"kyc_nin_slip","status":"extracted","review_state":"pending_review","created_at":"2026-01-02T03:04:05Z"}],"limit":50,"offset":0}`

// w65FakeKYCList is a fake kyc-service that records the raw query string
// of every list call and answers 200 with the pinned contract envelope.
func w65FakeKYCList(t *testing.T) (upstreamURL string, queries *[]string, calls *int) {
	t.Helper()
	var qs []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/kyc/documents" {
			n++
			qs = append(qs, r.URL.RawQuery)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(w65ListBody))
			return
		}
		http.Error(w, "nope", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &qs, &n
}

// P5 permission matrix: member / no-roles => 403 BEFORE any upstream call;
// owner and admin pass through to the (fake) kyc-service.
func TestKYCListPermissionMatrix(t *testing.T) {
	upstream, _, calls := w65FakeKYCList(t)
	router := w58Router(t, kycclient.New(upstream, "sekrit", nil), nil)

	for _, roles := range []string{"member", ""} {
		before := *calls
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, w58Req(t, http.MethodGet, "/v1/kyc/documents", nil, "", roles))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("roles=%q: must 403, got %d (%s)", roles, rec.Code, rec.Body.String())
		}
		if *calls != before {
			t.Fatalf("roles=%q: upstream must NOT be called on a denied request", roles)
		}
	}

	for _, roles := range []string{"owner", "admin"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, w58Req(t, http.MethodGet, "/v1/kyc/documents", nil, "", roles))
		if rec.Code != http.StatusOK {
			t.Fatalf("roles=%q: got %d %s", roles, rec.Code, rec.Body.String())
		}
	}
}

// Param validation: bad review_state / limit / offset => 400, upstream
// never called (fail-fast mirrors the mediaInboxList idiom).
func TestKYCListParamValidation(t *testing.T) {
	upstream, _, calls := w65FakeKYCList(t)
	router := w58Router(t, kycclient.New(upstream, "sekrit", nil), nil)

	for _, qs := range []string{
		"review_state=bogus",
		"review_state=approvedx", // enum prefix is not enough
		"limit=0",
		"limit=201",
		"limit=abc",
		"limit=1.5",
		"offset=-1",
		"offset=abc",
	} {
		before := *calls
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, w58Req(t, http.MethodGet, "/v1/kyc/documents?"+qs, nil, "", "owner"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("?%s: must 400, got %d (%s)", qs, rec.Code, rec.Body.String())
		}
		if *calls != before {
			t.Fatalf("?%s: invalid params must never reach upstream", qs)
		}
	}
}

// Query passthrough: the booking handler forwards review_state/limit/offset
// verbatim (defaults applied: limit=50, offset=0), and the upstream
// envelope passes through untouched (hash-only projections incl.
// subject_phone).
func TestKYCListPassthrough(t *testing.T) {
	upstream, queries, _ := w65FakeKYCList(t)
	router := w58Router(t, kycclient.New(upstream, "sekrit", nil), nil)

	// Defaults: no query params => upstream sees limit=50 (offset=0 omitted
	// so kyc-service's own default applies — zero values are not sent).
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, w58Req(t, http.MethodGet, "/v1/kyc/documents", nil, "", "owner"))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != w65ListBody {
		t.Fatalf("envelope must pass through verbatim, got %s", rec.Body.String())
	}
	if got := (*queries)[len(*queries)-1]; got != "limit=50" {
		t.Fatalf("default query passthrough = %q (want limit=50)", got)
	}
	// Contract conformance: items carry the hash-only projection keys the
	// C side pinned (subject_phone included).
	var env struct {
		Items []map[string]any `json:"items"`
		Limit int              `json:"limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Limit != 50 || len(env.Items) != 1 {
		t.Fatalf("envelope shape: %v %s", err, rec.Body.String())
	}
	if env.Items[0]["subject_phone"] != "+2348012345678" {
		t.Fatalf("subject_phone must ride the projection: %v", env.Items[0])
	}

	// Explicit filter: review_state normalized to lower-case, limit/offset
	// forwarded as-is.
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, w58Req(t, http.MethodGet,
		"/v1/kyc/documents?review_state=PENDING_REVIEW&limit=25&offset=50", nil, "", "admin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered list: %d %s", rec.Code, rec.Body.String())
	}
	if got := (*queries)[len(*queries)-1]; got != "limit=25&offset=50&review_state=pending_review" {
		t.Fatalf("filtered query passthrough = %q", got)
	}
}

// Fail-closed wiring + kyc-down => 502 for the list route (same posture
// as the other kyc routes).
func TestKYCListFailClosedAndDown(t *testing.T) {
	router := w58Router(t, nil, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, w58Req(t, http.MethodGet, "/v1/kyc/documents", nil, "", "owner"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil client must 503, got %d", rec.Code)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close() // connection-refused upstream
	router = w58Router(t, kycclient.New(dead.URL, "sekrit", nil), nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, w58Req(t, http.MethodGet, "/v1/kyc/documents", nil, "", "owner"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("kyc down must 502, got %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["code"] != "KYC_UNAVAILABLE" {
		t.Fatalf("502 body must carry code KYC_UNAVAILABLE: %s", rec.Body.String())
	}
}

// P5/R8: every served list view emits the structured audit line (actor,
// tenant, filter — no subject data).
func TestKYCListAuditLog(t *testing.T) {
	upstream, _, _ := w65FakeKYCList(t)
	core, observed := observer.New(zap.InfoLevel)
	router := w58Router(t, kycclient.New(upstream, "sekrit", nil), zap.New(core))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, w58Req(t, http.MethodGet,
		"/v1/kyc/documents?review_state=approved", nil, "", "owner"))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	logs := observed.FilterMessage("kyc document list view").All()
	if len(logs) != 1 {
		t.Fatalf("want exactly 1 list audit line, got %d", len(logs))
	}
	fields := logs[0].ContextMap()
	if fields["actor"] != "user-owner" {
		t.Fatalf("audit actor = %v", fields["actor"])
	}
	if fields["tenant_id"] != w58Tenant.ID.String() {
		t.Fatalf("audit tenant_id = %v", fields["tenant_id"])
	}
	if fields["audit"] != "kyc_document_list" {
		t.Fatalf("audit marker = %v", fields["audit"])
	}
	if fields["review_state"] != "approved" {
		t.Fatalf("audit review_state = %v", fields["review_state"])
	}
}

// Route wiring proof: without a KYC client the list route still answers
// (503, not 404/405) — the router actually mounts GET /v1/kyc/documents.
func TestKYCListRouteWired(t *testing.T) {
	router := w58Router(t, nil, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, w58Req(t, http.MethodGet, "/v1/kyc/documents", nil, "", "owner"))
	if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/kyc/documents not wired (%d)", rec.Code)
	}
	// And the detail GET must still bind on the {id} route (chi sibling
	// ordering sanity: /documents and /documents/{id} coexist).
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, w58Req(t, http.MethodGet, "/v1/kyc/documents/x", nil, "", "owner"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("detail route must still answer 503 with nil client, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Fatalf("detail route body: %s", rec.Body.String())
	}
}
