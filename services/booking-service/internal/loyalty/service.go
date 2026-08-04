package loyalty

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"go.uber.org/zap"
)

// Service carries the accrue/redeem business flow: resolve the active
// program, apply earn rules, post journals via the store and emit
// CloudEvents + usage-metering records (best-effort, post-commit —
// mirroring the W14 referrals posture: emission must never block a
// mutation, failures are logged for reconciliation).
type Service struct {
	Store *Store
	// Ledger is the points double-entry seam (PostgresLedger today; the
	// TigerBeetle adapter swaps in without touching handlers).
	Ledger Ledger
	// EventsTopic is the points lifecycle topic (LOYALTY_EVENTS_TOPIC,
	// default opendesk.loyalty.events.v1). Empty disables emission.
	EventsTopic string
	// UsageTopic is the usage-metering topic (USAGE_EVENTS_TOPIC, default
	// opendesk.usage.events). Empty disables points_redeemed metering.
	UsageTopic string
	Log        *zap.Logger
}

func (s *Service) log() *zap.Logger {
	if s.Log != nil {
		return s.Log
	}
	return zap.NewNop()
}

// Accrue applies the active program's earn rule for one event to one
// contact (POST /v1/loyalty/accrue). ErrNoActiveProgram when the tenant
// has no active program; ErrInvalidInput when the event is unknown or not
// awarded by the program.
func (s *Service) Accrue(ctx context.Context, tenantID, contactID uuid.UUID, event, refID string) (AccrueResult, error) {
	if contactID == uuid.Nil {
		return AccrueResult{}, errors.Join(ErrInvalidInput, errors.New("contact_id is required"))
	}
	if err := ValidateEvent(event); err != nil {
		return AccrueResult{}, err
	}
	if refID == "" {
		return AccrueResult{}, errors.Join(ErrInvalidInput, errors.New("ref_id is required"))
	}
	prog, err := s.Store.ActiveProgram(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return AccrueResult{}, ErrNoActiveProgram
		}
		return AccrueResult{}, err
	}
	points := prog.PointsForEvent(event)
	if points <= 0 {
		return AccrueResult{}, errors.Join(ErrInvalidInput,
			errors.New("active program does not award event "+event))
	}
	res, err := s.Store.Accrue(ctx, AccrueParams{
		TenantID:  tenantID,
		ContactID: contactID,
		Event:     event,
		RefID:     refID,
		Points:    points,
		CapPerDay: prog.CapPerDay,
		Tiers:     prog.Tiers,
	})
	if err != nil {
		return AccrueResult{}, err
	}
	if res.Applied {
		// Non-idempotent path only — a replayed accrual never re-emits.
		s.emitLifecycle(ctx, EventTypePointsIssued, tenantID, contactID, map[string]any{
			"event":         event,
			"ref_id":        refID,
			"points":        res.Awarded,
			"program_id":    prog.ID.String(),
			"balance_after": res.Wallet.Balance,
			"tier":          res.Wallet.Tier,
		})
	}
	return res, nil
}

// Redeem debits points from one contact's wallet (POST
// /v1/loyalty/redeem). *InsufficientError (→ 409) when the balance is
// short. Meters points_redeemed on the non-idempotent path only.
func (s *Service) Redeem(ctx context.Context, tenantID, contactID uuid.UUID, points int64, reason, refID string) (RedeemResult, error) {
	if contactID == uuid.Nil {
		return RedeemResult{}, errors.Join(ErrInvalidInput, errors.New("contact_id is required"))
	}
	res, err := s.Store.Redeem(ctx, RedeemParams{
		TenantID:  tenantID,
		ContactID: contactID,
		Points:    points,
		Reason:    reason,
		RefID:     refID,
	})
	if err != nil {
		return RedeemResult{}, err
	}
	if res.Applied {
		s.emitLifecycle(ctx, EventTypePointsRedeemed, tenantID, contactID, map[string]any{
			"ref_id":        res.RedeemRef,
			"reason":        reason,
			"points":        res.Redeemed,
			"balance_after": res.Wallet.Balance,
		})
		s.meterPointsRedeemed(ctx, tenantID, contactID, res)
	}
	return res, nil
}

// emitLifecycle writes one points lifecycle CloudEvent to the outbox
// (best-effort, post-commit — the same posture as the W14 referrals
// metering: emission must never block a mutation).
func (s *Service) emitLifecycle(ctx context.Context, eventType string, tenantID, contactID uuid.UUID, data map[string]any) {
	if s.EventsTopic == "" {
		return
	}
	data["tenant_id"] = tenantID.String()
	data["contact_id"] = contactID.String()
	data["ts"] = time.Now().UTC()
	evt := events.New("booking-service", eventType, contactID.String(), tenantID.String(), data)
	payload, err := json.Marshal(evt)
	if err != nil {
		s.log().Warn("loyalty event marshal failed; skipping emission", zap.Error(err))
		return
	}
	if err := s.Store.EnqueueOutbox(ctx, contactID, s.EventsTopic, payload); err != nil {
		s.log().Warn("loyalty event enqueue failed; skipping emission", zap.Error(err))
	}
}
