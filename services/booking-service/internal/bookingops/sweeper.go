package bookingops

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// Sweeper re-drives StartBookingSaga for bookings stuck in `pending`
// because the saga start failed at create time (Temporal outage, SPEC-W43
// K-08). It is idempotent: the workflow ID is deterministic
// ("booking-saga-{bookingID}") with a reject-duplicate reuse policy, so a
// booking whose saga DID start is a cheap no-op.
//
// Defaults: interval 2min, min age 2min (a fresh pending row is likely
// mid-saga-start on the create path — never raced). Enabled by default in
// main (BOOKING_SWEEPER_ENABLED, default true).
type Sweeper struct {
	Store *store.Store
	Saga  SagaStarter
	Log   *zap.Logger

	// Interval between sweeps (default 2min).
	Interval time.Duration
	// MinAge is how old a pending booking must be before the sweeper
	// re-drives its saga (default 2min).
	MinAge time.Duration
	// Batch bounds one sweep (default 100).
	Batch int

	// ResolveSlug resolves the tenant slug for the saga input (events use it
	// as the CloudEvent subject). Optional: nil leaves TenantSlug empty.
	ResolveSlug func(ctx context.Context, tenantID uuid.UUID) string
}

func (s *Sweeper) logger() *zap.Logger {
	if s.Log != nil {
		return s.Log
	}
	return zap.NewNop()
}

// Run sweeps on a ticker until ctx is cancelled. A failing sweep is logged
// and retried next tick (self-healing background job).
func (s *Sweeper) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.SweepOnce(ctx)
			if err != nil {
				s.logger().Error("pending-booking sweep failed", zap.Error(err))
			} else if n > 0 {
				s.logger().Info("pending-booking sweep re-drove sagas", zap.Int("redriven", n))
			}
		}
	}
}

// SweepOnce re-drives the saga for one batch of stale pending bookings.
// Returns the number of sagas (re)started.
func (s *Sweeper) SweepOnce(ctx context.Context) (int, error) {
	if s.Saga == nil {
		return 0, nil // Temporal unreachable at boot — nothing to re-drive with
	}
	minAge := s.MinAge
	if minAge <= 0 {
		minAge = 2 * time.Minute
	}
	batch := s.Batch
	if batch <= 0 {
		batch = 100
	}
	pending, err := s.Store.ListStalePendingBookings(ctx, minAge, batch)
	if err != nil {
		return 0, err
	}
	redriven := 0
	for _, b := range pending {
		if err := s.redrive(ctx, b); err != nil {
			// Log and continue: one poison row must not starve the batch;
			// it is retried by the next sweep.
			s.logger().Error("saga re-drive failed; booking stays pending",
				zap.String("booking_id", b.ID.String()), zap.Error(err))
			continue
		}
		redriven++
	}
	return redriven, nil
}

func (s *Sweeper) redrive(ctx context.Context, b store.Booking) error {
	offering, err := s.Store.GetOffering(ctx, b.TenantID, b.OfferingID)
	if err != nil {
		return err
	}
	contact, err := s.Store.GetContact(ctx, b.TenantID, b.ContactID)
	if err != nil {
		return err
	}
	var slug string
	if s.ResolveSlug != nil {
		slug = s.ResolveSlug(ctx, b.TenantID)
	}
	// Deposit policy is not recoverable from the booking row; DepositKnown
	// stays false, matching the legacy full-price hold (safe direction).
	_, err = s.Saga.StartBookingSaga(ctx, SagaInput{
		BookingID:    b.ID.String(),
		TenantID:     b.TenantID.String(),
		TenantSlug:   slug,
		OfferingID:   b.OfferingID.String(),
		TeamMemberID: b.TeamMemberID.String(),
		ContactID:    b.ContactID.String(),
		ContactPhone: contact.Phone,
		ContactEmail: contact.Email,
		ContactName:  contact.Name,
		StartsAt:     b.StartsAt,
		EndsAt:       b.EndsAt,
		PriceCents:   offering.PriceCents,
		Currency:     offering.Currency,
		Source:       b.Source,
	})
	return err
}
