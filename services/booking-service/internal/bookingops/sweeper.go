package bookingops

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
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

	// WaitlistSweepInterval enables the waitlist auto-expiry sweep
	// (SPEC-W48 A2) when > 0: every interval, waiting|notified entries
	// whose window_end has passed flip to expired (batched, SKIP LOCKED),
	// one audit outbox row per entry. <= 0 disables the sweep entirely
	// (WAITLIST_SWEEP_INTERVAL, default 5m, wired in cmd/server/main.go).
	WaitlistSweepInterval time.Duration
	// WaitlistEventsTopic is the CloudEvents topic for the per-entry expiry
	// audit rows (empty = status flips still happen, no audit events).
	WaitlistEventsTopic string
}

func (s *Sweeper) logger() *zap.Logger {
	if s.Log != nil {
		return s.Log
	}
	return zap.NewNop()
}

// Run sweeps on a ticker until ctx is cancelled. A failing sweep is logged
// and retried next tick (self-healing background job). The waitlist
// auto-expiry sweep (SPEC-W48 A2) runs on its own ticker when
// WaitlistSweepInterval > 0.
func (s *Sweeper) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// The waitlist sweep ticker is nil-channel disabled (a nil channel
	// blocks forever in select) when the interval is not positive.
	var waitlistC <-chan time.Time
	var waitlistT *time.Ticker
	if s.WaitlistSweepInterval > 0 {
		waitlistT = time.NewTicker(s.WaitlistSweepInterval)
		waitlistC = waitlistT.C
		defer waitlistT.Stop()
	}
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
		case <-waitlistC:
			n, err := s.SweepWaitlistOnce(ctx)
			if err != nil {
				s.logger().Error("waitlist expiry sweep failed", zap.Error(err))
			} else if n > 0 {
				s.logger().Info("waitlist expiry sweep expired entries", zap.Int("expired", n))
			}
		}
	}
}

// SweepWaitlistOnce expires one batch of stale waitlist entries
// (waiting|notified with window_end < now()) and writes one audit outbox
// row per expired entry (SPEC-W48 A2). Returns the number expired.
// Audit-row failures are logged and skipped — the expiry itself is already
// committed and the sweeper must not stall on a Kafka-facing hiccup.
func (s *Sweeper) SweepWaitlistOnce(ctx context.Context) (int, error) {
	if s.Store == nil {
		return 0, nil
	}
	expired, err := s.Store.ExpireStaleWaitlist(ctx, s.Batch)
	if err != nil {
		return 0, err
	}
	if s.WaitlistEventsTopic == "" {
		return len(expired), nil
	}
	for _, e := range expired {
		slug := ""
		if s.ResolveSlug != nil {
			slug = s.ResolveSlug(ctx, e.TenantID)
		}
		payload, merr := json.Marshal(events.New("booking-service", EventTypeWaitlistEntryExpired,
			slug, e.TenantID.String(), map[string]any{
				"entry_id":    e.ID.String(),
				"offering_id": e.OfferingID.String(),
				"actor":       "waitlist-sweeper",
				"via":         "sweep",
				"window_end":  e.WindowEnd,
			}))
		if merr != nil {
			s.logger().Warn("waitlist expiry audit marshal failed; skipping audit row",
				zap.String("entry_id", e.ID.String()), zap.Error(merr))
			continue
		}
		if err := s.Store.EnqueueOutbox(ctx, e.ID, s.WaitlistEventsTopic, payload); err != nil {
			s.logger().Warn("waitlist expiry audit enqueue failed; skipping audit row",
				zap.String("entry_id", e.ID.String()), zap.Error(err))
		}
	}
	return len(expired), nil
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
