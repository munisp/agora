package campaignstudio

// Send dispatch for journey send steps (SPEC-W19 Agent D), modeled on the
// W8 geo campaign idiom: booking-service owns the StudioSendWorkflow and
// its DB activity; the workflow runs on booking-service's Temporal worker
// (shared task queue, registered by the INTEGRATOR via RegisterWorker) and
// every recipient send is delegated to the EXISTING paced notification
// path — the "NotifyPaced" activity the notification-worker executes
// (CPS pacing + sender rotation + the SPEC-W12 §3 DND guard) with the
// MARKETING kinds only:
//
//   - kind sms            → paced kind "geo_campaign" channel "sms" (the
//                           notification-worker's only SMS marketing route;
//                           its SendGeoCampaignMessage activity renders
//                           channel sms via the twilio binding)
//   - kind push_marketing → paced kind "push_marketing" (SendPushNotification
//                           fan-out; the payload carries the contact phone
//                           so the DND guard can check the phone-keyed
//                           registries)
//
// Both kinds are marketing-class in the notification-worker pacer table,
// so DND suppression applies ACTIVITY-SIDE automatically and quiet-hours
// DEFERRAL is applied WORKFLOW-SIDE here (guardedPacedSend mirrors
// geo.quiet.go, which itself mirrors notification-worker
// workflows.GuardedPacedSend — default window 20:00-08:00 Africa/Lagos,
// per-channel overrides).
//
// Advancement vs dispatch: POST /journeys/{id}/step advances send-step
// enrollments and queues the payloads in ONE transaction, then starts
// this workflow for the batch (workflow id "studio-send-{batchID}"). A
// step call retried after a crash finds the enrollments already advanced,
// so no send is queued twice; the workflow's outcome activity records
// send_sent / send_suppressed / send_failed per enrollment (audit +
// per-step stats).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/geo"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"go.uber.org/zap"
)

const (
	// WorkflowTypeStudioSend is the registered name of the send workflow.
	WorkflowTypeStudioSend = "StudioSendWorkflow"

	// ActivityStudioRecordSend is the outcome-recording activity
	// (registered on booking-service's worker by RegisterWorker).
	ActivityStudioRecordSend = "StudioRecordSendOutcome"

	// ActivityNotifyPaced mirrors notification-worker's paced wrapper
	// activity name (service boundary: duplicated, not shared).
	ActivityNotifyPaced = "NotifyPaced"

	// PacedSendGeoCampaign is the NotifyPaced kind used for journey SMS
	// sends (the notification-worker's only SMS marketing route).
	PacedSendGeoCampaign = "geo_campaign"
	// PacedSendPushMarketing is the NotifyPaced kind for marketing push
	// (mirror of notification-worker workflows.PacedSendPushMarketing).
	PacedSendPushMarketing = "push_marketing"

	// PacedSendStatusSent / PacedSendStatusSuppressedDND mirror the
	// NotifyPaced result contract (geo re-exports the same strings; kept
	// local so this file stays a self-contained mirror).
	PacedSendStatusSent          = "sent"
	PacedSendStatusSuppressedDND = "suppressed_dnd"
)

// ---------------------------------------------------------------------------
// Paced send contract mirror (notification-worker internal/workflows/paced.go)
// Only the two marketing shapes studio emits are mirrored; the JSON tags
// must stay field-compatible (service boundary: duplicated, not shared).
// ---------------------------------------------------------------------------

// PacedSendRequest is the NotifyPaced payload for the marketing kinds.
type PacedSendRequest struct {
	Kind string                     `json:"kind"` // geo_campaign | push_marketing
	Geo  *PacedGeoCampaignSend      `json:"geo_campaign,omitempty"`
	Push *PacedPushNotificationSend `json:"push,omitempty"`
}

// PacedGeoCampaignSend carries the SendGeoCampaignMessage arguments
// (channel fixed to "sms" by studio).
type PacedGeoCampaignSend struct {
	TenantSlug string `json:"tenant_slug"`
	CampaignID string `json:"campaign_id"` // studio: the JOURNEY id
	Channel    string `json:"channel"`     // "sms"
	Phone      string `json:"phone"`
	Name       string `json:"name"`
	Text       string `json:"text"` // {name} already substituted
}

// PacedPushNotificationSend carries the SendPushNotification arguments
// (SPEC-W16 contract §1 shape, marketing class).
type PacedPushNotificationSend struct {
	TenantSlug string            `json:"tenant_slug"`
	ContactID  string            `json:"contact_id,omitempty"`
	Phone      string            `json:"phone,omitempty"` // lets the DND guard check phone-keyed registries
	Title      string            `json:"title"`
	Body       string            `json:"body"`
	Data       map[string]string `json:"data,omitempty"`
}

// PacedSendResult mirrors the NotifyPaced outcome contract.
type PacedSendResult struct {
	Status string `json:"status"` // sent | suppressed_dnd
	Reason string `json:"reason,omitempty"`
}

// ---------------------------------------------------------------------------
// Workflow IO
// ---------------------------------------------------------------------------

// StudioSendBatchInput starts a StudioSendWorkflow: one batch of queued
// sends collected by a single POST /journeys/{id}/step call.
type StudioSendBatchInput struct {
	BatchID     string       `json:"batch_id"`
	TenantID    string       `json:"tenant_id"`
	TenantSlug  string       `json:"tenant_slug"`
	JourneyID   string       `json:"journey_id"`
	JourneyName string       `json:"journey_name"` // push title
	Sends       []QueuedSend `json:"sends"`
	// Quiet-hours configuration captured at STEP time (QUIET_HOURS_* env,
	// SPEC-W12 §8) so replay stays deterministic if the env changes
	// mid-batch. Empty fields fall back to the contract defaults.
	QuietHoursWindow    string            `json:"quiet_hours_window,omitempty"`
	QuietHoursTimezone  string            `json:"quiet_hours_timezone,omitempty"`
	QuietHoursOverrides map[string]string `json:"quiet_hours_overrides,omitempty"`
}

// RecordSendRequest is the ActivityStudioRecordSend input.
type RecordSendRequest struct {
	TenantID     string `json:"tenant_id"`
	JourneyID    string `json:"journey_id"`
	EnrollmentID string `json:"enrollment_id"`
	StepIdx      int    `json:"step_idx"`
	Status       string `json:"status"` // sent | suppressed_dnd | failed
	Reason       string `json:"reason,omitempty"`
}

// buildPacedRequest renders one queued send to the NotifyPaced payload.
// Pure function — unit-tested without Temporal.
func buildPacedRequest(in StudioSendBatchInput, send QueuedSend) (PacedSendRequest, error) {
	switch send.Kind {
	case KindSMS:
		return PacedSendRequest{Kind: PacedSendGeoCampaign, Geo: &PacedGeoCampaignSend{
			TenantSlug: in.TenantSlug,
			CampaignID: in.JourneyID,
			Channel:    "sms",
			Phone:      send.Phone,
			Name:       send.Name,
			Text:       send.Text,
		}}, nil
	case KindPushMarketing:
		title := in.JourneyName
		if title == "" {
			title = "Campaign Studio"
		}
		return PacedSendRequest{Kind: PacedSendPushMarketing, Push: &PacedPushNotificationSend{
			TenantSlug: in.TenantSlug,
			ContactID:  send.ContactID.String(),
			Phone:      send.Phone,
			Title:      title,
			Body:       send.Text,
			Data: map[string]string{
				"journey_id":    in.JourneyID,
				"enrollment_id": send.EnrollmentID.String(),
			},
		}}, nil
	}
	return PacedSendRequest{}, fmt.Errorf("studio send kind %q has no paced route", send.Kind)
}

// pacedSendChannel extracts the delivery channel for quiet-hours
// per-channel overrides (mirrors notification-worker's
// workflows.PacedSendChannel for the two studio kinds).
func pacedSendChannel(req PacedSendRequest) string {
	switch req.Kind {
	case PacedSendGeoCampaign:
		if req.Geo != nil {
			return req.Geo.Channel
		}
	case PacedSendPushMarketing:
		return "push" // fixed channel key (SPEC-W16 §1)
	}
	return ""
}

// guardedPacedSend executes one paced send with the SPEC-W12 §3 quiet-hours
// guard applied workflow-side, mirroring geo.quiet.go (itself a mirror of
// notification-worker's workflows.GuardedPacedSend). Both studio kinds are
// marketing-class in the pacer table, so the guard always applies; DND
// suppression itself is activity-side and surfaces via the result status.
// The geo package's exported quiet-hours math (QuietHoursOpenAt) is reused
// so the window semantics stay single-sourced within booking-service.
func guardedPacedSend(ctx workflow.Context, req PacedSendRequest, quiet geo.QuietHoursConfig) (PacedSendResult, error) {
	var res PacedSendResult
	open, inWindow, err := geo.QuietHoursOpenAt(workflow.Now(ctx), pacedSendChannel(req), quiet)
	if err != nil {
		return res, err
	}
	if inWindow {
		delay := open.Sub(workflow.Now(ctx))
		if delay > 0 {
			workflow.GetLogger(ctx).Info("quiet hours: deferring studio marketing send until window opens",
				"kind", req.Kind, "channel", pacedSendChannel(req),
				"window_open", open.String(), "delay", delay.String())
			if err := workflow.Sleep(ctx, delay); err != nil {
				return res, err
			}
		}
	}
	if err := workflow.ExecuteActivity(ctx, ActivityNotifyPaced, req).Get(ctx, &res); err != nil {
		return res, err
	}
	if res.Status == "" {
		// Older workers returned no result payload; the send happened.
		res.Status = PacedSendStatusSent
	}
	return res, nil
}

// StudioSendWorkflow performs one batch of journey sends: each queued send
// goes through the guarded paced path (marketing kinds only — DND applied
// activity-side, quiet-hours deferred workflow-side), then the outcome is
// recorded via the StudioRecordSendOutcome activity. A failed send is
// recorded as send_failed and does NOT abort the batch (waitlist backfill
// pattern — one recipient's provider failure must not block the rest).
func StudioSendWorkflow(ctx workflow.Context, in StudioSendBatchInput) error {
	logger := workflow.GetLogger(ctx)
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		HeartbeatTimeout:    30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)
	quiet := geo.QuietHoursFromEnv(in.QuietHoursWindow, in.QuietHoursTimezone, in.QuietHoursOverrides)

	sent, suppressed, failed := 0, 0, 0
	for _, send := range in.Sends {
		req, err := buildPacedRequest(in, send)
		if err != nil {
			logger.Error("studio send has no paced route; recording skipped",
				"enrollment_id", send.EnrollmentID.String(), "error", err)
			continue
		}
		res, err := guardedPacedSend(ctx, req, quiet)
		status := PacedSendStatusSent
		reason := ""
		switch {
		case err != nil:
			status, failed = "failed", failed+1
			reason = err.Error()
			logger.Error("studio paced send failed",
				"journey_id", in.JourneyID, "enrollment_id", send.EnrollmentID.String(), "error", err)
		case res.Status == PacedSendStatusSuppressedDND:
			status, suppressed, reason = PacedSendStatusSuppressedDND, suppressed+1, res.Reason
		default:
			sent++
		}
		// Best-effort outcome recording: a recording failure is logged but
		// never aborts the remaining sends.
		if err := workflow.ExecuteActivity(ctx, ActivityStudioRecordSend, RecordSendRequest{
			TenantID:     in.TenantID,
			JourneyID:    in.JourneyID,
			EnrollmentID: send.EnrollmentID.String(),
			StepIdx:      send.StepIdx,
			Status:       status,
			Reason:       reason,
		}).Get(ctx, nil); err != nil {
			logger.Error("studio send outcome recording failed",
				"enrollment_id", send.EnrollmentID.String(), "status", status, "error", err)
		}
	}
	logger.Info("studio send batch completed", "journey_id", in.JourneyID,
		"batch_id", in.BatchID, "sent", sent, "suppressed_dnd", suppressed, "failed", failed)
	return nil
}

// ---------------------------------------------------------------------------
// Activities + worker registration (integrator wiring)
// ---------------------------------------------------------------------------

// SendActivities bundles the DB-backed activity dependencies. The
// integrator registers them on booking-service's Temporal worker via
// RegisterWorker (additive block in cmd/server/main.go, mirroring the geo
// campaign registration).
type SendActivities struct {
	Store  *Store
	Logger *zap.Logger
}

func (a *SendActivities) log() *zap.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return zap.NewNop()
}

// RecordSendOutcome persists the send_sent / send_suppressed / send_failed
// step event for one queued send.
func (a *SendActivities) RecordSendOutcome(ctx context.Context, req RecordSendRequest) error {
	if a.Store == nil {
		return fmt.Errorf("studio send activities: store not configured")
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		return fmt.Errorf("record send outcome: bad tenant id: %w", err)
	}
	journeyID, err := uuid.Parse(req.JourneyID)
	if err != nil {
		return fmt.Errorf("record send outcome: bad journey id: %w", err)
	}
	enrollmentID, err := uuid.Parse(req.EnrollmentID)
	if err != nil {
		return fmt.Errorf("record send outcome: bad enrollment id: %w", err)
	}
	kind := EventSendSent
	switch req.Status {
	case PacedSendStatusSent:
		kind = EventSendSent
	case PacedSendStatusSuppressedDND:
		kind = EventSendSuppressed
	case "failed":
		kind = EventSendFailed
	default:
		return fmt.Errorf("record send outcome: unknown status %q", req.Status)
	}
	return a.Store.RecordSendOutcome(ctx, tenantID, journeyID, enrollmentID, req.StepIdx, kind, req.Reason)
}

// RegisterWorker registers the StudioSendWorkflow + its outcome activity
// on booking-service's Temporal worker. The INTEGRATOR calls this in the
// additive worker block of cmd/server/main.go (mirroring the geo campaign
// registration):
//
//	studioActs := &campaignstudio.SendActivities{Store: studioStore, Logger: logger}
//	campaignstudio.RegisterWorker(w, studioActs)
func RegisterWorker(w worker.Worker, acts *SendActivities) {
	w.RegisterWorkflowWithOptions(StudioSendWorkflow, workflow.RegisterOptions{Name: WorkflowTypeStudioSend})
	w.RegisterActivityWithOptions(acts.RecordSendOutcome, activity.RegisterOptions{Name: ActivityStudioRecordSend})
}

// ---------------------------------------------------------------------------
// Starter (HTTP step endpoint → workflow)
// ---------------------------------------------------------------------------

// SendStarter abstracts the Temporal starter (mirrors geo.CampaignStarter)
// so handlers stay testable. nil on Handlers → send steps are deferred
// (SendsDeferred) instead of erroring the whole step call.
type SendStarter interface {
	StartStudioSendBatch(ctx context.Context, in StudioSendBatchInput) (string, error)
}

// TemporalStarter implements SendStarter against a real Temporal client.
// The integrator builds it from temporalclient.Client.Underlying() +
// cfg.TemporalTaskQueue — no temporalclient edits required:
//
//	Starter: campaignstudio.TemporalStarter{Client: tc.Underlying(), TaskQueue: cfg.TemporalTaskQueue}
type TemporalStarter struct {
	Client    client.Client
	TaskQueue string
}

// StartStudioSendBatch starts StudioSendWorkflow with workflow ID
// "studio-send-{batchID}" (REJECT_DUPLICATE + already-started tolerance,
// mirroring temporalclient.StartGeoCampaign): a retried step call that
// re-uses the batch id can never fan a batch out twice.
func (s TemporalStarter) StartStudioSendBatch(ctx context.Context, in StudioSendBatchInput) (string, error) {
	id := "studio-send-" + in.BatchID
	_, err := s.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        id,
		TaskQueue: s.TaskQueue,
	}, WorkflowTypeStudioSend, in)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			return id, nil
		}
		return "", fmt.Errorf("execute %s: %w", WorkflowTypeStudioSend, err)
	}
	return id, nil
}
