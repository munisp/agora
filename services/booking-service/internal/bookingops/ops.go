package bookingops

// Booking operations facade: create (availability check + conflict
// detection + outbox event in one transaction) and the saga kickoff.
//
// SPEC-W44 W-A/F15-04 (U7) — CREATE PATH REDESIGN (SPEC §3/§7): the
// transaction above is the ONLY transaction Create now opens. The guard
// validation (rules, business-open, blackout) that used to run as a
// read-only PRE-TX (Store.ValidateGuardTx) is folded INTO the write
// transaction via store.GuardErrorFromValidation — same round-trip count
// as before (1 validation + 1 write), but the guard and the booking INSERT
// now run against ONE consistent snapshot, so a rule change between the
// two transactions can no longer admit a booking that violates the
// committed rules. A race between THIS create and a rule change (new
// blackout/rule) is now a SERIALIZABLE conflict on availability_rules /
// store_blackouts instead of a silent admission. metrics.go is unchanged
// (it already emitted only the post-commit counters).
//
// SPEC-W44 W-A/F15-05: Store.CreateBookingTx already ran guard
// validation serializably (ValidateGuardTx inside the write tx); the
// CreateBookingTxWithMetrics wrapper folded a SECOND ValidateGuardTx call
// into the same path as a belt-and-suspenders. The wrapper is REMOVED —
// metrics and store now call CreateBookingTx once. Guard acceptance is
// counted by ObserveBookingCreated (metrics.go, store_emit path) after
// the commit, and rejected candidates by the ErrGuardRejected branch in
// Create below — no observation is lost.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/cache"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// ErrSlotUnavailable is returned when the requested window conflicts with
// an existing booking (with buffer applied) or has no capacity left.
var ErrSlotUnavailable = errors.New("requested slot is not available")

// ErrPhoneRequired is returned when a booking is attempted without a
// contact phone number (needed for confirmation/reminders).
var ErrPhoneRequired = errors.New("contact phone is required for booking")

// ErrGuardRejected marks a booking rejected by a tenant guard rule
// (advance window, notice, duration, blackout, business hours). Handlers
// map it to 422. It wraps *store.GuardError so the details survive.
type ErrGuardRejected struct{ inner error }

func (e *ErrGuardRejected) Error() string { return e.inner.Error() }
func (e *ErrGuardRejected) Unwrap() error { return e.inner }

// GuardRejectionDetails unwraps a *store.GuardError from any error chain.
func GuardRejectionDetails(err error) *store.GuardError {
	var gr *ErrGuardRejected
	if errors.As(err, &gr) {
		var ge *store.GuardError
		if errors.As(gr.inner, &ge) {
			return ge
		}
	}
	return nil
}

// ErrInvalidInput is a 400-class validation failure.
var ErrInvalidInput = errors.New("invalid booking input")

// BookingPolicy is the per-tenant booking constraint set evaluated at
// Create time (SPEC-CRM §C3). Nil means defaults.
type BookingPolicy struct {
	RequirePhone     bool
	RequireDeposit   bool
	NoShowWindowMin  int // 0 = default 15
	BufferOverride   *int
	MaxActivePerPhone int // 0 = unlimited
}

// CreateInput describes one booking attempt.
type CreateInput struct {
	TenantID      uuid.UUID
	TenantSlug    string
	OfferingID    uuid.UUID
	TeamMemberID  uuid.UUID
	ContactName   string
	ContactPhone  string
	ContactEmail  string
	StartsAt      time.Time
	Source        string // voice|chat|web|api
	IdempotencyKey string
	Timezone      string // IANA, from the tenant record

	// SPEC-CRM §C3: industry id (for no-show window + terminology logging)
	// and the tenant's resolved booking policy. Nil → defaults.
	Industry      string
	BookingPolicy *BookingPolicy
}

// Service wires the store, cache and event sink.
type Service struct {
	Store       *store.Store
	Saga        SagaStarter
	EventsTopic string
	UsageTopic  string
	Logger      *zap.Logger
	Cache       *cache.Cache
	// SPEC-W45 K11: the loyalty accrual hook — the bookingops half of the
	// loyalty contract (see complete.go BookingCompletedAccruer for the
	// CONTRACT NOTE / CODER-H handoff). Nil = accrual skipped.
	Loyalty BookingCompletedAccruer
	// SPEC-W45 K11/K12: the payments rail client (deposit capture +
	// refunds). Nil = rail not wired → money-moving endpoints fail closed
	// with ErrPaymentsNotConfigured (503 at httpapi).
	Payments *PaymentsClient
}

// SagaStarter abstracts the Temporal client (test seam).
type SagaStarter interface {
	StartBookingSaga(ctx context.Context, in StartSagaInput) (workflowID string, err error)
}

// MarshalBookingEvent builds the CloudEvents JSON payload for a booking
// lifecycle event (booking.service marshals — SPEC-W3 §4/§12). The event
// `type` selects the event name; the data payload is the booking snapshot.
func MarshalBookingEvent(eventType, tenantSlug string, booking store.Booking, offering store.Offering, contact store.Contact) ([]byte, error) {
	return store.MarshalBookingEvent(store.BookingEventPayload{
		SpecVersion: "1.0",
		Type:        eventType,
		Source:      "//opendesk/booking-service",
		Subject:     "booking/" + booking.ID.String(),
		TenantSlug:  tenantSlug,
		TenantID:    booking.TenantID.String(),
		Data: store.BookingEventData{
			BookingID:    booking.ID.String(),
			OfferingID:   booking.OfferingID.String(),
			OfferingName: offering.Name,
			TeamMemberID: uuidToString(booking.TeamMemberID),
			ContactID:    uuidToString(booking.ContactID),
			ContactName:  contact.Name,
			ContactPhone: contact.Phone,
			StartsAt:     booking.StartsAt.UTC().Format(time.RFC3339),
			EndsAt:       booking.EndsAt.UTC().Format(time.RFC3339),
			Status:       booking.Status,
			Source:       booking.Source,
		},
	})
}

func uuidToString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func loadLocation(tz string) (*time.Location, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("%w: unknown timezone %q", ErrInvalidInput, tz)
	}
	return loc, nil
}

// policyFromInput resolves the effective booking policy for a create.
func policyFromInput(in CreateInput) BookingPolicy {
	if in.BookingPolicy != nil {
		return *in.BookingPolicy
	}
	return BookingPolicy{RequirePhone: true}
}

// Create validates input, evaluates the booking policy, inserts the booking
// transactionally (conflict re-check happens in the store), writes the
// outbox event and kicks the booking saga.
func (s *Service) Create(ctx context.Context, in CreateInput) (store.Booking, error) {
	policy := policyFromInput(in)
	if policy.RequirePhone && strings.TrimSpace(in.ContactPhone) == "" {
		return store.Booking{}, ErrPhoneRequired
	}
	if in.OfferingID == uuid.Nil || in.TeamMemberID == uuid.Nil {
		return store.Booking{}, fmt.Errorf("%w: offering and team member are required", ErrInvalidInput)
	}
	if in.StartsAt.IsZero() || in.StartsAt.Before(time.Now().Add(-time.Minute)) {
		return store.Booking{}, fmt.Errorf("%w: starts_at must be in the future", ErrInvalidInput)
	}
	source := strings.ToLower(strings.TrimSpace(in.Source))
	switch source {
	case "voice", "chat", "web", "api":
	case "":
		source = "api"
	default:
		return store.Booking{}, fmt.Errorf("%w: unknown source %q", ErrInvalidInput, in.Source)
	}
	loc, err := loadLocation(in.Timezone)
	if err != nil {
		return store.Booking{}, err
	}

	offering, err := s.Store.GetOffering(ctx, in.TenantID, in.OfferingID)
	if err != nil {
		return store.Booking{}, err
	}
	member, err := s.Store.GetTeamMember(ctx, in.TenantID, in.TeamMemberID)
	if err != nil {
		return store.Booking{}, err
	}
	_ = member // name is carried into the saga input

	contact := store.Contact{
		TenantID: in.TenantID,
		Name:     strings.TrimSpace(in.ContactName),
		Phone:    strings.TrimSpace(in.ContactPhone),
		Email:    strings.TrimSpace(in.ContactEmail),
	}
	if err := s.Store.CreateContact(ctx, &contact); err != nil {
		return store.Booking{}, err
	}

	// SPEC-W44 W-B/F16-07: per-phone active-booking cap — the invariant
	// (derived from BookingPolicy.MaxActivePerPhone, N=3 rollout default set
	// by the caller — read from the tenant's booking policy resolution) is
	// enforced INSIDE the create transaction (advisory lock on the phone
	// hash makes concurrent creates serialize; capped callers get 429).
	maxActive := policy.MaxActivePerPhone
	if maxActive < 0 {
		maxActive = 0
	}

	booking := store.Booking{
		TenantID:       in.TenantID,
		OfferingID:     in.OfferingID,
		TeamMemberID:   in.TeamMemberID,
		ContactID:      contact.ID,
		StartsAt:       in.StartsAt,
		EndsAt:         in.StartsAt.Add(time.Duration(offering.DurationMin) * time.Minute),
		Status:         store.StatusPending,
		Source:         source,
		IdempotencyKey: strings.TrimSpace(in.IdempotencyKey),
	}

	payload, err := MarshalBookingEvent("com.opendesk.booking.BookingCreated", in.TenantSlug, booking, offering, contact)
	if err != nil {
		return store.Booking{}, err
	}

	// SPEC-W44 W-A/F15-04+F15-05: ONE write transaction (CreateBookingTx),
	// no pre-validation transaction, no metrics wrapper — the guard is
	// validated inside the SAME serializable snapshot as the INSERT
	// (store.GuardErrorFromValidation converts a rejection to *GuardError).
	created, err := s.Store.CreateBookingTx(ctx, booking, loc, s.EventsTopic, payload,
		store.WithMaxActivePerPhone(maxActive))
	if err != nil {
		var ge *store.GuardError
		if errors.As(err, &ge) {
			s.observeGuardRejected(in.TenantSlug, ge)
			return store.Booking{}, &ErrGuardRejected{inner: err}
		}
		if errors.Is(err, store.ErrConflict) {
			return store.Booking{}, ErrSlotUnavailable
		}
		return store.Booking{}, err
	}

	s.Cache.Invalidate(ctx, booking.TenantID, booking.OfferingID, booking.TeamMemberID, booking.StartsAt, booking.EndsAt)
	s.startSaga(ctx, created, offering, contact, in)
	return created, nil
}

// startSaga kicks the booking saga workflow; failures leave the booking
// pending and are logged for reconciliation.
func (s *Service) startSaga(ctx context.Context, booking store.Booking, offering store.Offering, contact store.Contact, in CreateInput) {
	if s.Saga == nil {
		s.Logger.Warn("temporal not configured; booking saga not started", zap.String("booking_id", booking.ID.String()))
		return
	}
	_, err := s.Saga.StartBookingSaga(ctx, StartSagaInput{
		TenantID:      booking.TenantID,
		TenantSlug:    in.TenantSlug,
		BookingID:     booking.ID,
		ContactName:   contact.Name,
		ContactPhone:  contact.Phone,
		OfferingName:  offering.Name,
		StartsAt:      booking.StartsAt,
		DepositCents:  depositForPolicy(offering, in.BookingPolicy),
		NoShowWaitMin: noShowWindow(in.Industry, in.BookingPolicy),
		Industry:      in.Industry,
	})
	if err != nil {
		s.Logger.Error("failed to start booking saga; booking left pending",
			zap.String("booking_id", booking.ID.String()), zap.Error(err))
	}
}

// depositForPolicy maps the booking policy to a deposit amount (cents).
func depositForPolicy(offering store.Offering, p *BookingPolicy) int {
	if p != nil && p.RequireDeposit && offering.PriceCents > 0 {
		return offering.PriceCents / 2 // 50% deposit
	}
	return 0
}

// noShowWindow resolves the no-show auto-mark window (minutes) from
// industry defaults + policy override.
func noShowWindow(industry string, p *BookingPolicy) int {
	if p != nil && p.NoShowWindowMin > 0 {
		return p.NoShowWindowMin
	}
	switch industry {
	case "clinic":
		return 30
	case "consultancy":
		return 20
	default: // salon, support-desk, unspecified
		return 15
	}
}

// Cancel transitions a booking to cancelled and emits BookingCancelled.
func (s *Service) Cancel(ctx context.Context, tenantID uuid.UUID, tenantSlug string, bookingID uuid.UUID, reason string) (store.Booking, error) {
	booking, err := s.Store.GetBooking(ctx, tenantID, bookingID)
	if err != nil {
		return store.Booking{}, err
	}
	if booking.Status == store.StatusCancelled || booking.Status == store.StatusCompleted {
		return booking, nil // idempotent
	}
	offering, _ := s.Store.GetOffering(ctx, tenantID, booking.OfferingID)
	contact, _ := s.Store.GetContact(ctx, tenantID, booking.ContactID)
	payload, err := MarshalBookingEvent("com.opendesk.booking.BookingCancelled", tenantSlug, booking, offering, contact)
	if err != nil {
		return store.Booking{}, err
	}
	if err := s.Store.SetBookingStatus(ctx, tenantID, bookingID, store.StatusCancelled, s.EventsTopic, payload); err != nil {
		return store.Booking{}, err
	}
	booking.Status = store.StatusCancelled
	s.Cache.Invalidate(ctx, booking.TenantID, booking.OfferingID, booking.TeamMemberID, booking.StartsAt, booking.EndsAt)
	return booking, nil
}

// Confirm transitions pending -> confirmed (saga path) and emits
// BookingConfirmed. Idempotent.
func (s *Service) Confirm(ctx context.Context, tenantID uuid.UUID, tenantSlug string, bookingID uuid.UUID) error {
	booking, err := s.Store.GetBooking(ctx, tenantID, bookingID)
	if err != nil {
		return err
	}
	if booking.Status == store.StatusConfirmed {
		return nil
	}
	if booking.Status != store.StatusPending {
		return fmt.Errorf("cannot confirm booking in status %q", booking.Status)
	}
	offering, _ := s.Store.GetOffering(ctx, tenantID, booking.OfferingID)
	contact, _ := s.Store.GetContact(ctx, tenantID, booking.ContactID)
	payload, err := MarshalBookingEvent("com.opendesk.booking.BookingConfirmed", tenantSlug, booking, offering, contact)
	if err != nil {
		return err
	}
	if err := s.Store.SetBookingStatus(ctx, tenantID, bookingID, store.StatusConfirmed, s.EventsTopic, payload); err != nil {
		return err
	}
	s.Cache.Invalidate(ctx, booking.TenantID, booking.OfferingID, booking.TeamMemberID, booking.StartsAt, booking.EndsAt)
	return nil
}

// MarkNoShow transitions confirmed -> no_show and emits BookingNoShow.
func (s *Service) MarkNoShow(ctx context.Context, tenantID uuid.UUID, tenantSlug string, bookingID uuid.UUID) error {
	booking, err := s.Store.GetBooking(ctx, tenantID, bookingID)
	if err != nil {
		return err
	}
	if booking.Status == store.StatusNoShow {
		return nil
	}
	if booking.Status != store.StatusConfirmed {
		return fmt.Errorf("cannot mark no-show booking in status %q", booking.Status)
	}
	offering, _ := s.Store.GetOffering(ctx, tenantID, booking.OfferingID)
	contact, _ := s.Store.GetContact(ctx, tenantID, booking.ContactID)
	payload, err := MarshalBookingEvent("com.opendesk.booking.BookingNoShow", tenantSlug, booking, offering, contact)
	if err != nil {
		return err
	}
	if err := s.Store.SetBookingStatus(ctx, tenantID, bookingID, store.StatusNoShow, s.EventsTopic, payload); err != nil {
		return err
	}
	s.Cache.Invalidate(ctx, booking.TenantID, booking.OfferingID, booking.TeamMemberID, booking.StartsAt, booking.EndsAt)
	return nil
}

// CompleteFromSaga marks a booking completed after the appointment time
// (saga path; idempotent). Distinct from Complete (K11 operator flow with
// deposit capture + loyalty accrual): the saga auto-completion does NOT
// move money and emits BookingCompleted directly.
func (s *Service) CompleteFromSaga(ctx context.Context, tenantID uuid.UUID, tenantSlug string, bookingID uuid.UUID) error {
	booking, err := s.Store.GetBooking(ctx, tenantID, bookingID)
	if err != nil {
		return err
	}
	if booking.Status == store.StatusCompleted {
		return nil
	}
	offering, _ := s.Store.GetOffering(ctx, tenantID, booking.OfferingID)
	contact, _ := s.Store.GetContact(ctx, tenantID, booking.ContactID)
	payload, err := MarshalBookingEvent("com.opendesk.booking.BookingCompleted", tenantSlug, booking, offering, contact)
	if err != nil {
		return err
	}
	if err := s.Store.SetBookingStatus(ctx, tenantID, bookingID, store.StatusCompleted, s.EventsTopic, payload); err != nil {
		return err
	}
	s.Cache.Invalidate(ctx, booking.TenantID, booking.OfferingID, booking.TeamMemberID, booking.StartsAt, booking.EndsAt)
	return nil
}
