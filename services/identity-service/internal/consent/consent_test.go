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
	mu     sync.Mutex
	recs   map[string]Record // key tenant|subject|purpose
	calls  int
	outbox []OutboxEvent
	nextID int64
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
	if n > 0 {
		// Mirrors the Postgres Erase: tombstone + durable outbox row are one
		// atomic unit (SPEC-W43 I-04).
		f.nextID++
		f.outbox = append(f.outbox, OutboxEvent{
			ID:            f.nextID,
			TenantID:      tenantID,
			DataSubjectID: subject,
			Purpose:       purpose,
			ErasedRecords: n,
			Synthetic:     EvaluateErasureEligibility(subject).Synthetic,
			CreatedAt:     now,
		})
	}
	return n, nil
}

// MarkOutboxSent deletes the row, so FetchUnsentOutbox returns what is left.
func (f *fakeRepo) FetchUnsentOutbox(_ context.Context, limit int) ([]OutboxEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]OutboxEvent{}, f.outbox...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeRepo) MarkOutboxSent(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, e := range f.outbox {
		if e.ID == id {
			f.outbox = append(f.outbox[:i], f.outbox[i+1:]...)
			return nil
		}
	}
	return nil
}

func (f *fakeRepo) Ping(context.Context) error { return nil }

type fakeTenants struct {
	bySlug map[string]uuid.UUID
	byID   map[uuid.UUID]string
}

func (f fakeTenants) GetTenantBySlug(_ context.Context, slug string) (store.Tenant, error) {
	id, ok := f.bySlug[slug]
	if !ok {
		return store.Tenant{}, store.ErrNotFound
	}
	return store.Tenant{ID: id, Slug: slug}, nil
}

func (f fakeTenants) GetTenantByID(_ context.Context, id uuid.UUID) (store.Tenant, error) {
	slug, ok := f.byID[id]
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
	if f.err != nil {
		return f.err // failed publish records nothing (real Dapr semantics)
	}
	f.events = append(f.events, publishedEvent{topic: topic, data: data})
	return nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	repo  *fakeRepo
	pub   *fakePublisher
	Relay *Relay
	h     *Handler // exposed so auth-matrix tests can mutate gate config
	slug  string
	tid   uuid.UUID
	http  http.Handler
}

// testInternalToken is the K2 service credential the harness injects by
// default (the V2-D3 gate requires auth on erasure + list; pre-existing
// behaviour tests exercise the handler logic as a service caller).
const testInternalToken = "test-internal-token"

func newHarness() *harness {
	repo := newFakeRepo()
	pub := &fakePublisher{}
	tid := uuid.New()
	relay := &Relay{
		Repo:         repo,
		Events:       pub,
		PubSub:       "pubsub-kafka",
		ConsentTopic: DefaultErasureTopic,
		PrivacyTopic: DefaultPrivacyTopic,
		Logger:       zap.NewNop(),
	}
	h := &Handler{
		Repo:          repo,
		Tenants:       fakeTenants{bySlug: map[string]uuid.UUID{"acme": tid}, byID: map[uuid.UUID]string{tid: "acme"}},
		Relay:         relay,
		InternalToken: testInternalToken,
		Logger:        zap.NewNop(),
	}
	r := chi.NewRouter()
	h.RegisterRoutes(r)
	return &harness{repo: repo, pub: pub, Relay: relay, h: h, slug: "acme", tid: tid, http: r}
}

func (h *harness) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	// Default to the K2 service credential; explicit X-Internal-Token in
	// headers (including a wrong one, for the auth matrix) wins.
	if _, ok := headers["X-Internal-Token"]; !ok {
		if headers == nil {
			headers = map[string]string{}
		}
		headers["X-Internal-Token"] = testInternalToken
	}
	return h.doRaw(method, path, body, headers)
}

// doRaw sends a request WITHOUT injecting any credential (auth-matrix tests).
func (h *harness) doRaw(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
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

	// CloudEvents on BOTH contract topics (K4): the consent erasure event
	// plus the PrivacyEraseRequested tombstone the booking/conversation
	// consumers actually listen for (F15-06).
	if len(h.pub.events) != 2 {
		t.Fatalf("published events = %d, want 2 (consent + privacy)", len(h.pub.events))
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

	// K4 privacy tombstone — exact shape of booking-service
	// internal/consumer/privacy.go's privacyEnvelope.
	pevt := h.pub.events[1]
	if pevt.topic != DefaultPrivacyTopic {
		t.Errorf("privacy topic = %q, want %q", pevt.topic, DefaultPrivacyTopic)
	}
	ppayload, _ := json.Marshal(pevt.data)
	var pce map[string]any
	_ = json.Unmarshal(ppayload, &pce)
	if pce["type"] != PrivacyEraseEventType {
		t.Errorf("privacy event type = %v", pce["type"])
	}
	pdata, _ := pce["data"].(map[string]any)
	if pdata["phone"] != "+2348012345678" || pdata["email"] != "" {
		t.Errorf("privacy event locators: %v", pdata)
	}
	if pdata["tenant_id"] != h.tid.String() {
		t.Errorf("privacy tenant_id = %v", pdata["tenant_id"])
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

	// SPEC-W43 I-04: the outbox row survives the publish failure and the
	// relay republishes once Dapr recovers (durable, not best-effort).
	if len(h.repo.outbox) != 1 {
		t.Fatalf("unsent outbox rows = %d, want 1", len(h.repo.outbox))
	}
	h.pub.err = nil
	if _, err := h.Relay.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.repo.outbox) != 0 {
		t.Errorf("outbox not drained after successful sweep")
	}
	if len(h.pub.events) != 2 {
		t.Errorf("republished events = %d, want 2 (consent + privacy)", len(h.pub.events))
	}
}

// TestErasureSkipsPrivacyEventForNonContactSubject: uuid-shaped subjects
// carry no phone/email locator — the K4 privacy event would be a poison
// message downstream (booking dead-letters empty phone+email), so only the
// consent event is published.
func TestErasureSkipsPrivacyEventForNonContactSubject(t *testing.T) {
	h := newHarness()
	subject := uuid.NewString()
	h.do(http.MethodPost, "/v1/consents",
		`{"tenant":"acme","subject":"`+subject+`","purpose":"kyc"}`, nil)
	rec := h.do(http.MethodPost, "/v1/consents/erasure",
		`{"tenant":"acme","subject":"`+subject+`","purpose":"kyc"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if len(h.pub.events) != 1 {
		t.Fatalf("published events = %d, want 1 (consent only)", len(h.pub.events))
	}
	if h.pub.events[0].topic != DefaultErasureTopic {
		t.Errorf("topic = %q", h.pub.events[0].topic)
	}
}

// TestRelaySweepPublishesOnce: a second sweep over a drained outbox must not
// republish (no duplicate fanout).
func TestRelaySweepPublishesOnce(t *testing.T) {
	h := newHarness()
	h.captureOnce(t, "kyc")
	h.do(http.MethodPost, "/v1/consents/erasure",
		`{"tenant":"acme","subject":"+2348012345678","purpose":"kyc"}`, nil)
	if len(h.pub.events) != 2 {
		t.Fatalf("published events = %d, want 2", len(h.pub.events))
	}
	if _, err := h.Relay.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.pub.events) != 2 {
		t.Errorf("second sweep republished: events = %d", len(h.pub.events))
	}
}
