package activities

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/opendesk/notification-worker/internal/pacer"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// SPEC-W51-A: SendPaymentMismatchAlert message rendering (both amounts),
// ops-channel routing, the no-customer-send guarantee, fail-closed
// validation, and the transactional/system DND classification.

func paymentMismatchSend() workflows.PacedPaymentMismatchSend {
	return workflows.PacedPaymentMismatchSend{
		BookingID:           "b-42",
		TenantID:            "t-1",
		TenantSlug:          "acme",
		TxRef:               "tx_mm789",
		AmountMinor:         450000,
		ExpectedAmountMinor: 500000,
		Currency:            "NGN",
		BookingRef:          "BK-7XQ4V2PZ",
		PaidAt:              "2030-01-01T10:00:00Z",
	}
}

// Message content: tenant, human booking ref + id, tx_ref, EXPECTED vs
// CAPTURED amounts (both present, FormatMinorAmount-rendered), paid_at.
func TestPaymentMismatchMessageContent(t *testing.T) {
	msg := paymentMismatchMessage(paymentMismatchSend())
	require.Contains(t, msg, "PAYMENT AMOUNT MISMATCH")
	require.Contains(t, msg, "tenant acme")
	require.Contains(t, msg, "BK-7XQ4V2PZ", "human public ref present")
	require.Contains(t, msg, "b-42", "booking id present alongside the ref")
	require.Contains(t, msg, "expected NGN 5,000.00", "expected amount rendered from expected_amount_minor")
	require.Contains(t, msg, "captured NGN 4,500.00", "captured amount rendered from amount_minor")
	require.Contains(t, msg, "tx_mm789")
	require.Contains(t, msg, "2030-01-01T10:00:00Z", "paid_at present")
}

// booking_ref falls back to booking_id when the producer could not resolve
// the human public ref; empty paid_at is omitted.
func TestPaymentMismatchMessageRefFallback(t *testing.T) {
	in := paymentMismatchSend()
	in.BookingRef = ""
	in.PaidAt = ""
	msg := paymentMismatchMessage(in)
	require.Contains(t, msg, "booking b-42 (b-42)")
	require.NotContains(t, msg, "Paid at")
}

// Routing: exactly one CloudEvent to the OPS-alerts topic (never a customer
// binding), type com.opendesk.ops.PaymentAmountMismatch, severity critical,
// keyed by booking id, both amounts in the data payload.
func TestSendPaymentMismatchAlertPublishesToOpsTopic(t *testing.T) {
	dapr := newFakeDapr(t)
	prod := &fakeTrajectoryProducer{}
	a := pacedTestActivities(nil)
	a.Dapr = dapr.client(t)
	a.Ops = OpsAlertDeps{Producer: prod, Topic: DefaultOpsAlertsTopic}

	require.NoError(t, a.SendPaymentMismatchAlert(context.Background(), paymentMismatchSend()))

	require.Equal(t, 1, prod.count(), "exactly one ops alert event")
	row := prod.rows[0]
	require.Equal(t, DefaultOpsAlertsTopic, row.topic, "routes via the existing ops-alert channel binding")
	require.Equal(t, "b-42", row.key, "keyed by booking id, mirroring EmitOpsAlert")

	var evt map[string]any
	require.NoError(t, json.Unmarshal(row.payload, &evt))
	require.Equal(t, EventTypePaymentAmountMismatch, evt["type"])
	require.Equal(t, "com.opendesk.ops.PaymentAmountMismatch", evt["type"])
	require.Equal(t, "notification-worker", evt["source"])
	require.Equal(t, "t-1", evt["tenantid"])
	require.NotEmpty(t, evt["id"], "CloudEvent id present for opsalerts-consumer dedupe")
	data, ok := evt["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "critical", data["severity"])
	require.Equal(t, "b-42", data["booking_id"])
	require.Equal(t, "BK-7XQ4V2PZ", data["booking_ref"])
	require.Equal(t, "tx_mm789", data["tx_ref"])
	require.Equal(t, float64(500000), data["expected_amount_minor"])
	require.Equal(t, float64(450000), data["captured_amount_minor"])
	require.Contains(t, data["message"], "expected NGN 5,000.00, captured NGN 4,500.00")

	// NO-CUSTOMER-SEND assertion: no Dapr binding invocation of any kind
	// (no sms/email/whatsapp to the customer contact) — the ops topic is
	// the only egress.
	dapr.mu.Lock()
	defer dapr.mu.Unlock()
	require.Empty(t, dapr.invokeCalls, "mismatch alerts must never touch a customer-facing binding")
}

// Log-only degradation (mirrors EmitOpsAlert): nil producer or "off" topic
// still logs CRITICAL and returns nil.
func TestSendPaymentMismatchAlertLogOnlyDegradation(t *testing.T) {
	for name, ops := range map[string]OpsAlertDeps{
		"nil producer": {Producer: nil, Topic: DefaultOpsAlertsTopic},
		"topic off":    {Producer: &fakeTrajectoryProducer{}, Topic: "off"},
		"topic empty":  {Producer: &fakeTrajectoryProducer{}, Topic: ""},
	} {
		t.Run(name, func(t *testing.T) {
			a := pacedTestActivities(nil)
			a.Ops = ops
			require.NoError(t, a.SendPaymentMismatchAlert(context.Background(), paymentMismatchSend()))
		})
	}
}

// Fail-closed: missing booking_id/tx_ref is an error, never a silent no-op
// (the bridge already fail-closes such events).
func TestSendPaymentMismatchAlertMissingAnchorsFails(t *testing.T) {
	a := pacedTestActivities(nil)
	in := paymentMismatchSend()
	in.BookingID = ""
	require.ErrorContains(t, a.SendPaymentMismatchAlert(context.Background(), in), "missing booking_id/tx_ref")
	in = paymentMismatchSend()
	in.TxRef = ""
	require.ErrorContains(t, a.SendPaymentMismatchAlert(context.Background(), in), "missing booking_id/tx_ref")
}

// Publish failures surface as activity errors so Temporal retries them —
// the alert is the only signal of a capture/price divergence.
func TestSendPaymentMismatchAlertProducerErrorSurfaces(t *testing.T) {
	a := pacedTestActivities(nil)
	a.Ops = OpsAlertDeps{Producer: &fakeTrajectoryProducer{fail: true}, Topic: DefaultOpsAlertsTopic}
	require.ErrorContains(t, a.SendPaymentMismatchAlert(context.Background(), paymentMismatchSend()),
		"publish payment mismatch ops alert")
}

// ops_alert is SYSTEM/TRANSACTIONAL: classified like the payment receipt,
// so marketing DND suppression never blocks an alert — the DND registry is
// not even consulted (the ops-addressed payload has no recipient phone),
// and the alert dispatches despite a registry hit.
func TestPaymentMismatchAlertIsTransactionalNotDNDBlocked(t *testing.T) {
	require.Equal(t, pacer.ClassTransactional, pacer.ClassifyKind(workflows.PacedSendOpsAlert),
		"ops_alert must be transactional/system-class, exactly like payment_receipt")

	dapr := newFakeDapr(t)
	dnd := &fakeDNDChecker{suppressed: true, reason: pacer.ReasonGlobalDND}
	guards := pacer.NewGuards(pacer.GuardConfig{DNDEnforcement: true, DND: dnd}, zap.NewNop())
	prod := &fakeTrajectoryProducer{}
	a := pacedTestActivities(nil)
	a.Dapr = dapr.client(t)
	a.Guards = guards
	a.Ops = OpsAlertDeps{Producer: prod, Topic: DefaultOpsAlertsTopic}

	res, err := a.NotifyPaced(context.Background(), workflows.PacedSendRequest{
		Kind:            workflows.PacedSendOpsAlert,
		PaymentMismatch: ptrMismatch(paymentMismatchSend()),
	})
	require.NoError(t, err)
	require.Equal(t, workflows.PacedSendStatusSent, res.Status,
		"a DND registry hit must NOT suppress an ops alert")
	require.Equal(t, 0, dnd.calls, "transactional kinds must not consult the DND registry")
	require.Empty(t, guards.SuppressedStats())
	require.Equal(t, 1, prod.count(), "the alert must publish despite the DND hit")

	dapr.mu.Lock()
	defer dapr.mu.Unlock()
	require.Empty(t, dapr.invokeCalls, "still no customer-facing binding call")
}

// NotifyPaced dispatch validation for the new kind.
func TestNotifyPacedOpsAlertValidation(t *testing.T) {
	a := pacedTestActivities(nil)
	require.ErrorContains(t, notifyPacedErr(a, context.Background(),
		workflows.PacedSendRequest{Kind: workflows.PacedSendOpsAlert}),
		"missing payment_mismatch payload")
}

func ptrMismatch(s workflows.PacedPaymentMismatchSend) *workflows.PacedPaymentMismatchSend {
	return &s
}
