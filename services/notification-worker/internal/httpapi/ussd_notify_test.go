package httpapi

// SPEC-W47-C tests: UIMOB-06 USSD action-confirmation intake
// (POST /v1/notify/ussd-action, K2-gated, idempotent workflow id) and GW-02
// (X-Tenant-Slugs plural acceptance on POST /v1/audiences).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/opendesk/notification-worker/internal/activities"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

func ussdActionReq(t *testing.T, s *Server, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/notify/ussd-action", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	NewRouter(s).ServeHTTP(rec, req)
	return rec
}

func TestUSSDActionNotifyGate(t *testing.T) {
	// K2 fail-closed: token unset → 503 even with the right header value.
	s, _ := newAuthedServer()
	s.InternalToken = ""
	rec := ussdActionReq(t, s, `{}`, map[string]string{"X-Internal-Token": "tok-1"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	// Missing / wrong token → 401.
	s, _ = newAuthedServer()
	rec = ussdActionReq(t, s, `{}`, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	rec = ussdActionReq(t, s, `{}`, map[string]string{"X-Internal-Token": "wrong"})
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestUSSDActionNotifyValidation(t *testing.T) {
	s, _ := newAuthedServer()
	auth := map[string]string{"X-Internal-Token": "tok-1"}
	rec := ussdActionReq(t, s, `not-json`, auth)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = ussdActionReq(t, s, `{"tenant_slug":"acme"}`, auth)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUSSDActionNotifyStartsDeterministicWorkflow(t *testing.T) {
	s, ft := newAuthedServer()
	rec := ussdActionReq(t, s,
		`{"tenant_slug":"acme","phone":"+2348012345678","text":"Your booking request was received.","action":"book","session_id":"sess-1"}`,
		map[string]string{"X-Internal-Token": "tok-1"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, []string{"ussd-action-sess-1-book"}, ft.executed)
}

// duplicateTemporal reports WorkflowExecutionAlreadyStarted, as the real
// client does on the second StartWorkflow with the same (running) id.
type duplicateTemporal struct {
	client.Client
}

func (duplicateTemporal) ExecuteWorkflow(context.Context, client.StartWorkflowOptions, interface{}, ...interface{}) (client.WorkflowRun, error) {
	return nil, &serviceerror.WorkflowExecutionAlreadyStarted{}
}

func TestUSSDActionNotifyDuplicateIsIdempotent(t *testing.T) {
	s := &Server{Log: zap.NewNop(), Temporal: duplicateTemporal{}, TaskQueue: "q", InternalToken: "tok-1"}
	rec := ussdActionReq(t, s,
		`{"tenant_slug":"acme","phone":"+2348012345678","text":"hi","action":"handoff","session_id":"sess-1"}`,
		map[string]string{"X-Internal-Token": "tok-1"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"duplicate":true`)
}

func TestUSSDActionNotifyNoTemporal503(t *testing.T) {
	s := &Server{Log: zap.NewNop(), InternalToken: "tok-1"}
	rec := ussdActionReq(t, s,
		`{"tenant_slug":"acme","phone":"+1","text":"hi","action":"info","session_id":"s"}`,
		map[string]string{"X-Internal-Token": "tok-1"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// --- GW-02: X-Tenant-Slugs plural acceptance on POST /v1/audiences ---

type fakeIntake struct {
	req activities.AudienceIntakeRequest
}

func (f *fakeIntake) Intake(_ context.Context, req activities.AudienceIntakeRequest) (activities.AudienceIntakeResult, error) {
	f.req = req
	return activities.AudienceIntakeResult{}, nil
}

func audienceReq(t *testing.T, intake AudienceIntaker, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/audiences",
		strings.NewReader(`{"segment_id":"seg-1","message":"hi","channel":"sms"}`))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux := chi.NewRouter()
	RegisterAudienceRoutes(mux, NewAudienceHandler(intake, zap.NewNop()))
	mux.ServeHTTP(rec, req)
	return rec
}

func TestAudienceAcceptsPluralTenantSlugsHeader(t *testing.T) {
	in := &fakeIntake{}
	// The APISIX api-notifications route injects ONLY the plural
	// membership list — before GW-02 this was a 400.
	rec := audienceReq(t, in, map[string]string{HeaderTenantSlugs: "acme-ng,acme-other"})
	require.NotEqual(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	// First entry scopes single-tenant callers.
	require.Equal(t, "acme-ng", in.req.TenantSlug)
	require.Equal(t, "acme-ng", in.req.TenantID)
}

func TestAudiencePluralPreferredOverSingular(t *testing.T) {
	in := &fakeIntake{}
	// A caller-supplied singular header (stripped at the edge, so only
	// in-cluster callers can even send one) must NOT override the
	// gateway-injected plural membership.
	rec := audienceReq(t, in, map[string]string{
		HeaderTenantSlugs: "acme-ng",
		"X-Tenant-Slug":   "caller-supplied",
	})
	require.NotEqual(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, "acme-ng", in.req.TenantSlug)
}

func TestAudienceSingularFallbackPreserved(t *testing.T) {
	in := &fakeIntake{}
	// Legacy pre-gateway / direct callers: singular still works.
	rec := audienceReq(t, in, map[string]string{"X-Tenant-Slug": "acme-ng"})
	require.NotEqual(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, "acme-ng", in.req.TenantSlug)
}

func TestAudienceNoTenantHeaderStill400(t *testing.T) {
	rec := audienceReq(t, &fakeIntake{}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}
