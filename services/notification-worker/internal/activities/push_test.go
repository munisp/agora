package activities

// SPEC-W16 Agent A: SendPushNotification fan-out + paced-kind integration.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opendesk/notification-worker/internal/daprc"
	"github.com/opendesk/notification-worker/internal/pacer"
	"github.com/opendesk/notification-worker/internal/provider"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// pushTestActivities builds activities with the FCM mock provider and the
// APNs stub wired (the production default set).
func pushTestActivities(t *testing.T) *Activities {
	t.Helper()
	a := pacedTestActivities(nil)
	fcm, err := provider.NewFCM(provider.FCMConfig{Mock: true}, zap.NewNop())
	require.NoError(t, err)
	a.Push = PushDeps{Providers: map[string]provider.PushProvider{
		"fcm":  fcm,
		"apns": &provider.APNS{},
	}}
	return a
}

// deviceFakeDapr emulates daprd: serves GET
// /v1.0/invoke/booking/method/internal/devices?contact_id= with a scripted
// device list and records the forwarded tenant header.
type deviceFakeDapr struct {
	srv *httptest.Server
	mu  sync.Mutex

	devices   []DeviceToken
	gotTenant string
	gotQuery  string
	calls     int
}

func newDeviceFakeDapr(t *testing.T, devices []DeviceToken) *deviceFakeDapr {
	t.Helper()
	f := &deviceFakeDapr{devices: devices}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = body
		f.mu.Lock()
		f.calls++
		f.gotTenant = r.Header.Get("X-Tenant-Slug")
		f.gotQuery = r.URL.RawQuery
		f.mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/v1.0/invoke/booking/method/internal/devices") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.devices)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *deviceFakeDapr) client(t *testing.T) *daprc.Client {
	t.Helper()
	u, err := url.Parse(f.srv.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return daprc.New(u.Hostname(), port)
}

// ---------------------------------------------------------------------------
// Explicit token list fan-out (FCM_MOCK default)
// ---------------------------------------------------------------------------

func TestSendPushNotificationExplicitTokens(t *testing.T) {
	a := pushTestActivities(t)

	res, err := a.SendPushNotification(context.Background(), workflows.PacedPushNotificationSend{
		TenantSlug: "acme",
		Title:      "Booking confirmed",
		Body:       "See you at 10:00",
		Data:       map[string]string{"booking_id": "b-1"},
		Tokens: []workflows.PushTarget{
			{Token: "device-ok", Platform: "android"},
			{Token: "device-web", Platform: "web"},
			{Token: "mock-fail", Platform: "android"},
			{Token: "mock-unregistered", Platform: "android"},
			{Token: "ios-device", Platform: "ios"},
		},
	})
	require.NoError(t, err, "per-token failures are results, not activity errors")
	require.Len(t, res.Results, 5)
	require.Equal(t, 2, res.Sent)
	require.Equal(t, 3, res.Failed)

	byToken := map[string]workflows.PushTokenResult{}
	for _, r := range res.Results {
		byToken[r.Token] = r
	}
	require.True(t, byToken["device-ok"].Success)
	require.Equal(t, "fcm", byToken["device-ok"].Provider)
	require.True(t, byToken["device-web"].Success, "web platform routes to fcm")

	require.False(t, byToken["mock-fail"].Success)
	require.Contains(t, byToken["mock-fail"].Error, "mock")

	require.False(t, byToken["mock-unregistered"].Success)
	require.True(t, byToken["mock-unregistered"].Unregistered, "UNREGISTERED must flag the token for pruning")

	// iOS routes to the APNs STUB: an honest "not implemented" failure.
	require.False(t, byToken["ios-device"].Success)
	require.Equal(t, "apns", byToken["ios-device"].Provider)
	require.Contains(t, byToken["ios-device"].Error, "not")
}

// An unknown platform maps to fcm; a missing provider yields an unroutable
// per-token failure, not a panic.
func TestSendPushNotificationProviderRouting(t *testing.T) {
	a := pushTestActivities(t)
	res, err := a.SendPushNotification(context.Background(), workflows.PacedPushNotificationSend{
		TenantSlug: "acme", Body: "hi",
		Tokens: []workflows.PushTarget{{Token: "tok-no-platform"}},
	})
	require.NoError(t, err)
	require.Len(t, res.Results, 1)
	require.True(t, res.Results[0].Success)
	require.Equal(t, "fcm", res.Results[0].Provider, "empty platform defaults to fcm")

	a.Push = PushDeps{} // no providers wired
	res, err = a.SendPushNotification(context.Background(), workflows.PacedPushNotificationSend{
		TenantSlug: "acme", Body: "hi",
		Tokens: []workflows.PushTarget{{Token: "tok", Platform: "android"}},
	})
	require.NoError(t, err)
	require.False(t, res.Results[0].Success)
	require.Contains(t, res.Results[0].Error, "no push provider configured")
}

// ---------------------------------------------------------------------------
// Device-token fetch via Dapr invoke (contract §1, Agent B endpoint)
// ---------------------------------------------------------------------------

func TestSendPushNotificationFetchesDevices(t *testing.T) {
	devices := []DeviceToken{
		{TenantID: "t-1", ContactID: "c-1", Token: "adm-1", Platform: "android", App: "admin"},
		{TenantID: "t-1", ContactID: "c-1", Token: "fld-1", Platform: "android", App: "field"},
		{TenantID: "t-1", ContactID: "c-1", Token: "ios-1", Platform: "ios", App: "field"},
	}
	dapr := newDeviceFakeDapr(t, devices)
	a := pushTestActivities(t)
	a.Dapr = dapr.client(t)

	// No app filter: every device is targeted.
	res, err := a.SendPushNotification(context.Background(), workflows.PacedPushNotificationSend{
		TenantSlug: "acme", ContactID: "c-1", Title: "Hello",
	})
	require.NoError(t, err)
	require.Len(t, res.Results, 3)
	require.Equal(t, 2, res.Sent) // ios → apns stub failure
	require.Equal(t, 1, res.Failed)

	dapr.mu.Lock()
	require.Equal(t, 1, dapr.calls)
	require.Equal(t, "acme", dapr.gotTenant, "X-Tenant-Slug must be forwarded (service-to-service pattern)")
	require.Equal(t, "contact_id=c-1", dapr.gotQuery)
	dapr.mu.Unlock()

	// App filter: only the field app's devices.
	res, err = a.SendPushNotification(context.Background(), workflows.PacedPushNotificationSend{
		TenantSlug: "acme", ContactID: "c-1", Title: "Hello", App: "field",
	})
	require.NoError(t, err)
	require.Len(t, res.Results, 2)
	for _, r := range res.Results {
		require.NotEqual(t, "adm-1", r.Token)
	}
}

func TestSendPushNotificationNoDevices(t *testing.T) {
	dapr := newDeviceFakeDapr(t, nil)
	a := pushTestActivities(t)
	a.Dapr = dapr.client(t)

	res, err := a.SendPushNotification(context.Background(), workflows.PacedPushNotificationSend{
		TenantSlug: "acme", ContactID: "c-none", Title: "Hello",
	})
	require.NoError(t, err, "a contact without devices is not an error")
	require.Empty(t, res.Results)
	require.Equal(t, 0, res.Sent)
}

// A device-fetch failure IS an activity error (retriable as a whole —
// nothing was delivered yet).
func TestSendPushNotificationDeviceFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	a := pushTestActivities(t)
	a.Dapr = daprc.New(u.Hostname(), port)
	_, err := a.SendPushNotification(context.Background(), workflows.PacedPushNotificationSend{
		TenantSlug: "acme", ContactID: "c-1", Title: "Hello",
	})
	require.ErrorContains(t, err, "fetch device tokens")
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestSendPushNotificationValidation(t *testing.T) {
	a := pushTestActivities(t)
	ctx := context.Background()

	_, err := a.SendPushNotification(ctx, workflows.PacedPushNotificationSend{
		TenantSlug: "acme", Tokens: []workflows.PushTarget{{Token: "t"}},
	})
	require.ErrorContains(t, err, "title or body is required")

	_, err = a.SendPushNotification(ctx, workflows.PacedPushNotificationSend{
		TenantSlug: "acme", Title: "T",
	})
	require.ErrorContains(t, err, "tokens or contact_id is required")
}

// ---------------------------------------------------------------------------
// Paced-kind integration (classification behavior end-to-end)
// ---------------------------------------------------------------------------

// push_notification is TRANSACTIONAL: the DND guard is never consulted and
// the send dispatches immediately through NotifyPaced.
func TestNotifyPacedPushNotificationTransactionalBypass(t *testing.T) {
	a := pushTestActivities(t)
	dnd := &fakeDNDChecker{suppressed: true, reason: pacer.ReasonGlobalDND}
	a.Guards = pacer.NewGuards(pacer.GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())

	res, err := a.NotifyPaced(context.Background(), workflows.PacedSendRequest{
		Kind: workflows.PacedSendPushNotification,
		Push: &workflows.PacedPushNotificationSend{
			TenantSlug: "acme", Title: "Booking confirmed",
			Tokens: []workflows.PushTarget{{Token: "device-ok", Platform: "android"}},
		},
	})
	require.NoError(t, err)
	require.Equal(t, workflows.PacedSendStatusSent, res.Status,
		"transactional push must never be DND-suppressed")
	require.Equal(t, 0, dnd.calls, "transactional kinds must not consult the DND registry")
}

// push_marketing is MARKETING: with a phone in the payload the DND guard
// suppresses before dispatch; without one the guard passes (warn) and the
// send dispatches.
func TestNotifyPacedPushMarketingGuards(t *testing.T) {
	// With phone → suppressed (registry hit), no provider fan-out.
	a := pushTestActivities(t)
	dnd := &fakeDNDChecker{suppressed: true, reason: pacer.ReasonTenantOptOut}
	a.Guards = pacer.NewGuards(pacer.GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())

	res, err := a.NotifyPaced(context.Background(), workflows.PacedSendRequest{
		Kind: workflows.PacedSendPushMarketing,
		Push: &workflows.PacedPushNotificationSend{
			TenantSlug: "acme", Title: "Flash sale", Phone: "+2348012345678",
			Tokens: []workflows.PushTarget{{Token: "device-ok", Platform: "android"}},
		},
	})
	require.NoError(t, err, "suppression is a completion, not an error")
	require.Equal(t, workflows.PacedSendStatusSuppressedDND, res.Status)
	require.Equal(t, pacer.ReasonTenantOptOut, res.Reason)
	require.Equal(t, 1, dnd.calls)
	require.Equal(t, "+2348012345678", dnd.gotPhone)
	require.Equal(t, "acme", dnd.gotTenant)

	// Without phone → guard passes with the no-recipient warn; send goes out.
	a2 := pushTestActivities(t)
	dnd2 := &fakeDNDChecker{}
	a2.Guards = pacer.NewGuards(pacer.GuardConfig{DNDEnforcement: true, DND: dnd2}, zap.NewNop())
	res, err = a2.NotifyPaced(context.Background(), workflows.PacedSendRequest{
		Kind: workflows.PacedSendPushMarketing,
		Push: &workflows.PacedPushNotificationSend{
			TenantSlug: "acme", Title: "Flash sale",
			Tokens: []workflows.PushTarget{{Token: "device-ok", Platform: "android"}},
		},
	})
	require.NoError(t, err)
	require.Equal(t, workflows.PacedSendStatusSent, res.Status)
	require.Equal(t, 0, dnd2.calls, "no phone → registry cannot be consulted (documented gap)")
}

// Dispatch validation for the push kinds.
func TestNotifyPacedPushDispatchValidation(t *testing.T) {
	a := pushTestActivities(t)
	require.ErrorContains(t, notifyPacedErr(a, context.Background(), workflows.PacedSendRequest{
		Kind: workflows.PacedSendPushNotification,
	}), "missing push payload")
}

// The push kinds report the fixed "push" channel (quiet-hours override key).
func TestPushPacedSendChannel(t *testing.T) {
	require.Equal(t, "push", workflows.PacedSendChannel(workflows.PacedSendRequest{Kind: workflows.PacedSendPushNotification}))
	require.Equal(t, "push", workflows.PacedSendChannel(workflows.PacedSendRequest{Kind: workflows.PacedSendPushMarketing}))
}
