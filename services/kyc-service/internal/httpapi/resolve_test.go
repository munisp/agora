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

func (f *fakeAuditStore) Ping(context.Context) error { return nil }

type fakeConsent struct {
	tenantID uuid.UUID
	err      error
	gotHdr   map[string]string // captured tenant header(s) via client, n/a for fake
}

func (f fakeConsent) CheckConsent(_ context.Context, tenantRef, subject, purpose string) (uuid.UUID, error) {
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

type harness struct {
	audits *fakeAuditStore
	pub    *fakePublisher
	tid    uuid.UUID
	http   http.Handler
}

func newHarness(consentErr error) *harness {
	audits := &fakeAuditStore{}
	pub := &fakePublisher{}
	tid := uuid.New()
	d := Deps{
		Store:       audits,
		Consent:     fakeConsent{tenantID: tid, err: consentErr},
		Resolver:    MockResolver{},
		Events:      pub,
		PubSub:      "pubsub-kafka",
		EventsTopic: "opendesk.kyc.resolved.v1",
		Logger:      zap.NewNop(),
	}
	r := chi.NewRouter()
	r.Mount("/", NewRouter(d))
	return &harness{audits: audits, pub: pub, tid: tid, http: r}
}

func (h *harness) resolve(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/kyc/resolve", strings.NewReader(body))
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

	c := NewConsentClient(nil, "", stub.URL)
	// uuid tenant ref -> X-Tenant-ID header.
	got, err := c.CheckConsent(context.Background(), tid.String(), "+2348", "kyc")
	if err != nil || got != tid {
		t.Errorf("uuid path: got %v err %v", got, err)
	}
	if gotTenantID != tid.String() || gotSlug != "" {
		t.Errorf("headers: X-Tenant-ID=%q X-Tenant-Slug=%q", gotTenantID, gotSlug)
	}
	if !strings.Contains(gotQuery, "purpose=kyc") {
		t.Errorf("query: %q", gotQuery)
	}
	// slug tenant ref -> X-Tenant-Slug header.
	if _, err := c.CheckConsent(context.Background(), "acme", "+2348", "kyc"); err != nil {
		t.Errorf("slug path: %v", err)
	}
	if gotSlug != "acme" {
		t.Errorf("slug header = %q", gotSlug)
	}
	// 403 -> ErrConsentDenied.
	if _, err := c.CheckConsent(context.Background(), tid.String(), "denied", "kyc"); !errors.Is(err, ErrConsentDenied) {
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
