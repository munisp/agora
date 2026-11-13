package httpapi

// SPEC-W58 Scope C item C4: route-level tests for the KYC document
// surface — the owner/admin permission matrix (P5: member => 403 on ALL
// three routes, owner/admin pass through), fail-closed wiring (nil /
// unconfigured client => 503), kyc-down => 502, upload pre-checks
// (415/400/413), the P5 view-audit log line and the chi-timeout carve-out.
// Full-router tests ride the w56 fixture idiom (stub identity resolver);
// no DB is touched (the deny/503/pre-check paths never reach the client).

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/daprc"
	"github.com/opendesk/booking-service/internal/kycclient"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

var w58Tenant = bookingops.TenantInfo{ID: uuid.New(), Slug: "acme-w58", Name: "Acme W58", Timezone: "UTC", Currency: "NGN"}

// w58Resolver stubs identity resolution for the tenant middleware (w56
// recon test idiom).
func w58Resolver(t *testing.T) *bookingops.TenantResolver {
	t.Helper()
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tenants/"+w58Tenant.Slug {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(w58Tenant)
	}))
	t.Cleanup(identity.Close)
	return bookingops.NewTenantResolver(daprc.New("localhost", 1), "identity", time.Minute, zap.NewNop(),
		bookingops.WithIdentityBaseURL(identity.URL))
}

// w58PNG is a minimal magic-byte-valid PNG payload for upload tests.
var w58PNG = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x01, 0x02}

// w58MultipartBody builds the multipart upload body the admin-web BFF
// sends (file + subject_phone + doc_type + optional consent_id).
func w58MultipartBody(t *testing.T, mime string, data []byte, subjectPhone, docType, consentID string) (body *bytes.Buffer, contentType string) {
	t.Helper()
	body = &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	for k, v := range map[string]string{"subject_phone": subjectPhone, "doc_type": docType, "consent_id": consentID} {
		if v != "" {
			if err := mw.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	fh := textproto.MIMEHeader{}
	fh.Set("Content-Disposition", `form-data; name="file"; filename="doc.bin"`)
	fh.Set("Content-Type", mime)
	part, err := mw.CreatePart(fh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return body, mw.FormDataContentType()
}

// w58Router builds the full router with a kycclient pointed at upstream
// (may be nil/unconfigured for the fail-closed tests). AuthzDisabled
// isolates the P5 realm-role gate (the Permify require() becomes a
// pass-through — the member 403s below are then attributable to
// requireOwnerAdmin alone).
func w58Router(t *testing.T, client *kycclient.Client, log *zap.Logger) http.Handler {
	t.Helper()
	if log == nil {
		log = zap.NewNop()
	}
	return NewRouter(Deps{
		Logger:        log,
		AuthzDisabled: true,
		Resolver:      w58Resolver(t),
		KYC:           client,
	})
}

// w58Req stamps the tenant + user + roles the gateway/middleware would.
func w58Req(t *testing.T, method, path string, body *bytes.Buffer, contentType, roles string) *http.Request {
	t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, body)
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("X-Tenant-Slug", w58Tenant.Slug)
	r.Header.Set("X-User-Id", "user-"+roles)
	if roles != "" {
		r.Header.Set("X-User-Roles", roles)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return r
}

// w58FakeKYC is a fake kyc-service that records calls and answers 200.
func w58FakeKYC(t *testing.T) (upstreamURL string, calls *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/kyc/documents":
			_, _ = w.Write([]byte(`{"document_id":"doc-1","reference":"ref-1","status":"extracted","review_state":"pending_review"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/kyc/documents/"):
			_, _ = w.Write([]byte(`{"document_id":"doc-1","fields_available":true,"fields":{"full_name":"Ada Lovelace"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/review"):
			_, _ = w.Write([]byte(`{"document_id":"doc-1","review_state":"approved"}`))
		default:
			http.Error(w, "nope", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

func w58UploadReq(t *testing.T, roles string) *http.Request {
	t.Helper()
	body, ct := w58MultipartBody(t, "image/png", w58PNG, "+2348012345678", "kyc_nin_slip", "consent-1")
	return w58Req(t, http.MethodPost, "/v1/kyc/documents", body, ct, roles)
}

// P5 permission matrix: member => 403 on ALL three routes BEFORE any
// upstream call; owner and admin pass through to the (fake) kyc-service.
func TestKYCPermissionMatrix(t *testing.T) {
	upstream, calls := w58FakeKYC(t)
	router := w58Router(t, kycclient.New(upstream, "sekrit", nil), nil)

	docID := "11111111-1111-1111-1111-111111111111"
	reviewBody := func() *bytes.Buffer { return bytes.NewBufferString(`{"decision":"approved","reason":"ok"}`) }

	// member (manage_bookings holder per docs/security/roles.md) — denied.
	for _, tc := range []struct {
		name string
		req  *http.Request
	}{
		{"member upload", w58UploadReq(t, "member")},
		{"member get", w58Req(t, http.MethodGet, "/v1/kyc/documents/"+docID, nil, "", "member")},
		{"member review", w58Req(t, http.MethodPost, "/v1/kyc/documents/"+docID+"/review", reviewBody(), "application/json", "member")},
		{"no-roles upload", w58UploadReq(t, "")},
	} {
		before := *calls
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, tc.req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: member must 403, got %d (%s)", tc.name, rec.Code, rec.Body.String())
		}
		if *calls != before {
			t.Fatalf("%s: upstream must NOT be called on a denied request", tc.name)
		}
	}

	// owner + admin — pass through on all three routes.
	for _, roles := range []string{"owner", "admin"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, w58UploadReq(t, roles))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "document_id") {
			t.Fatalf("%s upload: got %d %s", roles, rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		router.ServeHTTP(rec, w58Req(t, http.MethodGet, "/v1/kyc/documents/"+docID, nil, "", roles))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fields_available") {
			t.Fatalf("%s get: got %d %s", roles, rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		router.ServeHTTP(rec, w58Req(t, http.MethodPost, "/v1/kyc/documents/"+docID+"/review", reviewBody(), "application/json", roles))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "review_state") {
			t.Fatalf("%s review: got %d %s", roles, rec.Code, rec.Body.String())
		}
	}
}

// Fail-closed wiring: nil client and unconfigured client both 503.
func TestKYCFailClosedWhenUnwired(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client *kycclient.Client
	}{
		{"nil client", nil},
		{"url unset", kycclient.New("", "sekrit", nil)},
		{"token unset", kycclient.New("http://kyc.invalid", "", nil)},
	} {
		router := w58Router(t, tc.client, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, w58Req(t, http.MethodGet, "/v1/kyc/documents/x", nil, "", "owner"))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: got %d (want 503)", tc.name, rec.Code)
		}
	}
}

// kyc-down => 502 {code:"KYC_UNAVAILABLE"} (transport failure mapping).
func TestKYCUpstreamDown502(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close() // connection-refused upstream
	router := w58Router(t, kycclient.New(dead.URL, "sekrit", nil), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, w58UploadReq(t, "owner"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("kyc down must 502, got %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["code"] != "KYC_UNAVAILABLE" {
		t.Fatalf("502 body must carry code KYC_UNAVAILABLE: %s", rec.Body.String())
	}
}

// Downstream domain statuses (403 consent denied) pass through verbatim —
// booking is a faithful proxy, not an error remapper.
func TestKYCDownstreamStatusPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"consent denied for purpose kyc_document"}`))
	}))
	t.Cleanup(srv.Close)
	router := w58Router(t, kycclient.New(srv.URL, "sekrit", nil), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, w58UploadReq(t, "owner"))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "consent denied") {
		t.Fatalf("downstream 403 must pass through, got %d %s", rec.Code, rec.Body.String())
	}
}

// Upload pre-checks fire BEFORE any upstream call: 415 mime/magic, 400
// doc_type, 400 missing subject_phone, 400 missing file.
func TestKYCUploadPreChecks(t *testing.T) {
	upstream, calls := w58FakeKYC(t)
	router := w58Router(t, kycclient.New(upstream, "sekrit", nil), nil)

	body, ct := w58MultipartBody(t, "text/plain", []byte("not an image"), "+2348012345678", "kyc_nin_slip", "")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, w58Req(t, http.MethodPost, "/v1/kyc/documents", body, ct, "owner"))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("mime pre-check must 415, got %d", rec.Code)
	}

	body, ct = w58MultipartBody(t, "image/png", w58PNG, "+2348012345678", "receipt", "")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, w58Req(t, http.MethodPost, "/v1/kyc/documents", body, ct, "owner"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("doc_type pre-check must 400, got %d", rec.Code)
	}

	body, ct = w58MultipartBody(t, "image/png", w58PNG, "", "kyc_nin_slip", "")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, w58Req(t, http.MethodPost, "/v1/kyc/documents", body, ct, "owner"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("subject_phone pre-check must 400, got %d", rec.Code)
	}

	if *calls != 0 {
		t.Fatalf("pre-checked requests must never reach upstream (%d calls)", *calls)
	}
}

// Review decision enum validated client-side (400), consent_id optional.
func TestKYCReviewDecisionEnum(t *testing.T) {
	upstream, calls := w58FakeKYC(t)
	router := w58Router(t, kycclient.New(upstream, "sekrit", nil), nil)
	docID := "11111111-1111-1111-1111-111111111111"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, w58Req(t, http.MethodPost, "/v1/kyc/documents/"+docID+"/review",
		bytes.NewBufferString(`{"decision":"maybe"}`), "application/json", "owner"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid decision must 400, got %d", rec.Code)
	}
	if *calls != 0 {
		t.Fatalf("invalid decision must never reach upstream")
	}
}

// P5/R8: every served review GET emits the structured view-audit line
// (actor, document_id, tenant_id).
func TestKYCViewAuditLog(t *testing.T) {
	upstream, _ := w58FakeKYC(t)
	core, observed := observer.New(zap.InfoLevel)
	router := w58Router(t, kycclient.New(upstream, "sekrit", nil), zap.New(core))
	docID := "11111111-1111-1111-1111-111111111111"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, w58Req(t, http.MethodGet, "/v1/kyc/documents/"+docID, nil, "", "owner"))
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	logs := observed.FilterMessage("kyc document view").All()
	if len(logs) != 1 {
		t.Fatalf("want exactly 1 view-audit line, got %d", len(logs))
	}
	fields := logs[0].ContextMap()
	if fields["actor"] != "user-owner" {
		t.Fatalf("audit actor = %v", fields["actor"])
	}
	if fields["document_id"] != docID {
		t.Fatalf("audit document_id = %v", fields["document_id"])
	}
	if fields["tenant_id"] != w58Tenant.ID.String() {
		t.Fatalf("audit tenant_id = %v", fields["tenant_id"])
	}
	if fields["audit"] != "kyc_document_view" {
		t.Fatalf("audit marker = %v", fields["audit"])
	}
}

// Timeout carve-out: POST /v1/kyc/documents rides the 150s+30s bound; the
// review POST and every other route keep the global 60s. Observed via the
// derived context deadline (requestTimeoutMiddleware is path-aware at the
// outermost layer — same test shape as the receipt precedent).
func TestKYCTimeoutCarveOut(t *testing.T) {
	deadlineOf := func(method, path string) time.Duration {
		var got time.Duration
		h := requestTimeoutMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			dl, ok := r.Context().Deadline()
			if !ok {
				t.Error("no deadline derived")
				return
			}
			got = time.Until(dl)
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return got
	}
	if d := deadlineOf(http.MethodPost, "/v1/kyc/documents"); d < 170*time.Second || d > 181*time.Second {
		t.Fatalf("kyc upload must ride ~180s carve-out, got %v", d)
	}
	if d := deadlineOf(http.MethodPost, "/v1/kyc/documents/x/review"); d < 55*time.Second || d > 61*time.Second {
		t.Fatalf("kyc review must keep the global 60s bound, got %v", d)
	}
	if d := deadlineOf(http.MethodGet, "/v1/kyc/documents/x"); d < 55*time.Second || d > 61*time.Second {
		t.Fatalf("kyc get must keep the global 60s bound, got %v", d)
	}
	if d := deadlineOf(http.MethodPost, "/v1/bookings"); d < 55*time.Second || d > 61*time.Second {
		t.Fatalf("unrelated routes keep the global 60s bound, got %v", d)
	}
}

// Route wiring proof: without a KYC client the routes still answer (503,
// not 404) — the router actually mounts all three (W53 lesson).
func TestKYCRoutesWired(t *testing.T) {
	router := w58Router(t, nil, nil)
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/v1/kyc/documents"},
		{http.MethodGet, "/v1/kyc/documents/x"},
		{http.MethodPost, "/v1/kyc/documents/x/review"},
	} {
		rec := httptest.NewRecorder()
		var body *bytes.Buffer
		ct := ""
		if tc.method == http.MethodPost && strings.HasSuffix(tc.path, "/review") {
			body = bytes.NewBufferString(`{"decision":"approved"}`)
			ct = "application/json"
		}
		router.ServeHTTP(rec, w58Req(t, tc.method, tc.path, body, ct, "owner"))
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Fatalf("%s %s not wired (%d)", tc.method, tc.path, rec.Code)
		}
	}
}
