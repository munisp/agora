package surveys

// CloudEvents lifecycle events, usage metering and the invite PacedSend
// command envelopes for SPEC-W20 Agent B. All three ride the transactional
// outbox (Store.EnqueueOutbox) and are best-effort post-commit — the same
// posture as internal/referrals/metering.go: eventing/metering/notification
// must never block a send or a response; failures are logged for
// reconciliation.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// Lifecycle event types published to the surveys events topic
// (opendesk.surveys.events.v1, SPEC-W20 contract §5: sent/answered).
const (
	// EventTypeInviteSent fires once per invite whose paced send command
	// was successfully enqueued to the notifications outbox.
	EventTypeInviteSent = "com.opendesk.surveys.InviteSent"
	// EventTypeResponseReceived fires once per accepted public response
	// (never on the idempotent-replay path, which 409s before insert).
	EventTypeResponseReceived = "com.opendesk.surveys.ResponseReceived"
)

// UsageMetricResponseReceived is the metered unit emitted once per
// accepted survey response (SPEC-W20 contract §4:
// survey_response_received) on the usage topic (opendesk.usage.events).
// Value is ALWAYS 1; context lives in meta.
const UsageMetricResponseReceived = "survey_response_received"

// ---------------------------------------------------------------------------
// PacedSend CloudEvent contract mirror (notification-worker
// internal/notifyoutbox/consumer.go + internal/workflows/paced.go).
// Duplicated, not shared (service boundary) — the JSON tags MUST stay
// field-compatible with workflows.PacedSendRequest.
// ---------------------------------------------------------------------------

const (
	// EventTypePacedSend is the notifications-topic command type whose
	// CloudEvent data IS a workflows.PacedSendRequest; the worker's
	// notifyoutbox consumer starts one PacedSendWorkflow per command
	// (workflow id "paced-send-<cloudevent id>", so redelivery cannot
	// double-send).
	EventTypePacedSend = "com.opendesk.notifications.PacedSend"

	// pacedKindGeoCampaign mirrors workflows.PacedSendGeoCampaign — the
	// notification-worker's ONLY SMS marketing route
	// (SendGeoCampaignMessage renders channel sms via the twilio binding).
	// MARKETING-class: DND-suppressed activity-side, quiet-hours deferred
	// workflow-side (GuardedPacedSend).
	pacedKindGeoCampaign = "geo_campaign"
	// pacedKindPushMarketing mirrors workflows.PacedSendPushMarketing —
	// MARKETING-class push (SendPushNotification fan-out; the phone lets
	// the DND guard check the phone-keyed registries).
	pacedKindPushMarketing = "push_marketing"
)

// MarshalInviteSentEvent builds the opendesk.surveys.events.v1 envelope
// for one sent invite.
func MarshalInviteSentEvent(tenantSlug string, inv Invite, channel string) ([]byte, error) {
	return json.Marshal(events.New("booking-service", EventTypeInviteSent, tenantSlug, inv.TenantID.String(), map[string]any{
		"tenant_id":  inv.TenantID.String(),
		"survey_id":  inv.SurveyID.String(),
		"invite_id":  inv.ID.String(),
		"contact_id": inv.ContactID.String(),
		"channel":    channel,
		"ts":         time.Now().UTC(),
	}))
}

// MarshalResponseReceivedEvent builds the opendesk.surveys.events.v1
// envelope for one accepted response.
func MarshalResponseReceivedEvent(tenantSlug string, res SubmitResult) ([]byte, error) {
	data := map[string]any{
		"tenant_id":    res.Response.TenantID.String(),
		"survey_id":    res.Response.SurveyID.String(),
		"response_id":  res.Response.ID.String(),
		"kind":         res.Survey.Kind,
		"submitted_at": res.Response.SubmittedAt,
	}
	if res.Response.InviteID != nil {
		data["invite_id"] = res.Response.InviteID.String()
	}
	if res.Response.ContactID != nil {
		data["contact_id"] = res.Response.ContactID.String()
	}
	if res.Response.Score != nil {
		data["score"] = *res.Response.Score
	}
	return json.Marshal(events.New("booking-service", EventTypeResponseReceived, tenantSlug, res.Response.TenantID.String(), data))
}

// MarshalResponseUsageRecord builds the usage-record payload for one
// accepted response (mirrors referrals.MarshalReferralVerifiedUsageRecord).
func MarshalResponseUsageRecord(tenantSlug string, res SubmitResult) ([]byte, error) {
	meta := map[string]any{
		"survey_id":   res.Response.SurveyID.String(),
		"response_id": res.Response.ID.String(),
		"kind":        res.Survey.Kind,
	}
	if res.Response.InviteID != nil {
		meta["invite_id"] = res.Response.InviteID.String()
	}
	if res.Response.Score != nil {
		meta["score"] = *res.Response.Score
	}
	evt := events.New("booking-service", "com.opendesk.usage.UsageRecord", tenantSlug, res.Response.TenantID.String(), map[string]any{
		"tenant_id": res.Response.TenantID.String(),
		"metric":    UsageMetricResponseReceived,
		"value":     1,
		"ts":        time.Now().UTC(),
		"meta":      meta,
	})
	return json.Marshal(evt)
}

// inviteLink renders the public respond link embedded in invite messages
// (<base>?t=<token>). The link target is any surface that POSTs
// /v1/surveys/respond with the token (a hosted landing page is a
// documented follow-up).
func (h *Handlers) inviteLink(token string) string {
	base := h.PublicBaseURL
	if base == "" {
		base = DefaultPublicBaseURL
	}
	return strings.TrimRight(base, "/") + "?t=" + token
}

// DefaultPublicBaseURL is the zero-config invite link base (the integrator
// overrides via SURVEYS_PUBLIC_BASE_URL).
const DefaultPublicBaseURL = "https://app.opendesk.ng/s"

// MarshalInvitePacedSend builds the notifications-topic PacedSend envelope
// for one invite. The CloudEvent data mirrors workflows.PacedSendRequest
// EXACTLY for the two marketing kinds surveys use:
//
//	channel sms            → {kind: "geo_campaign", geo_campaign:
//	                         {tenant_slug, campaign_id: <survey id>,
//	                          channel: "sms", phone, name, text}}
//	channel push_marketing → {kind: "push_marketing", push:
//	                         {tenant_slug, contact_id, phone, title,
//	                          body, data}}
//
// Both kinds are MARKETING-class in the worker pacer table, so the
// SPEC-W12 DND/quiet-hours guards apply automatically worker-side.
func (h *Handlers) MarshalInvitePacedSend(tenantSlug string, sv Survey, inv Invite, c ResolvedContact) ([]byte, error) {
	link := h.inviteLink(inv.Token)
	name := strings.TrimSpace(c.Name)
	if name == "" {
		name = "there"
	}
	var data map[string]any
	switch sv.Channel {
	case ChannelSMS:
		text := fmt.Sprintf("Hi %s, we would love your feedback — %s: %s", name, sv.Name, link)
		data = map[string]any{
			"kind": pacedKindGeoCampaign,
			"geo_campaign": map[string]any{
				"tenant_slug": tenantSlug,
				"campaign_id": sv.ID.String(),
				"channel":     "sms",
				"phone":       c.Phone,
				"name":        c.Name,
				"text":        text,
			},
		}
	case ChannelPushMarketing:
		push := map[string]any{
			"tenant_slug": tenantSlug,
			"contact_id":  inv.ContactID.String(),
			"title":       sv.Name,
			"body":        "We would love your feedback — tap to answer a short survey.",
			"data": map[string]string{
				"kind":      "survey_invite",
				"survey_id": sv.ID.String(),
				"invite_id": inv.ID.String(),
				"link":      link,
			},
		}
		if c.Phone != "" {
			push["phone"] = c.Phone // lets the DND guard check phone-keyed registries
		}
		data = map[string]any{
			"kind": pacedKindPushMarketing,
			"push": push,
		}
	default:
		return nil, fmt.Errorf("survey channel %q has no paced route", sv.Channel)
	}
	return json.Marshal(events.New("booking-service", EventTypePacedSend, tenantSlug, sv.TenantID.String(), data))
}

// ---------------------------------------------------------------------------
// best-effort publishers (post-commit; never block the mutation)
// ---------------------------------------------------------------------------

// publishInviteSent emits the InviteSent lifecycle event when the surveys
// events topic is configured (empty = graceful no-op, contract §5).
func (h *Handlers) publishInviteSent(ctx context.Context, tenantSlug string, inv Invite, channel string) {
	if h.EventsTopic == "" {
		return
	}
	payload, err := MarshalInviteSentEvent(tenantSlug, inv, channel)
	if err != nil {
		h.log().Warn("invite sent event marshal failed; skipping emission", zap.Error(err))
		return
	}
	if err := h.Store.EnqueueOutbox(ctx, inv.ID, h.EventsTopic, payload); err != nil {
		h.log().Warn("invite sent event enqueue failed; skipping emission",
			zap.String("invite_id", inv.ID.String()), zap.Error(err))
	}
}

// publishAnswered emits the ResponseReceived lifecycle event + the metered
// survey_response_received usage record (each gated on its own topic).
// Called only on the NON-replay path, so a replayed submit (409) can never
// double-meter.
func (h *Handlers) publishAnswered(ctx context.Context, tenantSlug string, res SubmitResult) {
	if h.EventsTopic != "" {
		payload, err := MarshalResponseReceivedEvent(tenantSlug, res)
		if err != nil {
			h.log().Warn("response received event marshal failed; skipping emission", zap.Error(err))
		} else if err := h.Store.EnqueueOutbox(ctx, res.Response.ID, h.EventsTopic, payload); err != nil {
			h.log().Warn("response received event enqueue failed; skipping emission",
				zap.String("response_id", res.Response.ID.String()), zap.Error(err))
		}
	}
	if h.UsageTopic == "" {
		return
	}
	payload, err := MarshalResponseUsageRecord(tenantSlug, res)
	if err != nil {
		h.log().Warn("survey usage record marshal failed; skipping metering", zap.Error(err))
		return
	}
	if err := h.Store.EnqueueOutbox(ctx, res.Response.ID, h.UsageTopic, payload); err != nil {
		h.log().Warn("survey usage record enqueue failed; skipping metering",
			zap.String("response_id", res.Response.ID.String()), zap.Error(err))
	}
}
