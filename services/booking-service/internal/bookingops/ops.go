// Package bookingops holds the booking write-path business logic shared by
// the REST handlers, the public booking endpoints and the Kafka command
// consumer: validation (incl. the phone-confirmation policy), availability
// checks, transactional insert + outbox, and saga kickoff.
package bookingops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/availability"
	"github.com/opendesk/booking-service/internal/cache"
	"github.com/opendesk/booking-service/internal/events"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// Sentinel errors mapped to HTTP statuses / DLQ decisions by callers.
var (
	ErrPhoneRequired   = errors.New("contact phone is required (phone-confirmation policy)")
	ErrSlotUnavailable = errors.New("requested slot is not available")
	ErrInvalidInput    = errors.New("invalid input")
)

// SagaStarter abstracts the Temporal client so bookingops stays testable.
type SagaStarter interface {
	StartBookingSaga(ctx context.Context, in SagaInput) (string, error)
}

// SagaInput is the input contract of BookingSagaWorkflow (SPEC §6), mirrored
// by notification-worker.
type SagaInput struct {
	BookingID    string    `json:"booking_id"`
	TenantID     string    `json:"tenant_id"`
	TenantSlug   string    `json:"tenant_slug"`
	OfferingID   string    `json:"offering_id"`
	TeamMemberID string    `json:"team_member_id"`
	ContactID    string    `json:"contact_id"`
	ContactPhone string    `json:"contact_phone"`
	ContactEmail string    `json:"contact_email"`
	ContactName  string    `json:"contact_name"`
	StartsAt     time.Time `json:"starts_at"`
	EndsAt       time.Time `json:"ends_at"`
	PriceCents   int64     `json:"price_cents"`
	Currency     string    `json:"currency"`
	Source       string    `json:"source"`

	// SPEC-CRM §C2/C3: tenant's industry pack + resolved deposit policy.
	Industry string `json:"industry,omitempty"`
	// DepositCents is ceil(price_cents * depositPercent/100); only consulted
	// when DepositKnown is true (otherwise the saga holds the full price).
	DepositCents int64 `json:"deposit_cents,omitempty"`
	DepositKnown bool  `json:"deposit_known,omitempty"`
	// Pack policy values the saga's pack child workflow needs.
	NoShowFeeCents          int64 `json:"no_show_fee_cents,omitempty"`
	CancellationWindowHours int   `json:"cancellation_window_hours,omitempty"`
}

// Service bundles dependencies of the booking write path.
type Service struct {
	Store       *store.Store
	Saga        SagaStarter // may be nil in tests / when Temporal is down
	EventsTopic string
	// UsageTopic is the Kafka topic for usage-metering records
	// (opendesk.usage.events, Wave 5 #9). Empty disables metering.
	UsageTopic string
	Logger     *zap.Logger
	// Cache invalidates availability day-bucket keys on every successful
	// write (create/reschedule/cancel). Nil disables invalidation; both the
	// REST handlers and the Kafka command consumer share this Service, so
	// one hook covers all write paths (SPEC-W3 §3).
	Cache *cache.Cache
}

// ContactInput carries inline contact data for a new booking.
type ContactInput struct {
	Name  string `json:"name"`
	Phone string `json:"phone"`
	Email string `json:"email"`
}

// CreateInput describes a new booking.
type CreateInput struct {
	TenantID       uuid.UUID
	TenantSlug     string
	Timezone       string // IANA name from identity-service tenant context
	OfferingID     uuid.UUID
	TeamMemberID   uuid.UUID
	ContactID      *uuid.UUID    // existing contact, or
	Contact        *ContactInput // inline contact to create
	StartsAt       time.Time
	Source         string // web|voice|api
	IdempotencyKey string

	// SPEC-CRM §C3: tenant industry + pack booking policy, captured from the
	// identity tenant context by the HTTP handlers.
	Industry      string
	BookingPolicy *BookingPolicy // nil when the tenant has no resolved pack
}

// Create validates and inserts a booking transactionally with its outbox
// event, then starts the BookingSagaWorkflow. Retries with the same
// idempotency key return the original booking.
func (s *Service) Create(ctx context.Context, in CreateInput) (store.Booking, error) {
	if in.TenantID == uuid.Nil || in.OfferingID == uuid.Nil || in.TeamMemberID == uuid.Nil {
		return store.Booking{}, fmt.Errorf("%w: tenant, offering and team member are required", ErrInvalidInput)
	}
	if in.StartsAt.IsZero() {
		return store.Booking{}, fmt.Errorf("%w: starts_at is required", ErrInvalidInput)
	}
	if in.Source == "" {
		in.Source = "api"
	}

	offering, err := s.Store.GetOffering(ctx, in.TenantID, in.OfferingID)
	if err != nil {
		return store.Booking{}, err
	}
	if !offering.Bookable {
		return store.Booking{}, fmt.Errorf("%w: offering is not bookable", ErrInvalidInput)
	}
	if _, err := s.Store.GetTeamMember(ctx, in.TenantID, in.TeamMemberID); err != nil {
		return store.Booking{}, err
	}

	// Resolve or create the contact. Phone-confirmation policy (SPEC §1/§11):
	// mutations are rejected without a verified contact phone.
	var contact store.Contact
	switch {
	case in.ContactID != nil:
		contact, err = s.Store.GetContact(ctx, in.TenantID, *in.ContactID)
		if err != nil {
			return store.Booking{}, err
		}
		if contact.Phone == "" {
			return store.Booking{}, ErrPhoneRequired
		}
	case in.Contact != nil:
		if in.Contact.Phone == "" {
			return store.Booking{}, ErrPhoneRequired
		}
		contact = store.Contact{
			TenantID: in.TenantID,
			Name:     in.Contact.Name,
			Phone:    in.Contact.Phone,
			Email:    in.Contact.Email,
		}
		if err := s.Store.CreateContact(ctx, &contact); err != nil {
			return store.Booking{}, err
		}
	default:
		return store.Booking{}, fmt.Errorf("%w: contact or contact_id is required", ErrInvalidInput)
	}

	// Idempotent replay: return the existing booking for this key.
	if in.IdempotencyKey != "" {
		if existing, err := s.Store.GetBookingByIdempotencyKey(ctx, in.TenantID, in.IdempotencyKey); err == nil {
			return existing, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return store.Booking{}, err
		}
	}

	endsAt := in.StartsAt.Add(time.Duration(offering.DurationMin) * time.Minute)
	if err := s.checkSlot(ctx, in.TenantID, in.Timezone, offering, in.TeamMemberID, in.StartsAt, endsAt, nil); err != nil {
		return store.Booking{}, err
	}

	booking := store.Booking{
		ID:             uuid.New(), // assigned up front so events carry the id
		TenantID:       in.TenantID,
		OfferingID:     in.OfferingID,
		TeamMemberID:   in.TeamMemberID,
		ContactID:      contact.ID,
		StartsAt:       in.StartsAt,
		EndsAt:         endsAt,
		Status:         store.StatusPending, // confirmed by the booking saga
		Source:         in.Source,
		IdempotencyKey: in.IdempotencyKey,
	}
	payload, err := MarshalBookingEvent("com.opendesk.booking.BookingCreated", in.TenantSlug, booking, offering, contact)
	if err != nil {
		return store.Booking{}, err
	}
	guard := store.SlotGuard{
		Buffer:   time.Duration(offering.BufferMin) * time.Minute,
		Capacity: offering.Capacity,
	}
	if err := s.Store.CreateBookingTx(ctx, &booking, guard, s.EventsTopic, payload,
		s.UsageExtra(in.TenantSlug, booking.TenantID, booking.ID, offering)...); err != nil {
		if errors.Is(err, store.ErrSlotConflict) {
			// In-transaction re-check fired (SPEC-W43 K-01): a concurrent
			// writer won the slot after the fast pre-check passed.
			return store.Booking{}, ErrSlotUnavailable
		}
		if errors.Is(err, store.ErrConflict) && in.IdempotencyKey != "" {
			// Lost the unique race — the other writer won; return its row.
			return s.Store.GetBookingByIdempotencyKey(ctx, in.TenantID, in.IdempotencyKey)
		}
		return store.Booking{}, err
	}

	// Invalidate cached availability for the newly occupied day-range.
	s.Cache.Invalidate(ctx, booking.TenantID, booking.OfferingID, booking.TeamMemberID, booking.StartsAt, booking.EndsAt)

	s.startSaga(ctx, booking, offering, contact, in)
	return booking, nil
}

// depositFor computes the saga deposit from the pack booking policy
// (SPEC-CRM §C3): ceil(price_cents * depositPercent/100) when the policy is
// known, so a 0% policy skips the hold entirely. When no pack policy was
// resolved, DepositKnown stays false and the saga holds the full price
// (matching the pre-CRM HoldDeposit behavior).
func depositFor(priceCents int64, policy *BookingPolicy) (depositCents int64, known bool) {
	if policy == nil {
		return 0, false
	}
	pct := policy.DepositPercent
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return (priceCents*int64(pct) + 99) / 100, true
}

// Reschedule moves a booking, re-validating availability against all other
// bookings, and emits BookingRescheduled.
func (s *Service) Reschedule(ctx context.Context, tenantID uuid.UUID, tenantSlug, timezone string, bookingID uuid.UUID, startsAt time.Time) (store.Booking, error) {
	if startsAt.IsZero() {
		return store.Booking{}, fmt.Errorf("%w: starts_at is required", ErrInvalidInput)
	}
	booking, err := s.Store.GetBooking(ctx, tenantID, bookingID)
	if err != nil {
		return store.Booking{}, err
	}
	if booking.Status == store.StatusCancelled {
		return store.Booking{}, fmt.Errorf("%w: cannot reschedule a cancelled booking", ErrInvalidInput)
	}
	offering, err := s.Store.GetOffering(ctx, tenantID, booking.OfferingID)
	if err != nil {
		return store.Booking{}, err
	}
	endsAt := startsAt.Add(time.Duration(offering.DurationMin) * time.Minute)
	if err := s.checkSlot(ctx, tenantID, timezone, offering, booking.TeamMemberID, startsAt, endsAt, &bookingID); err != nil {
		return store.Booking{}, err
	}
	contact, _ := s.Store.GetContact(ctx, tenantID, booking.ContactID)
	// Remember the OLD slot for cache invalidation: rescheduling frees it.
	oldStart, oldEnd := booking.StartsAt, booking.EndsAt
	// Point the booking at the NEW slot before marshalling so the
	// BookingRescheduled event (and its WS fan-out) carries the new times.
	booking.StartsAt, booking.EndsAt = startsAt, endsAt
	payload, err := MarshalBookingEvent("com.opendesk.booking.BookingRescheduled", tenantSlug, booking, offering, contact)
	if err != nil {
		return store.Booking{}, err
	}
	guard := store.SlotGuard{
		Buffer:   time.Duration(offering.BufferMin) * time.Minute,
		Capacity: offering.Capacity,
	}
	if err := s.Store.RescheduleBooking(ctx, tenantID, bookingID, booking.TeamMemberID, startsAt, endsAt, guard, s.EventsTopic, payload); err != nil {
		if errors.Is(err, store.ErrSlotConflict) {
			// In-transaction re-check fired (SPEC-W43 K-01).
			return store.Booking{}, ErrSlotUnavailable
		}
		return store.Booking{}, err
	}
	s.Cache.Invalidate(ctx, tenantID, booking.OfferingID, booking.TeamMemberID, oldStart, oldEnd)
	s.Cache.Invalidate(ctx, tenantID, booking.OfferingID, booking.TeamMemberID, startsAt, endsAt)
	return booking, nil
}

// Cancel marks a booking cancelled and emits BookingCancelled.
func (s *Service) Cancel(ctx context.Context, tenantID uuid.UUID, tenantSlug string, bookingID uuid.UUID, reason string) (store.Booking, error) {
	booking, err := s.Store.GetBooking(ctx, tenantID, bookingID)
	if err != nil {
		return store.Booking{}, err
	}
	if booking.Status == store.StatusCancelled {
		return booking, nil // idempotent
	}
	offering, _ := s.Store.GetOffering(ctx, tenantID, booking.OfferingID)
	contact, _ := s.Store.GetContact(ctx, tenantID, booking.ContactID)
	payload, err := marshalEvent("com.opendesk.booking.BookingCancelled", tenantSlug, booking, map[string]any{
		"offering_id": booking.OfferingID.String(),
		"reason":      reason,
		"price_cents": offering.PriceCents,
		"currency":    offering.Currency,
		"phone":       contact.Phone,
		"email":       contact.Email,
	})
	if err != nil {
		return store.Booking{}, err
	}
	if err := s.Store.SetBookingStatus(ctx, tenantID, bookingID, store.StatusCancelled, s.EventsTopic, payload); err != nil {
		return store.Booking{}, err
	}
	s.Cache.Invalidate(ctx, tenantID, booking.OfferingID, booking.TeamMemberID, booking.StartsAt, booking.EndsAt)
	booking.Status = store.StatusCancelled
	return booking, nil
}

// checkSlot validates the candidate against weekly rules + existing bookings
// using the pure availability engine.
func (s *Service) checkSlot(ctx context.Context, tenantID uuid.UUID, timezone string, offering store.Offering, teamMemberID uuid.UUID, start, end time.Time, exclude *uuid.UUID) error {
	loc, err := loadLocation(timezone)
	if err != nil {
		return err
	}
	// widen lookup range so buffer interactions at the edges are captured
	from := start.Add(-time.Duration(offering.BufferMin+offering.DurationMin) * time.Minute)
	to := end.Add(time.Duration(offering.BufferMin+offering.DurationMin) * time.Minute)

	rules, err := s.Store.ListAvailabilityRules(ctx, tenantID, teamMemberID)
	if err != nil {
		return err
	}
	engineRules := make([]availability.Rule, 0, len(rules))
	for _, r := range rules {
		engineRules = append(engineRules, availability.Rule{
			Weekday:       time.Weekday(r.Weekday),
			StartMin:      r.StartMin,
			EndMin:        r.EndMin,
			EffectiveFrom: r.EffectiveFrom,
			EffectiveTo:   r.EffectiveTo,
		})
	}
	if !availability.Covers(engineRules, loc, start, end) {
		return ErrSlotUnavailable
	}

	bookings, err := s.Store.ListBookingsForRange(ctx, tenantID, teamMemberID, from, to)
	if err != nil {
		return err
	}
	engineBookings := make([]availability.Booking, 0, len(bookings))
	for _, b := range bookings {
		if exclude != nil && b.ID == *exclude {
			continue
		}
		engineBookings = append(engineBookings, availability.Booking{StartsAt: b.StartsAt, EndsAt: b.EndsAt})
	}
	if !availability.Fits(start, end, time.Duration(offering.BufferMin)*time.Minute, offering.Capacity, engineBookings) {
		return ErrSlotUnavailable
	}
	return nil
}

// startSaga kicks off BookingSagaWorkflow. Failure is logged, not fatal:
// the booking stays `pending` and can be reconciled (outbox + retry).
func (s *Service) startSaga(ctx context.Context, b store.Booking, o store.Offering, c store.Contact, in CreateInput) {
	if s.Saga == nil {
		return
	}
	depositCents, depositKnown := depositFor(o.PriceCents, in.BookingPolicy)
	sagaIn := SagaInput{
		BookingID:    b.ID.String(),
		TenantID:     b.TenantID.String(),
		TenantSlug:   in.TenantSlug,
		OfferingID:   b.OfferingID.String(),
		TeamMemberID: b.TeamMemberID.String(),
		ContactID:    c.ID.String(),
		ContactPhone: c.Phone,
		ContactEmail: c.Email,
		ContactName:  c.Name,
		StartsAt:     b.StartsAt,
		EndsAt:       b.EndsAt,
		PriceCents:   o.PriceCents,
		Currency:     o.Currency,
		Source:       b.Source,
		Industry:     in.Industry,
		DepositCents: depositCents,
		DepositKnown: depositKnown,
	}
	if in.BookingPolicy != nil {
		sagaIn.NoShowFeeCents = in.BookingPolicy.NoShowFeeCents
		sagaIn.CancellationWindowHours = in.BookingPolicy.CancellationWindowHours
	}
	runID, err := s.Saga.StartBookingSaga(ctx, sagaIn)
	if err != nil {
		s.Logger.Error("failed to start BookingSagaWorkflow; booking left pending",
			zap.String("booking_id", b.ID.String()), zap.Error(err))
		return
	}
	s.Logger.Info("BookingSagaWorkflow started", zap.String("booking_id", b.ID.String()), zap.String("run_id", runID))
}

// MarshalBookingEvent builds the CloudEvents payload for outbox rows.
func MarshalBookingEvent(eventType, tenantSlug string, b store.Booking, o store.Offering, c store.Contact) ([]byte, error) {
	return marshalEvent(eventType, tenantSlug, b, map[string]any{
		"offering_id":    b.OfferingID.String(),
		"offering_name":  o.Name,
		"team_member_id": b.TeamMemberID.String(),
		"contact_id":     b.ContactID.String(),
		"contact_name":   c.Name,
		"phone":          c.Phone,
		"email":          c.Email,
		"price_cents":    o.PriceCents,
		"currency":       o.Currency,
	})
}

func marshalEvent(eventType, tenantSlug string, b store.Booking, data map[string]any) ([]byte, error) {
	data["booking_id"] = b.ID.String()
	data["starts_at"] = b.StartsAt
	data["ends_at"] = b.EndsAt
	data["status"] = b.Status
	data["source"] = b.Source
	evt := events.New("booking-service", eventType, tenantSlug, b.TenantID.String(), data)
	return json.Marshal(evt)
}

func loadLocation(tz string) (*time.Location, error) {
	if tz == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown timezone %q", ErrInvalidInput, tz)
	}
	return loc, nil
}
