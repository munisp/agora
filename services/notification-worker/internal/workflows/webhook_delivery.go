package workflows

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Webhook delivery (Wave 5 #10): one WebhookDeliveryWorkflow per
// subscription×event. The workflow owns the retry schedule with durable
// timers, so a worker restart never loses a pending retry.

// Activity names (registered in cmd/worker/main.go).
const (
	ActivityDeliverWebhookHTTP    = "DeliverWebhookHTTP"
	ActivityUpdateWebhookDelivery = "UpdateWebhookDelivery"
)

// WebhookBackoff is the retry schedule AFTER the initial attempt:
// 1m, 5m, 15m, 1h, 4h — then the delivery is marked dlq.
var WebhookBackoff = []time.Duration{
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
	4 * time.Hour,
}

// Payload types (SPEC-W11 Part B §4): empty/"cloudevent" is the Wave-5
// behavior (CloudEvents envelope, sha256=-prefixed signature); "incident"
// delivers a raw Incident Data Packet with a plain-hex HMAC signature and
// the X-OpenDesk-Incident header, and persists attempt outcomes to
// booking-service's incident_deliveries ledger via Dapr invocation.
const (
	PayloadTypeCloudEvent = "cloudevent"
	PayloadTypeIncident   = "incident"
)

// WebhookDeliveryInput starts a WebhookDeliveryWorkflow.
type WebhookDeliveryInput struct {
	DeliveryID string `json:"delivery_id"`
	URL        string `json:"url"`
	Secret     string `json:"secret"`
	EventType  string `json:"event_type"`
	// PayloadType selects the delivery shape: "" / "cloudevent" (default) or
	// "incident" (SPEC-W11 Part B §4).
	PayloadType string `json:"payload_type,omitempty"`
	// IncidentID is set for payload type "incident" (X-OpenDesk-Incident).
	IncidentID string `json:"incident_id,omitempty"`
	// Body is the raw CloudEvents envelope (or IDP JSON for payload type
	// "incident"), POSTed verbatim.
	Body []byte `json:"body"`
}

// WebhookDeliveryUpdate is the persistence update after each attempt.
type WebhookDeliveryUpdate struct {
	DeliveryID string `json:"delivery_id"`
	// PayloadType routes the persistence path ("" → webhook_deliveries
	// table; "incident" → booking-service incident_deliveries via Dapr).
	PayloadType string     `json:"payload_type,omitempty"`
	Status      string     `json:"status"` // retrying | delivered | dlq
	Attempts    int        `json:"attempts"`
	StatusCode  int        `json:"status_code"` // 0 = transport error
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`
}

// WebhookDeliveryWorkflow delivers one webhook with up to
// 1+len(WebhookBackoff) attempts. Terminal states (delivered, dlq) are
// persisted before completion; the workflow itself never fails on receiver
// errors — dlq IS the failure signal.
func WebhookDeliveryWorkflow(ctx workflow.Context, in WebhookDeliveryInput) error {
	ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		// The workflow owns the retry schedule; Temporal must not retry the
		// HTTP attempt itself (it would bypass the backoff + bookkeeping).
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	maxAttempts := len(WebhookBackoff) + 1
	log := workflow.GetLogger(ctx)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var statusCode int
		err := workflow.ExecuteActivity(ao, ActivityDeliverWebhookHTTP, in).Get(ctx, &statusCode)
		if err != nil {
			statusCode = 0 // transport-level failure (DNS, connect, timeout)
		}
		upd := WebhookDeliveryUpdate{DeliveryID: in.DeliveryID, PayloadType: in.PayloadType, Attempts: attempt, StatusCode: statusCode}
		switch {
		case err == nil && statusCode >= 200 && statusCode < 300:
			upd.Status = "delivered"
			// A bookkeeping failure must not redeliver — swallow with a log.
			if uerr := workflow.ExecuteActivity(ao, ActivityUpdateWebhookDelivery, upd).Get(ctx, nil); uerr != nil {
				log.Warn("webhook delivery bookkeeping failed (terminal delivered)", "delivery_id", in.DeliveryID, "error", uerr)
			}
			return nil
		case attempt == maxAttempts:
			upd.Status = "dlq"
			// A bookkeeping failure must not redeliver — swallow with a log.
			if uerr := workflow.ExecuteActivity(ao, ActivityUpdateWebhookDelivery, upd).Get(ctx, nil); uerr != nil {
				log.Warn("webhook delivery bookkeeping failed (terminal dlq)", "delivery_id", in.DeliveryID, "error", uerr)
			}
			return nil
		default:
			upd.Status = "retrying"
			next := workflow.Now(ao).Add(WebhookBackoff[attempt-1])
			upd.NextRetryAt = &next
			if uerr := workflow.ExecuteActivity(ao, ActivityUpdateWebhookDelivery, upd).Get(ctx, nil); uerr != nil {
				// Persistence is broken; retrying blindly would hammer the
				// receiver — fail the run so it surfaces in Temporal.
				return uerr
			}
			if serr := workflow.Sleep(ao, WebhookBackoff[attempt-1]); serr != nil {
				return serr
			}
		}
	}
	return nil
}
