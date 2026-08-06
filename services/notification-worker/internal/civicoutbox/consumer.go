// Package civicoutbox consumes opendesk.civic.events.v1 (SPEC-W32 §0.3:
// every civic case event is a CloudEvent on this topic via booking-service's
// outbox) and drives the notification-worker's civic halves (SPEC-W32 §3
// WS-B):
//
//   - com.opendesk.civic.ReportReceived → start one CivicSLAWorkflow with
//     the deterministic workflow ID civic-sla-{tenant}-{ref} (a redelivered
//     event hits WorkflowExecutionAlreadyStarted and is acknowledged —
//     the notifyoutbox PacedSend idiom).
//   - com.opendesk.civic.StatusChanged → (a) signal the case's SLA workflow
//     (SignalCivicStatus) so pending ack/resolve timers are satisfied;
//     (b) when the data carries reporter_phone (wants_updates), start one
//     CivicStatusNotifyWorkflow (workflow ID civic-notify-{event id}) — a
//     TRANSACTIONAL-class "Case {ref}: now {status}" paced send (DND
//     bypass, quiet-hours hold, civic delivery ledger).
//   - com.opendesk.civic.Merged → remember ref→canonical and signal the
//     merged case's SLA workflow (SignalCivicMerged) so its timers are
//     cancelled; later notifications reference the CANONICAL ref
//     (SPEC-W32 §4.3). The ref→canonical map is process-local: the SLA
//     workflow signal is the durable half, and StatusChanged events after
//     a merge are emitted by booking-service on the canonical ref anyway —
//     the map only upgrades a straggler event for the merged ref.
//
// Topic configuration: CIVIC_EVENTS_TOPIC (default
// opendesk.civic.events.v1); an empty/"off" topic disables the consumer
// entirely (main does not start it). Unknown event types and malformed
// payloads are acknowledged (forward-compatible, never hot-looped).
package civicoutbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/workflows"
	"github.com/segmentio/kafka-go"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
)

// Civic CloudEvent types (SPEC-W32 §3 WS-A: emitted by booking-service via
// the outbox, id tenant:civic:ref:{seq}, tenantid extension).
const (
	EventTypeReportReceived = "com.opendesk.civic.ReportReceived"
	EventTypeStatusChanged  = "com.opendesk.civic.StatusChanged"
	EventTypeMerged         = "com.opendesk.civic.Merged"

	// DefaultTopic is the civic case-events topic (CIVIC_EVENTS_TOPIC
	// overrides; empty/"off" disables the consumer).
	DefaultTopic = "opendesk.civic.events.v1"
)

// TopicEnabled resolves the configured civic events topic: unset → the
// default; "off"/"disabled" → "" (consumer disabled).
func TopicEnabled(configured string) string {
	t := strings.TrimSpace(configured)
	if t == "" {
		return DefaultTopic
	}
	if strings.EqualFold(t, "off") || strings.EqualFold(t, "disabled") {
		return ""
	}
	return t
}

// Temporal is the slice of the Temporal client the consumer needs
// (client.Client satisfies it; tests use a fake).
type Temporal interface {
	SignalWorkflow(ctx context.Context, workflowID, runID, signalName string, arg interface{}) error
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflowType interface{}, args ...interface{}) (client.WorkflowRun, error)
}

// Consumer reads the civic events topic and drives the civic workflows.
type Consumer struct {
	reader        *kafka.Reader
	temporal      Temporal
	taskQueue     string
	notifyChannel string
	log           *zap.Logger

	mu        sync.Mutex
	canonical map[string]string // "tenant/ref" → canonical ref (Merged events)
}

// Option customizes a Consumer.
type Option func(*Consumer)

// WithNotifyChannel overrides the citizen notification channel
// (CIVIC_STATUS_CHANNEL; default sms).
func WithNotifyChannel(channel string) Option {
	return func(c *Consumer) { c.notifyChannel = channel }
}

// New builds the consumer (explicit commits, like the signal bridge and the
// notifications outbox consumer). temporal nil → events are logged and
// acknowledged (graceful degradation, same posture as notifyoutbox).
func New(brokers []string, topic, group string, temporal Temporal, taskQueue string, log *zap.Logger, opts ...Option) *Consumer {
	if log == nil {
		log = zap.NewNop()
	}
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
		temporal:      temporal,
		taskQueue:     taskQueue,
		notifyChannel: "sms",
		log:           log,
		canonical:     map[string]string{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Run consumes until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	c.log.Info("civic events consumer started")
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("fetch message: %w", err)
		}
		if err := c.Process(ctx, msg.Value); err != nil {
			// Signals are best-effort and workflow starts are idempotent:
			// log and ack instead of redelivering forever (signal-bridge
			// posture).
			c.log.Error("civic event processing failed; acknowledging anyway",
				zap.String("key", string(msg.Key)), zap.Error(err))
		}
		if err := c.reader.CommitMessages(ctx, msg); err != nil {
			c.log.Error("commit failed", zap.Error(err))
		}
	}
}

// Close releases the reader.
func (c *Consumer) Close() error { return c.reader.Close() }

// envelope is the CloudEvents wrapper of civic case events.
type envelope struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Subject  string         `json:"subject"`  // tenant slug
	TenantID string         `json:"tenantid"` // CloudEvents extension
	Data     map[string]any `json:"data"`
}

// tenant resolves the tenant scope: slug from the CloudEvent subject
// (tenant id from the tenantid extension), falling back to additive data
// fields for tolerance.
func (env envelope) tenant() (id, slug string) {
	id, slug = env.TenantID, env.Subject
	if v, ok := env.Data["tenant_id"].(string); ok && id == "" {
		id = v
	}
	if v, ok := env.Data["tenant_slug"].(string); ok && slug == "" {
		slug = v
	}
	return id, slug
}

// tenantKey identifies the tenant half of a workflow ID: the slug wins
// (stable + readable), the id is the fallback.
func (env envelope) tenantKey() string {
	id, slug := env.tenant()
	if slug != "" {
		return slug
	}
	return id
}

func dataString(data map[string]any, key string) string {
	v, _ := data[key].(string)
	return strings.TrimSpace(v)
}

func dataTime(data map[string]any, key string) time.Time {
	s := dataString(data, key)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// dataTimePtr extracts an optional RFC3339 timestamp: absent/null/empty →
// nil ("don't re-arm"); malformed → nil + a warn (the signal itself is
// never dropped over a bad timestamp — SPEC-W32 W3 backward compat:
// StatusChanged events without due times must keep working).
func (c *Consumer) dataTimePtr(data map[string]any, key, ref, eventID string) *time.Time {
	raw, ok := data[key]
	if !ok || raw == nil {
		return nil
	}
	s, _ := raw.(string)
	if strings.TrimSpace(s) == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		c.log.Warn("civic event carries a malformed due time; ignoring the field (signal still delivered)",
			zap.String("field", key), zap.String("ref", ref), zap.String("event_id", eventID),
			zap.Error(err))
		return nil
	}
	return &t
}

// Process handles one raw civic event payload (exported for testing).
func (c *Consumer) Process(ctx context.Context, raw []byte) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		c.log.Warn("malformed civic event; skipping", zap.Error(err))
		return nil
	}
	switch env.Type {
	case EventTypeReportReceived:
		return c.reportReceived(ctx, env)
	case EventTypeStatusChanged:
		return c.statusChanged(ctx, env)
	case EventTypeMerged:
		return c.merged(ctx, env)
	default:
		return nil // unknown events are acknowledged (forward-compatible)
	}
}

// reportReceived starts the case's CivicSLAWorkflow (deterministic ID —
// redeliveries are acknowledged via AlreadyStarted).
func (c *Consumer) reportReceived(ctx context.Context, env envelope) error {
	ref := dataString(env.Data, "ref")
	if ref == "" {
		c.log.Warn("ReportReceived without ref; skipping", zap.String("event_id", env.ID))
		return nil
	}
	if c.temporal == nil {
		c.log.Warn("ReportReceived received but no Temporal client is wired; acknowledging without starting the SLA workflow",
			zap.String("ref", ref), zap.String("event_id", env.ID))
		return nil
	}
	tenantID, tenantSlug := env.tenant()
	in := workflows.CivicSLAInput{
		TenantID:     tenantID,
		TenantSlug:   tenantSlug,
		Ref:          ref,
		MDAQueue:     dataString(env.Data, "mda_queue"),
		AckDueAt:     dataTime(env.Data, "ack_due_at"),
		ResolveDueAt: dataTime(env.Data, "resolve_due_at"),
	}
	workflowID := workflows.CivicSLAWorkflowID(env.tenantKey(), ref)
	_, err := c.temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: c.taskQueue,
	}, workflows.WorkflowTypeCivicSLA, in)
	if err != nil {
		if isAlreadyStarted(err) {
			c.log.Info("CivicSLAWorkflow already running; event acknowledged",
				zap.String("workflow_id", workflowID))
			return nil
		}
		return fmt.Errorf("start %s: %w", workflows.WorkflowTypeCivicSLA, err)
	}
	c.log.Info("CivicSLAWorkflow started",
		zap.String("workflow_id", workflowID), zap.String("ref", ref),
		zap.Time("ack_due_at", in.AckDueAt), zap.Time("resolve_due_at", in.ResolveDueAt))
	return nil
}

// statusChanged satisfies the case's SLA timers (signal) and, when the
// reporter wants updates (reporter_phone present), starts the citizen
// notification workflow. A merged ref is rewritten to the canonical ref
// for the notification (SPEC-W32 §4.3); the SLA signal always targets the
// event's own ref (a completed workflow tolerates the signal as NotFound).
func (c *Consumer) statusChanged(ctx context.Context, env envelope) error {
	ref := dataString(env.Data, "ref")
	status := dataString(env.Data, "status")
	if ref == "" || status == "" {
		c.log.Warn("StatusChanged without ref/status; skipping",
			zap.String("ref", ref), zap.String("status", status), zap.String("event_id", env.ID))
		return nil
	}
	if c.temporal == nil {
		c.log.Warn("StatusChanged received but no Temporal client is wired; acknowledging",
			zap.String("ref", ref), zap.String("status", status))
		return nil
	}
	tenantID, tenantSlug := env.tenant()

	// (a) satisfy the SLA timers. The signal also carries booking-service's
	// RECOMPUTED due times when present (triage can change the category,
	// hence the SLA — SPEC-W32 W3); absent/malformed decode to nil = don't
	// re-arm, so old StatusChanged events behave exactly as before.
	workflowID := workflows.CivicSLAWorkflowID(env.tenantKey(), ref)
	sig := workflows.CivicStatusSignal{
		Status:       status,
		AckDueAt:     c.dataTimePtr(env.Data, "ack_due_at", ref, env.ID),
		ResolveDueAt: c.dataTimePtr(env.Data, "resolve_due_at", ref, env.ID),
	}
	if err := c.temporal.SignalWorkflow(ctx, workflowID, "",
		workflows.SignalCivicStatus, sig); err != nil {
		var nf *serviceerror.NotFound
		if errors.As(err, &nf) || strings.Contains(err.Error(), "workflow not found") {
			c.log.Info("no running SLA workflow for status change; acknowledged",
				zap.String("workflow_id", workflowID), zap.String("status", status))
		} else {
			return fmt.Errorf("signal %s: %w", workflowID, err)
		}
	}

	// (b) citizen notification (wants_updates → reporter_phone present).
	phone := dataString(env.Data, "reporter_phone")
	if phone == "" {
		return nil
	}
	canonical := c.canonicalRef(env.tenantKey(), ref)
	in := workflows.CivicStatusNotifyInput{
		TenantID:     tenantID,
		TenantSlug:   tenantSlug,
		Ref:          ref,
		CanonicalRef: canonical,
		Status:       status,
		Phone:        phone,
		Channel:      c.notifyChannel,
	}
	notifyID := "civic-notify-" + env.ID
	if env.ID == "" {
		// Producers are contract-bound to set the CloudEvent id; a missing
		// id falls back to a random one (at-least-once beats dropping — the
		// notifyoutbox idiom).
		notifyID = "civic-notify-" + uuid.NewString()
	}
	_, err := c.temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        notifyID,
		TaskQueue: c.taskQueue,
	}, workflows.WorkflowTypeCivicStatusNotify, in)
	if err != nil {
		if isAlreadyStarted(err) {
			c.log.Info("civic status notification already running; event acknowledged",
				zap.String("workflow_id", notifyID))
			return nil
		}
		return fmt.Errorf("start %s: %w", workflows.WorkflowTypeCivicStatusNotify, err)
	}
	c.log.Info("civic status notification started",
		zap.String("workflow_id", notifyID), zap.String("ref", ref),
		zap.String("effective_ref", in.EffectiveRef()), zap.String("status", status))
	return nil
}

// merged records ref→canonical and cancels the merged case's SLA timers
// (signal; a completed/absent workflow tolerates NotFound).
func (c *Consumer) merged(ctx context.Context, env envelope) error {
	ref := dataString(env.Data, "ref")
	canonical := dataString(env.Data, "canonical_ref")
	if canonical == "" {
		canonical = dataString(env.Data, "merged_into")
	}
	if ref == "" || canonical == "" {
		c.log.Warn("Merged without ref/canonical_ref; skipping",
			zap.String("ref", ref), zap.String("canonical_ref", canonical), zap.String("event_id", env.ID))
		return nil
	}
	key := env.tenantKey() + "/" + ref
	c.mu.Lock()
	c.canonical[key] = canonical
	c.mu.Unlock()
	if c.temporal == nil {
		return nil
	}
	workflowID := workflows.CivicSLAWorkflowID(env.tenantKey(), ref)
	if err := c.temporal.SignalWorkflow(ctx, workflowID, "",
		workflows.SignalCivicMerged, workflows.CivicMergedSignal{CanonicalRef: canonical}); err != nil {
		var nf *serviceerror.NotFound
		if errors.As(err, &nf) || strings.Contains(err.Error(), "workflow not found") {
			c.log.Info("no running SLA workflow for merged case; acknowledged",
				zap.String("workflow_id", workflowID), zap.String("canonical_ref", canonical))
			return nil
		}
		return fmt.Errorf("signal merged %s: %w", workflowID, err)
	}
	c.log.Info("civic case merged; SLA workflow signalled",
		zap.String("ref", ref), zap.String("canonical_ref", canonical))
	return nil
}

// canonicalRef returns the canonical ref of a (possibly merged) case, or ""
// when the case was never merged.
func (c *Consumer) canonicalRef(tenantKey, ref string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.canonical[tenantKey+"/"+ref]
}

func isAlreadyStarted(err error) bool {
	var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
	return errors.As(err, &alreadyStarted) || strings.Contains(err.Error(), "already started")
}
