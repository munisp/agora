package bookingops

// Booking completion & the money loop (SPEC-W45 K11/K12):
//
//   - Complete: POST /v1/bookings/{id}/complete (staff, manage_bookings).
//     Status must be confirmed|checked_in → completed (store.StatusCompleted
//     gets its first caller) with the BookingCompleted CloudEvent in the
//     same transaction. When the caller references a verified deposit hold,
//     the hold is captured via the payments rail (fail-closed on missing
//     PAYMENTS_URL; idempotency key "complete-{booking_id}"). The loyalty
//     accrual hook runs when the loyalty package exposes it (see the
//     CONTRACT NOTE on BookingCompletedAccruer).
//   - Refund: POST /v1/bookings/{id}/refund (staff) → payments /v1/refunds
//     (K12; the rail execution — Flutterwave vs queued_manual — is
//     payments-side, CODER-K).

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// ErrInvalidTransition marks illegal booking status transitions (HTTP 409).
var ErrInvalidTransition = errors.New("invalid booking status transition")

// completableFrom are the statuses a booking may be completed from
// (SPEC-W45 K11). checked_in is reserved for the check-in flow; the schema
// CHECK allows it once that lands — validating it here costs nothing and
// keeps the contract honest.
var completableFrom = map[string]bool{
	store.StatusConfirmed: true,
	"checked_in":          true,
}

// BookingCompletedAccruer is the loyalty accrual hook for K11.
//
// CONTRACT NOTE (CODER-H, W45 work order H-7): loyalty exposes
// AccrueOnBookingCompleted(tenantID, bookingID). The wiring in
// cmd/server/main.go type-asserts the loyalty Deps against this interface —
// while the method does not exist yet the assertion yields nil and the hook
// is skipped (build stays green either way); once H lands it the hook
// activates with no further change here.
type BookingCompletedAccruer interface {
	AccrueOnBookingCompleted(tenantID, bookingID uuid.UUID)
}

// CompleteResult reports the outcome of Complete.
type CompleteResult struct {
	Booking store.Booking `json:"booking"`
	// AlreadyCompleted is true on idempotent replays.
	AlreadyCompleted bool `json:"already_completed"`
	// Capture is set when a deposit hold was captured via the payments rail.
	Capture *CaptureResult `json:"capture,omitempty"`
}

// Complete moves a booking confirmed|checked_in → completed, emits
// BookingCompleted (transactional outbox), captures the referenced deposit
// hold via the payments rail when depositID is given, and fires the loyalty
// accrual hook. Idempotent: completing an already-completed booking returns
// the current row (a deposit capture is still attempted when referenced —
// the rail dedupes it).
//
// Fail-closed money posture: a referenced deposit with NO payments rail
// wired errors with ErrPaymentsNotConfigured BEFORE any state mutation.
// A capture failure after the status flip leaves the booking completed and
// is surfaced as an error — the operator retries the endpoint (capture is
// idempotent at payments by deposit id).
func (s *Service) Complete(ctx context.Context, tenantID uuid.UUID, tenantSlug string, bookingID uuid.UUID, depositID *uuid.UUID, caller CallerIdentity) (CompleteResult, error) {
	if depositID != nil && s.Payments == nil {
		return CompleteResult{}, ErrPaymentsNotConfigured
	}
	booking, err := s.Store.GetBooking(ctx, tenantID, bookingID)
	if err != nil {
		return CompleteResult{}, err
	}
	res := CompleteResult{Booking: booking}
	if booking.Status == store.StatusCompleted {
		res.AlreadyCompleted = true
	} else {
		if !completableFrom[booking.Status] {
			return CompleteResult{}, fmt.Errorf("%w: %s → completed (want confirmed|checked_in)",
				ErrInvalidTransition, booking.Status)
		}
		offering, _ := s.Store.GetOffering(ctx, tenantID, booking.OfferingID)
		contact, _ := s.Store.GetContact(ctx, tenantID, booking.ContactID)
		payload, err := MarshalBookingEvent("com.opendesk.booking.BookingCompleted", tenantSlug, booking, offering, contact)
		if err != nil {
			return CompleteResult{}, err
		}
		if err := s.Store.SetBookingStatus(ctx, tenantID, bookingID, store.StatusCompleted, s.EventsTopic, payload); err != nil {
			return CompleteResult{}, err
		}
		booking.Status = store.StatusCompleted
		res.Booking = booking
	}

	if depositID != nil {
		capture, err := s.Payments.CaptureDeposit(ctx, tenantID.String(), *depositID,
			"complete-"+bookingID.String(), caller)
		if err != nil {
			s.Logger.Error("booking completed but deposit capture failed — retry the complete endpoint (idempotent at payments)",
				zap.String("booking_id", bookingID.String()),
				zap.String("deposit_id", depositID.String()), zap.Error(err))
			return res, fmt.Errorf("deposit capture: %w", err)
		}
		res.Capture = &capture
	}

	// Loyalty accrual hook (best-effort; see the CONTRACT NOTE on
	// BookingCompletedAccruer). Only on the NON-replay path — accrual must
	// not double-count.
	if !res.AlreadyCompleted && s.Loyalty != nil {
		s.Loyalty.AccrueOnBookingCompleted(tenantID, bookingID)
	}
	return res, nil
}

// RefundRequest is one POST /v1/bookings/{id}/refund call (K12).
type RefundRequest struct {
	DepositID   *uuid.UUID
	AmountCents int64
	Reason      string
	// IdempotencyKey defaults to "refund-{booking_id}" when empty.
	IdempotencyKey string
}

// Refund validates the booking exists in the tenant and posts the refund to
// the payments rail (fail-closed without PAYMENTS_URL). Booking-side state
// is NOT mutated (cancellation stays a separate, explicit operator action).
func (s *Service) Refund(ctx context.Context, tenantID uuid.UUID, bookingID uuid.UUID, in RefundRequest, caller CallerIdentity) (RefundResult, error) {
	if s.Payments == nil {
		return RefundResult{}, ErrPaymentsNotConfigured
	}
	if in.AmountCents <= 0 {
		return RefundResult{}, fmt.Errorf("%w: amount_cents must be > 0", ErrInvalidInput)
	}
	if _, err := s.Store.GetBooking(ctx, tenantID, bookingID); err != nil {
		return RefundResult{}, err
	}
	key := in.IdempotencyKey
	if key == "" {
		key = "refund-" + bookingID.String()
	}
	return s.Payments.Refund(ctx, tenantID.String(), in.DepositID, in.AmountCents, in.Reason, key, caller)
}
