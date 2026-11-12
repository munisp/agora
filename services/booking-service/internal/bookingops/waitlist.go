package bookingops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/availability"
	"github.com/opendesk/booking-service/internal/events"
	"github.com/opendesk/booking-service/internal/store"
)

// ErrClaimExpired marks a waitlist claim token whose entry can never be
// claimed again — the window has passed or the entry left the waiting
// state without a booking to replay. Handlers map it to HTTP 410 (Gone).
var ErrClaimExpired = errors.New("waitlist claim token expired or consumed")

// Waitlist lifecycle event types (SPEC-W48 A2), CloudEvents published to
// the booking events topic via the transactional outbox.
const (
	// EventTypeWaitlistEntryNotified accompanies the waiting → notified
	// flip (staff-triggered manual notify, POST /v1/waitlist/{id}/notify).
	// The data payload mirrors the notification-worker's WaitlistBackfill
	// send shape (PacedWaitlistSend: input + entry with claim_token), so
	// the existing notification bridge can deliver it with the same
	// claim-link template as the cancellation backfill.
	EventTypeWaitlistEntryNotified = "com.opendesk.booking.WaitlistEntryNotified"
	// EventTypeWaitlistEntryExpired is the audit row of every expiry —
	// staff manual expire (actor) or the auto-expiry sweeper
	// (actor "waitlist-sweeper").
	EventTypeWaitlistEntryExpired = "com.opendesk.booking.WaitlistEntryExpired"
)

// waitlistNotifyData builds the WaitlistEntryNotified payload data in the
// WaitlistBackfillWorkflow's notification shape: the backfill input
// (tenant addressing) + the entry (contact phone + claim_token — the
// consumer builds the single-use claim link from it exactly as the
// cancellation backfill does). Channel: the entry carries a phone (SMS/
// WhatsApp per tenant routing), same as the backfill send.
func waitlistNotifyData(tenantSlug string, e store.WaitlistEntry) map[string]any {
	return map[string]any{
		"input": map[string]any{
			"tenant_id":   e.TenantID.String(),
			"tenant_slug": tenantSlug,
			"offering_id": e.OfferingID.String(),
		},
		"entry": map[string]any{
			"id":            e.ID.String(),
			"offering_id":   e.OfferingID.String(),
			"contact_name":  e.ContactName,
			"contact_phone": e.ContactPhone,
			"window_start":  e.WindowStart,
			"window_end":    e.WindowEnd,
			"status":        e.Status,
			"claim_token":   e.ClaimToken.String(),
		},
		// Relative claim link (the public host prefix is the consumer's
		// PublicBaseURL, same as the backfill claimLink builder).
		"claim_path": "/p/" + tenantSlug + "/claim?entry=" + e.ID.String() + "&token=" + e.ClaimToken.String(),
	}
}

// NotifyWaitlistEntry performs the staff-triggered manual notify
// (SPEC-W48 A2): waiting → notified with the WaitlistEntryNotified audit/
// delivery event committed in the SAME transaction (store.MarkWaitlistNotifiedTx).
// 409 (store.ErrConflict) when the entry is no longer waiting.
func (s *Service) NotifyWaitlistEntry(ctx context.Context, tenantID uuid.UUID, tenantSlug string, entryID uuid.UUID) (store.WaitlistEntry, error) {
	entry, err := s.Store.GetWaitlistEntry(ctx, tenantID, entryID)
	if err != nil {
		return store.WaitlistEntry{}, err
	}
	payload, err := json.Marshal(events.New("booking-service", EventTypeWaitlistEntryNotified,
		tenantSlug, tenantID.String(), waitlistNotifyData(tenantSlug, entry)))
	if err != nil {
		return store.WaitlistEntry{}, fmt.Errorf("marshal waitlist-notified event: %w", err)
	}
	return s.Store.MarkWaitlistNotifiedTx(ctx, tenantID, entryID, s.EventsTopic, payload)
}

// ExpireWaitlistEntry performs the staff-triggered manual expire
// (SPEC-W48 A2): waiting|notified → expired with the audit event committed
// in the same transaction (store.ExpireWaitlistTx). 409 (store.ErrConflict)
// when the entry is already claimed/expired.
func (s *Service) ExpireWaitlistEntry(ctx context.Context, tenantID uuid.UUID, tenantSlug string, entryID uuid.UUID, actorID string) (store.WaitlistEntry, error) {
	entry, err := s.Store.GetWaitlistEntry(ctx, tenantID, entryID)
	if err != nil {
		return store.WaitlistEntry{}, err
	}
	payload, err := json.Marshal(events.New("booking-service", EventTypeWaitlistEntryExpired,
		tenantSlug, tenantID.String(), map[string]any{
			"entry_id":    entryID.String(),
			"offering_id": entry.OfferingID.String(),
			"actor":       actorID,
			"via":         "manual",
		}))
	if err != nil {
		return store.WaitlistEntry{}, fmt.Errorf("marshal waitlist-expired event: %w", err)
	}
	return s.Store.ExpireWaitlistTx(ctx, tenantID, entryID, actorID, s.EventsTopic, payload)
}


// ClaimInput describes a waitlist claim attempt (SPEC-W3 §3 innovation 7).
// The claim token is the capability secret delivered to the contact by the
// WaitlistBackfillWorkflow notification; StartsAt is the concrete slot the
// contact picked inside their window.
type ClaimInput struct {
	TenantID     uuid.UUID
	TenantSlug   string
	Timezone     string
	EntryID      uuid.UUID
	Token        uuid.UUID
	TeamMemberID uuid.UUID
	StartsAt     time.Time

	// SPEC-CRM §C3: same industry/policy plumbing as Create.
	Industry      string
	BookingPolicy *BookingPolicy
}

// ClaimWaitlist turns a waitlist entry into a real booking, transactionally
// re-validating token, window and slot availability in the store (see
// store.ClaimWaitlistTx). On success the booking saga starts and the
// availability cache is invalidated exactly like a regular create.
func (s *Service) ClaimWaitlist(ctx context.Context, in ClaimInput) (store.Booking, store.WaitlistEntry, error) {
	if in.EntryID == uuid.Nil || in.Token == uuid.Nil || in.TeamMemberID == uuid.Nil || in.StartsAt.IsZero() {
		return store.Booking{}, store.WaitlistEntry{}, fmt.Errorf("%w: entry, token, team member and starts_at are required", ErrInvalidInput)
	}

	entry, err := s.Store.GetWaitlistEntry(ctx, in.TenantID, in.EntryID)
	if err != nil {
		return store.Booking{}, store.WaitlistEntry{}, err
	}
	offering, err := s.Store.GetOffering(ctx, in.TenantID, entry.OfferingID)
	if err != nil {
		return store.Booking{}, store.WaitlistEntry{}, err
	}
	if _, err := s.Store.GetTeamMember(ctx, in.TenantID, in.TeamMemberID); err != nil {
		return store.Booking{}, store.WaitlistEntry{}, err
	}
	loc, err := loadLocation(in.Timezone)
	if err != nil {
		return store.Booking{}, store.WaitlistEntry{}, err
	}

	// Phone-confirmation policy: waitlist entries are only created with a
	// phone, but enforce it again here like Create does for contacts.
	if entry.ContactPhone == "" {
		return store.Booking{}, store.WaitlistEntry{}, ErrPhoneRequired
	}
	contact := store.Contact{
		TenantID: in.TenantID,
		Name:     entry.ContactName,
		Phone:    entry.ContactPhone,
	}
	if err := s.Store.CreateContact(ctx, &contact); err != nil {
		return store.Booking{}, store.WaitlistEntry{}, err
	}

	booking := store.Booking{
		TenantID:       in.TenantID,
		OfferingID:     entry.OfferingID,
		TeamMemberID:   in.TeamMemberID,
		ContactID:      contact.ID,
		StartsAt:       in.StartsAt,
		EndsAt:         in.StartsAt.Add(time.Duration(offering.DurationMin) * time.Minute),
		Status:         store.StatusPending,
		Source:         "web", // user-driven claim link (bookings.source CHECK)
		IdempotencyKey: "waitlist-claim:" + in.EntryID.String(),
	}
	// SPEC-W51 B1/B3: assign the id + canonical public_ref up front so the
	// BookingCreated payload carries both; ClaimWaitlistTx prefers this ref,
	// regenerates on collision and writes the authoritative value back.
	booking.ID = uuid.New()
	booking.PublicRef = store.PublicRefForBooking(booking.ID)
	payload, err := MarshalBookingEvent("com.opendesk.booking.BookingCreated", in.TenantSlug, booking, offering, contact)
	if err != nil {
		return store.Booking{}, store.WaitlistEntry{}, err
	}

	claimed, err := s.Store.ClaimWaitlistTx(ctx, in.TenantID, in.EntryID, in.Token, offering, &booking, loc, s.EventsTopic, payload)
	if err != nil {
		return store.Booking{}, store.WaitlistEntry{}, err
	}

	s.Cache.Invalidate(ctx, booking.TenantID, booking.OfferingID, booking.TeamMemberID, booking.StartsAt, booking.EndsAt)
	s.startSaga(ctx, booking, offering, contact, CreateInput{
		TenantSlug:    in.TenantSlug,
		Industry:      in.Industry,
		BookingPolicy: in.BookingPolicy,
	})
	return booking, claimed, nil
}

// ---------------------------------------------------------------------------
// Public token-only claim (SPEC-W45 CODER-M, K13 completion)
// ---------------------------------------------------------------------------

// ClaimByTokenInput describes the PUBLIC waitlist claim (POST
// /v1/waitlist/claim): the claimant presents only the unguessable
// claim_token from the backfill notification — the server resolves the
// entry + tenant from it and auto-picks the earliest slot still open
// inside the entry's window.
type ClaimByTokenInput struct {
	TenantID   uuid.UUID
	TenantSlug string
	Timezone   string
	Token      uuid.UUID

	// SPEC-CRM §C3: same industry/policy plumbing as Create.
	Industry      string
	BookingPolicy *BookingPolicy
}

// ClaimByTokenResult is the outcome of ClaimWaitlistByToken. Replayed is
// true when the token was already consumed and the ORIGINAL booking is
// returned (idempotent replay) instead of creating a new one.
type ClaimByTokenResult struct {
	Booking  store.Booking
	Entry    store.WaitlistEntry
	Replayed bool
}

// ClaimWaitlistByToken performs the public token-only claim:
//
//  1. resolves the entry by claim_token (store.ErrNotFound → HTTP 404);
//  2. already-claimed entries replay the ORIGINAL booking (idempotent on
//     token); a claimed entry whose booking cannot be found is a conflict
//     (store.ErrConflict → HTTP 409, replay mismatch);
//  3. an entry that left the waiting state or whose window has passed is
//     gone for good (ErrClaimExpired → HTTP 410);
//  4. otherwise the earliest open slot in the window (across all active
//     team members) is picked and the regular transactional claim runs;
//     no open slot → ErrSlotUnavailable (HTTP 409, slot lost).
func (s *Service) ClaimWaitlistByToken(ctx context.Context, in ClaimByTokenInput) (ClaimByTokenResult, error) {
	if in.Token == uuid.Nil || in.TenantID == uuid.Nil {
		return ClaimByTokenResult{}, fmt.Errorf("%w: token is required", ErrInvalidInput)
	}
	entry, err := s.Store.GetWaitlistEntryByToken(ctx, in.Token)
	if err != nil {
		return ClaimByTokenResult{}, err
	}
	if entry.Status == store.WaitlistClaimed {
		return s.replayClaim(ctx, entry)
	}
	if entry.Status != store.WaitlistWaiting || time.Now().After(entry.WindowEnd) {
		return ClaimByTokenResult{}, ErrClaimExpired
	}
	offering, err := s.Store.GetOffering(ctx, entry.TenantID, entry.OfferingID)
	if err != nil {
		return ClaimByTokenResult{}, err
	}
	loc, err := loadLocation(in.Timezone)
	if err != nil {
		return ClaimByTokenResult{}, err
	}
	memberID, start, ok, err := s.EarliestClaimSlot(ctx, entry.TenantID, loc, offering, entry)
	if err != nil {
		return ClaimByTokenResult{}, err
	}
	if !ok {
		return ClaimByTokenResult{}, fmt.Errorf("%w: no open slot remains inside the waitlist window", ErrSlotUnavailable)
	}
	booking, claimed, err := s.ClaimWaitlist(ctx, ClaimInput{
		TenantID:      entry.TenantID,
		TenantSlug:    in.TenantSlug,
		Timezone:      in.Timezone,
		EntryID:       entry.ID,
		Token:         in.Token,
		TeamMemberID:  memberID,
		StartsAt:      start,
		Industry:      in.Industry,
		BookingPolicy: in.BookingPolicy,
	})
	if err != nil {
		// A concurrent claim with the same token flipped the entry between
		// our read and the transactional claim: answer with the winner's
		// booking so replays stay idempotent.
		if errors.Is(err, store.ErrConflict) {
			if res, rerr := s.replayClaim(ctx, entry); rerr == nil {
				return res, nil
			}
		}
		return ClaimByTokenResult{}, err
	}
	return ClaimByTokenResult{Booking: booking, Entry: claimed}, nil
}

// replayClaim returns the booking originally created by this entry's claim
// (idempotency key "waitlist-claim:<entryID>", shared with the legacy
// /v1/waitlist/{id}/claim route). A claimed entry with no recoverable
// booking is a replay mismatch → store.ErrConflict.
func (s *Service) replayClaim(ctx context.Context, entry store.WaitlistEntry) (ClaimByTokenResult, error) {
	booking, err := s.Store.GetBookingByIdempotencyKey(ctx, entry.TenantID, "waitlist-claim:"+entry.ID.String())
	if errors.Is(err, store.ErrNotFound) {
		return ClaimByTokenResult{}, fmt.Errorf("%w: entry already claimed and the original booking is unrecoverable", store.ErrConflict)
	}
	if err != nil {
		return ClaimByTokenResult{}, err
	}
	entry.Status = store.WaitlistClaimed
	return ClaimByTokenResult{Booking: booking, Entry: entry, Replayed: true}, nil
}

// EarliestClaimSlot finds the earliest (team member, start) pair with an
// open slot inside the entry's window using the same availability engine
// as the booking read/write paths (weekly rules + overlapping bookings +
// buffers, expanded in the tenant timezone). ok=false means no active team
// member has any open slot left in the window (slot gone). Exported so the
// public claim-info preview can compute its `claimable` flag without
// duplicating the engine wiring.
func (s *Service) EarliestClaimSlot(ctx context.Context, tenantID uuid.UUID, loc *time.Location, offering store.Offering, entry store.WaitlistEntry) (memberID uuid.UUID, start time.Time, ok bool, err error) {
	from := entry.WindowStart
	if now := time.Now(); now.After(from) {
		from = now
	}
	if !entry.WindowEnd.After(from) {
		return uuid.Nil, time.Time{}, false, nil
	}
	members, err := s.Store.ListTeamMembers(ctx, tenantID)
	if err != nil {
		return uuid.Nil, time.Time{}, false, err
	}
	duration := time.Duration(offering.DurationMin) * time.Minute
	buffer := time.Duration(offering.BufferMin) * time.Minute
	pad := buffer + duration
	for _, m := range members {
		if !m.Active {
			continue
		}
		rules, err := s.Store.ListAvailabilityRules(ctx, tenantID, m.ID)
		if err != nil {
			return uuid.Nil, time.Time{}, false, err
		}
		engineRules := make([]availability.Rule, 0, len(rules))
		for _, rl := range rules {
			engineRules = append(engineRules, availability.Rule{
				Weekday:       time.Weekday(rl.Weekday),
				StartMin:      rl.StartMin,
				EndMin:        rl.EndMin,
				EffectiveFrom: rl.EffectiveFrom,
				EffectiveTo:   rl.EffectiveTo,
			})
		}
		bookings, err := s.Store.ListBookingsForRange(ctx, tenantID, m.ID, from.Add(-pad), entry.WindowEnd.Add(pad))
		if err != nil {
			return uuid.Nil, time.Time{}, false, err
		}
		engineBookings := make([]availability.Booking, 0, len(bookings))
		for _, b := range bookings {
			engineBookings = append(engineBookings, availability.Booking{StartsAt: b.StartsAt, EndsAt: b.EndsAt})
		}
		slots := availability.Slots(availability.Params{
			From:     from,
			To:       entry.WindowEnd,
			Duration: duration,
			Buffer:   buffer,
			Capacity: offering.Capacity,
			Rules:    engineRules,
			Bookings: engineBookings,
			Location: loc,
		})
		if len(slots) > 0 && (!ok || slots[0].StartsAt.Before(start)) {
			ok, start, memberID = true, slots[0].StartsAt, m.ID
		}
	}
	return memberID, start, ok, nil
}
