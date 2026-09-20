package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/kyc-service/internal/store"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeAuditStore struct {
	mu   sync.Mutex
	rows []store.Audit
	err  error
}

func (f *fakeAuditStore) InsertAudit(_ context.Context, a *store.Audit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if a.AuditID == uuid.Nil {
		a.AuditID = uuid.New()
	}
	f.rows = append(f.rows, *a)
	return nil
}

// IDHashOwner implements the SPEC-W45 K22 binding lookup over the recorded
// rows: the subject_phone of the FIRST resolution of (id_type, id_hash).
func (f *fakeAuditStore) IDHashOwner(_ context.Context, tenantID uuid.UUID, idType, idValueHash string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.rows {
		if a.TenantID == tenantID && a.IDType == idType && a.IDValueHash == idValueHash {
			return a.SubjectPhone, nil
		}
	}
	return "", nil
}

func (f *fakeAuditStore) Ping(context.Context) error { return nil }

type fakeConsent struct {
	tenantID uuid.UUID
	err      error
	// captured last call (K22 binding context forwarded to the gate).
	gotSubject, gotIDType, gotIDHash string
}

func (f *fakeConsent) CheckConsent(_ context.Context, tenantRef, subject, purpose, idType, idHash string) (uuid.UUID, error) {
	f.gotSubject, f.gotIDType, f.gotIDHash = subject, idType, idHash
	return f.tenantID, f.err
}

type publishedEvent struct {
	topic string
	data  any
}

type fakePublisher struct {
	mu     sync.Mutex
	events []publishedEvent
	err    error
}

func (f *fakePublisher) PublishEvent(_ context.Context, _, topic string, data any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, publishedEvent{topic: topic, data: data})
	return f.err
}

// testInternalToken is the harness X-Internal-Token (SPEC-W45 K22 gate).
const testInternalToken = "test-kyc-internal-token"

// testHashSecret is the harness HMAC key for id_value_hash.
const testHashSecret = "test-kyc-hash-secret"

type harness struct {
	audits  *fakeAuditStore
	pub     *fakePublisher
	consent *fakeConsent
	tid     uuid.UUID
	http    http.Handler
}

func newHarness(consentErr error) *harness {
	audits := &fakeAuditStore{}
	pub := &fakePublisher{}
	consent := &fakeConsent{tenantID: uuid.New()}
	consent.err = consentErr
	tid := consent.tenantID
	d := Deps{
		Store:         audits,
		Consent:       consent,
		Resolver:      MockResolver{},
		Events:        pub,
		PubSub:        "pubsub-kafka",
		EventsTopic:   "opendesk.kyc.resolved.v1",
		InternalToken: testInternalToken,
		HashSecret:    testHashSecret,
		Logger:        zap.NewNop(),
	}
	r := chi.NewRouter()
	r.Mount("/", NewRouter(d))
	return &harness{audits: audits, pub: pub, consent: consent, tid: tid, http: r}
}

// resolve posts with the valid internal token (the K22 happy path).
func (h *harness) resolve(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return h.resolveWithToken(t, body, testInternalToken)
}

func (h *harness) resolveWithToken(t *testing.T, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/kyc/resolve", strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	rec := httptest.NewRecorder()
	h.http.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// Happy path + contract
// ---------------------------------------------------------------------------

func TestResolveVerifiedMock(t *testing.T) {
	h := newHarness(nil)
	rec := h.resolve(t, `{"tenant_id":"`+h.tid.String()+`","subject_phone":"+2348012345678","id_type":"bvn","id_value":"22223333444"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var out resolveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusVerified {
		t.Errorf("status = %q, want verified (all digits, len>=10)", out.Status)
	}
	if !strings.HasPrefix(out.Reference, "kyc_") {
		t.Errorf("reference = %q", out.Reference)
	}

	// Exactly one audit row, raw id_value NEVER stored.
	if len(h.audits.rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(h.audits.rows))
	}
	a := h.audits.rows[0]
	if a.TenantID != h.tid || a.SubjectPhone != "+2348012345678" || a.IDType != "bvn" {
		t.Errorf("audit identity fields: %+v", a)
	}
	if a.Status != StatusVerified || a.Reference != out.Reference {
		t.Errorf("audit result fields: %+v", a)
	}
	if len(a.IDValueHash) != 64 || strings.Contains(a.IDValueHash, "2222") {
		t.Errorf("id_value must be sha256 hex, got %q", a.IDValueHash)
	}

	// CloudEvent on the contract topic.
	if len(h.pub.events) != 1 {
		t.Fatalf("events = %d, want 1", len(h.pub.events))
	}
	evt := h.pub.events[0]
	if evt.topic != "opendesk.kyc.resolved.v1" {
		t.Errorf("topic = %q", evt.topic)
	}
	payload, _ := json.Marshal(evt.data)
	var ce map[string]any
	_ = json.Unmarshal(payload, &ce)
	if ce["type"] != ResolvedEventType {
		t.Errorf("event type = %v", ce["type"])
	}
	data, _ := ce["data"].(map[string]any)
	if data["status"] != StatusVerified || data["reference"] != out.Reference {
		t.Errorf("event data: %v", data)
	}
	if _, raw := data["id_value"]; raw {
		t.Errorf("raw id_value must not appear in the event: %v", data)
	}
}

func TestResolveMismatchMock(t *testing.T) {
	h := newHarness(nil)
	for _, idValue := range []string{"12345", "12345678901a", "", "123 456 7890"} {
		body := fmt.Sprintf(`{"tenant_id":"%s","subject_phone":"+2348","id_type":"nin","id_value":"%s"}`,
			h.tid.String(), idValue)
		rec := h.resolve(t, body)
		if idValue == "" {
			if rec.Code != http.StatusBadRequest {
				t.Errorf("empty id_value: status = %d, want 400", rec.Code)
			}
			continue
		}
		var out resolveResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out.Status != StatusMismatch {
			t.Errorf("id_value %q: status = %q, want mismatch", idValue, out.Status)
		}
	}
}

func TestResolveReferenceDeterministic(t *testing.T) {
	h := newHarness(nil)
	body := `{"tenant_id":"` + h.tid.String() + `","subject_phone":"+2348012345678","id_type":"bvn","id_value":"22223333444"}`
	var refs []string
	for i := 0; i < 2; i++ {
		rec := h.resolve(t, body)
		var out resolveResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		refs = append(refs, out.Reference)
	}
	if refs[0] != refs[1] {
		t.Errorf("reference must be deterministic: %s vs %s", refs[0], refs[1])
	}
	if len(h.audits.rows) != 2 {
		t.Errorf("every attempt must audit: rows = %d", len(h.audits.rows))
	}
}

// ---------------------------------------------------------------------------
// Consent gate
// ---------------------------------------------------------------------------

func TestResolveConsentDenied403(t *testing.T) {
	h := newHarness(ErrConsentDenied)
	rec := h.resolve(t, `{"tenant_id":"acme","subject_phone":"+2348","id_type":"bvn","id_value":"22223333444"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["error"] != "consent_required" {
		t.Errorf("body: %v", out)
	}
	if len(h.audits.rows) != 0 {
		t.Errorf("denied requests must not resolve/audit: rows = %d", len(h.audits.rows))
	}
}

func TestResolveConsentGateDown502(t *testing.T) {
	h := newHarness(errors.New("identity unreachable"))
	rec := h.resolve(t, `{"tenant_id":"acme","subject_phone":"+2348","id_type":"bvn","id_value":"22223333444"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

// ConsentClient against a real HTTP stub (identity contract: 200 allowed /
// 403 denied, tenant uuid returned; headers forwarded).
func TestConsentClientAgainstIdentityStub(t *testing.T) {
	tid := uuid.New()
	var gotTenantID, gotSlug, gotQuery string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenantID = r.Header.Get("X-Tenant-ID")
		gotSlug = r.Header.Get("X-Tenant-Slug")
		gotQuery = r.URL.RawQuery
		if r.URL.Path != "/internal/consents/check" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("subject") == "denied" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"allowed":false}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"allowed":true,"tenant_id":"` + tid.String() + `"}`))
	}))
	defer stub.Close()

	c := NewConsentClient(nil, "", stub.URL, "")
	// uuid tenant ref -> X-Tenant-ID header.
	got, err := c.CheckConsent(context.Background(), tid.String(), "+2348", "kyc", "bvn", "abc123hash")
	if err != nil || got != tid {
		t.Errorf("uuid path: got %v err %v", got, err)
	}
	if gotTenantID != tid.String() || gotSlug != "" {
		t.Errorf("headers: X-Tenant-ID=%q X-Tenant-Slug=%q", gotTenantID, gotSlug)
	}
	if !strings.Contains(gotQuery, "purpose=kyc") {
		t.Errorf("query: %q", gotQuery)
	}
	// SPEC-W45 K22: the binding context (id_type + id_hash) is forwarded.
	if !strings.Contains(gotQuery, "id_type=bvn") || !strings.Contains(gotQuery, "id_hash=abc123hash") {
		t.Errorf("K22 binding context missing from query: %q", gotQuery)
	}
	// slug tenant ref -> X-Tenant-Slug header.
	if _, err := c.CheckConsent(context.Background(), "acme", "+2348", "kyc", "bvn", "abc123hash"); err != nil {
		t.Errorf("slug path: %v", err)
	}
	if gotSlug != "acme" {
		t.Errorf("slug header = %q", gotSlug)
	}
	// 403 -> ErrConsentDenied.
	if _, err := c.CheckConsent(context.Background(), tid.String(), "denied", "kyc", "bvn", "abc123hash"); !errors.Is(err, ErrConsentDenied) {
		t.Errorf("denied: %v, want ErrConsentDenied", err)
	}
}

// ---------------------------------------------------------------------------
// Validation + failure paths
// ---------------------------------------------------------------------------

func TestResolveValidation(t *testing.T) {
	h := newHarness(nil)
	cases := []struct{ name, body string }{
		{"bad json", `{`},
		{"empty", `{}`},
		{"bad id_type", `{"tenant_id":"a","subject_phone":"p","id_type":"passport","id_value":"1"}`},
		{"missing phone", `{"tenant_id":"a","id_type":"bvn","id_value":"1"}`},
		{"missing tenant", `{"subject_phone":"p","id_type":"bvn","id_value":"1"}`},
	}
	for _, tc := range cases {
		if rec := h.resolve(t, tc.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, rec.Code)
		}
	}
	if len(h.audits.rows) != 0 {
		t.Errorf("invalid requests must not audit: rows = %d", len(h.audits.rows))
	}
}

func TestResolveAuditFailureIs500(t *testing.T) {
	h := newHarness(nil)
	h.audits.err = errors.New("db down")
	rec := h.resolve(t, `{"tenant_id":"acme","subject_phone":"+2348","id_type":"bvn","id_value":"22223333444"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("audit failure must fail the request: status = %d, want 500", rec.Code)
	}
}

func TestResolvePublishFailureStill200(t *testing.T) {
	h := newHarness(nil)
	h.pub.err = errors.New("dapr down")
	rec := h.resolve(t, `{"tenant_id":"acme","subject_phone":"+2348","id_type":"bvn","id_value":"22223333444"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("publish failure must not fail resolution: status = %d", rec.Code)
	}
	if len(h.audits.rows) != 1 {
		t.Errorf("audit row must still exist: rows = %d", len(h.audits.rows))
	}
}

// ---------------------------------------------------------------------------
// SPEC-W45 K22 (OOS-07): X-Internal-Token gate on /v1/kyc/*
// ---------------------------------------------------------------------------

func TestResolveInternalTokenMatrix(t *testing.T) {
	body := func(tid uuid.UUID) string {
		return `{"tenant_id":"` + tid.String() + `","subject_phone":"+2348012345678","id_type":"bvn","id_value":"22223333444"}`
	}
	t.Run("missing token -> 401", func(t *testing.T) {
		h := newHarness(nil)
		if rec := h.resolveWithToken(t, body(h.tid), ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("wrong token -> 401", func(t *testing.T) {
		h := newHarness(nil)
		if rec := h.resolveWithToken(t, body(h.tid), "nope"); rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("correct token -> 200", func(t *testing.T) {
		h := newHarness(nil)
		if rec := h.resolve(t, body(h.tid)); rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
		}
	})
	t.Run("token unset on server -> 503 fail-closed", func(t *testing.T) {
		h := newHarness(nil)
		d := Deps{
			Store: h.audits, Consent: h.consent, Resolver: MockResolver{},
			Events: h.pub, PubSub: "pubsub-kafka", EventsTopic: "t",
			InternalToken: "", HashSecret: testHashSecret, Logger: zap.NewNop(),
		}
		r := chi.NewRouter()
		r.Mount("/", NewRouter(d))
		req := httptest.NewRequest(http.MethodPost, "/v1/kyc/resolve", strings.NewReader(body(h.tid)))
		req.Header.Set("X-Internal-Token", testInternalToken)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 (fail-closed unset)", rec.Code)
		}
	})
	t.Run("unauthenticated request resolves nothing", func(t *testing.T) {
		h := newHarness(nil)
		rec := h.resolveWithToken(t, body(h.tid), "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", rec.Code)
		}
		if len(h.audits.rows) != 0 || len(h.pub.events) != 0 {
			t.Errorf("rejected requests must not audit/publish: audits=%d events=%d",
				len(h.audits.rows), len(h.pub.events))
		}
	})
}

// ---------------------------------------------------------------------------
// SPEC-W45 K22 (OOS-17): consent<->ID binding rule
// ---------------------------------------------------------------------------

// An ID resolved once under a phone is bound to that phone (first consented
// resolution wins); resolving the same ID under a DIFFERENT phone -> 403.
func TestResolveIDBindingRule(t *testing.T) {
	h := newHarness(nil)
	resolve := func(phone, idValue string) *httptest.ResponseRecorder {
		return h.resolve(t, `{"tenant_id":"`+h.tid.String()+`","subject_phone":"`+phone+
			`","id_type":"bvn","id_value":"`+idValue+`"}`)
	}
	// First resolution binds 22223333444 -> phone A.
	if rec := resolve("+2348011111111", "22223333444"); rec.Code != http.StatusOK {
		t.Fatalf("first resolution: status = %d, body %s", rec.Code, rec.Body)
	}
	// Same ID + same phone -> allowed (idempotent re-resolution).
	if rec := resolve("+2348011111111", "22223333444"); rec.Code != http.StatusOK {
		t.Errorf("same phone re-resolution: status = %d, want 200", rec.Code)
	}
	// Same ID under a DIFFERENT phone -> 403 id_binding_violation.
	rec := resolve("+2348099999999", "22223333444")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-phone resolution: status = %d, want 403", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["error"] != "id_binding_violation" {
		t.Errorf("error = %v, want id_binding_violation", out)
	}
	// The violation must not resolve/audit (no provider call, no audit row).
	if len(h.audits.rows) != 2 {
		t.Errorf("audit rows = %d, want 2 (the two allowed resolutions)", len(h.audits.rows))
	}
	// A different ID under the second phone is fine (phone consented, ID unbound).
	if rec := resolve("+2348099999999", "55556666777"); rec.Code != http.StatusOK {
		t.Errorf("new id under other phone: status = %d, want 200", rec.Code)
	}
}

// The consent check must carry the binding context: the request's
// subject_phone (consented subject) plus id_type + id_hash.
func TestResolveConsentCheckCarriesBindingContext(t *testing.T) {
	h := newHarness(nil)
	rec := h.resolve(t, `{"tenant_id":"`+h.tid.String()+`","subject_phone":"+2348012345678","id_type":"nin","id_value":"22223333444"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if h.consent.gotSubject != "+2348012345678" {
		t.Errorf("consent subject = %q, want the request phone", h.consent.gotSubject)
	}
	if h.consent.gotIDType != "nin" {
		t.Errorf("consent id_type = %q", h.consent.gotIDType)
	}
	if h.consent.gotIDHash != hashIDValue(testHashSecret, "22223333444") {
		t.Errorf("consent id_hash = %q, want HMAC digest", h.consent.gotIDHash)
	}
}

// HMAC key separation: the stored digest must differ from the bare SHA-256
// of the raw value and from a digest keyed with another secret.
func TestResolveHashIsKeyedHMAC(t *testing.T) {
	h := newHarness(nil)
	rec := h.resolve(t, `{"tenant_id":"`+h.tid.String()+`","subject_phone":"+2348012345678","id_type":"bvn","id_value":"22223333444"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := h.audits.rows[0].IDValueHash
	if got == hashIDValue("", "22223333444") {
		t.Errorf("digest must depend on the configured key (empty-key HMAC must differ)")
	}
	if got == hashIDValue("other-secret", "22223333444") {
		t.Errorf("digest must depend on the configured secret")
	}
	if got != hashIDValue(testHashSecret, "22223333444") {
		t.Errorf("digest = %q, want HMAC-SHA256(testHashSecret, id)", got)
	}
}
