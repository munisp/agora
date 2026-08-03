package geo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	"go.uber.org/zap"
)

// Geo-targeted campaigns (SPEC-W8 A2). booking-service owns the
// GeoCampaignWorkflow and its DB activities; it runs its own Temporal
// worker on the shared opendesk-main task queue (main.go). Every
// recipient send is delegated to the EXISTING paced notification path:
// the workflow schedules the "NotifyPaced" activity, which the
// notification-worker executes (CPS pacing + sender rotation) with the
// paced kind "geo_campaign".
//
// Idempotent replay: sends are recorded in the geo_campaign_sends ledger
// (one row per campaign+contact). Each batch is filtered against the
// ledger BEFORE sending, so a campaign restarted after a failure skips
// contacts already sent; ledger inserts are ON CONFLICT DO NOTHING, so
// activity retries can never double-count either.
//
// Heartbeats: the batch/record activities heartbeat their progress
// (HeartbeatTimeout 30s) so a stuck worker fails over instead of hanging
// a running campaign.

const (
	// WorkflowType is the registered name of the geo campaign workflow.
	WorkflowType = "GeoCampaignWorkflow"

	// Activity names (registered on booking-service's worker).
	ActivityGeoAudienceBatch    = "GeoCampaignAudienceBatch"
	ActivityGeoFilterUnsent     = "GeoCampaignFilterUnsent"
	ActivityGeoRecordSends      = "GeoCampaignRecordSends"
	ActivityGeoCompleteCampaign = "GeoCampaignCompleteCampaign"
	ActivityGeoFailCampaign     = "GeoCampaignFailCampaign"

	// ActivityNotifyPaced mirrors notification-worker's paced wrapper
	// activity name (service boundary: duplicated, not shared).
	ActivityNotifyPaced = "NotifyPaced"

	// PacedSendGeoCampaign is the NotifyPaced kind for campaign sends;
	// notification-worker routes it to SendGeoCampaignMessage.
	PacedSendGeoCampaign = "geo_campaign"

	// DefaultGeoCampaignBatch applies when GEO_CAMPAIGN_BATCH is unset.
	DefaultGeoCampaignBatch = 50
)

// GeoCampaignInput starts a GeoCampaignWorkflow.
type GeoCampaignInput struct {
	CampaignID string `json:"campaign_id"`
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Channel    string `json:"channel"` // whatsapp | telegram | sms
	Message    string `json:"message"` // {name} personalization token supported
	BatchSize  int    `json:"batch_size"`
	// Quiet-hours configuration captured at SCHEDULE time (from the
	// QUIET_HOURS_* env by the API handler) so replay stays deterministic
	// when the env changes mid-campaign (SPEC-W12 §8, SPEC-W14 Agent D).
	// Empty fields fall back to the contract defaults (20:00-08:00,
	// Africa/Lagos).
	QuietHoursWindow    string            `json:"quiet_hours_window,omitempty"`
	QuietHoursTimezone  string            `json:"quiet_hours_timezone,omitempty"`
	QuietHoursOverrides map[string]string `json:"quiet_hours_overrides,omitempty"`
}

// PacedSendRequest mirrors notification-worker's workflows.PacedSendRequest
// for the geo_campaign kind only (service boundary: duplicated, not
// shared); the JSON contract must stay field-compatible.
type PacedSendRequest struct {
	Kind string                `json:"kind"`
	Geo  *PacedGeoCampaignSend `json:"geo_campaign,omitempty"`
}

// PacedGeoCampaignSend carries the SendGeoCampaignMessage arguments.
type PacedGeoCampaignSend struct {
	TenantSlug string `json:"tenant_slug"`
	CampaignID string `json:"campaign_id"`
	Channel    string `json:"channel"`
	Phone      string `json:"phone"`
	Name       string `json:"name"`
	Text       string `json:"text"` // {name} already substituted
}

// Personalize substitutes the {name} message token (deterministic, safe
// for workflow code).
func Personalize(message, name string) string {
	return strings.ReplaceAll(message, "{name}", name)
}

// Activity IO contracts.
type (
	// AudienceBatchRequest pages the campaign audience (keyset on
	// contact_id; After="" starts from the beginning).
	AudienceBatchRequest struct {
		TenantID   string `json:"tenant_id"`
		CampaignID string `json:"campaign_id"`
		After      string `json:"after"`
		Limit      int    `json:"limit"`
	}
	// FilterUnsentRequest asks for the ledger-not-yet-sent subset.
	FilterUnsentRequest struct {
		TenantID   string                    `json:"tenant_id"`
		CampaignID string                    `json:"campaign_id"`
		Recipients []store.CampaignRecipient `json:"recipients"`
	}
	// RecordSendsRequest records one sent batch (ledger + usage outbox +
	// audience_count bump, atomically).
	RecordSendsRequest struct {
		TenantID   string                    `json:"tenant_id"`
		TenantSlug string                    `json:"tenant_slug"`
		CampaignID string                    `json:"campaign_id"`
		Recipients []store.CampaignRecipient `json:"recipients"`
	}
	// CampaignStatusRequest completes/fails a campaign.
	CampaignStatusRequest struct {
		TenantID   string `json:"tenant_id"`
		CampaignID string `json:"campaign_id"`
	}
)

// GeoCampaignWorkflow batches the audience of one campaign (GEO_CAMPAIGN_BATCH
// contacts per batch), sends the personalized message to every recipient via
// the guarded paced send (kind geo_campaign — marketing, so quiet-hours
// deferred per SPEC-W12 §8 and DND-suppressed activity-side per §3), records
// sends for idempotent replay, and finally flips the campaign to completed
// (or failed). A failed send skips that recipient without aborting the
// campaign (waitlist backfill pattern).
//
// suppressed_dnd outcomes (SPEC-W14 Agent D): a suppressed send is a
// completion status, not an error — the recipient consumed no CPS token and
// no message was sent, so it is NOT written to the geo_campaign_sends
// ledger (which tracks messages actually sent and drives the
// geo_campaign_message metering) and NOT counted in audience_count. The
// workflow records the suppression in its logs and campaign totals; a
// restarted campaign re-evaluates the recipient against the DND registry,
// which is authoritative at send time (an opt-out honored between runs
// lifts the suppression).
func GeoCampaignWorkflow(ctx workflow.Context, in GeoCampaignInput) error {
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

	batchSize := in.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultGeoCampaignBatch
	}
	// Quiet-hours config captured at schedule time (SPEC-W12 §8): marketing
	// sends inside the window are deferred until it opens.
	quiet := QuietHoursFromEnv(in.QuietHoursWindow, in.QuietHoursTimezone, in.QuietHoursOverrides)

	after := ""
	sentTotal := 0
	suppressedTotal := 0
	var runErr error
	for {
		var batch []store.CampaignRecipient
		if err := workflow.ExecuteActivity(ctx, ActivityGeoAudienceBatch,
			AudienceBatchRequest{TenantID: in.TenantID, CampaignID: in.CampaignID, After: after, Limit: batchSize}).Get(ctx, &batch); err != nil {
			runErr = fmt.Errorf("audience batch: %w", err)
			break
		}
		if len(batch) == 0 {
			break
		}
		// Idempotent replay: skip contacts already sent for this campaign.
		var unsent []store.CampaignRecipient
		if err := workflow.ExecuteActivity(ctx, ActivityGeoFilterUnsent,
			FilterUnsentRequest{TenantID: in.TenantID, CampaignID: in.CampaignID, Recipients: batch}).Get(ctx, &unsent); err != nil {
			runErr = fmt.Errorf("filter unsent: %w", err)
			break
		}
		sent := make([]store.CampaignRecipient, 0, len(unsent))
		for _, r := range unsent {
			req := PacedSendRequest{Kind: PacedSendGeoCampaign, Geo: &PacedGeoCampaignSend{
				TenantSlug: in.TenantSlug,
				CampaignID: in.CampaignID,
				Channel:    in.Channel,
				Phone:      r.Phone,
				Name:       r.Name,
				Text:       Personalize(in.Message, r.Name),
			}}
			res, err := guardedPacedSend(ctx, req, quiet)
			if err != nil {
				logger.Error("geo campaign send failed; skipping recipient",
					"campaign_id", in.CampaignID, "contact_id", r.ContactID.String(), "error", err)
				continue
			}
			if res.Status == PacedSendStatusSuppressedDND {
				// suppressed_dnd is a completion status, not an error: no
				// message was sent, so the recipient is recorded as a
				// suppression (logs + totals) and kept out of the send
				// ledger / usage metering.
				suppressedTotal++
				logger.Info("geo campaign send suppressed by DND guard",
					"campaign_id", in.CampaignID, "contact_id", r.ContactID.String(),
					"phone", MaskPhone(r.Phone), "reason", res.Reason,
					"status", res.Status)
				continue
			}
			sent = append(sent, r)
		}
		if len(sent) > 0 {
			if err := workflow.ExecuteActivity(ctx, ActivityGeoRecordSends,
				RecordSendsRequest{TenantID: in.TenantID, TenantSlug: in.TenantSlug, CampaignID: in.CampaignID, Recipients: sent}).Get(ctx, nil); err != nil {
				runErr = fmt.Errorf("record sends: %w", err)
				break
			}
			sentTotal += len(sent)
		}
		after = batch[len(batch)-1].ContactID.String()
	}

	// Terminal status transition, even when the workflow is being cancelled
	// (disconnected context).
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	dctx = workflow.WithActivityOptions(dctx, ao)
	statusActivity := ActivityGeoCompleteCampaign
	if runErr != nil {
		statusActivity = ActivityGeoFailCampaign
	}
	if err := workflow.ExecuteActivity(dctx, statusActivity,
		CampaignStatusRequest{TenantID: in.TenantID, CampaignID: in.CampaignID}).Get(dctx, nil); err != nil {
		logger.Error("geo campaign status update failed", "campaign_id", in.CampaignID, "error", err)
	}
	if runErr != nil {
		return runErr
	}
	logger.Info("geo campaign completed", "campaign_id", in.CampaignID,
		"sent", sentTotal, "suppressed_dnd", suppressedTotal)
	return nil
}

// CampaignActivities bundles the DB-backed activity dependencies.
type CampaignActivities struct {
	Store      *store.Store
	UsageTopic string // opendesk.usage.events; empty disables metering
	Logger     *zap.Logger
}

func parseIDs(tenant, campaign string) (uuid.UUID, uuid.UUID, error) {
	tenantID, err := uuid.Parse(tenant)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("tenant_id: %w", err)
	}
	campaignID, err := uuid.Parse(campaign)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("campaign_id: %w", err)
	}
	return tenantID, campaignID, nil
}

// AudienceBatch returns the next keyset page of campaign audience contacts.
func (a *CampaignActivities) AudienceBatch(ctx context.Context, req AudienceBatchRequest) ([]store.CampaignRecipient, error) {
	tenantID, campaignID, err := parseIDs(req.TenantID, req.CampaignID)
	if err != nil {
		return nil, err
	}
	after := uuid.Nil
	if req.After != "" {
		if after, err = uuid.Parse(req.After); err != nil {
			return nil, fmt.Errorf("after: %w", err)
		}
	}
	activity.RecordHeartbeat(ctx, req.After)
	return a.Store.GeoCampaignAudienceBatch(ctx, tenantID, campaignID, after, req.Limit)
}

// FilterUnsent drops recipients already present in the send ledger
// (idempotent replay skip).
func (a *CampaignActivities) FilterUnsent(ctx context.Context, req FilterUnsentRequest) ([]store.CampaignRecipient, error) {
	tenantID, campaignID, err := parseIDs(req.TenantID, req.CampaignID)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(req.Recipients))
	for _, r := range req.Recipients {
		ids = append(ids, r.ContactID)
	}
	sent, err := a.Store.GeoCampaignSentContacts(ctx, tenantID, campaignID, ids)
	if err != nil {
		return nil, err
	}
	unsent := make([]store.CampaignRecipient, 0, len(req.Recipients))
	for _, r := range req.Recipients {
		if !sent[r.ContactID] {
			unsent = append(unsent, r)
		}
	}
	activity.RecordHeartbeat(ctx, len(unsent))
	return unsent, nil
}

// RecordSends records one sent batch: ledger rows (ON CONFLICT DO NOTHING),
// one geo_campaign_message usage outbox row per NEW send, and the
// audience_count bump — all in one transaction.
func (a *CampaignActivities) RecordSends(ctx context.Context, req RecordSendsRequest) (int, error) {
	tenantID, campaignID, err := parseIDs(req.TenantID, req.CampaignID)
	if err != nil {
		return 0, err
	}
	ids := make([]uuid.UUID, 0, len(req.Recipients))
	for _, r := range req.Recipients {
		ids = append(ids, r.ContactID)
	}
	var usageExtra func(contactID uuid.UUID) []store.ExtraOutbox
	if a.UsageTopic != "" {
		usageExtra = func(contactID uuid.UUID) []store.ExtraOutbox {
			payload, err := MarshalGeoUsageRecord(req.TenantSlug, tenantID, campaignID, contactID)
			if err != nil {
				a.Logger.Warn("geo usage record marshal failed; skipping metering", zap.Error(err))
				return nil
			}
			return []store.ExtraOutbox{{Topic: a.UsageTopic, Payload: payload}}
		}
	}
	recorded, err := a.Store.RecordGeoCampaignSends(ctx, tenantID, campaignID, ids, usageExtra)
	activity.RecordHeartbeat(ctx, recorded)
	return recorded, err
}

// CompleteCampaign flips the campaign to completed.
func (a *CampaignActivities) CompleteCampaign(ctx context.Context, req CampaignStatusRequest) error {
	tenantID, campaignID, err := parseIDs(req.TenantID, req.CampaignID)
	if err != nil {
		return err
	}
	return a.Store.SetGeoCampaignStatus(ctx, tenantID, campaignID, store.GeoCampaignCompleted)
}

// FailCampaign flips the campaign to failed.
func (a *CampaignActivities) FailCampaign(ctx context.Context, req CampaignStatusRequest) error {
	tenantID, campaignID, err := parseIDs(req.TenantID, req.CampaignID)
	if err != nil {
		return err
	}
	return a.Store.SetGeoCampaignStatus(ctx, tenantID, campaignID, store.GeoCampaignFailed)
}
