package referrals

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Mock provider (deterministic, PAYOUT_MOCK=1 default)
// ---------------------------------------------------------------------------

func TestMockProviderDeterministic(t *testing.T) {
	m := MockProvider{}
	id := uuid.New()
	in := TransferRequest{
		PayoutID:      id,
		TenantID:      uuid.New(),
		BeneficiaryID: "agent-1",
		AmountNGN:     150_00,
		Currency:      "NGN",
		Reference:     PayoutReference(id),
	}
	r1, err := m.Transfer(context.Background(), in)
	require.NoError(t, err)
	r2, err := m.Transfer(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, r1, r2, "mock transfer must be deterministic")
	require.Equal(t, in.Reference, r1.ProviderRef)
	require.Equal(t, "success", r1.Status)

	st, err := m.TransferStatus(context.Background(), r1.ProviderRef)
	require.NoError(t, err)
	require.Equal(t, "success", st.Status)

	st, err = m.TransferStatus(context.Background(), "never-issued")
	require.NoError(t, err)
	require.Equal(t, "failed", st.Status, "unknown ref ⇒ failed (recon mismatch)")
}

func TestMockProviderFailureHooks(t *testing.T) {
	m := MockProvider{}
	in := TransferRequest{PayoutID: uuid.New(), BeneficiaryID: "mock-fail", Reference: "x"}
	_, err := m.Transfer(context.Background(), in)
	require.ErrorContains(t, err, "declined")

	in.BeneficiaryID = "mock-pending"
	res, err := m.Transfer(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, "pending", res.Status)
}

func TestPayoutReferenceDeterministic(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	a, b := PayoutReference(id), PayoutReference(id)
	require.Equal(t, a, b)
	require.Contains(t, a, "cpay_")
	require.NotEqual(t, a, PayoutReference(uuid.New()))
}

// ---------------------------------------------------------------------------
// Paystack-shape client against an httptest fake (ASSUMPTION shape)
// ---------------------------------------------------------------------------

// paystackFake serves the assumed Paystack transfer API.
func paystackFake(t *testing.T, wantSecret string, transferStatus string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantSecret != "" {
			require.Equal(t, "Bearer "+wantSecret, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/transfer":
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			require.NoError(t, json.Unmarshal(body, &req))
			require.Equal(t, "balance", req["source"])
			require.NotEmpty(t, req["recipient"])
			require.NotEmpty(t, req["reference"])
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  true,
				"message": "Transfer has been queued",
				"data": map[string]any{
					"reference":     req["reference"],
					"transfer_code": "TRF_abc123",
					"status":        transferStatus,
					"amount":        req["amount"],
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/transfer/TRF_abc123":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": true,
				"data": map[string]any{
					"reference":     "ref-1",
					"transfer_code": "TRF_abc123",
					"status":        transferStatus,
					"amount":        15000,
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": false, "message": "not found"})
		}
	}))
}

func TestPaystackClientTransfer(t *testing.T) {
	srv := paystackFake(t, "sk_test_secret", "success")
	defer srv.Close()
	c := NewPaystackClient(srv.URL, "sk_test_secret")
	require.Equal(t, ProviderPaystack, c.Name())

	id := uuid.New()
	res, err := c.Transfer(context.Background(), TransferRequest{
		PayoutID:      id,
		TenantID:      uuid.New(),
		BeneficiaryID: "RCP_1",
		AmountNGN:     150_00,
		Currency:      "NGN",
		Reason:        "commission payout",
		Reference:     PayoutReference(id),
	})
	require.NoError(t, err)
	require.Equal(t, "TRF_abc123", res.ProviderRef)
	require.Equal(t, "success", res.Status)

	st, err := c.TransferStatus(context.Background(), "TRF_abc123")
	require.NoError(t, err)
	require.Equal(t, "success", st.Status)
	require.Equal(t, int64(15000), st.AmountNGN)
}

func TestPaystackClientHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := NewPaystackClient(srv.URL, "")
	_, err := c.Transfer(context.Background(), TransferRequest{PayoutID: uuid.New(), Reference: "r"})
	require.ErrorContains(t, err, "HTTP 502")
}

func TestPaystackClientStatusFalseEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": false, "message": "Insufficient balance"})
	}))
	defer srv.Close()
	c := NewPaystackClient(srv.URL, "")
	_, err := c.Transfer(context.Background(), TransferRequest{PayoutID: uuid.New(), Reference: "r"})
	require.ErrorContains(t, err, "Insufficient balance")
}

// ---------------------------------------------------------------------------
// Env selection (contract §7)
// ---------------------------------------------------------------------------

func TestProviderFromEnvDefaultsToMock(t *testing.T) {
	t.Setenv(EnvPayoutMock, "")
	require.Equal(t, ProviderMock, ProviderFromEnv().Name(), "unset PAYOUT_MOCK ⇒ mock default")
	t.Setenv(EnvPayoutMock, "1")
	require.Equal(t, ProviderMock, ProviderFromEnv().Name())
	t.Setenv(EnvPayoutMock, "0")
	t.Setenv(EnvPayoutProvider, "")
	require.Equal(t, ProviderPaystack, ProviderFromEnv().Name(), "mock off + no provider ⇒ paystack")
	t.Setenv(EnvPayoutProvider, "flutterwave")
	require.Equal(t, ProviderFlutterwave, ProviderFromEnv().Name())
}

func TestMinPayoutFromEnv(t *testing.T) {
	t.Setenv(EnvPayoutMinNGN, "")
	require.Equal(t, int64(100_00), MinPayoutFromEnv(), "default ₦100 ⇒ 10000 kobo")
	t.Setenv(EnvPayoutMinNGN, "250")
	require.Equal(t, int64(250_00), MinPayoutFromEnv())
	t.Setenv(EnvPayoutMinNGN, "bogus")
	require.Equal(t, int64(100_00), MinPayoutFromEnv())
}

// ---------------------------------------------------------------------------
// Recon mismatch classification (pure function, table-driven)
// ---------------------------------------------------------------------------

func TestClassifyMismatch(t *testing.T) {
	cases := []struct {
		ledger, provider, want string
	}{
		{PayoutStatusPaid, "success", ""},
		{PayoutStatusPaid, "failed", MismatchPaidNotSuccessful},
		{PayoutStatusPaid, "reversed", MismatchPaidNotSuccessful},
		{PayoutStatusPaid, "pending", MismatchPaidNotSuccessful},
		{PayoutStatusProcessing, "success", MismatchProcessingSucceeded},
		{PayoutStatusProcessing, "failed", MismatchProcessingFailed},
		{PayoutStatusProcessing, "reversed", MismatchProcessingFailed},
		{PayoutStatusProcessing, "pending", ""},
		{PayoutStatusQueued, "success", ""},
		{PayoutStatusFailed, "failed", ""},
	}
	for _, c := range cases {
		require.Equal(t, c.want, classifyMismatch(c.ledger, c.provider),
			"ledger=%s provider=%s", c.ledger, c.provider)
	}
}

// ---------------------------------------------------------------------------
// Usage metering payload
// ---------------------------------------------------------------------------

func TestMarshalPayoutUsageRecord(t *testing.T) {
	p := Payout{
		ID:            uuid.New(),
		TenantID:      uuid.New(),
		BeneficiaryID: "agent-9",
		AmountNGN:     500_00,
		Provider:      ProviderPaystack,
	}
	raw, err := MarshalPayoutUsageRecord("acme", p)
	require.NoError(t, err)
	var evt map[string]any
	require.NoError(t, json.Unmarshal(raw, &evt))
	require.Equal(t, "com.opendesk.usage.UsageRecord", evt["type"])
	data := evt["data"].(map[string]any)
	require.Equal(t, UsageMetricCommissionPayout, data["metric"])
	require.Equal(t, float64(1), data["value"])
	meta := data["meta"].(map[string]any)
	require.Equal(t, p.ID.String(), meta["payout_id"])
	require.Equal(t, float64(500_00), meta["amount_ngn"])
}
