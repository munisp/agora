package activities

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/opendesk/notification-worker/internal/daprc"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// geoTestActivities wires an Activities whose Dapr client points at the
// given httptest fake sidecar.
func geoTestActivities(t *testing.T, daprURL string) *Activities {
	t.Helper()
	u, err := url.Parse(daprURL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return New(daprc.New(u.Hostname(), port), "booking", "payments", "identity",
		"bindings-smtp", "bindings-twilio", "no-reply@test", "+10000000000", "", IndustryDeps{}, zap.NewNop())
}

// whatsapp/telegram channels dispatch to the messaging-gateway HTTP
// binding convention ("bindings-"+channel, operation post, {to, message}).
func TestSendGeoCampaignMessageTelegram(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	a := geoTestActivities(t, srv.URL)
	err := a.SendGeoCampaignMessage(context.Background(), workflows.PacedGeoCampaignSend{
		TenantSlug: "acme",
		CampaignID: "c-1",
		Channel:    "telegram",
		Phone:      "+23480000001",
		Text:       "Hi Ada, flash sale!",
	})
	require.NoError(t, err)
	require.Equal(t, "/v1.0/bindings/bindings-telegram", gotPath)
	require.Equal(t, "post", gotBody["operation"])
	data, ok := gotBody["data"].(map[string]any)
	require.True(t, ok, "binding data: %v", gotBody)
	require.Equal(t, "+23480000001", data["to"])
	require.Equal(t, "Hi Ada, flash sale!", data["message"])
}

// Validation: unknown channel, missing phone/text fail fast.
func TestSendGeoCampaignMessageValidation(t *testing.T) {
	a := geoTestActivities(t, "http://127.0.0.1:1")
	ctx := context.Background()

	require.ErrorContains(t, a.SendGeoCampaignMessage(ctx, workflows.PacedGeoCampaignSend{
		Channel: "sms", Text: "x"}), "phone is required")
	require.ErrorContains(t, a.SendGeoCampaignMessage(ctx, workflows.PacedGeoCampaignSend{
		Channel: "sms", Phone: "+1", Text: "x"}), "text is required")
	require.ErrorContains(t, a.SendGeoCampaignMessage(ctx, workflows.PacedGeoCampaignSend{
		Channel: "pigeon", Phone: "+1", Text: "x"}), "unknown channel")
}

// NotifyPaced routes the geo_campaign kind to SendGeoCampaignMessage and
// rejects a missing payload.
func TestNotifyPacedGeoCampaignDispatch(t *testing.T) {
	a := pacedTestActivities(nil)
	require.ErrorContains(t, notifyPacedErr(a, context.Background(),
		workflows.PacedSendRequest{Kind: workflows.PacedSendGeoCampaign}), "missing geo_campaign payload")

	// With a payload the dispatch reaches the send (which then fails on the
	// unreachable fake sidecar — proving it was routed).
	err := notifyPacedErr(a, context.Background(), workflows.PacedSendRequest{
		Kind: workflows.PacedSendGeoCampaign,
		GeoCampaign: &workflows.PacedGeoCampaignSend{
			Channel: "telegram", Phone: "+1", Text: "hi",
		},
	})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "missing geo_campaign payload")
}
