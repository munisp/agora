package activities

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/webhooks"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.uber.org/zap"
)

// WebhookStore is the persistence slice of the webhook delivery activities
// (*store.Store satisfies it; tests use a fake).
type WebhookStore interface {
	UpdateDelivery(ctx context.Context, id uuid.UUID, status string, attempts int, statusCode *int, nextRetryAt *time.Time) error
}

// WebhookDeps bundles the webhook delivery activity dependencies; set by
// main after New (nil Store disables persistence updates).
type WebhookDeps struct {
	Store WebhookStore
	// HTTPClient posts deliveries; nil → a 15s-timeout default client.
	HTTPClient *http.Client
	// BookingAppID is the Dapr app-id of booking-service; required for
	// payload type "incident" attempt updates (incident_deliveries ledger,
	// SPEC-W11 Part B §4).
	BookingAppID string
}

// DeliverWebhookHTTP performs ONE signed POST of the CloudEvents envelope to
// the subscriber URL and returns the HTTP status code (0 on transport
// error). Signing: X-OpenDesk-Signature carries hex HMAC-SHA256 of the raw
// body with the subscription secret (Stripe-style "sha256=" prefix), plus
// X-OpenDesk-Event / X-OpenDesk-Timestamp / X-OpenDesk-Delivery headers.
func (a *Activities) DeliverWebhookHTTP(ctx context.Context, in workflows.WebhookDeliveryInput) (int, error) {
	hc := http.DefaultClient
	if a.Webhooks.HTTPClient != nil {
		hc = a.Webhooks.HTTPClient
	} else {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, in.URL, bytes.NewReader(in.Body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set(webhooks.HeaderTimestamp, strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set(webhooks.HeaderDelivery, in.DeliveryID)
	if in.PayloadType == workflows.PayloadTypeIncident {
		// SPEC-W11 Part B §4: raw IDP JSON, plain-hex HMAC signature and the
		// X-OpenDesk-Incident header.
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(webhooks.HeaderIncident, in.IncidentID)
		if sig := webhooks.SignatureHex(in.Secret, in.Body); sig != "" {
			req.Header.Set(webhooks.HeaderSignature, sig)
		}
	} else {
		req.Header.Set("Content-Type", "application/cloudevents+json")
		req.Header.Set(webhooks.HeaderEvent, in.EventType)
		if sig := webhooks.SignatureHeader(in.Secret, in.Body); sig != "" {
			req.Header.Set(webhooks.HeaderSignature, sig)
		}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("post webhook: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	a.Log.Info("webhook attempt",
		zap.String("delivery_id", in.DeliveryID), zap.Int("status", resp.StatusCode))
	return resp.StatusCode, nil
}

// UpdateWebhookDelivery persists the outcome of one attempt (retrying with
// the next timer, or the terminal delivered/dlq state). Payload type
// "incident" routes to booking-service's incident_deliveries ledger via
// Dapr service invocation (SPEC-W11 Part B §4); everything else updates the
// webhook_deliveries table as in Wave 5.
func (a *Activities) UpdateWebhookDelivery(ctx context.Context, upd workflows.WebhookDeliveryUpdate) error {
	if upd.PayloadType == workflows.PayloadTypeIncident {
		if a.Webhooks.BookingAppID == "" {
			return fmt.Errorf("booking app id not configured for incident delivery updates")
		}
		body := map[string]any{
			"status":      upd.Status,
			"attempts":    upd.Attempts,
			"status_code": upd.StatusCode,
		}
		return a.Dapr.InvokeServiceMethod(ctx, http.MethodPost, a.Webhooks.BookingAppID,
			"internal/incidents/deliveries/"+upd.DeliveryID, body,
			internalHeaders(a.BookingInternalToken), nil)
	}
	if a.Webhooks.Store == nil {
		return fmt.Errorf("webhook store not configured")
	}
	id, err := uuid.Parse(upd.DeliveryID)
	if err != nil {
		return fmt.Errorf("bad delivery id: %w", err)
	}
	var code *int
	if upd.StatusCode > 0 {
		c := upd.StatusCode
		code = &c
	}
	return a.Webhooks.Store.UpdateDelivery(ctx, id, upd.Status, upd.Attempts, code, upd.NextRetryAt)
}
