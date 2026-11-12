package activities

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/opendesk/notification-worker/internal/daprc"
	"github.com/opendesk/notification-worker/internal/pacer"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// SPEC-W50-B: SendPaymentReceipt channel choice, message rendering,
// amount formatting, provider-error propagation, and the
// DND-transactional classification.

func paymentReceiptSend() workflows.PacedPaymentReceiptSend {
	return workflows.PacedPaymentReceiptSend{
		BookingID:    "b-42",
		TenantID:     "t-1",
		TenantSlug:   "acme",
		TxRef:        "tx_abc123",
		AmountMinor:  500000,
		Currency:     "NGN",
		CustomerName: "Ada",
		ContactPhone: "+2348012345678",
		ContactEmail: "ada@example.com",
		BookingRef:   "BK-1042",
	}
}

// decodeBindingBody unwraps the Dapr binding envelope
// {"operation","data","metadata"} captured by fakeDapr.
func decodeBindingBody(t *testing.T, raw []byte) (data any, metadata map[string]string) {
	t.Helper()
	var env struct {
		Operation string            `json:"operation"`
		Data      any               `json:"data"`
		Metadata  map[string]string `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(raw, &env))
	return env.Data, env.Metadata
}

// Phone present → the sms channel binding fires (twilio by default), the
// email binding does not, and the message carries the formatted amount,
// human booking ref, and tx ref.
func TestSendPaymentReceiptSMSPath(t *testing.T) {
	dapr := newFakeDapr(t)
	a := pacedTestActivities(nil)
	a.Dapr = dapr.client(t)

	require.NoError(t, a.SendPaymentReceipt(context.Background(), paymentReceiptSend()))

	dapr.mu.Lock()
	smsBody, smsOK := dapr.invokeCalls["/v1.0/bindings/bindings-twilio"]
	_, emailOK := dapr.invokeCalls["/v1.0/bindings/bindings-smtp"]
	dapr.mu.Unlock()
	require.True(t, smsOK, "phone present must dispatch the sms binding")
	require.False(t, emailOK, "phone present must NOT also send email")

	data, meta := decodeBindingBody(t, smsBody)
	text, ok := data.(string)
	require.True(t, ok, "twilio binding data is the message text, got %T", data)
	require.Contains(t, text, "Payment received — NGN 5,000.00 for booking BK-1042. Ref: tx_abc123. Thank you!")
	require.Equal(t, "+2348012345678", meta["toNumber"])
}

// Per-tenant channel routing: a tenant mapped to termii sends via the
// messaging-gateway HTTP binding ("post", {"to","message"}), not twilio.
func TestSendPaymentReceiptTenantChannelRouting(t *testing.T) {
	dapr := newFakeDapr(t)
	a := pacedTestActivities(nil)
	a.Dapr = dapr.client(t)
	r, err := NewChannelRouter("", `{"acme": {"sms": "termii"}}`)
	require.NoError(t, err)
	a.Channels = r

	require.NoError(t, a.SendPaymentReceipt(context.Background(), paymentReceiptSend()))

	dapr.mu.Lock()
	gwBody, gwOK := dapr.invokeCalls["/v1.0/bindings/bindings-termii"]
	_, twilioOK := dapr.invokeCalls["/v1.0/bindings/bindings-twilio"]
	dapr.mu.Unlock()
	require.True(t, gwOK, "tenant override must route sms to termii")
	require.False(t, twilioOK)

	data, meta := decodeBindingBody(t, gwBody)
	msg, ok := data.(map[string]any)
	require.True(t, ok, "gateway binding data is {to,message}, got %T", data)
	require.Equal(t, "+2348012345678", msg["to"])
	require.Contains(t, msg["message"], "NGN 5,000.00")
	require.Nil(t, meta)
}

// No phone → email fallback via the smtp binding.
func TestSendPaymentReceiptEmailFallback(t *testing.T) {
	dapr := newFakeDapr(t)
	a := pacedTestActivities(nil)
	a.Dapr = dapr.client(t)

	in := paymentReceiptSend()
	in.ContactPhone = ""
	require.NoError(t, a.SendPaymentReceipt(context.Background(), in))

	dapr.mu.Lock()
	emailBody, emailOK := dapr.invokeCalls["/v1.0/bindings/bindings-smtp"]
	_, smsOK := dapr.invokeCalls["/v1.0/bindings/bindings-twilio"]
	dapr.mu.Unlock()
	require.True(t, emailOK, "no phone must fall back to the smtp binding")
	require.False(t, smsOK)

	data, meta := decodeBindingBody(t, emailBody)
	require.Contains(t, data.(string), "NGN 5,000.00 for booking BK-1042")
	require.Equal(t, "ada@example.com", meta["emailTo"])
	require.Equal(t, "no-reply@test", meta["emailFrom"])
	require.Equal(t, "Payment received", meta["subject"])
}

// booking_ref falls back to booking_id when the producer could not resolve
// the human reference.
func TestSendPaymentReceiptBookingRefFallback(t *testing.T) {
	dapr := newFakeDapr(t)
	a := pacedTestActivities(nil)
	a.Dapr = dapr.client(t)

	in := paymentReceiptSend()
	in.BookingRef = ""
	require.NoError(t, a.SendPaymentReceipt(context.Background(), in))

	dapr.mu.Lock()
	body := dapr.invokeCalls["/v1.0/bindings/bindings-twilio"]
	dapr.mu.Unlock()
	data, _ := decodeBindingBody(t, body)
	require.Contains(t, data.(string), "for booking b-42.")
}

// Both contacts empty is a contract failure: an error, never a silent
// no-op (the bridge already fail-closes these events).
func TestSendPaymentReceiptNoContactFails(t *testing.T) {
	a := pacedTestActivities(nil)
	in := paymentReceiptSend()
	in.ContactPhone, in.ContactEmail = "", ""
	require.ErrorContains(t, a.SendPaymentReceipt(context.Background(), in), "no reachable contact")
}

// Provider errors surface as activity errors (Temporal retries them).
func TestSendPaymentReceiptProviderErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)

	a := pacedTestActivities(nil)
	a.Dapr = daprc.New(u.Hostname(), port)
	err = a.SendPaymentReceipt(context.Background(), paymentReceiptSend())
	require.ErrorContains(t, err, "twilio binding", "provider failure must surface (retryable)")
}

// Amount formatting: minor→major with the currency code prefix and
// thousands separators, no new deps.
func TestFormatMinorAmount(t *testing.T) {
	for want, args := range map[string][2]any{
		"NGN 5,000.00":     {"NGN", int64(500000)},
		"USD 0.99":         {"USD", int64(99)},
		"USD 1.00":         {"USD", int64(100)},
		"NGN 0.00":         {"NGN", int64(0)},
		"USD 1,234,567.89": {"USD", int64(123456789)},
		"USD 12.30":        {"USD", int64(1230)},
	} {
		require.Equal(t, want, FormatMinorAmount(args[0].(string), args[1].(int64)))
	}
}

// payment_receipt is TRANSACTIONAL: classified like confirmation, so
// marketing DND suppression never blocks a receipt — the DND registry is
// not even consulted, and the send dispatches despite a registry hit.
func TestPaymentReceiptIsTransactionalNotDNDBlocked(t *testing.T) {
	require.Equal(t, pacer.ClassTransactional, pacer.ClassifyKind(workflows.PacedSendPaymentReceipt),
		"payment_receipt must be transactional-class, exactly like confirmation")

	dapr := newFakeDapr(t)
	dnd := &fakeDNDChecker{suppressed: true, reason: pacer.ReasonGlobalDND}
	guards := pacer.NewGuards(pacer.GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())
	a := pacedTestActivities(nil)
	a.Dapr = dapr.client(t)
	a.Guards = guards

	res, err := a.NotifyPaced(context.Background(), workflows.PacedSendRequest{
		Kind:           workflows.PacedSendPaymentReceipt,
		PaymentReceipt: ptrReceipt(paymentReceiptSend()),
	})
	require.NoError(t, err)
	require.Equal(t, workflows.PacedSendStatusSent, res.Status,
		"a DND registry hit must NOT suppress a payment receipt")
	require.Equal(t, 0, dnd.calls, "transactional kinds must not consult the DND registry")
	require.Empty(t, guards.SuppressedStats())

	dapr.mu.Lock()
	_, sent := dapr.invokeCalls["/v1.0/bindings/bindings-twilio"]
	dapr.mu.Unlock()
	require.True(t, sent, "the receipt must dispatch despite the DND hit")
}

// NotifyPaced dispatch validation for the new kind.
func TestNotifyPacedPaymentReceiptValidation(t *testing.T) {
	a := pacedTestActivities(nil)
	require.ErrorContains(t, notifyPacedErr(a, context.Background(),
		workflows.PacedSendRequest{Kind: workflows.PacedSendPaymentReceipt}),
		"missing payment_receipt payload")
}

func ptrReceipt(s workflows.PacedPaymentReceiptSend) *workflows.PacedPaymentReceiptSend {
	return &s
}
