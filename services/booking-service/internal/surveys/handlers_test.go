package surveys

import (
	"context"
	"encoding/json"
	"fmt"
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
// (the integrator-facing entry point) with a fake tenant resolver. The
// PUBLIC respond path is exercised WITHOUT any tenant header.

type fakeResolver map[string]bookingops.TenantInfo

func (f fakeResolver) BySlug(_ context.Context, slug string) (bookingops.TenantInfo, error) {
	t, ok := f[slug]
	if !ok {
		return bookingops.TenantInfo{}, fmt.Errorf("unknown tenant %q", slug)
	}
	return t, nil
}

type testEnv struct {
	router http.Handler
	store  *Store
	tenant bookingops.TenantInfo
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st := newTestStore(t)
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme", Name: "Acme"}
	r := chi.NewRouter()
	RegisterRoutes(r, &Deps{
		Store:              st,
		Resolver:           fakeResolver{"acme": tenant},
		NotificationsTopic: "test.notifications",
		UsageTopic:         "test.usage",
		EventsTopic:        "test.surveys.events",
		PublicBaseURL:      "https://s.acme.test",
	})
	return &testEnv{router: r, store: st, tenant: tenant}
}

// do issues one request; withTenant controls the X-Tenant-Slug header (the
// respond tests pass false to prove the path is public).
func (e *testEnv) do(t *testing.T, method, path, body string, withTenant bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if withTenant {
		req.Header.Set("X-Tenant-Slug", "acme")
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func (e *testEnv) outboxRows(t *testing.T, topic string) []map[string]any {
	t.Helper()
	rows, err := e.store.pool.Query(context.Background(),
		`SELECT payload FROM outbox WHERE topic=$1 ORDER BY id`, topic)
	if err != nil {
		t.Fatalf("outbox query: %v", err)
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("payload: %v", err)
		}
		out = append(out, payload)
	}
	return out
}

const surveyBody = `{
  "name": "NPS Q3",
  "kind": "nps",
  "channel": "sms",
  "questions": [
    {"id": "nps", "type": "rating", "label": "How likely are you to recommend us?", "required": true},
    {"id": "why", "type": "text", "label": "Why that score?"},
    {"id": "channel", "type": "single", "label": "Preferred channel", "options": ["sms", "app"]}
  ]
}`

func (e *testEnv) createActiveSurvey(t *testing.T) string {
	t.Helper()
	rec := e.do(t, http.MethodPost, "/v1/surveys/surveys", surveyBody, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	id := decode(t, rec)["survey"].(map[string]any)["id"].(string)
	rec = e.do(t, http.MethodPatch, "/v1/surveys/surveys/"+id, `{"status":"active"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("activate = %d (%s)", rec.Code, rec.Body.String())
	}
	return id
}

// Full flow: create → (send on draft 409) → activate → send → outbox paced
// commands + sent events → PUBLIC respond (no tenant header) → replay 409 →
// unknown token 404 → results → themes → metering/event outbox rows.
func TestSurveysAPIFlow(t *testing.T) {
	e := newTestEnv(t)

	// Tenant header required on the scoped group.
	rec := e.do(t, http.MethodGet, "/v1/surveys/surveys", "", false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no tenant header = %d, want 400", rec.Code)
	}

	// Validation: single with <2 options rejected.
	rec = e.do(t, http.MethodPost, "/v1/surveys/surveys",
		`{"name":"bad","questions":[{"id":"q","type":"single","label":"L","options":["only"]}]}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad questions = %d, want 400", rec.Code)
	}

	// Create starts in draft; send on draft → 409.
	rec = e.do(t, http.MethodPost, "/v1/surveys/surveys", surveyBody, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	survey := decode(t, rec)["survey"].(map[string]any)
	surveyID := survey["id"].(string)
	if survey["status"] != "draft" {
		t.Fatalf("status = %v, want draft", survey["status"])
	}
	ada := seedContact(t, e.store, e.tenant.ID, "Ada", "+234801")
	grace := seedContact(t, e.store, e.tenant.ID, "Grace", "+234802")
	rec = e.do(t, http.MethodPost, "/v1/surveys/surveys/"+surveyID+"/send",
		fmt.Sprintf(`{"contact_ids":["%s"]}`, ada), true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("send on draft = %d, want 409", rec.Code)
	}

	// Illegal transition draft→paused → 409; draft→active ok.
	rec = e.do(t, http.MethodPatch, "/v1/surveys/surveys/"+surveyID, `{"status":"paused"}`, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("draft->paused = %d, want 409", rec.Code)
	}
	rec = e.do(t, http.MethodPatch, "/v1/surveys/surveys/"+surveyID, `{"status":"active"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("activate = %d (%s)", rec.Code, rec.Body.String())
	}

	// Send to two contacts (+ one ghost skipped).
	rec = e.do(t, http.MethodPost, "/v1/surveys/surveys/"+surveyID+"/send",
		fmt.Sprintf(`{"contact_ids":["%s","%s","%s"]}`, ada, grace, uuid.New()), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("send = %d (%s)", rec.Code, rec.Body.String())
	}
	send := decode(t, rec)
	if send["invites_created"].(float64) != 2 || send["sent"].(float64) != 2 || send["queued"].(float64) != 0 {
		t.Fatalf("send = %v", send)
	}
	if len(send["skipped"].([]any)) != 1 {
		t.Fatalf("skipped = %v", send["skipped"])
	}
	invites := send["invites"].([]any)
	token := invites[0].(map[string]any)["token"].(string)
	link := invites[0].(map[string]any)["link"].(string)
	if !strings.HasPrefix(link, "https://s.acme.test?t=") {
		t.Fatalf("link = %q", link)
	}

	// Outbox: 2 paced commands (geo_campaign sms) + 2 invite-sent events.
	paced := e.outboxRows(t, "test.notifications")
	if len(paced) != 2 {
		t.Fatalf("paced commands = %d, want 2", len(paced))
	}
	for _, p := range paced {
		if p["type"] != "com.opendesk.notifications.PacedSend" {
			t.Fatalf("paced type = %v", p["type"])
		}
		data := p["data"].(map[string]any)
		if data["kind"] != "geo_campaign" {
			t.Fatalf("paced kind = %v", data["kind"])
		}
		geo := data["geo_campaign"].(map[string]any)
		if geo["channel"] != "sms" || geo["tenant_slug"] != "acme" || geo["campaign_id"] != surveyID {
			t.Fatalf("geo payload = %v", geo)
		}
	}
	sentEvents := e.outboxRows(t, "test.surveys.events")
	if len(sentEvents) != 2 || sentEvents[0]["type"] != "com.opendesk.surveys.InviteSent" {
		t.Fatalf("sent events = %v", sentEvents)
	}

	// PUBLIC respond — NO tenant header, NO JWT.
	rec = e.do(t, http.MethodPost, "/v1/surveys/respond",
		fmt.Sprintf(`{"token":"%s","answers":{"nps":9,"why":"fast delivery, great app","channel":"sms"}}`, token), false)
	if rec.Code != http.StatusCreated {
		t.Fatalf("respond = %d (%s)", rec.Code, rec.Body.String())
	}
	resp := decode(t, rec)["response"].(map[string]any)
	if resp["score"].(float64) != 9 {
		t.Fatalf("score = %v", resp["score"])
	}

	// Replay → 409 already_answered.
	rec = e.do(t, http.MethodPost, "/v1/surveys/respond",
		fmt.Sprintf(`{"token":"%s","answers":{"nps":1}}`, token), false)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already_answered") {
		t.Fatalf("replay = %d (%s), want 409 already_answered", rec.Code, rec.Body.String())
	}

	// Unknown token → 404.
	rec = e.do(t, http.MethodPost, "/v1/surveys/respond",
		`{"token":"deadbeefdeadbeefdeadbeefdeadbeef","answers":{"nps":1}}`, false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token = %d, want 404", rec.Code)
	}

	// Answered event + usage record enqueued exactly once (no double meter).
	all := e.outboxRows(t, "test.surveys.events")
	typeCounts := map[string]int{}
	for _, evt := range all {
		typeCounts[evt["type"].(string)]++
	}
	if typeCounts["com.opendesk.surveys.InviteSent"] != 2 ||
		typeCounts["com.opendesk.surveys.ResponseReceived"] != 1 {
		t.Fatalf("event types = %v", typeCounts)
	}
	usage := e.outboxRows(t, "test.usage")
	if len(usage) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(usage))
	}
	if usage[0]["data"].(map[string]any)["metric"] != "survey_response_received" {
		t.Fatalf("usage = %v", usage[0])
	}

	// Second invite answers a detractor score → NPS over 2 scored.
	token2 := invites[1].(map[string]any)["token"].(string)
	rec = e.do(t, http.MethodPost, "/v1/surveys/respond",
		fmt.Sprintf(`{"token":"%s","answers":{"nps":4,"why":"delivery was late"}}`, token2), false)
	if rec.Code != http.StatusCreated {
		t.Fatalf("respond 2 = %d (%s)", rec.Code, rec.Body.String())
	}

	// Results: 2 responses, NPS = 50 - 50 = 0, mean 6.5, channel breakdown.
	rec = e.do(t, http.MethodGet, "/v1/surveys/surveys/"+surveyID+"/results", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("results = %d (%s)", rec.Code, rec.Body.String())
	}
	results := decode(t, rec)["results"].(map[string]any)
	if results["response_count"].(float64) != 2 || results["scored_count"].(float64) != 2 {
		t.Fatalf("results = %v", results)
	}
	if results["nps"].(float64) != 0 {
		t.Fatalf("nps = %v, want 0 (1 promoter 1 detractor)", results["nps"])
	}
	if results["mean_score"].(float64) != 6.5 {
		t.Fatalf("mean = %v, want 6.5", results["mean_score"])
	}
	if results["promoters"].(float64) != 1 || results["detractors"].(float64) != 1 {
		t.Fatalf("p/d = %v", results)
	}

	// Themes over the text answers (naive keyword frequency).
	rec = e.do(t, http.MethodGet, "/v1/surveys/voc/themes?survey_id="+surveyID, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("themes = %d (%s)", rec.Code, rec.Body.String())
	}
	themes := decode(t, rec)["themes"].([]any)
	if len(themes) == 0 || themes[0].(map[string]any)["term"] != "delivery" {
		t.Fatalf("themes = %v", themes)
	}

	// Get returns stats rollup.
	rec = e.do(t, http.MethodGet, "/v1/surveys/surveys/"+surveyID, "", true)
	stats := decode(t, rec)["stats"].(map[string]any)
	if stats["responses"].(float64) != 2 || stats["invites_answered"].(float64) != 2 {
		t.Fatalf("stats = %v", stats)
	}

	// Archive is terminal: PATCH → 409.
	rec = e.do(t, http.MethodPatch, "/v1/surveys/surveys/"+surveyID, `{"status":"archived"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = e.do(t, http.MethodPatch, "/v1/surveys/surveys/"+surveyID, `{"name":"rename"}`, true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("patch archived = %d, want 409", rec.Code)
	}
}

// Sends stay queued (gracefully) when the notifications topic is disabled.
func TestSendDeferredWithoutTopic(t *testing.T) {
	st := newTestStore(t)
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme", Name: "Acme"}
	r := chi.NewRouter()
	RegisterRoutes(r, &Deps{
		Store:    st,
		Resolver: fakeResolver{"acme": tenant},
		// NotificationsTopic empty on purpose.
		EventsTopic: "test.surveys.events",
	})
	e := &testEnv{router: r, store: st, tenant: tenant}

	surveyID := e.createActiveSurvey(t)
	ada := seedContact(t, st, tenant.ID, "Ada", "+234801")
	rec := e.do(t, http.MethodPost, "/v1/surveys/surveys/"+surveyID+"/send",
		fmt.Sprintf(`{"contact_ids":["%s"]}`, ada), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("send = %d (%s)", rec.Code, rec.Body.String())
	}
	send := decode(t, rec)
	if send["sends_deferred"] != true || send["sent"].(float64) != 0 || send["queued"].(float64) != 1 {
		t.Fatalf("deferred send = %v", send)
	}
	if n := len(e.outboxRows(t, "test.notifications")); n != 0 {
		t.Fatalf("outbox rows = %d, want 0", n)
	}
}
