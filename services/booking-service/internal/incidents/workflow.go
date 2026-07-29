package incidents

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Incident outreach (SPEC-W11 Part B §5). booking-service owns the tiny
// IncidentAlertWorkflow; it runs on booking-service's own Temporal worker
// on the shared opendesk-main task queue (same pattern as
// GeoCampaignWorkflow). The actual send is delegated to the EXISTING paced
// notification path: the workflow schedules the "NotifyPaced" activity with
// kind incident_alert and priority=true, which the notification-worker
// executes through its CPS pacer PRIORITY FAST-LANE (immediate dispatch,
// still metered).
const (
	// WorkflowTypeAlert is the registered name of the incident alert workflow.
	WorkflowTypeAlert = "IncidentAlertWorkflow"

	// ActivityNotifyPaced mirrors notification-worker's paced wrapper
	// activity name (service boundary: duplicated, not shared).
	ActivityNotifyPaced = "NotifyPaced"

	// PacedSendIncidentAlert is the NotifyPaced kind for incident outreach;
	// notification-worker routes it to SendIncidentAlert.
	PacedSendIncidentAlert = "incident_alert"
)

// PacedSendRequest mirrors notification-worker's workflows.PacedSendRequest
// for the incident_alert kind only (service boundary: duplicated, not
// shared); the JSON contract must stay field-compatible. Priority engages
// the pacer fast-lane.
type PacedSendRequest struct {
	Kind          string                 `json:"kind"`
	Priority      bool                   `json:"priority,omitempty"`
	IncidentAlert *PacedIncidentAlertArg `json:"incident_alert,omitempty"`
}

// PacedIncidentAlertArg carries the SendIncidentAlert arguments.
type PacedIncidentAlertArg struct {
	TenantSlug string `json:"tenant_slug"`
	IncidentID string `json:"incident_id"`
	Channel    string `json:"channel"` // whatsapp | telegram | sms
	Phone      string `json:"phone"`
	Text       string `json:"text"`
}

// IncidentAlertWorkflow sends one incident outreach message via the paced
// fast-lane. One activity, no timers: urgency beats durability here — a
// lost alert is re-drivable by re-ingesting the incident (idempotent).
func IncidentAlertWorkflow(ctx workflow.Context, in AlertStart) error {
	ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    15 * time.Second,
			MaximumAttempts:    3,
		},
	})
	req := PacedSendRequest{
		Kind:     PacedSendIncidentAlert,
		Priority: true,
		IncidentAlert: &PacedIncidentAlertArg{
			TenantSlug: in.TenantSlug,
			IncidentID: in.IncidentID,
			Channel:    in.Channel,
			Phone:      in.Phone,
			Text:       in.Text,
		},
	}
	return workflow.ExecuteActivity(ao, ActivityNotifyPaced, req).Get(ctx, nil)
}
