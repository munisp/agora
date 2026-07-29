package activities

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/opendesk/notification-worker/internal/daprc"
	"github.com/opendesk/notification-worker/internal/pacer"
	"github.com/opendesk/notification-worker/internal/webhooks"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeDapr emulates a daprd sidecar: captures invoke/binding calls, 200s.
type fakeDapr struct {
	srv *httptest.Server
	mu  sync.Mutex
	// invokeCalls records "appID/method" → raw body.
	invokeCalls map[string][]byte
	bindings    []string
}

func newFakeDapr(t *testing.T) *fakeDapr {
	t.Helper()
	f := &fakeDapr{invokeCalls: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		f.mu.Lock()
		f.invokeCalls[r.URL.Path] = body
		f.bindings = append(f.bindings, r.URL.Path)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDapr) client(t *testing.T) *daprc.Client {
	t.Helper()
	u, err := url.Parse(f.srv.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return daprc.New(u.Hostname(), port)
}

// NotifyPaced priority fast-lane end-to-end (SPEC-W11 Part B §5): with the
// CPS bucket drained, an incident_alert priority send still dispatches
// IMMEDIATELY through the channel router (twilio binding here) and stays
// metered.
func TestNotifyPacedIncidentAlertPriorityLane(t *testing.T) {
	dapr := newFakeDapr(t)
	p := pacer.New(pacer.Config{CPS: 0.1, Burst: 1, Backend: "local"}, zap.NewNop())
	a := pacedTestActivities(p)
	a.Dapr = dapr.client(t)
	ctx := context.Background()

	// Drain the single burst token with a normal (non-priority) send.
	require.NoError(t, a.NotifyPaced(ctx, waitlistClaimReq()))

	req := workflows.PacedSendRequest{
		Kind:     workflows.PacedSendIncidentAlert,
		Priority: true,
		IncidentAlert: &workflows.PacedIncidentAlertSend{
			TenantSlug: "acme-ng",
			IncidentID: "11111111-1111-1111-1111-111111111111",
			Channel:    ChannelSMS,
			Phone:      "+2348012345678",
			Text:       "EMERGENCY ALERT INC-2026-000123: fire incident reported. Severity: critical.",
		},
	}
	start := time.Now()
	require.NoError(t, a.NotifyPaced(ctx, req))
	require.Less(t, time.Since(start), 500*time.Millisecond,
		"priority incident_alert must bypass the exhausted bucket (10s refill)")

	// The send actually went out (twilio binding invoke).
	dapr.mu.Lock()
	_, sent := dapr.invokeCalls["/v1.0/bindings/bindings-twilio"]
	dapr.mu.Unlock()
	require.True(t, sent, "incident alert must invoke the sms binding")

	// Metered: 1 bucket grant (drain) + 1 priority bypass.
	granted, priority := p.Stats()
	require.Equal(t, uint64(1), granted)
	require.Equal(t, uint64(1), priority)
}

// incident_alert dispatch validation.
func TestNotifyPacedIncidentAlertValidation(t *testing.T) {
	a := pacedTestActivities(nil)
	ctx := context.Background()
	require.ErrorContains(t, a.NotifyPaced(ctx, workflows.PacedSendRequest{Kind: workflows.PacedSendIncidentAlert}),
		"missing incident_alert payload")
	require.ErrorContains(t, a.SendIncidentAlert(ctx, workflows.PacedIncidentAlertSend{Text: "x"}),
		"phone is required")
	require.ErrorContains(t, a.SendIncidentAlert(ctx, workflows.PacedIncidentAlertSend{Phone: "+1"}),
		"text is required")
	require.ErrorContains(t, a.SendIncidentAlert(ctx, workflows.PacedIncidentAlertSend{Phone: "+1", Text: "x", Channel: "pager"}),
		"unknown channel")
}

// Incident delivery HTTP shape (SPEC-W11 Part B §4): raw IDP JSON,
// X-OpenDesk-Incident header, plain-hex HMAC signature (known vector from
// python3 hmac).
func TestDeliverWebhookHTTPIncidentHeaders(t *testing.T) {
	body := []byte(`{"incident_id":"11111111-1111-1111-1111-111111111111","severity":"critical"}`)
	var gotHeaders http.Header
	var gotBody []byte
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	a := pacedTestActivities(nil)
	code, err := a.DeliverWebhookHTTP(context.Background(), workflows.WebhookDeliveryInput{
		DeliveryID:  "d-inc-1",
		URL:         receiver.URL,
		Secret:      "top-secret",
		PayloadType: workflows.PayloadTypeIncident,
		IncidentID:  "11111111-1111-1111-1111-111111111111",
		Body:        body,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, body, gotBody, "IDP posted verbatim")
	require.Equal(t, "application/json", gotHeaders.Get("Content-Type"))
	require.Equal(t, "11111111-1111-1111-1111-111111111111", gotHeaders.Get(webhooks.HeaderIncident))
	// Known vector: python3 hmac.new(b'top-secret', body, hashlib.sha256).hexdigest()
	require.Equal(t, "2b514d4c2cd1a494ad8b4a05145975ec3cedb88fbff4017e3e20bdab0a618ca6",
		gotHeaders.Get(webhooks.HeaderSignature), "plain-hex HMAC (no sha256= prefix)")
}

// Incident attempt updates route to booking-service's ledger via Dapr
// (payload type "incident"), NOT to the local webhook store.
func TestUpdateWebhookDeliveryIncidentRoutesToBooking(t *testing.T) {
	dapr := newFakeDapr(t)
	a := pacedTestActivities(nil)
	a.Dapr = dapr.client(t)
	a.Webhooks = WebhookDeps{BookingAppID: "booking"} // no Store: must not be touched

	err := a.UpdateWebhookDelivery(context.Background(), workflows.WebhookDeliveryUpdate{
		DeliveryID:  "d-inc-1",
		PayloadType: workflows.PayloadTypeIncident,
		Status:      "retrying",
		Attempts:    2,
		StatusCode:  500,
	})
	require.NoError(t, err)

	dapr.mu.Lock()
	body, ok := dapr.invokeCalls["/v1.0/invoke/booking/method/internal/incidents/deliveries/d-inc-1"]
	dapr.mu.Unlock()
	require.True(t, ok, "incident update must invoke booking-service ledger endpoint")
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(body, &parsed))
	require.Equal(t, "retrying", parsed["status"])
	require.EqualValues(t, 2, parsed["attempts"])
	require.EqualValues(t, 500, parsed["status_code"])
}
