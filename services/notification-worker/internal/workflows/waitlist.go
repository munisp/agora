package workflows

import (
	"time"

	"go.temporal.io/sdk/workflow"
)

// Waitlist backfill (SPEC-W3 §3 innovation 7): when a booking is cancelled,
// the top-3 waiting waitlist entries for the offering are notified with
// their single-use claim token + link. Claiming itself is user-driven
// (POST /v1/waitlist/{id}/claim on booking-service, transactional) — this
// workflow deliberately stops after notifying.
const (
	ActivityListWaitlistEntries   = "ListWaitlistEntries"
	ActivitySendWaitlistClaimNote = "SendWaitlistClaimNotification"

	// WaitlistBackfillBatch is how many waiting entries are notified per
	// cancelled booking (SPEC-W3: top 3, FIFO by created_at).
	WaitlistBackfillBatch = 3
)

// WaitlistBackfillInput starts a WaitlistBackfillWorkflow.
type WaitlistBackfillInput struct {
	BookingID  string `json:"booking_id"` // the cancelled booking (workflow-id suffix)
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	OfferingID string `json:"offering_id"`
}

// WaitlistEntry mirrors booking-service's waitlist entry JSON (service
// boundary: duplicated, not shared).
type WaitlistEntry struct {
	ID           string    `json:"id"`
	OfferingID   string    `json:"offering_id"`
	ContactName  string    `json:"contact_name"`
	ContactPhone string    `json:"contact_phone"`
	WindowStart  time.Time `json:"window_start"`
	WindowEnd    time.Time `json:"window_end"`
	Status       string    `json:"status"`
	ClaimToken   string    `json:"claim_token"`
}

// WaitlistBackfillWorkflow is started by the signals bridge on every
// BookingCancelled event: fetch waiting entries (Dapr invoke booking
// GET /v1/waitlist?offering_id&status=waiting) and notify the top 3 with
// their claim token + link via the email/sms bindings activities.
func WaitlistBackfillWorkflow(ctx workflow.Context, in WaitlistBackfillInput) error {
	logger := workflow.GetLogger(ctx)
	ctx = workflow.WithActivityOptions(ctx, sagaActivityOptions())

	var entries []WaitlistEntry
	if err := workflow.ExecuteActivity(ctx, ActivityListWaitlistEntries, in).Get(ctx, &entries); err != nil {
		return err
	}
	if len(entries) == 0 {
		logger.Info("waitlist backfill: nobody waiting", "offering_id", in.OfferingID)
		return nil
	}
	if len(entries) > WaitlistBackfillBatch {
		entries = entries[:WaitlistBackfillBatch]
	}
	for _, e := range entries {
		// A failed notification must not block the remaining candidates.
		// Sends go through NotifyPaced: the activity acquires an outbound
		// CPS token + rotates the sender number before dialing (VOICE-SCALING §4).
		req := PacedSendRequest{Kind: PacedSendWaitlistClaim, Waitlist: &PacedWaitlistSend{Input: in, Entry: e}}
		if err := workflow.ExecuteActivity(ctx, ActivityNotifyPaced, req).Get(ctx, nil); err != nil {
			logger.Error("waitlist claim notification failed", "entry_id", e.ID, "error", err)
		}
	}
	logger.Info("waitlist backfill notified", "offering_id", in.OfferingID, "notified", len(entries))
	return nil
}

// WaitlistNotifyWorkflow is started by the signals bridge on a
// com.opendesk.booking.WaitlistEntryNotified event (SPEC-W48 A2/G): a staff
// member manually notified ONE waitlist entry on booking-service (which
// already flipped it waiting → notified and carries the entry — claim_token
// included — in the event payload). It delivers exactly one claim
// notification through the SAME paced send path as the backfill
// (PacedSendWaitlistClaim → SendWaitlistClaimNotification; the claim link
// is built from the entry's single-use claim_token).
//
// It deliberately does NOT list the waitlist: the target entry is no longer
// `waiting`, and re-running the backfill listing would notify the next
// candidates instead of the staff-chosen entry (double-send).
func WaitlistNotifyWorkflow(ctx workflow.Context, send PacedWaitlistSend) error {
	logger := workflow.GetLogger(ctx)
	ctx = workflow.WithActivityOptions(ctx, sagaActivityOptions())
	req := PacedSendRequest{Kind: PacedSendWaitlistClaim, Waitlist: &send}
	if err := workflow.ExecuteActivity(ctx, ActivityNotifyPaced, req).Get(ctx, nil); err != nil {
		// Mirrors the backfill: a failed notification is logged, never
		// fatal — delivery is best-effort and the claim link stays valid.
		logger.Error("waitlist notify send failed", "entry_id", send.Entry.ID, "error", err)
		return nil
	}
	logger.Info("waitlist entry notified", "entry_id", send.Entry.ID)
	return nil
}
