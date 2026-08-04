package campaignstudio

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
)

// Handler tests run the FULL RegisterRoutes surface against embedded
// Postgres (same harness as the store tests) with a fake send starter.

type fakeStarter struct {
	batches []StudioSendBatchInput
	err     error
}

func (f *fakeStarter) StartStudioSendBatch(_ context.Context, in StudioSendBatchInput) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.batches = append(f.batches, in)
	return "studio-send-" + in.BatchID, nil
}

type studioTestEnv struct {
	router  chi.Router
	store   *Store
	starter *fakeStarter
	tenant  bookingops.TenantInfo
}

func newStudioTestEnv(t *testing.T) *studioTestEnv {
	t.Helper()
	st := newTestStore(t)
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme", Name: "Acme"}
	starter := &fakeStarter{}
	r := chi.NewRouter()
	RegisterRoutes(r, &Deps{
		Store:   st,
		Starter: starter,
		TenantFromContext: func(context.Context) bookingops.TenantInfo {
			return tenant
		},
		UsageTopic:  "opendesk.usage.events",
		EventsTopic: DefaultEventsTopic,
	})
	return &studioTestEnv{router: r, store: st, starter: starter, tenant: tenant}
}

func (e *studioTestEnv) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: decode response: %v (body %q)", method, path, err, rec.Body.String())
		}
	}
	return rec.Code, out
}

func (e *studioTestEnv) mkJourney(t *testing.T, steps []map[string]any) string {
	t.Helper()
	code, resp := e.do(t, http.MethodPost, "/v1/studio/journeys", map[string]any{
		"name": "Journey", "trigger_kind": "manual", "steps": steps,
	})
	if code != http.StatusCreated {
		t.Fatalf("create journey = %d %v", code, resp)
	}
	return resp["journey"].(map[string]any)["id"].(string)
}

func (e *studioTestEnv) transition(t *testing.T, id, status string, want int) {
	t.Helper()
	code, resp := e.do(t, http.MethodPatch, "/v1/studio/journeys/"+id, map[string]any{"status": status})
	if code != want {
		t.Fatalf("transition → %s = %d %v, want %d", status, code, resp, want)
	}
}

// Full REST surface: segments + journeys + enrollment + step + stats.
func TestStudioAPIFlow(t *testing.T) {
	e := newStudioTestEnv(t)
	ctx := context.Background()

	// --- Segments ---
	code, resp := e.do(t, http.MethodPost, "/v1/studio/segments", map[string]any{
		"name": "CRM", "definition": map[string]any{"filters": []map[string]any{
			{"field": "source", "op": "eq", "value": "twenty"},
		}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create segment = %d %v", code, resp)
	}
	segID := resp["segment"].(map[string]any)["id"].(string)

	// Bad definition rejected (unknown field — injection guard).
	code, _ = e.do(t, http.MethodPost, "/v1/studio/segments", map[string]any{
		"name": "evil", "definition": map[string]any{"filters": []map[string]any{
			{"field": "name; DROP TABLE contacts--", "op": "eq", "value": "x"},
		}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("evil segment = %d, want 400", code)
	}

	// Count endpoint (empty contacts → 0).
	code, resp = e.do(t, http.MethodPost, "/v1/studio/segments/"+segID+"/count", nil)
	if code != http.StatusOK || resp["count"].(float64) != 0 {
		t.Fatalf("count = %d %v", code, resp)
	}

	// --- Journeys ---
	// Invalid steps rejected (unknown kind).
	code, _ = e.do(t, http.MethodPost, "/v1/studio/journeys", map[string]any{
		"name": "bad", "steps": []map[string]any{{"type": "send", "kind": "whatsapp", "template": "x"}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("bad journey = %d, want 400", code)
	}

	journeyID := e.mkJourney(t, []map[string]any{
		{"type": "send", "kind": "sms", "template": "Hi {name}, come back!"},
		{"type": "wait", "wait_hours": 1},
	})

	// Enroll into a DRAFT journey → 409.
	c1 := seedContact(t, e.store, e.tenant.ID, "Ada", "+2348011111111", "a@b.c", "")
	code, _ = e.do(t, http.MethodPost, "/v1/studio/journeys/"+journeyID+"/enroll",
		map[string]any{"contact_ids": []string{c1.String()}})
	if code != http.StatusConflict {
		t.Fatalf("enroll draft = %d, want 409", code)
	}

	// Status machine: draft→paused illegal (409); draft→active ok.
	e.transition(t, journeyID, StatusPaused, http.StatusConflict)
	e.transition(t, journeyID, StatusActive, http.StatusOK)

	// Enroll (idempotent) → metering + lifecycle event outbox rows.
	code, resp = e.do(t, http.MethodPost, "/v1/studio/journeys/"+journeyID+"/enroll",
		map[string]any{"contact_ids": []string{c1.String()}})
	if code != http.StatusOK || resp["enrolled"].(float64) != 1 {
		t.Fatalf("enroll = %d %v", code, resp)
	}
	code, resp = e.do(t, http.MethodPost, "/v1/studio/journeys/"+journeyID+"/enroll",
		map[string]any{"contact_ids": []string{c1.String()}})
	if code != http.StatusOK || resp["existing"].(float64) != 1 || resp["enrolled"].(float64) != 0 {
		t.Fatalf("enroll replay = %d %v", code, resp)
	}
	var usageRows, eventRows int
	if err := e.store.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE topic='opendesk.usage.events'`).Scan(&usageRows); err != nil {
		t.Fatalf("usage outbox: %v", err)
	}
	if usageRows != 1 { // one new enrollment → exactly one journey_enrolled meter
		t.Fatalf("usage outbox rows = %d, want 1 (replay must not double-meter)", usageRows)
	}
	if err := e.store.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE topic=$1`, DefaultEventsTopic).Scan(&eventRows); err != nil {
		t.Fatalf("events outbox: %v", err)
	}
	if eventRows != 1 { // JourneyEnrolled
		t.Fatalf("events outbox rows = %d, want 1", eventRows)
	}

	// --- Step ---
	// Step 0 is a send: advances + queues; starter receives one batch.
	code, resp = e.do(t, http.MethodPost, "/v1/studio/journeys/"+journeyID+"/step", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("step = %d %v", code, resp)
	}
	if resp["sends_queued"].(float64) != 1 || resp["dispatch"].(string) != "started" || resp["advanced"].(float64) != 1 {
		t.Fatalf("step resp = %v", resp)
	}
	if len(e.starter.batches) != 1 {
		t.Fatalf("starter batches = %d, want 1", len(e.starter.batches))
	}
	batch := e.starter.batches[0]
	if batch.JourneyID != journeyID || batch.TenantSlug != "acme" || len(batch.Sends) != 1 {
		t.Fatalf("batch mismatch: %+v", batch)
	}
	if batch.Sends[0].Text != "Hi Ada, come back!" || batch.Sends[0].Kind != KindSMS {
		t.Fatalf("batch send mismatch: %+v", batch.Sends[0])
	}

	// Next step: the wait (1h) is not due → wait_not_due, no batch.
	code, resp = e.do(t, http.MethodPost, "/v1/studio/journeys/"+journeyID+"/step", nil)
	if code != http.StatusOK || resp["wait_not_due"].(float64) != 1 || resp["sends_queued"].(float64) != 0 {
		t.Fatalf("step2 = %d %v", code, resp)
	}
	if len(e.starter.batches) != 1 {
		t.Fatalf("starter batches = %d after no-op step, want 1", len(e.starter.batches))
	}

	// Stats endpoint reflects the flow.
	code, resp = e.do(t, http.MethodGet, "/v1/studio/journeys/"+journeyID+"/stats", nil)
	if code != http.StatusOK {
		t.Fatalf("stats = %d %v", code, resp)
	}
	stats := resp["stats"].(map[string]any)
	if stats["enrolled"].(float64) != 1 || stats["active"].(float64) != 1 {
		t.Fatalf("stats = %v", stats)
	}
}

// Step on a non-active journey → 409; starter failure → 502 with the
// advancement honestly reported.
func TestStepEdgeCases(t *testing.T) {
	e := newStudioTestEnv(t)
	journeyID := e.mkJourney(t, []map[string]any{
		{"type": "send", "kind": "sms", "template": "x"},
	})

	// Draft → step 409.
	code, _ := e.do(t, http.MethodPost, "/v1/studio/journeys/"+journeyID+"/step", nil)
	if code != http.StatusConflict {
		t.Fatalf("step draft = %d, want 409", code)
	}

	e.transition(t, journeyID, StatusActive, http.StatusOK)
	contactID := seedContact(t, e.store, e.tenant.ID, "Ada", "+234801", "a@b.c", "")
	code, _ = e.do(t, http.MethodPost, "/v1/studio/journeys/"+journeyID+"/enroll",
		map[string]any{"contact_ids": []string{contactID.String()}})
	if code != http.StatusOK {
		t.Fatalf("enroll = %d", code)
	}

	// Starter failure: enrollment advanced (idempotent — a retry finds
	// nothing due) and the dispatch failure surfaces as 502.
	e.starter.err = context.DeadlineExceeded
	code, resp := e.do(t, http.MethodPost, "/v1/studio/journeys/"+journeyID+"/step", nil)
	if code != http.StatusBadGateway {
		t.Fatalf("step dispatch failure = %d %v, want 502", code, resp)
	}
	e.starter.err = nil
	code, resp = e.do(t, http.MethodPost, "/v1/studio/journeys/"+journeyID+"/step", nil)
	if code != http.StatusOK || resp["sends_queued"].(float64) != 0 {
		t.Fatalf("step retry = %d %v — replay must not re-queue the send", code, resp)
	}
	if len(e.starter.batches) != 0 {
		t.Fatalf("starter batches = %d, want 0 (no double-queue)", len(e.starter.batches))
	}
}

// nil starter → due sends defer (sends_deferred) instead of erroring.
func TestStepDefersWithoutStarter(t *testing.T) {
	st := newTestStore(t)
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r := chi.NewRouter()
	RegisterRoutes(r, &Deps{
		Store:             st,
		Starter:           nil,
		TenantFromContext: func(context.Context) bookingops.TenantInfo { return tenant },
	})
	e := &studioTestEnv{router: r, store: st, tenant: tenant}

	journeyID := e.mkJourney(t, []map[string]any{
		{"type": "send", "kind": "sms", "template": "x"},
	})
	e.transition(t, journeyID, StatusActive, http.StatusOK)
	contactID := seedContact(t, st, tenant.ID, "Ada", "+234801", "a@b.c", "")
	if code, _ := e.do(t, http.MethodPost, "/v1/studio/journeys/"+journeyID+"/enroll",
		map[string]any{"contact_ids": []string{contactID.String()}}); code != http.StatusOK {
		t.Fatalf("enroll = %d", code)
	}
	code, resp := e.do(t, http.MethodPost, "/v1/studio/journeys/"+journeyID+"/step", nil)
	if code != http.StatusOK || resp["sends_deferred"].(bool) != true || resp["sends_queued"].(float64) != 0 {
		t.Fatalf("deferred step = %d %v", code, resp)
	}
}
