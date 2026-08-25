package activities

// SPEC-W44 K2/K5 tests: the saga/civic/twin activities forward the peer
// internal tokens (X-Internal-Token via Dapr header passthrough) and the K5
// slug namespace; HoldDeposit decodes canonical deposit_id with the hold_id
// legacy fallback (C2); twin cleanup targets the internauth-guarded
// internal route (404 → success).

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

	"github.com/opendesk/notification-worker/internal/daprc"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakePeerDapr emulates a daprd sidecar with per-path response bodies;
// records path, method, body AND headers.
type fakePeerDapr struct {
	srv     *httptest.Server
	mu      sync.Mutex
	calls   []recordedCall
	respond map[string]peerResponse
}

type recordedCall struct {
	method  string
	path    string
	body    []byte
	headers http.Header
}

type peerResponse struct {
	status int
	body   string
}

func newFakePeerDapr(t *testing.T, respond map[string]peerResponse) *fakePeerDapr {
	t.Helper()
	f := &fakePeerDapr{respond: respond}
	if f.respond == nil {
		f.respond = map[string]peerResponse{}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		f.mu.Lock()
		f.calls = append(f.calls, recordedCall{r.Method, r.URL.Path, body, r.Header.Clone()})
		f.mu.Unlock()
		resp, ok := f.respond[r.URL.Path]
		if !ok {
			resp = peerResponse{status: 200}
		}
		w.WriteHeader(resp.status)
		if resp.body != "" {
			_, _ = w.Write([]byte(resp.body))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePeerDapr) client(t *testing.T) *daprc.Client {
	t.Helper()
	u, err := url.Parse(f.srv.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return daprc.New(u.Hostname(), port)
}

func (f *fakePeerDapr) last(t *testing.T) recordedCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.calls)
	return f.calls[len(f.calls)-1]
}

func sagaIn() workflows.SagaInput {
	return workflows.SagaInput{
		BookingID: "b1", TenantID: "uuid-1", TenantSlug: "acme",
		PriceCents: 10000, Currency: "NGN",
	}
}

// newPeerActivities builds Activities against the fake sidecar.
func newPeerActivities(d *daprc.Client) *Activities {
	return New(d, "booking", "payments", "identity", "smtp", "twilio", "from@x", "+100", "", IndustryDeps{}, zap.NewNop())
}

func TestHoldDepositCanonicalAndLegacyIDs(t *testing.T) {
	// Canonical deposit_id (C2).
	fake := newFakePeerDapr(t, map[string]peerResponse{
		"/v1.0/invoke/payments/method/activities/hold-deposit": {200, `{"deposit_id":"dep-9"}`},
	})
	a := newPeerActivities(fake.client(t))
	a.PaymentsInternalToken = "pay-tok"
	id, err := a.HoldDeposit(context.Background(), sagaIn())
	require.NoError(t, err)
	require.Equal(t, "dep-9", id)
	call := fake.last(t)
	require.Equal(t, "pay-tok", call.headers.Get("X-Internal-Token"), "K2: PAYMENTS_INTERNAL_TOKEN forwarded")
	var payload map[string]any
	require.NoError(t, json.Unmarshal(call.body, &payload))
	require.Equal(t, "acme", payload["tenant_slug"], "K5: slug namespace")
	require.Equal(t, "acme", payload["tenant_id"])

	// Legacy hold_id fallback.
	fake.respond["/v1.0/invoke/payments/method/activities/hold-deposit"] = peerResponse{200, `{"hold_id":"hold-3"}`}
	id, err = a.HoldDeposit(context.Background(), sagaIn())
	require.NoError(t, err)
	require.Equal(t, "hold-3", id)

	// Neither → error (fail loud, never invent ids).
	fake.respond["/v1.0/invoke/payments/method/activities/hold-deposit"] = peerResponse{200, `{}`}
	_, err = a.HoldDeposit(context.Background(), sagaIn())
	require.Error(t, err)
}

func TestHoldDepositTenantSlugFallbackWarn(t *testing.T) {
	fake := newFakePeerDapr(t, map[string]peerResponse{
		"/v1.0/invoke/payments/method/activities/hold-deposit": {200, `{"deposit_id":"d"}`},
	})
	a := newPeerActivities(fake.client(t))
	in := sagaIn()
	in.TenantSlug = ""
	_, err := a.HoldDeposit(context.Background(), in)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(fake.last(t).body, &payload))
	require.Equal(t, "uuid-1", payload["tenant_slug"], "K5 fallback: tenant_id with WARN")
}

func TestHoldDepositDirectHTTPFallback(t *testing.T) {
	// PAYMENTS_URL set → direct HTTP (no Dapr), same token + payload.
	var got recordedCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = recordedCall{r.Method, r.URL.Path, b, r.Header.Clone()}
		w.Write([]byte(`{"deposit_id":"dep-direct"}`))
	}))
	t.Cleanup(srv.Close)
	a := newPeerActivities(nil)
	a.PaymentsURL = srv.URL
	a.PaymentsInternalToken = "pay-tok"
	id, err := a.HoldDeposit(context.Background(), sagaIn())
	require.NoError(t, err)
	require.Equal(t, "dep-direct", id)
	require.Equal(t, "/activities/hold-deposit", got.path)
	require.Equal(t, "pay-tok", got.headers.Get("X-Internal-Token"))
}

func TestSagaActivitiesInternalTokens(t *testing.T) {
	fake := newFakePeerDapr(t, nil)
	a := newPeerActivities(fake.client(t))
	a.BookingInternalToken = "book-tok"
	ctx := context.Background()

	require.NoError(t, a.ReserveSlot(ctx, sagaIn()))
	require.Equal(t, "book-tok", fake.last(t).headers.Get("X-Internal-Token"))

	require.NoError(t, a.ReleaseSlot(ctx, sagaIn(), "reason"))
	require.Equal(t, "book-tok", fake.last(t).headers.Get("X-Internal-Token"))

	require.NoError(t, a.ConfirmBooking(ctx, sagaIn()))
	require.Equal(t, "book-tok", fake.last(t).headers.Get("X-Internal-Token"))

	require.NoError(t, a.MarkNoShow(ctx, workflows.NoShowInput{BookingID: "b1", TenantID: "uuid-1", TenantSlug: "acme"}))
	require.Equal(t, "book-tok", fake.last(t).headers.Get("X-Internal-Token"))

	require.NoError(t, a.SeedTenantData(ctx, workflows.OnboardingInput{TenantID: "uuid-1", Slug: "acme"}))
	require.Equal(t, "book-tok", fake.last(t).headers.Get("X-Internal-Token"))

	a.IdentityInternalToken = "id-tok"
	require.NoError(t, a.EnsureKeycloakGroup(ctx, workflows.OnboardingInput{Slug: "acme"}))
	require.Equal(t, "id-tok", fake.last(t).headers.Get("X-Internal-Token"))
	require.NoError(t, a.EnsurePermifyTenant(ctx, workflows.OnboardingInput{Slug: "acme"}))
	require.Equal(t, "id-tok", fake.last(t).headers.Get("X-Internal-Token"))
}

func TestDeleteTwinTenantInternalRoute(t *testing.T) {
	fake := newFakePeerDapr(t, nil)
	a := newPeerActivities(fake.client(t))
	a.IdentityInternalToken = "id-tok"
	ctx := context.Background()

	require.NoError(t, a.DeleteTwinTenant(ctx, workflows.TwinCleanupInput{TenantID: "uuid", Slug: "acme-twin-x"}))
	call := fake.last(t)
	require.Equal(t, http.MethodDelete, call.method)
	require.Equal(t, "/v1.0/invoke/identity/method/internal/tenants/acme-twin-x", call.path,
		"W44: twin cleanup deletes via the internauth-guarded internal route")
	require.Equal(t, "id-tok", call.headers.Get("X-Internal-Token"))

	// 404 → treated as success (already gone).
	fake.respond["/v1.0/invoke/identity/method/internal/tenants/acme-twin-x"] = peerResponse{404, `{"error":"not found"}`}
	require.NoError(t, a.DeleteTwinTenant(ctx, workflows.TwinCleanupInput{TenantID: "uuid", Slug: "acme-twin-x"}))

	// 500 → error.
	fake.respond["/v1.0/invoke/identity/method/internal/tenants/acme-twin-x"] = peerResponse{500, `{"error":"boom"}`}
	require.Error(t, a.DeleteTwinTenant(ctx, workflows.TwinCleanupInput{TenantID: "uuid", Slug: "acme-twin-x"}))

	// Non-twin slug → refused locally, no HTTP call.
	require.Error(t, a.DeleteTwinTenant(ctx, workflows.TwinCleanupInput{TenantID: "uuid", Slug: "acme"}))
}

func salonDepositIn() workflows.SalonDepositInput {
	return workflows.SalonDepositInput{
		BookingID: "b1", TenantID: "uuid-1", TenantSlug: "acme",
		HoldID: "dep-9", NoShowFeeCents: 2500, Currency: "NGN",
	}
}

func TestChargeNoShowFeeRequestShape(t *testing.T) {
	// V1 (claim-6): the no-show charge goes through the payments invoke
	// helper — K2 token header, K5 slug namespace, deterministic
	// idempotency_key (noshowfee-{booking_id}).
	fake := newFakePeerDapr(t, nil)
	a := newPeerActivities(fake.client(t))
	a.PaymentsInternalToken = "pay-tok"
	require.NoError(t, a.ChargeNoShowFee(context.Background(), salonDepositIn()))
	call := fake.last(t)
	require.Equal(t, http.MethodPost, call.method)
	require.Equal(t, "/v1.0/invoke/payments/method/v1/no-show-fee", call.path)
	require.Equal(t, "pay-tok", call.headers.Get("X-Internal-Token"), "K2: PAYMENTS_INTERNAL_TOKEN forwarded")
	var payload map[string]any
	require.NoError(t, json.Unmarshal(call.body, &payload))
	require.Equal(t, "acme", payload["tenant_slug"], "K5: slug namespace")
	require.Equal(t, "acme", payload["tenant_id"])
	require.Equal(t, "dep-9", payload["deposit_id"])
	require.Equal(t, "noshowfee-b1", payload["idempotency_key"], "deterministic idempotency key")

	// K5 fallback: uuid tenant + WARN when the slug is missing.
	in := salonDepositIn()
	in.TenantSlug = ""
	require.NoError(t, a.ChargeNoShowFee(context.Background(), in))
	require.NoError(t, json.Unmarshal(fake.last(t).body, &payload))
	require.Equal(t, "uuid-1", payload["tenant_slug"])

	// No hold → local error, no HTTP call.
	calls := len(fake.calls)
	require.Error(t, a.ChargeNoShowFee(context.Background(), workflows.SalonDepositInput{BookingID: "b2", TenantSlug: "acme"}))
	require.Len(t, fake.calls, calls)
}

func TestChargeNoShowFeeDirectHTTPFallback(t *testing.T) {
	// PAYMENTS_URL set → direct HTTP (no Dapr), same token + payload shape.
	var got recordedCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = recordedCall{r.Method, r.URL.Path, b, r.Header.Clone()}
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	a := newPeerActivities(nil)
	a.PaymentsURL = srv.URL
	a.PaymentsInternalToken = "pay-tok"
	require.NoError(t, a.ChargeNoShowFee(context.Background(), salonDepositIn()))
	require.Equal(t, "/v1/no-show-fee", got.path)
	require.Equal(t, "pay-tok", got.headers.Get("X-Internal-Token"))
	var payload map[string]any
	require.NoError(t, json.Unmarshal(got.body, &payload))
	require.Equal(t, "acme", payload["tenant_slug"])
	require.Equal(t, "noshowfee-b1", payload["idempotency_key"])
}

func TestVerifyDepositHoldRequestShape(t *testing.T) {
	// V1 (claim-6): the balance probe is a GET through the payments invoke
	// helper — K2 token header, K5 slug in the account path.
	fake := newFakePeerDapr(t, map[string]peerResponse{
		"/v1.0/invoke/payments/method/v1/accounts/acme/balance": {200, `{"accounts":[{"debits_pending":500}]}`},
	})
	a := newPeerActivities(fake.client(t))
	a.PaymentsInternalToken = "pay-tok"
	ok, err := a.VerifyDepositHold(context.Background(), salonDepositIn())
	require.NoError(t, err)
	require.True(t, ok, "pending debits ⇒ open hold")
	call := fake.last(t)
	require.Equal(t, http.MethodGet, call.method)
	require.Equal(t, "pay-tok", call.headers.Get("X-Internal-Token"), "K2: PAYMENTS_INTERNAL_TOKEN forwarded")
	require.Empty(t, call.body, "GET sends no request body")

	// No pending amounts → false.
	fake.respond["/v1.0/invoke/payments/method/v1/accounts/acme/balance"] = peerResponse{200, `{"accounts":[{"debits_pending":0,"credits_pending":0}]}`}
	ok, err = a.VerifyDepositHold(context.Background(), salonDepositIn())
	require.NoError(t, err)
	require.False(t, ok)

	// K5 fallback: uuid tenant (with WARN) addresses the account path.
	in := salonDepositIn()
	in.TenantSlug = ""
	fake.respond["/v1.0/invoke/payments/method/v1/accounts/uuid-1/balance"] = peerResponse{200, `{"accounts":[]}`}
	_, err = a.VerifyDepositHold(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, "/v1.0/invoke/payments/method/v1/accounts/uuid-1/balance", fake.last(t).path)
}

func TestVerifyDepositHoldDirectHTTPFallback(t *testing.T) {
	var got recordedCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = recordedCall{r.Method, r.URL.Path, nil, r.Header.Clone()}
		w.Write([]byte(`{"accounts":[{"credits_pending":1}]}`))
	}))
	t.Cleanup(srv.Close)
	a := newPeerActivities(nil)
	a.PaymentsURL = srv.URL
	a.PaymentsInternalToken = "pay-tok"
	ok, err := a.VerifyDepositHold(context.Background(), salonDepositIn())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, http.MethodGet, got.method)
	require.Equal(t, "/v1/accounts/acme/balance", got.path)
	require.Equal(t, "pay-tok", got.headers.Get("X-Internal-Token"))
}

func TestCivicSLABreachInternalToken(t *testing.T) {
	fake := newFakePeerDapr(t, nil)
	a := newPeerActivities(fake.client(t))
	a.BookingInternalToken = "book-tok"
	require.NoError(t, a.ReportCivicSLABreach(context.Background(), workflows.CivicSLABreachReport{
		Ref: "ref-1", TenantSlug: "acme", Kind: workflows.CivicBreachKindAck,
	}))
	call := fake.last(t)
	require.Equal(t, "book-tok", call.headers.Get("X-Internal-Token"))
	require.Equal(t, "acme", call.headers.Get("X-Tenant-Slug"))
}
