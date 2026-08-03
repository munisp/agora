package consent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/identity-service/internal/store"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeRepo struct {
	mu    sync.Mutex
	recs  map[string]Record // key tenant|subject|purpose
	calls int
}

func newFakeRepo() *fakeRepo { return &fakeRepo{recs: map[string]Record{}} }

func key(tenantID uuid.UUID, subject, purpose string) string {
	return tenantID.String() + "|" + subject + "|" + purpose
}

func (f *fakeRepo) Capture(_ context.Context, rec *Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	k := key(rec.TenantID, rec.DataSubjectID, rec.Purpose)
	if existing, ok := f.recs[k]; ok {
		// Idempotent replay: keep consent_id + captured_ts; clear tombstone.
		existing.CapturedChannel = rec.CapturedChannel
		existing.CapturedLocale = rec.CapturedLocale
		existing.ErasureTS = nil
		f.recs[k] = existing
		*rec = existing
		return nil
	}
	if rec.ConsentID == uuid.Nil {
		rec.ConsentID = uuid.New()
	}
	rec.CapturedTS = time.Now().UTC()
	f.recs[k] = *rec
	return nil
}

func (f *fakeRepo) List(_ context.Context, tenantID uuid.UUID, subject string) ([]Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Record
	for _, r := range f.recs {
		if r.TenantID == tenantID && r.DataSubjectID == subject {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeRepo) Active(_ context.Context, tenantID uuid.UUID, subject, purpose string) (Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.recs[key(tenantID, subject, purpose)]
	if !ok || r.ErasureTS != nil {
		return Record{}, ErrNotFound
	}
	return r, nil
}

func (f *fakeRepo) Erase(_ context.Context, tenantID uuid.UUID, subject, purpose string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	now := time.Now().UTC()
	for k, r := range f.recs {
		if r.TenantID != tenantID || r.DataSubjectID != subject || r.ErasureTS != nil {
			continue
		}
		if purpose != "" && r.Purpose != purpose {
			continue
		}
		r.ErasureTS = &now
		f.recs[k] = r
		n++
	}
	return n, nil
}

func (f *fakeRepo) Ping(context.Context) error { return nil }

type fakeTenants struct{ bySlug map[string]uuid.UUID }

func (f fakeTenants) GetTenantBySlug(_ context.Context, slug string) (store.Tenant, error) {
	id, ok := f.bySlug[slug]
	if !ok {
		return store.Tenant{}, store.ErrNotFound
	}
	return store.Tenant{ID: id, Slug: slug}, nil
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

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	repo *fakeRepo
	pub  *fakePublisher
	slug string
	tid  uuid.UUID
	http http.Handler
}

func newHarness() *harness {
	repo := newFakeRepo()
	pub := &fakePublisher{}
	tid := uuid.New()
	h := &Handler{
		Repo:         repo,
		Tenants:      fakeTenants{bySlug: map[string]uuid.UUID{"acme": tid}},
		Events:       pub,
		PubSub:       "pubsub-kafka",
		ErasureTopic: DefaultErasureTopic,
		Logger:       zap.NewNop(),
	}
	r := chi.NewRouter()
	h.RegisterRoutes(r)
	return &harness{repo: repo, pub: pub, slug: "acme", tid: tid, http: r}
}

func (h *harness) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.http.ServeHTTP(rec, req)
	return rec
}

func (h *harness) captureOnce(t *testing.T, purpose string) *httptest.ResponseRecorder {
	t.Helper()
	return h.do(http.MethodPost, "/v1/consents",
		`{"tenant":"acme","subject":"+2348012345678","purpose":"`+purpose+`","captured_channel":"ussd","captured_locale":"en-NG"}`, nil)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCaptureCreatesRecord(t *testing.T) {
	h := newHarness()
	rec := h.captureOnce(t, "kyc")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var out Record
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ConsentID == uuid.Nil || out.TenantID != h.tid {
		t.Errorf("bad record: %+v", out)
	}
	if out.DataSubjectID != "+2348012345678" || out.Purpose != "kyc" {
		t.Errorf("subject/purpose: %+v", out)
	}
	if out.CapturedChannel != "ussd" || out.CapturedLocale != "en-NG" {
		t.Errorf("channel/locale: %+v", out)
	}
	if out.ErasureTS != nil {
		t.Errorf("erasure_ts must be null on capture")
	}
}

func TestCaptureIsIdempotentOnTenantSubjectPurpose(t *testing.T) {
	h := newHarness()
	first := h.captureOnce(t, "kyc")
	second := h.captureOnce(t, "kyc")
	var a, b Record
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(second.Body.Bytes(), &b)
	if a.ConsentID != b.ConsentID {
		t.Errorf("replay must keep consent_id: %s vs %s", a.ConsentID, b.ConsentID)
	}
	if !a.CapturedTS.Equal(b.CapturedTS) {
		t.Errorf("replay must keep original captured_ts")
	}
	if h.repo.calls != 2 {
		t.Errorf("repo calls = %d", h.repo.calls)
	}
}

func TestCaptureValidation(t *testing.T) {
	h := newHarness()
	cases := []struct{ name, body string }{
		{"empty body", `{}`},
		{"missing purpose", `{"tenant":"acme","subject":"+2348"}`},
		{"missing subject", `{"tenant":"acme","purpose":"kyc"}`},
		{"missing tenant", `{"subject":"+2348","purpose":"kyc"}`},
		{"bad json", `{`},
	}
	for _, tc := range cases {
		if rec := h.do(http.MethodPost, "/v1/consents", tc.body, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, rec.Code)
		}
	}
	// Unknown tenant slug -> 404.
	if rec := h.do(http.MethodPost, "/v1/consents",
		`{"tenant":"nope","subject":"+2348","purpose":"kyc"}`, nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown tenant: status = %d, want 404", rec.Code)
	}
}

func TestCheckConsentGate(t *testing.T) {
	h := newHarness()
	// No consent captured yet -> 403.
	rec := h.do(http.MethodGet, "/internal/consents/check?subject=%2B2348012345678&purpose=kyc", "",
		map[string]string{"X-Tenant-Slug": "acme"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("pre-consent check: status = %d, want 403", rec.Code)
	}
	var denied map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &denied)
	if denied["allowed"] != false {
		t.Errorf("denied body: %v", denied)
	}

	h.captureOnce(t, "kyc")
	rec = h.do(http.MethodGet, "/internal/consents/check?subject=%2B2348012345678&purpose=kyc", "",
		map[string]string{"X-Tenant-Slug": "acme"})
	if rec.Code != http.StatusOK {
		t.Fatalf("post-consent check: status = %d, want 200", rec.Code)
	}
	var ok map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &ok)
	if ok["allowed"] != true || ok["tenant_id"] != h.tid.String() {
		t.Errorf("allowed body: %v", ok)
	}

	// Different purpose still denied; missing tenant header -> 400.
	rec = h.do(http.MethodGet, "/internal/consents/check?subject=%2B2348012345678&purpose=marketing", "",
		map[string]string{"X-Tenant-Slug": "acme"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("other purpose: status = %d, want 403", rec.Code)
	}
	if rec = h.do(http.MethodGet, "/internal/consents/check?subject=x&purpose=kyc", "", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("no tenant header: status = %d, want 400", rec.Code)
	}
}

func TestCheckAcceptsUUIDTenantHeader(t *testing.T) {
	h := newHarness()
	h.captureOnce(t, "kyc")
	rec := h.do(http.MethodGet, "/internal/consents/check?subject=%2B2348012345678&purpose=kyc", "",
		map[string]string{"X-Tenant-ID": h.tid.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("uuid header check: status = %d, body %s", rec.Code, rec.Body)
	}
}

func TestErasureTombstonesAndPublishes(t *testing.T) {
	h := newHarness()
	h.captureOnce(t, "kyc")
	h.captureOnce(t, "marketing")

	rec := h.do(http.MethodPost, "/v1/consents/erasure",
		`{"tenant":"acme","subject":"+2348012345678","purpose":"kyc"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("erasure: status = %d, body %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["erased_records"] != float64(1) {
		t.Errorf("erased_records: %v", out)
	}

	// CloudEvent on the contract topic.
	if len(h.pub.events) != 1 {
		t.Fatalf("published events = %d, want 1", len(h.pub.events))
	}
	evt := h.pub.events[0]
	if evt.topic != DefaultErasureTopic {
		t.Errorf("topic = %q", evt.topic)
	}
	payload, _ := json.Marshal(evt.data)
	var ce map[string]any
	_ = json.Unmarshal(payload, &ce)
	if ce["type"] != ErasureEventType {
		t.Errorf("event type = %v", ce["type"])
	}
	data, _ := ce["data"].(map[string]any)
	if data["data_subject_id"] != "+2348012345678" || data["purpose"] != "kyc" {
		t.Errorf("event data: %v", data)
	}

	// Gate now denies kyc but the marketing consent survives.
	rec = h.do(http.MethodGet, "/internal/consents/check?subject=%2B2348012345678&purpose=kyc", "",
		map[string]string{"X-Tenant-Slug": "acme"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("post-erasure check: status = %d, want 403", rec.Code)
	}
	rec = h.do(http.MethodGet, "/internal/consents/check?subject=%2B2348012345678&purpose=marketing", "",
		map[string]string{"X-Tenant-Slug": "acme"})
	if rec.Code != http.StatusOK {
		t.Errorf("marketing consent must survive kyc erasure: status = %d", rec.Code)
	}

	// Re-erasure is a 404 (no active records left for that purpose).
	rec = h.do(http.MethodPost, "/v1/consents/erasure",
		`{"tenant":"acme","subject":"+2348012345678","purpose":"kyc"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("repeat erasure: status = %d, want 404", rec.Code)
	}
}

func TestErasureAllPurposes(t *testing.T) {
	h := newHarness()
	h.captureOnce(t, "kyc")
	h.captureOnce(t, "marketing")
	rec := h.do(http.MethodPost, "/v1/consents/erasure",
		`{"tenant":"acme","subject":"+2348012345678"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["erased_records"] != float64(2) {
		t.Errorf("erased_records = %v, want 2", out)
	}
}

func TestRecaptureAfterErasureReConsents(t *testing.T) {
	h := newHarness()
	h.captureOnce(t, "kyc")
	h.do(http.MethodPost, "/v1/consents/erasure",
		`{"tenant":"acme","subject":"+2348012345678","purpose":"kyc"}`, nil)
	h.captureOnce(t, "kyc")
	rec := h.do(http.MethodGet, "/internal/consents/check?subject=%2B2348012345678&purpose=kyc", "",
		map[string]string{"X-Tenant-Slug": "acme"})
	if rec.Code != http.StatusOK {
		t.Errorf("re-consent after erasure must re-allow: status = %d", rec.Code)
	}
}

func TestListIncludesTombstones(t *testing.T) {
	h := newHarness()
	h.captureOnce(t, "kyc")
	h.captureOnce(t, "marketing")
	h.do(http.MethodPost, "/v1/consents/erasure",
		`{"tenant":"acme","subject":"+2348012345678","purpose":"kyc"}`, nil)
	rec := h.do(http.MethodGet, "/v1/consents?subject=%2B2348012345678&tenant=acme", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status = %d", rec.Code)
	}
	var out struct {
		Consents []Record `json:"consents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Consents) != 2 {
		t.Fatalf("records = %d, want 2 (tombstone retained for audit)", len(out.Consents))
	}
	var erased, active int
	for _, c := range out.Consents {
		if c.ErasureTS != nil {
			erased++
		} else {
			active++
		}
	}
	if erased != 1 || active != 1 {
		t.Errorf("erased=%d active=%d, want 1/1", erased, active)
	}
}

func TestErasurePublishFailureStillTombstones(t *testing.T) {
	h := newHarness()
	h.captureOnce(t, "kyc")
	h.pub.err = errors.New("dapr down")
	rec := h.do(http.MethodPost, "/v1/consents/erasure",
		`{"tenant":"acme","subject":"+2348012345678","purpose":"kyc"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Errorf("publish failure must not fail erasure: status = %d", rec.Code)
	}
	rec = h.do(http.MethodGet, "/internal/consents/check?subject=%2B2348012345678&purpose=kyc", "",
		map[string]string{"X-Tenant-Slug": "acme"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("tombstone must hold despite publish failure: status = %d", rec.Code)
	}
}
