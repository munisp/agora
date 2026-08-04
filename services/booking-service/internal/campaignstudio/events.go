package campaignstudio

// SPEC-W19 shared contract §5: CloudEvents on the Campaign Studio
// lifecycle, topic opendesk.studio.events.v1 (empty topic disables
// publishing — graceful no-op), written to the same transactional outbox
// the rest of booking-service drains:
//
//   - com.opendesk.studio.JourneyEnrolled  — one per NEW enrollment
//   - com.opendesk.studio.JourneyCompleted — one per enrollment that
//     advanced past the last step
//
// Both are emitted POST-COMMIT best-effort (mirroring the referrals
// metering posture): a lost event is reconcilable from
// studio_enrollments/studio_step_events, a blocked enrollment is not
// acceptable.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// CloudEvent types on the studio events topic (SPEC-W19 contract §5:
// "journey enrolled/completed → opendesk.studio.events.v1").
const (
	EventTypeJourneyEnrolled  = "com.opendesk.studio.JourneyEnrolled"
	EventTypeJourneyCompleted = "com.opendesk.studio.JourneyCompleted"
)

// DefaultEventsTopic is the studio lifecycle topic (SPEC-W19 §5). NOTE
// for the integrator: the topic must be declared in
// infra/kafka/create-topics.sh (broker auto-create is OFF) — flagged in
// docs/apps/campaign-studio.md.
const DefaultEventsTopic = "opendesk.studio.events.v1"

// marshalJourneyEvent builds one lifecycle CloudEvent.
func marshalJourneyEvent(eventType, tenantSlug string, tenantID, journeyID, contactID, enrollmentID uuid.UUID) ([]byte, error) {
	evt := events.New("booking-service", eventType, tenantSlug, tenantID.String(), map[string]any{
		"tenant_id":     tenantID.String(),
		"journey_id":    journeyID.String(),
		"contact_id":    contactID.String(),
		"enrollment_id": enrollmentID.String(),
		"ts":            time.Now().UTC(),
	})
	return json.Marshal(evt)
}

// publishJourneyEvents enqueues one CloudEvent per enrollment with the
// given type (best-effort, post-commit).
func (h *Handlers) publishJourneyEvents(ctx context.Context, eventType, tenantSlug string, tenantID uuid.UUID, enrollments []Enrollment) {
	if h.EventsTopic == "" || len(enrollments) == 0 {
		return
	}
	for _, e := range enrollments {
		payload, err := marshalJourneyEvent(eventType, tenantSlug, tenantID, e.JourneyID, e.ContactID, e.ID)
		if err != nil {
			h.log().Warn("studio event marshal failed; skipping", zap.String("type", eventType), zap.Error(err))
			continue
		}
		if err := h.Store.EnqueueOutbox(ctx, e.ID, h.EventsTopic, payload); err != nil {
			h.log().Warn("studio event enqueue failed; skipping", zap.String("type", eventType), zap.Error(err))
		}
	}
}
