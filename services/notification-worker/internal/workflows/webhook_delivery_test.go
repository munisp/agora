package workflows

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// Backoff schedule (Wave 5 #10): the workflow must persist a "retrying"
// update with NextRetryAt after every failed attempt following
// 1m/5m/15m/1h/4h, then mark the delivery dlq after the 6th failure.

func TestWebhookBackoffSchedule(t *testing.T) {
	want := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour}
	require.Equal(t, want, WebhookBackoff)
}

func TestWebhookDeliveryRetriesThenDLQ(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	in := WebhookDeliveryInput{DeliveryID: "d-1", URL: "https://x.example/hook", Secret: "s", EventType: "com.opendesk.booking.BookingCreated", Body: []byte(`{}`)}

	// Receiver always fails with 500.
	env.RegisterActivityWithOptions(func(ctx context.Context, in WebhookDeliveryInput) (int, error) {
		return 500, nil
	}, activity.RegisterOptions{Name: ActivityDeliverWebhookHTTP})
	env.RegisterActivityWithOptions(func(ctx context.Context, upd WebhookDeliveryUpdate) error {
		return nil
	}, activity.RegisterOptions{Name: ActivityUpdateWebhookDelivery})

	var updates []WebhookDeliveryUpdate
	env.OnActivity(ActivityUpdateWebhookDelivery, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			updates = append(updates, args.Get(1).(WebhookDeliveryUpdate))
		}).Return(nil)

	env.ExecuteWorkflow(WebhookDeliveryWorkflow, in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	// 6 attempts (initial + 5 retries): 5 retrying updates + 1 dlq.
	require.Len(t, updates, 6)
	var retryAt []time.Time
	for i, upd := range updates[:5] {
		require.Equal(t, "retrying", upd.Status, "update %d", i)
		require.Equal(t, i+1, upd.Attempts)
		require.Equal(t, 500, upd.StatusCode)
		require.NotNil(t, upd.NextRetryAt, "update %d must schedule the next timer", i)
		retryAt = append(retryAt, *upd.NextRetryAt)
	}
	require.Equal(t, "dlq", updates[5].Status)
	require.Equal(t, 6, updates[5].Attempts)
	require.Nil(t, updates[5].NextRetryAt)

	// The durable timers follow the backoff schedule: the fake clock only
	// advances by Sleep, so consecutive NextRetryAt values differ exactly by
	// the next backoff step.
	require.Equal(t, WebhookBackoff[1], retryAt[1].Sub(retryAt[0]))
	require.Equal(t, WebhookBackoff[2], retryAt[2].Sub(retryAt[1]))
	require.Equal(t, WebhookBackoff[3], retryAt[3].Sub(retryAt[2]))
	require.Equal(t, WebhookBackoff[4], retryAt[4].Sub(retryAt[3]))
}

func TestWebhookDeliverySuccessAfterFailures(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	in := WebhookDeliveryInput{DeliveryID: "d-2", URL: "https://x.example/hook", EventType: "t", Body: []byte(`{}`)}

	attempt := 0
	env.RegisterActivityWithOptions(func(ctx context.Context, in WebhookDeliveryInput) (int, error) {
		attempt++
		if attempt < 3 {
			return 502, nil
		}
		return 200, nil
	}, activity.RegisterOptions{Name: ActivityDeliverWebhookHTTP})
	env.RegisterActivityWithOptions(func(ctx context.Context, upd WebhookDeliveryUpdate) error {
		return nil
	}, activity.RegisterOptions{Name: ActivityUpdateWebhookDelivery})

	var updates []WebhookDeliveryUpdate
	env.OnActivity(ActivityUpdateWebhookDelivery, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			updates = append(updates, args.Get(1).(WebhookDeliveryUpdate))
		}).Return(nil)

	env.ExecuteWorkflow(WebhookDeliveryWorkflow, in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	require.Len(t, updates, 3) // retrying, retrying, delivered
	require.Equal(t, "retrying", updates[0].Status)
	require.Equal(t, "retrying", updates[1].Status)
	require.Equal(t, "delivered", updates[2].Status)
	require.Equal(t, 3, updates[2].Attempts)
	require.Equal(t, 200, updates[2].StatusCode)
}

// Payload type "incident" (SPEC-W11 Part B §4): the retry schedule is
// reused unchanged and the payload type propagates to every persistence
// update so the activity routes them to booking-service's incident ledger.
func TestWebhookDeliveryIncidentPayloadTypePropagates(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	in := WebhookDeliveryInput{
		DeliveryID:  "d-inc-1",
		URL:         "https://psap.example/hook",
		Secret:      "s3",
		PayloadType: PayloadTypeIncident,
		IncidentID:  "11111111-1111-1111-1111-111111111111",
		Body:        []byte(`{"incident_id":"11111111-1111-1111-1111-111111111111"}`),
	}

	attempt := 0
	env.RegisterActivityWithOptions(func(ctx context.Context, in WebhookDeliveryInput) (int, error) {
		attempt++
		if attempt == 1 {
			return 500, nil
		}
		return 200, nil
	}, activity.RegisterOptions{Name: ActivityDeliverWebhookHTTP})
	env.RegisterActivityWithOptions(func(ctx context.Context, upd WebhookDeliveryUpdate) error {
		return nil
	}, activity.RegisterOptions{Name: ActivityUpdateWebhookDelivery})

	var updates []WebhookDeliveryUpdate
	env.OnActivity(ActivityUpdateWebhookDelivery, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			updates = append(updates, args.Get(1).(WebhookDeliveryUpdate))
		}).Return(nil)

	env.ExecuteWorkflow(WebhookDeliveryWorkflow, in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	require.Len(t, updates, 2) // retrying, delivered
	require.Equal(t, "retrying", updates[0].Status)
	require.Equal(t, "delivered", updates[1].Status)
	for i, upd := range updates {
		require.Equal(t, PayloadTypeIncident, upd.PayloadType, "update %d must carry the incident payload type", i)
	}
}

func TestWebhookDeliveryTransportErrorCountsAsFailure(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()

	in := WebhookDeliveryInput{DeliveryID: "d-3", URL: "https://gone.example", EventType: "t", Body: []byte(`{}`)}

	// Transport-level error (activity error with MaximumAttempts=1).
	env.RegisterActivityWithOptions(func(ctx context.Context, in WebhookDeliveryInput) (int, error) {
		return 0, context.DeadlineExceeded
	}, activity.RegisterOptions{Name: ActivityDeliverWebhookHTTP})
	env.RegisterActivityWithOptions(func(ctx context.Context, upd WebhookDeliveryUpdate) error {
		return nil
	}, activity.RegisterOptions{Name: ActivityUpdateWebhookDelivery})

	var updates []WebhookDeliveryUpdate
	env.OnActivity(ActivityUpdateWebhookDelivery, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			updates = append(updates, args.Get(1).(WebhookDeliveryUpdate))
		}).Return(nil)

	env.ExecuteWorkflow(WebhookDeliveryWorkflow, in)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, updates, 6)
	require.Equal(t, 0, updates[0].StatusCode) // transport error → status 0
	require.Equal(t, "dlq", updates[5].Status)
}
