package workflows

// Outbound CPS pacing (docs/VOICE-SCALING.md §4 telephony plane).
//
// Workflows are deterministic and must never sleep or rate-limit inline, so
// pacing lives activity-side: every outbound send goes through the single
// NotifyPaced activity, which acquires a CPS token and rotates the sender
// number (internal/pacer) BEFORE dispatching to the underlying send
// activity. Workflows only build a PacedSendRequest.

import (
	"time"

	"github.com/opendesk/notification-worker/internal/pacer"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	// ActivityNotifyPaced is the name of the pacing wrapper activity.
	ActivityNotifyPaced = "NotifyPaced"

	// PacedSendWaitlistClaim routes to SendWaitlistClaimNotification
	// (SPEC-W3 §3 innovation 7 waitlist backfill).
	PacedSendWaitlistClaim = "waitlist_claim"
	// PacedSendReminder routes to SendReminder (T-24h / T-1h reminders).
	PacedSendReminder = "reminder"
	// PacedSendDepositReminder routes to SendDepositReminder (salon pack:
	// missing-deposit nudge inside the cancellation window).
	PacedSendDepositReminder = "deposit_reminder"
	// PacedSendNoShow routes to SendNoShowNotification (no-show follow-up).
	PacedSendNoShow = "no_show"
	// PacedSendCancellation routes to SendCancellationNotice.
	PacedSendCancellation = "cancellation"
	// PacedSendIncidentAlert routes to SendIncidentAlert (SPEC-W11 Part B:
	// critical/high incident outreach, priority lane).
	PacedSendIncidentAlert = "incident_alert"
	// PacedSendGeoCampaign routes to SendGeoCampaignMessage (SPEC-W8:
	// proximity marketing batches, marketing class).
	PacedSendGeoCampaign = "geo_campaign"
	// PacedSendPushNotification routes to SendPushNotification (SPEC-W16
	// contract §3: transactional push — DND-exempt).
	PacedSendPushNotification = "push_notification"
	// PacedSendPushMarketing routes to SendPushNotification as the
	// MARKETING push class (SPEC-W16 contract §3: DND guard applies — the
	// payload phone field lets the guard check phone-keyed registries).
	PacedSendPushMarketing = "push_marketing"
)

// SendClass is the DND classification of one outbound send (SPEC-W12 §3).
type SendClass string

const (
	// SendClassTransactional sends are DND-exempt (reminders, claim links,
	// deposit nudges, cancellations, no-show follow-ups, incident alerts,
	// transactional push).
	SendClassTransactional SendClass = "transactional"
	// SendClassMarketing sends are suppressed by an active DND record
	// (geo campaigns, marketing push).
	SendClassMarketing SendClass = "marketing"
)

// sendClasses maps every paced kind to its DND class (SPEC-W12 §3 table).
// SPEC-W16 contract §3: push_notification = TRANSACTIONAL (DND-exempt),
// push_marketing = MARKETING (DND-suppressed).
var sendClasses = map[string]SendClass{
	PacedSendWaitlistClaim:    SendClassTransactional,
	PacedSendReminder:         SendClassTransactional,
	PacedSendDepositReminder:  SendClassTransactional,
	PacedSendNoShow:           SendClassTransactional,
	PacedSendCancellation:     SendClassTransactional,
	PacedSendIncidentAlert:    SendClassTransactional,
	PacedSendGeoCampaign:      SendClassMarketing,
	PacedSendPushNotification: SendClassTransactional,
	PacedSendPushMarketing:    SendClassMarketing,
}

// PacedSendRequest is the single pacing entry point's payload. Exactly one
// of the fields is set, selecting the underlying send activity.
type PacedSendRequest struct {
	Kind      string                     `json:"kind"`
	Claim     *WaitlistClaimNotification `json:"claim,omitempty"`
	Reminder  *ReminderNotification      `json:"reminder,omitempty"`
	NoShow    *NoShowNotification        `json:"no_show,omitempty"`
	Cancel    *CancellationNotification  `json:"cancel,omitempty"`
	Incident  *IncidentAlert             `json:"incident,omitempty"`
	Geo       *GeoCampaignSend           `json:"geo_campaign,omitempty"`
	Push      *PushNotificationSend      `json:"push,omitempty"`
	Priority  bool                       `json:"priority,omitempty"`  // incident alerts: use the priority lane
	RatePerSec int                      `json:"rate_per_sec,omitempty"` // override CPS (0 = default)
}

// WaitlistClaimNotification carries SendWaitlistClaimNotification args.
type WaitlistClaimNotification struct {
	TenantSlug string `json:"tenant_slug"`
	EntryID    string `json:"entry_id"`
	ClaimToken string `json:"claim_token"`
}

// ReminderNotification carries SendReminder / SendDepositReminder args.
type ReminderNotification struct {
	TenantSlug string `json:"tenant_slug"`
	BookingID  string `json:"booking_id"`
	Kind       string `json:"kind"` // t24h | t1h | deposit
}

// NoShowNotification carries SendNoShowNotification args.
type NoShowNotification struct {
	TenantSlug string `json:"tenant_slug"`
	BookingID  string `json:"booking_id"`
}

// CancellationNotification carries SendCancellationNotice args.
type CancellationNotification struct {
	TenantSlug string `json:"tenant_slug"`
	BookingID  string `json:"booking_id"`
	Reason     string `json:"reason"`
}

// IncidentAlert carries SendIncidentAlert args (SPEC-W11 Part B §5).
type IncidentAlert struct {
	TenantSlug string `json:"tenant_slug"`
	IncidentID string `json:"incident_id"`
	ContactID  string `json:"contact_id"`
	Channel    string `json:"channel"` // sms | push
	Text       string `json:"text"`
}

// GeoCampaignSend carries SendGeoCampaignMessage args (SPEC-W8).
type GeoCampaignSend struct {
	TenantSlug string `json:"tenant_slug"`
	CampaignID string `json:"campaign_id"`
	Channel    string `json:"channel"` // sms | push
	Phone      string `json:"phone"`
	Name       string `json:"name"`
	Text       string `json:"text"`
}

// PushNotificationSend carries SendPushNotification args (SPEC-W16
// contract §1/§3). Phone is OPTIONAL but recommended for marketing sends:
// it lets the DND guard check the phone-keyed registries (SPEC-W12 §3)
// for contacts whose DND record predates device registration.
type PushNotificationSend struct {
	TenantSlug string            `json:"tenant_slug"`
	ContactID  string            `json:"contact_id,omitempty"`
	Phone      string            `json:"phone,omitempty"`
	Title      string            `json:"title"`
	Body       string            `json:"body"`
	Data       map[string]string `json:"data,omitempty"`
	App        string            `json:"app,omitempty"` // device app filter ("" = all)
}

// PacedSendResult reports the pacing outcome to the workflow. Status
// "sent" | "suppressed_dnd"; suppressed sends carry the reason.
type PacedSendResult struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// pacedChannel extracts the delivery channel for quiet-hours overrides.
func pacedChannel(req PacedSendRequest) string {
	switch req.Kind {
	case PacedSendGeoCampaign:
		if req.Geo != nil {
			return req.Geo.Channel
		}
	case PacedSendIncidentAlert:
		if req.Incident != nil {
			return req.Incident.Channel
		}
	case PacedSendPushNotification, PacedSendPushMarketing:
		return "push"
	}
	return "sms"
}

// GuardedPacedSend executes one paced send with the SPEC-W12 §8 quiet-hours
// deferral applied workflow-side (activities may not sleep; the DND guard
// itself runs activity-side inside NotifyPaced). Transactional kinds are
// never deferred; marketing kinds are deferred until the quiet window
// opens (default 20:00-08:00 Africa/Lagos, per-channel overrides).
//
// This helper is the workflow-side half of the quiet-hours contract; the
// pacing + DND halves live in internal/pacer + NotifyPaced.
func GuardedPacedSend(ctx workflow.Context, req PacedSendRequest) (PacedSendResult, error) {
	var res PacedSendResult
	class := sendClasses[req.Kind]
	if class == "" {
		class = SendClassTransactional
	}
	if class == SendClassMarketing {
		open, inWindow, err := QuietHoursOpenAt(workflow.Now(ctx), pacedChannel(req))
		if err != nil {
			return res, err
		}
		if inWindow {
			delay := open.Sub(workflow.Now(ctx))
			if delay > 0 {
				workflow.GetLogger(ctx).Info("quiet hours: deferring marketing send until window opens",
					"kind", req.Kind, "channel", pacedChannel(req),
					"window_open", open.String(), "delay", delay.String())
				if err := workflow.Sleep(ctx, delay); err != nil {
					return res, err
				}
			}
		}
	}
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		HeartbeatTimeout:    30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    5,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)
	if err := workflow.ExecuteActivity(ctx, ActivityNotifyPaced, req).Get(ctx, &res); err != nil {
		return res, err
	}
	if res.Status == "" {
		res.Status = "sent"
	}
	return res, nil
}

// PacerDeps wires the activity-side pacer for NotifyPaced (registered by
// the worker bootstrap).
type PacerDeps struct {
	Pacer *pacer.Pacer
}
