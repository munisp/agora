package activities

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/stretchr/testify/require"
)

// SPEC-W56-B: SendReceiptReconAlert severity mapping (pinned decision 5),
// ops-channel routing, the structural no-customer-fields guarantee,
// fail-closed validation, and log-only degradation — mirroring the
// payment_mismatch activity test pattern.

func receiptReconSend() workflows.ReceiptReconAlertSend {
	return workflows.ReceiptReconAlertSend{
		FindingID:           "f-123",
		TenantID:            "t-1",
		TenantSlug:          "acme",
		Kind:                "amount_mismatch",
		Confidence:          "exact",
		CaptureClientID:     "cap-9",
		BookingID:           "b-42",
		AttemptTxRef:        "tx_abc",
		ExpectedAmountMinor: 500000,
		CapturedAmountMinor: 450000,
		ReceiptAmountMajor:  4500.5,
		Detail:              map[string]any{"currency_unknown": true, "amount_window_days": float64(3)},
	}
}

// Severity mapping (SPEC-W56 pinned decision 5): receipt_without_payment
// and amount_mismatch are high; payment_without_receipt is info — softer
// than the W49 critical mismatch. Unknown/future kinds (incl.
// currency_unknown) degrade to info.
func TestReceiptReconSeverityMapping(t *testing.T) {
	for kind, want := range map[string]string{
		"receipt_without_payment": "high",
		"amount_mismatch":         "high",
		"payment_without_receipt": "info",
		"currency_unknown":        "info",
		"some_future_kind":        "info",
	} {
		require.Equal(t, want, receiptReconSeverity(kind), "kind %s", kind)
	}
}

// Message content: kind, tenant slug, finding id, confidence, and — for
// amount_mismatch — both minor-unit amounts, the major-unit receipt amount,
// and the currency honesty note.
func TestReceiptReconMessageContent(t *testing.T) {
	msg := receiptReconMessage(receiptReconSend())
	require.Contains(t, msg, "RECEIPT RECONCILIATION")
	require.Contains(t, msg, "tenant acme")
	require.Contains(t, msg, "amount_mismatch")
	require.Contains(t, msg, "f-123")
	require.Contains(t, msg, "confidence exact")
	require.Contains(t, msg, "expected 500000 minor")
	require.Contains(t, msg, "captured 450000 minor")
	require.Contains(t, msg, "receipt 4500.5 major")
	require.Contains(t, msg, "currency unknown", "receipts carry no currency — the message must say so")
}

// Routing: exactly one CloudEvent to the OPS-alerts topic (never a customer
// binding), type com.opendesk.ops.ReceiptReconFinding, severity from the
// kind mapping, keyed by finding id, all contract fields + detail in the
// data payload.
func TestSendReceiptReconAlertPublishesToOpsTopic(t *testing.T) {
	dapr := newFakeDapr(t)
	prod := &fakeTrajectoryProducer{}
	a := pacedTestActivities(nil)
	a.Dapr = dapr.client(t)
	a.Ops = OpsAlertDeps{Producer: prod, Topic: DefaultOpsAlertsTopic}

	require.NoError(t, a.SendReceiptReconAlert(context.Background(), receiptReconSend()))

	require.Equal(t, 1, prod.count(), "exactly one ops alert event")
	row := prod.rows[0]
	require.Equal(t, DefaultOpsAlertsTopic, row.topic, "routes via the existing ops-alert channel binding")
	require.Equal(t, "f-123", row.key, "keyed by finding id, mirroring EmitOpsAlert's anchor keying")

	var evt map[string]any
	require.NoError(t, json.Unmarshal(row.payload, &evt))
	require.Equal(t, EventTypeReceiptReconFinding, evt["type"])
	require.Equal(t, "com.opendesk.ops.ReceiptReconFinding", evt["type"])
	require.Equal(t, "notification-worker", evt["source"])
	require.Equal(t, "t-1", evt["tenantid"])
	require.NotEmpty(t, evt["id"], "CloudEvent id present for opsalerts-consumer dedupe")
	data, ok := evt["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "high", data["severity"], "amount_mismatch → high (pinned decision 5)")
	require.Equal(t, "t-1", data["tenant_id"])
	require.Equal(t, "acme", data["tenant_slug"])
	require.Equal(t, "f-123", data["finding_id"])
	require.Equal(t, "amount_mismatch", data["kind"])
	require.Equal(t, "exact", data["confidence"])
	require.Equal(t, "cap-9", data["capture_client_id"])
	require.Equal(t, "b-42", data["booking_id"])
	require.Equal(t, "tx_abc", data["attempt_tx_ref"])
	require.Equal(t, float64(500000), data["expected_amount_minor"])
	require.Equal(t, float64(450000), data["captured_amount_minor"])
	require.InDelta(t, 4500.5, data["receipt_amount_major"], 1e-9)
	require.Equal(t, map[string]any{"currency_unknown": true, "amount_window_days": float64(3)}, data["detail"],
		"finding detail (incl. currency_unknown honesty flag) rides through")
	require.Contains(t, data["message"], "RECEIPT RECONCILIATION")

	// NO-CUSTOMER-SEND assertion: no Dapr binding invocation of any kind
	// (no sms/email/whatsapp to the customer contact) — the ops topic is
	// the only egress.
	dapr.mu.Lock()
	defer dapr.mu.Unlock()
	require.Empty(t, dapr.invokeCalls, "recon alerts must never touch a customer-facing binding")
}

// Severity lands in the emitted payload for every kind (all three contract
// kinds, exercised end-to-end through the activity).
func TestSendReceiptReconAlertSeverityPerKind(t *testing.T) {
	for kind, want := range map[string]string{
		"receipt_without_payment": "high",
		"amount_mismatch":         "high",
		"payment_without_receipt": "info",
	} {
		t.Run(kind, func(t *testing.T) {
			prod := &fakeTrajectoryProducer{}
			a := pacedTestActivities(nil)
			a.Ops = OpsAlertDeps{Producer: prod, Topic: DefaultOpsAlertsTopic}
			in := receiptReconSend()
			in.Kind = kind
			require.NoError(t, a.SendReceiptReconAlert(context.Background(), in))
			var evt map[string]any
			require.NoError(t, json.Unmarshal(prod.rows[0].payload, &evt))
			require.Equal(t, want, evt["data"].(map[string]any)["severity"])
		})
	}
}

// Absence honesty: optional anchors/amounts/detail that were absent in the
// source event are OMITTED from the ops payload — never fabricated zeros.
func TestSendReceiptReconAlertOmitsAbsentOptionalFields(t *testing.T) {
	prod := &fakeTrajectoryProducer{}
	a := pacedTestActivities(nil)
	a.Ops = OpsAlertDeps{Producer: prod, Topic: DefaultOpsAlertsTopic}
	in := workflows.ReceiptReconAlertSend{
		FindingID:  "f-min",
		TenantID:   "t-1",
		TenantSlug: "acme",
		Kind:       "receipt_without_payment",
		Confidence: "none",
	}
	require.NoError(t, a.SendReceiptReconAlert(context.Background(), in))
	var evt map[string]any
	require.NoError(t, json.Unmarshal(prod.rows[0].payload, &evt))
	data := evt["data"].(map[string]any)
	for _, k := range []string{
		"capture_client_id", "booking_id", "attempt_tx_ref",
		"expected_amount_minor", "captured_amount_minor", "receipt_amount_major", "detail",
	} {
		require.NotContains(t, data, k, "absent optional field %s must stay absent", k)
	}
	require.Equal(t, "high", data["severity"], "receipt_without_payment → high (pinned decision 5)")
}

// Structural discipline: the cross-scope payload struct carries NO customer
// contact fields — not by convention but by construction — so a recon alert
// can never leak customer PII to the ops channel or vice versa.
func TestReceiptReconPayloadStructurallyHasNoCustomerFields(t *testing.T) {
	typ := reflect.TypeOf(workflows.ReceiptReconAlertSend{})
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.ToLower(typ.Field(i).Tag.Get("json"))
		name := strings.ToLower(typ.Field(i).Name)
		for _, banned := range []string{"customer", "contact", "phone", "email", "recipient"} {
			require.NotContains(t, tag, banned, "json tag of field %s must not reference %s", name, banned)
			require.NotContains(t, name, banned, "field name %s must not reference %s", name, banned)
		}
	}
}

// Log-only degradation (mirrors EmitOpsAlert): nil producer or "off"/empty
// topic still logs and returns nil.
func TestSendReceiptReconAlertLogOnlyDegradation(t *testing.T) {
	for name, ops := range map[string]OpsAlertDeps{
		"nil producer": {Producer: nil, Topic: DefaultOpsAlertsTopic},
		"topic off":    {Producer: &fakeTrajectoryProducer{}, Topic: "off"},
		"topic empty":  {Producer: &fakeTrajectoryProducer{}, Topic: ""},
	} {
		t.Run(name, func(t *testing.T) {
			a := pacedTestActivities(nil)
			a.Ops = ops
			require.NoError(t, a.SendReceiptReconAlert(context.Background(), receiptReconSend()))
		})
	}
}

// Fail-closed: missing finding_id/kind is an error, never a silent no-op
// (the bridge already fail-closes such events).
func TestSendReceiptReconAlertMissingAnchorsFails(t *testing.T) {
	a := pacedTestActivities(nil)
	in := receiptReconSend()
	in.FindingID = ""
	require.ErrorContains(t, a.SendReceiptReconAlert(context.Background(), in), "missing finding_id/kind")
	in = receiptReconSend()
	in.Kind = ""
	require.ErrorContains(t, a.SendReceiptReconAlert(context.Background(), in), "missing finding_id/kind")
}

// Publish failures surface as activity errors so Temporal retries them —
// the alert is the only signal of a receipt/payment divergence.
func TestSendReceiptReconAlertProducerErrorSurfaces(t *testing.T) {
	a := pacedTestActivities(nil)
	a.Ops = OpsAlertDeps{Producer: &fakeTrajectoryProducer{fail: true}, Topic: DefaultOpsAlertsTopic}
	require.ErrorContains(t, a.SendReceiptReconAlert(context.Background(), receiptReconSend()),
		"publish receipt recon ops alert")
}
