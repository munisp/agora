// Package notifyoutbox consumes opendesk.notifications.outbox — the
// fire-and-forget notification command topic (SPEC §4). Wave 5 #7 adds the
// first producer: booking-service publishes
// com.opendesk.notifications.SendPortalCode when a customer requests a
// portal login code; this consumer is the delivery half — it owns the
// smtp/twilio Dapr bindings, exactly like the workflow activities.
package notifyoutbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/daprc"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/segmentio/kafka-go"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// BindingSender delivers notification commands through the Dapr output
// bindings (bindings-smtp / bindings-twilio) — the same path the workflow
// activities use.
type BindingSender struct {
	Dapr          *daprc.Client
	SMTPBinding   string
	TwilioBinding string
	SMTPFrom      string
	TwilioFrom    string
}

// Send implements Sender.
func (s BindingSender) Send(ctx context.Context, channel, destination, subject, text string) error {
	switch channel {
	case "email":
		return s.Dapr.InvokeBinding(ctx, s.SMTPBinding, "create", text, map[string]string{
			"emailTo":   destination,
			"emailFrom": s.SMTPFrom,
			"subject":   subject,
		})
	case "sms":
		return s.Dapr.InvokeBinding(ctx, s.TwilioBinding, "create", text, map[string]string{
			"toNumber":   destination,
			"fromNumber": s.TwilioFrom,
		})
	default:
		return fmt.Errorf("unknown channel %q", channel)
	}
}

// EventTypeSendPortalCode is the portal login code command (Wave 5 #7).
const EventTypeSendPortalCode = "com.opendesk.notifications.SendPortalCode"

// EventTypePacedSend is the fire-and-forget paced send command (SPEC-W19
// integrator, completing the W16/W19 contract): the CloudEvent data IS a
// workflows.PacedSendRequest (field tags kept in sync by contract —
// duplicated, not shared). Producer today: booking-service field-service
// dispatch push (kind push_notification, TRANSACTIONAL). The consumer
// starts one PacedSendWorkflow per command, which runs the send through
// the NotifyPaced activity (CPS pacing + sender rotation + the SPEC-W12
// DND/quiet-hours guards) instead of a raw binding call.
const EventTypePacedSend = "com.opendesk.notifications.PacedSend"

// Sender delivers one text message over a channel ("sms" or "email").
type Sender interface {
	Send(ctx context.Context, channel, destination, subject, text string) error
}

// WorkflowStarter abstracts Temporal workflow starts (client.Client
// satisfies it via ExecuteWorkflow) — same idiom as
// internal/signals.WorkflowStarter.
type WorkflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflowType interface{}, args ...interface{}) (client.WorkflowRun, error)
}

// Consumer reads the notifications outbox topic and delivers commands.
type Consumer struct {
	reader    *kafka.Reader
	sender    Sender
	starter   WorkflowStarter // nil → PacedSend commands are acked + logged (graceful)
	taskQueue string
	log       *zap.Logger
}

// Option customizes a Consumer.
type Option func(*Consumer)

// WithStarter enables PacedSendWorkflow starts for
// com.opendesk.notifications.PacedSend commands (SPEC-W19 integrator).
func WithStarter(starter WorkflowStarter, taskQueue string) Option {
	return func(c *Consumer) {
		c.starter = starter
		c.taskQueue = taskQueue
	}
}

// New builds the consumer (explicit commits, like the signal bridge).
func New(brokers []string, topic, group string, sender Sender, log *zap.Logger, opts ...Option) *Consumer {
	c := &Consumer{
		reader: kafka.NewReader(kafka.ReaderConfig{
			Brokers:        brokers,
			Topic:          topic,
			GroupID:        group,
			MinBytes:       1,
			MaxBytes:       10 << 20,
			CommitInterval: 0,
			StartOffset:    kafka.FirstOffset,
		}),
		sender: sender,
		log:    log,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Run consumes until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	c.log.Info("notifications outbox consumer started")
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("fetch message: %w", err)
		}
		if err := c.Process(ctx, msg.Value); err != nil {
			// A failed send is logged and acknowledged (never hot-looped);
			// the customer can simply request a fresh code.
			c.log.Error("notification command failed; acknowledging anyway",
				zap.String("key", string(msg.Key)), zap.Error(err))
		}
		if err := c.reader.CommitMessages(ctx, msg); err != nil {
			c.log.Error("commit failed", zap.Error(err))
		}
	}
}

// Close releases the reader.
func (c *Consumer) Close() error { return c.reader.Close() }

// envelope is the CloudEvents wrapper of notification commands.
type envelope struct {
	ID   string         `json:"id"`
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// Process handles one raw command payload (exported for testing).
func (c *Consumer) Process(ctx context.Context, raw []byte) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		c.log.Warn("malformed notification command; skipping", zap.Error(err))
		return nil
	}
	switch env.Type {
	case EventTypeSendPortalCode:
		return c.sendPortalCode(ctx, env.Data)
	case EventTypePacedSend:
		return c.pacedSend(ctx, env)
	default:
		return nil // unknown commands are acknowledged (forward-compatible)
	}
}

// pacedSend unpacks a PacedSend command (the CloudEvent data IS the W16
// PacedSendRequest shape — see EventTypePacedSend) and starts one
// PacedSendWorkflow for it. The workflow ID is derived from the CloudEvent
// id, so a redelivered command hits WorkflowExecutionAlreadyStarted and is
// acknowledged without a duplicate send. Without a starter (consumer built
// without WithStarter) the command is logged and acknowledged — the same
// graceful posture as unknown types.
func (c *Consumer) pacedSend(ctx context.Context, env envelope) error {
	raw, err := json.Marshal(env.Data)
	if err != nil {
		c.log.Warn("malformed PacedSend data; skipping", zap.Error(err))
		return nil
	}
	var req workflows.PacedSendRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.Kind == "" {
		c.log.Warn("PacedSend command carries no valid PacedSendRequest; skipping",
			zap.String("kind", req.Kind), zap.Error(err))
		return nil
	}
	if c.starter == nil {
		c.log.Warn("PacedSend command received but no Temporal starter is wired; acknowledging without delivery",
			zap.String("kind", req.Kind), zap.String("event_id", env.ID))
		return nil
	}
	workflowID := "paced-send-" + env.ID
	if env.ID == "" {
		// Producers are contract-bound to set the CloudEvent id; a missing
		// id falls back to a random one (at-least-once delivery beats
		// dropping; documented redelivery risk).
		workflowID = "paced-send-" + uuid.NewString()
	}
	_, err = c.starter.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: c.taskQueue,
	}, workflows.WorkflowTypePacedSend, req)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) || strings.Contains(err.Error(), "already started") {
			c.log.Info("PacedSendWorkflow already running; command acknowledged",
				zap.String("workflow_id", workflowID))
			return nil
		}
		return fmt.Errorf("start %s: %w", workflows.WorkflowTypePacedSend, err)
	}
	c.log.Info("PacedSendWorkflow started",
		zap.String("workflow_id", workflowID), zap.String("kind", req.Kind))
	return nil
}

// sendPortalCode delivers the 6-digit portal login code. The plaintext code
// exists only in this payload and in the message to the customer — the
// booking DB holds its SHA-256 hash.
func (c *Consumer) sendPortalCode(ctx context.Context, data map[string]any) error {
	channel, _ := data["channel"].(string)
	dest, _ := data["destination"].(string)
	code, _ := data["code"].(string)
	if (channel != "sms" && channel != "email") || dest == "" || code == "" {
		return fmt.Errorf("SendPortalCode: invalid payload (channel=%q)", channel)
	}
	text := fmt.Sprintf("Your OpenDesk booking portal code is %s. It is valid for 10 minutes.", code)
	subject := "Your booking portal login code"
	if err := c.sender.Send(ctx, channel, dest, subject, text); err != nil {
		return fmt.Errorf("SendPortalCode send: %w", err)
	}
	c.log.Info("portal code delivered", zap.String("channel", channel))
	return nil
}
