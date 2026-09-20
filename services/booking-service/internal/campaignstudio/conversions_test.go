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

// SPEC-W45 U6 conversion webhook tests (embedded Postgres + an httptest
// model-registry): the conversion is recorded durably, the outcome report
// is fail-closed, and validation/enrollment mismatches are honest.

type conversionTestEnv struct {
	router       chi.Router
	tenant       bookingops.TenantInfo
	outcomeHits  []outcomeRequest
	registry     *httptest.Server
	journeyID    uuid.UUID
	enrollmentID uuid.UUID
}

func newConversionTestEnv(t *testing.T, registryStatus int) *conversionTestEnv {
	t.Helper()
	st := newTestStore(t)
	e := &conversionTestEnv{tenant: bookingops.TenantInfo{ID: uuid.New(), Slug: "acme", Name: "Acme"}}
	e.registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req outcomeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		e.outcomeHits = append(e.outcomeHits, req)
		w.WriteHeader(registryStatus)
	}))
	t.Cleanup(e.registry.Close)
	e.router = chi.NewRouter()
	RegisterRoutes(e.router, &Deps{
		Store: st,
		TenantFromContext: func(context.Context) bookingops.TenantInfo {
			return e.tenant
		},
		Registry: &RegistryClient{BaseURL: e.registry.URL, InternalToken: "tok"},
	})
	// Active journey with one enrolled contact.
	j := mkActiveJourney(t, st, e.tenant.ID, Steps{
		{Type: StepSend, Kind: KindSMS, Template: "Hi", ExperimentID: ptrUUID(uuid.New())},
	})
	contactID := seedContact(t, st, e.tenant.ID, "Ada", "+2348011111111", "a@b.c", "")
	created, _, err := st.Enroll(context.Background(), e.tenant.ID, j.ID, []uuid.UUID{contactID})
	if err != nil || len(created) != 1 {
		t.Fatalf("enroll: %v (%d created)", err, len(created))
	}
	e.journeyID = j.ID
	e.enrollmentID = created[0].ID
	return e
}

func ptrUUID(id uuid.UUID) *uuid.UUID { return &id }

func (e *conversionTestEnv) post(t *testing.T, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/studio/journeys/"+e.journeyID.String()+"/conversions", &buf)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestReportConversionHappyPath(t *testing.T) {
	e := newConversionTestEnv(t, http.StatusCreated)
	experimentID := uuid.New()
	code, body := e.post(t, map[string]any{
		"enrollment_id": e.enrollmentID,
		"experiment_id": experimentID,
		"variant":       ChallengerArm,
		"converted":     true,
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d body %v, want 200", code, body)
	}
	if body["recorded"] != true || body["registry_report"] != "ok" {
		t.Fatalf("body = %v, want recorded + registry ok", body)
	}
	if len(e.outcomeHits) != 1 {
		t.Fatalf("registry hits = %d, want 1", len(e.outcomeHits))
	}
	hit := e.outcomeHits[0]
	if hit.AssignedArm != ChallengerArm || hit.PersonID != e.enrollmentID.String() ||
		hit.TenantID != e.tenant.ID || hit.TrueLabel != 1 || hit.PredictedLabel != 1 || hit.PredictedScore != 0.5 {
		t.Fatalf("outcome = %+v, want challenger neutral report with true_label=1", hit)
	}
}

// Fail-closed: a registry that rejects the outcome answers 502 (the
// recorded conversion keeps the signal replayable).
func TestReportConversionRegistryFailureIs502(t *testing.T) {
	e := newConversionTestEnv(t, http.StatusInternalServerError)
	code, body := e.post(t, map[string]any{
		"enrollment_id": e.enrollmentID,
		"experiment_id": uuid.New(),
		"variant":       ControlArm,
		"converted":     false,
	})
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d body %v, want 502 (fail closed)", code, body)
	}
}

func TestReportConversionValidation(t *testing.T) {
	e := newConversionTestEnv(t, http.StatusCreated)

	// Bad variant.
	code, _ := e.post(t, map[string]any{
		"enrollment_id": e.enrollmentID, "experiment_id": uuid.New(), "variant": "B",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("bad variant = %d, want 400", code)
	}
	// Missing experiment.
	code, _ = e.post(t, map[string]any{
		"enrollment_id": e.enrollmentID, "variant": ControlArm,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("missing experiment = %d, want 400", code)
	}
	// Unknown enrollment → 404 (no cross-journey guessing).
	code, _ = e.post(t, map[string]any{
		"enrollment_id": uuid.New(), "experiment_id": uuid.New(), "variant": ControlArm,
	})
	if code != http.StatusNotFound {
		t.Fatalf("unknown enrollment = %d, want 404", code)
	}
	if len(e.outcomeHits) != 0 {
		t.Fatalf("registry hits = %d, want 0 (validation failures never reach the registry)", len(e.outcomeHits))
	}
}
