package activities

// SPEC-W32 WS-B: civic reporting activities — citizen status notifications,
// SLA-breach reporting and the civic delivery ledger.
//
// SendCivicStatusUpdate delivers the transactional-class citizen update
// "Case {ref}: now {status}" (invoked through NotifyPaced kind
// civic_status, so CPS pacing + sender rotation + the SPEC-W12 guard
// pipeline are unchanged; transactional class bypasses DND per SPEC-W32
// §0.4, and the quiet-hours hold lives workflow-side in
// CivicStatusNotifyWorkflow). EVERY execution writes one civic
// delivery-ledger row (attempt number included), so a Temporal retry
// produces one row per attempt — "every attempt lands in the delivery
// ledger" (SPEC-W32 §3 WS-B).
//
// ReportCivicSLABreach posts booking-service's internal callback
// POST /v1/civic/internal/cases/{ref}/sla-breach {kind, mda_queue,
// notify_mda: true} via Dapr service invocation with the X-Tenant-Slug
// tenant-scoping header (the DaprLeadPhoneResolver idiom). That internal
// route (WS-A) sets the case's sla_breach_{ack,resolve} flag AND notifies
// the case's mda_queue dispatch endpoint through the W11 incident delivery
// path (signed webhook + incident_deliveries ledger — the endpoint
// URL/secret live in booking-service's store, so the delivery itself is
// triggered booking-service-side). The activity then emits the escalation
// CloudEvent com.opendesk.civic.SLABreachEscalated (topic:
// CIVIC_ESCALATION_TOPIC, default opendesk.civic.events.v1, "off"
// disables) and records an "escalated" row in the civic delivery ledger.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.temporal.io/sdk/activity"
	"go.uber.org/zap"
)

// Civic escalation event (SPEC-W32 §3 WS-B: "emit escalation event").
const (
	// EventTypeCivicSLABreachEscalated is emitted once per breach report.
	EventTypeCivicSLABreachEscalated = "com.opendesk.civic.SLABreachEscalated"
	// DefaultCivicEscalationTopic receives escalation events
	// (CIVIC_ESCALATION_TOPIC overrides; "off" disables).
	DefaultCivicEscalationTopic = "opendesk.civic.events.v1"
)

// Civic ledger outcomes.
const (
	CivicOutcomeSent      = "sent"
	CivicOutcomeFailed    = "failed"
	CivicOutcomeEscalated = "escalated"
)

// CivicNotification is one civic delivery-ledger row (SPEC-W32 §3 WS-B).
type CivicNotification struct {
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Ref        string `json:"ref"`     // canonical ref after a merge
	Status     string `json:"status"`  // case status, or sla_breach_{kind}
	Channel    string `json:"channel"` // sms | whatsapp | telegram | webhook
	Phone      string `json:"phone"`   // recipient (empty for escalations)
	Outcome    string `json:"outcome"` // sent | failed | escalated
	Attempt    int    `json:"attempt"` // Temporal activity attempt
	Error      string `json:"error,omitempty"`
}

// CivicLedgerStore is the persistence slice of the civic delivery ledger
// (*store.Store satisfies it via internal/store/civic_ledger.go; tests use
// a fake). Nil disables persistence (rows are still logged) — the same
// degradation posture as the webhook platform without DATABASE_URL.
type CivicLedgerStore interface {
	RecordCivicNotification(ctx context.Context, tenantID, tenantSlug, ref, status, channel, phone, outcome string, attempt int, errText string) error
}

// CivicDeps bundles the civic (SPEC-W32) activity dependencies; set by main
// after New.
type CivicDeps struct {
	// Ledger records every send attempt + escalation; nil → log-only.
	Ledger CivicLedgerStore
	// Escalations produces the SLA-breach escalation CloudEvents
	// (TrajectoryProducer's Produce signature; nil-safe).
	Escalations TrajectoryProducer
	// EscalationTopic is CIVIC_ESCALATION_TOPIC (default
	// opendesk.civic.events.v1; ""/"off" disables emission).
	EscalationTopic string
}

// SendCivicStatusUpdate delivers one citizen status notification on the
// requested channel (sms via the channel router with sender rotation;
// whatsapp/telegram via the messaging-gateway HTTP binding convention) and
// records the attempt in the civic delivery ledger — one row per Temporal
// attempt, including failures (a failed send is ledgered outcome=failed
// AND returned as an error so Temporal's retry schedule applies).
func (a *Activities) SendCivicStatusUpdate(ctx context.Context, in workflows.PacedCivicStatusSend) error {
	attempt := civicAttempt(ctx)
	ledger := CivicNotification{
		TenantID: in.TenantID, TenantSlug: in.TenantSlug, Ref: in.Ref,
		Status: in.Status, Channel: in.Channel, Phone: in.Phone, Attempt: attempt,
	}
	record := func(outcome, errText string) {
		ledger.Outcome = outcome
		ledger.Error = errText
		a.recordCivicNotification(ctx, ledger)
	}

	if in.Phone == "" {
		err := fmt.Errorf("civic status send: phone is required (case %s)", in.Ref)
		record(CivicOutcomeFailed, err.Error())
		return err
	}
	if in.Text == "" {
		err := fmt.Errorf("civic status send: text is required (case %s)", in.Ref)
		record(CivicOutcomeFailed, err.Error())
		return err
	}

	sender := a.TwilioFrom
	if a.Pacer != nil {
		if n := a.Pacer.NextSender(ctx); n != "" {
			sender = n
		}
	}

	channel := in.Channel
	if channel == "" {
		channel = ChannelSMS
	}
	switch channel {
	case ChannelSMS:
		provider := a.Channels.Provider(ChannelSMS, in.TenantSlug)
		if err := a.sendSMS(ctx, provider, in.Phone, in.Text, sender); err != nil {
			werr := fmt.Errorf("%s binding: %w", provider, err)
			record(CivicOutcomeFailed, werr.Error())
			return werr
		}
	case "whatsapp", "telegram":
		if err := a.Dapr.InvokeBinding(ctx, a.BindingName(channel), "post", map[string]string{
			"to":      in.Phone,
			"message": in.Text,
		}, nil); err != nil {
			werr := fmt.Errorf("%s binding: %w", channel, err)
			record(CivicOutcomeFailed, werr.Error())
			return werr
		}
	default:
		err := fmt.Errorf("civic status send: unknown channel %q (want whatsapp, telegram or sms)", channel)
		record(CivicOutcomeFailed, err.Error())
		return err
	}
	record(CivicOutcomeSent, "")
	a.Log.Info("civic status update sent",
		zap.String("ref", in.Ref), zap.String("status", in.Status),
		zap.String("channel", channel), zap.String("phone", in.Phone),
		zap.String("sender_number", sender), zap.Int("attempt", attempt))
	return nil
}

// ReportCivicSLABreach reports one unsatisfied SLA timer: (1) the
// booking-service internal callback (Dapr service invocation,
// X-Tenant-Slug) that flags the breach and drives the mda_queue dispatch
// endpoint notification via the W11 incident delivery path, then (2) the
// escalation CloudEvent, then (3) an "escalated" civic-ledger row. The
// callback is authoritative: escalation-emission and ledger failures are
// logged, never fail the activity (the breach flag + MDA notify are
// durable booking-service-side).
func (a *Activities) ReportCivicSLABreach(ctx context.Context, rep workflows.CivicSLABreachReport) error {
	if rep.Ref == "" {
		return fmt.Errorf("civic SLA breach: ref is required")
	}
	if rep.Kind != workflows.CivicBreachKindAck && rep.Kind != workflows.CivicBreachKindResolve {
		return fmt.Errorf("civic SLA breach: kind must be %q or %q (got %q)",
			workflows.CivicBreachKindAck, workflows.CivicBreachKindResolve, rep.Kind)
	}
	body := map[string]any{
		"kind": rep.Kind,
		// The internal route notifies the case's mda_queue dispatch endpoint
		// via the W11 delivery path (signed incident webhook + ledger).
		"notify_mda": true,
	}
	if rep.MDAQueue != "" {
		body["mda_queue"] = rep.MDAQueue
	}
	if err := a.Dapr.InvokeServiceMethod(ctx, http.MethodPost, a.BookingAppID,
		"v1/civic/internal/cases/"+rep.Ref+"/sla-breach", body,
		map[string]string{"X-Tenant-Slug": rep.TenantSlug,
			// K2 (S1-F7-05): the sla-breach route is internauth-guarded
			// booking-side (X-Internal-Token vs BOOKING_INTERNAL_TOKEN).
			"X-Internal-Token": a.BookingInternalToken}, nil); err != nil {
		return fmt.Errorf("booking-service civic sla-breach callback: %w", err)
	}
	a.emitCivicEscalation(ctx, rep)
	a.recordCivicNotification(ctx, CivicNotification{
		TenantID: rep.TenantID, TenantSlug: rep.TenantSlug, Ref: rep.Ref,
		Status: "sla_breach_" + rep.Kind, Channel: "webhook",
		Outcome: CivicOutcomeEscalated, Attempt: 1,
	})
	a.Log.Info("civic SLA breach reported",
		zap.String("ref", rep.Ref), zap.String("kind", rep.Kind),
		zap.String("mda_queue", rep.MDAQueue), zap.String("tenant", rep.TenantSlug))
	return nil
}

// emitCivicEscalation publishes the com.opendesk.civic.SLABreachEscalated
// CloudEvent (keyed by ref so one case's events stay ordered). Failures
// are logged-only: the internal callback already carried the breach.
func (a *Activities) emitCivicEscalation(ctx context.Context, rep workflows.CivicSLABreachReport) {
	if a.Civic.Escalations == nil || a.Civic.EscalationTopic == "" {
		return
	}
	now := time.Now().UTC()
	evt := map[string]any{
		"specversion": "1.0",
		"id":          uuid.NewString(),
		"source":      "notification-worker",
		"type":        EventTypeCivicSLABreachEscalated,
		"subject":     rep.TenantSlug,
		"time":        now.Format(time.RFC3339),
		"tenantid":    rep.TenantID,
		"data": map[string]any{
			"tenant_id": rep.TenantID,
			"ref":       rep.Ref,
			"kind":      rep.Kind,
			"mda_queue": rep.MDAQueue,
			"ts":        now.Format(time.RFC3339),
		},
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		a.Log.Warn("civic escalation marshal failed; skipping", zap.Error(err))
		return
	}
	if err := a.Civic.Escalations.Produce(ctx, a.Civic.EscalationTopic, []byte(rep.Ref), payload); err != nil {
		a.Log.Warn("civic escalation produce failed; event lost (breach flag is already set booking-service-side)",
			zap.String("ref", rep.Ref), zap.String("kind", rep.Kind), zap.Error(err))
	}
}

// recordCivicNotification writes one civic delivery-ledger row; failures
// are logged-only (the send outcome itself is authoritative).
func (a *Activities) recordCivicNotification(ctx context.Context, n CivicNotification) {
	if a.Civic.Ledger == nil {
		return
	}
	if err := a.Civic.Ledger.RecordCivicNotification(ctx,
		n.TenantID, n.TenantSlug, n.Ref, n.Status, n.Channel, n.Phone, n.Outcome, n.Attempt, n.Error); err != nil {
		a.Log.Warn("civic delivery ledger write failed",
			zap.String("ref", n.Ref), zap.String("outcome", n.Outcome), zap.Error(err))
	}
}

// civicAttempt resolves the Temporal activity attempt (1 outside an
// activity context — direct invocations such as /dev triggers and unit
// tests, where activity.GetInfo would panic).
func civicAttempt(ctx context.Context) (attempt int) {
	attempt = 1
	defer func() { _ = recover() }()
	if info := activity.GetInfo(ctx); info.Attempt > 0 {
		attempt = int(info.Attempt)
	}
	return attempt
}
