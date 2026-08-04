package referrals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/events"
	"github.com/opendesk/booking-service/internal/leads"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// EntityTypeCustomer is the funnel entity_type for referral conversion hooks
// (contract SPEC-W13 §2 enum: lead|customer|agent) — the referee became a
// customer when the referral verifies.
const EntityTypeCustomer = "customer"

// ChannelReferral is the funnel channel of referral-driven events.
const ChannelReferral = "referral"

// Service bundles the SPEC-W14 referral & commission orchestration: referral
// CRUD with the §1 dedupe, the verify flow (rules → balanced accrual
// postings → status flip, idempotent), rules CRUD and ledger reads. Funnel
// hooks (converted / first_txn) go to cac.events via the transactional
// outbox — the same posture as the Wave-13 leads service.
type Service struct {
	Store  *store.Store
	Ledger Ledger // Postgres today; TigerBeetle adapter seam — see ledger.go
	// Leads is the Wave-13 leads service used for the §6 conversion hook
	// (referral verify walks the referee's lead to `converted` through the
	// leads status machine). Nil disables the hook.
	Leads *leads.Service
	// CACEventsTopic is the funnel topic (CAC_EVENTS_TOPIC, default
	// cac.events). Empty disables emission.
	CACEventsTopic string
	// UsageTopic is the usage-metering topic (USAGE_EVENTS_TOPIC, default
	// opendesk.usage.events) for the referral_verified metered row
	// (SPEC-W14 Agent D (additive); empty disables metering).
	UsageTopic string
	Log        *zap.Logger
}

func (s *Service) log() *zap.Logger {
	if s.Log != nil {
		return s.Log
	}
	return zap.NewNop()
}

// ---------------------------------------------------------------------------
// Referrals
// ---------------------------------------------------------------------------

// CreateInput is one referral-creation request (POST /v1/referrals).
type CreateInput struct {
	TenantID     uuid.UUID
	ReferrerType string // contact | agent | staff
	ReferrerID   string
	RefereePhone string
	CampaignID   *uuid.UUID
	BountyRuleID *uuid.UUID
}

// Create persists a referral. Idempotent per contract §1: one OPEN referral
// per (tenant, referee_phone) — a duplicate returns the EXISTING open
// referral unchanged (created=false), mirroring the leads first-touch
// dedupe.
func (s *Service) Create(ctx context.Context, in CreateInput) (Referral, bool, error) {
	ref := Referral{
		ID:           uuid.New(),
		TenantID:     in.TenantID,
		ReferrerType: strings.ToLower(strings.TrimSpace(in.ReferrerType)),
		ReferrerID:   strings.TrimSpace(in.ReferrerID),
		RefereePhone: strings.TrimSpace(in.RefereePhone),
		CampaignID:   in.CampaignID,
		BountyRuleID: in.BountyRuleID,
	}
	if err := ValidateReferral(&ref); err != nil {
		return ref, false, err
	}
	created, err := s.Store.InsertReferral(ctx, &ref)
	return ref, created, err
}

// VerifyResult is the outcome of POST /v1/referrals/{id}/verify.
type VerifyResult struct {
	Referral Referral `json:"referral"`
	// AlreadyVerified is true on idempotent replays (the referral was
	// already verified/converted/paid — no new postings, no new events).
	AlreadyVerified bool    `json:"already_verified"`
	Awards          []Award `json:"awards"`
}

// Verify fires the commission rules for a trigger (contract §2), posts the
// balanced accrual pairs (§3) and flips the referral pending → verified
// (signup_verified) or pending → converted (revenue triggers) — atomically
// in one transaction. Idempotent: a replay on a non-pending referral returns
// the current row with AlreadyVerified=true and posts/emits nothing.
//
// Post-commit side effects (best-effort, same posture as leads.emit — the
// referral + postings are durable; failures are logged for reconciliation):
//   - §6 lead hook: the referee's open lead is walked to `converted` via
//     the Wave-13 leads SERVICE (emits its own FunnelEvents);
//   - §6 funnel hook: one FunnelEvent (converted, or first_txn when the
//     trigger is first_txn) with entity_type=customer on cac.events.
func (s *Service) Verify(ctx context.Context, tenantID, referralID uuid.UUID, trigger string, baseKobo int64, tenantSlug string) (VerifyResult, error) {
	trigger = strings.ToLower(strings.TrimSpace(trigger))
	if !validTriggers[trigger] {
		return VerifyResult{}, fmt.Errorf("%w: trigger %q (want signup_verified|first_booking|first_txn|sale)", ErrInvalidInput, trigger)
	}
	if baseKobo < 0 {
		return VerifyResult{}, fmt.Errorf("%w: base_amount_ngn must be >= 0", ErrInvalidInput)
	}
	ref, err := s.Store.GetReferral(ctx, tenantID, referralID)
	if err != nil {
		return VerifyResult{}, err
	}
	if ref.Status == StatusRejected {
		return VerifyResult{}, fmt.Errorf("%w: rejected referrals cannot be verified", ErrInvalidTransition)
	}
	if ref.Status != StatusPending {
		// Idempotent replay: return current state, no double posting.
		return VerifyResult{Referral: ref, AlreadyVerified: true, Awards: []Award{}}, nil
	}

	rules, err := s.Store.ListRules(ctx, tenantID)
	if err != nil {
		return VerifyResult{}, err
	}
	awards := EvaluateRules(rules, trigger, baseKobo, ref)

	entries := []LedgerEntry{}
	var bountyRuleID *uuid.UUID
	for _, a := range awards {
		if bountyRuleID == nil {
			id := a.RuleID
			bountyRuleID = &id
		}
		entries = append(entries, NewAccrualPair(tenantID, ref, a)...)
	}
	toStatus := StatusVerified
	if IsRevenueTrigger(trigger) {
		toStatus = StatusConverted
	}
	updated, already, err := s.Store.VerifyReferralTx(ctx, tenantID, referralID, toStatus, bountyRuleID, entries)
	if err != nil {
		return VerifyResult{}, err
	}
	if already {
		// Lost the race with a concurrent verify: same idempotent outcome.
		return VerifyResult{Referral: updated, AlreadyVerified: true, Awards: []Award{}}, nil
	}

	// Post-commit hooks (best-effort, logged — see method doc).
	s.convertRefereeLead(ctx, tenantID, updated.RefereePhone, tenantSlug)
	s.emitFunnelHook(ctx, updated, trigger, baseKobo, tenantSlug)
	// SPEC-W14 Agent D (additive): one referral_verified usage outbox row
	// per NON-idempotent verify (metering.go).
	s.meterReferralVerified(ctx, updated, tenantSlug)

	return VerifyResult{Referral: updated, Awards: awards}, nil
}

// Reject moves a referral pending|verified → rejected (the audit-preserving
// "delete" of the referrals CRUD — rows are never hard-deleted).
func (s *Service) Reject(ctx context.Context, tenantID, referralID uuid.UUID) (Referral, error) {
	ref, err := s.Store.RejectReferral(ctx, tenantID, referralID)
	if errors.Is(err, store.ErrConflict) {
		cur, getErr := s.Store.GetReferral(ctx, tenantID, referralID)
		if getErr != nil {
			return ref, getErr
		}
		return ref, fmt.Errorf("%w: %s→rejected", ErrInvalidTransition, cur.Status)
	}
	return ref, err
}

// ---------------------------------------------------------------------------
// Commission rules (tenant-editable, contract §2)
// ---------------------------------------------------------------------------

// CreateRule validates + persists one rule.
func (s *Service) CreateRule(ctx context.Context, r *CommissionRule) error {
	r.Trigger = strings.ToLower(strings.TrimSpace(r.Trigger))
	r.Beneficiary = strings.ToLower(strings.TrimSpace(r.Beneficiary))
	r.AmountType = strings.ToLower(strings.TrimSpace(r.AmountType))
	r.Name = strings.TrimSpace(r.Name)
	if err := ValidateRule(r); err != nil {
		return err
	}
	return s.Store.InsertRule(ctx, r)
}

// UpdateRule validates + replaces one rule (incl. the active toggle).
func (s *Service) UpdateRule(ctx context.Context, r *CommissionRule) error {
	r.Trigger = strings.ToLower(strings.TrimSpace(r.Trigger))
	r.Beneficiary = strings.ToLower(strings.TrimSpace(r.Beneficiary))
	r.AmountType = strings.ToLower(strings.TrimSpace(r.AmountType))
	r.Name = strings.TrimSpace(r.Name)
	if err := ValidateRule(r); err != nil {
		return err
	}
	return s.Store.UpdateRule(ctx, r)
}

// ---------------------------------------------------------------------------
// Ledger reads (contract §3)
// ---------------------------------------------------------------------------

// LedgerEntries lists the tenant's ledger rows in [from,to] (nil =
// unbounded) — GET /v1/commissions/ledger?from&to.
func (s *Service) LedgerEntries(ctx context.Context, tenantID uuid.UUID, from, to *time.Time) ([]LedgerEntry, error) {
	return s.Ledger.Entries(ctx, tenantID, from, to)
}

// CommissionBalance returns the beneficiary's payable balance in kobo:
// account 300 (commission_payable) credits − debits — GET
// /v1/commissions/balance/{beneficiary}.
func (s *Service) CommissionBalance(ctx context.Context, tenantID uuid.UUID, beneficiaryID string) (int64, error) {
	beneficiaryID = strings.TrimSpace(beneficiaryID)
	if beneficiaryID == "" {
		return 0, fmt.Errorf("%w: beneficiary is required", ErrInvalidInput)
	}
	return s.Ledger.Balance(ctx, tenantID, AccountCommissionPayable, beneficiaryID)
}

// ---------------------------------------------------------------------------
// Post-commit hooks (contract §6)
// ---------------------------------------------------------------------------

// nextLeadStep is the Wave-13 leads status machine path towards converted:
// new→contacted→qualified→converted (converted|lost terminal, filtered out
// by FindOpenLeadByPhone).
var nextLeadStep = map[string]string{
	leads.StatusNew:       leads.StatusContacted,
	leads.StatusContacted: leads.StatusQualified,
	leads.StatusQualified: leads.StatusConverted,
}

// convertRefereeLead implements the §6 coordination with W13: when the
// referee matches an open lead of the tenant, walk that lead to `converted`
// through the leads SERVICE (leads.Service.Transition emits the §2
// FunnelEvents per step). Wave-13 exported no by-phone lookup, so the
// resolution uses the additive store seam FindOpenLeadByPhone (documented
// there + in docs/referrals-commissions.md) — leads rows are only mutated
// by the leads service itself. Best-effort: the referral + postings are
// already durable; failures are logged for reconciliation.
func (s *Service) convertRefereeLead(ctx context.Context, tenantID uuid.UUID, refereePhone, tenantSlug string) {
	if s.Leads == nil {
		return
	}
	lead, err := s.Store.FindOpenLeadByPhone(ctx, tenantID, refereePhone)
	if errors.Is(err, store.ErrNotFound) {
		return // no open lead for this phone (or already converted/lost)
	}
	if err != nil {
		s.log().Error("referral lead hook: lookup failed; referral durable but lead not converted — reconcile",
			zap.String("phone", refereePhone), zap.Error(err))
		return
	}
	for lead.Status != leads.StatusConverted {
		next := nextLeadStep[lead.Status]
		if next == "" {
			return
		}
		lead, err = s.Leads.Transition(ctx, tenantID, lead.ID, next, tenantSlug)
		if err != nil {
			s.log().Error("referral lead hook: transition failed; referral durable but lead not converted — reconcile",
				zap.String("lead_id", lead.ID.String()), zap.String("to", next), zap.Error(err))
			return
		}
	}
}

// emitFunnelHook emits the §6 referral funnel hook on cac.events: event_name
// first_txn when the verify trigger is first_txn, else converted;
// entity_type=customer, entity_id=referee phone, channel=referral. The
// idempotency key is deterministic (referral × event) so the analytics
// consumer dedupes replays. Best-effort (same posture as leads.emit).
func (s *Service) emitFunnelHook(ctx context.Context, ref Referral, trigger string, baseKobo int64, tenantSlug string) {
	if s.CACEventsTopic == "" {
		return
	}
	eventName := leads.EventConverted
	if trigger == TriggerFirstTxn {
		eventName = leads.EventFirstTxn
	}
	var amount *float64
	if baseKobo > 0 {
		ngn := float64(baseKobo) / 100
		amount = &ngn
	}
	evt := leads.FunnelEvent{
		EventID:        uuid.NewString(),
		TenantID:       ref.TenantID.String(),
		EntityType:     EntityTypeCustomer,
		EntityID:       ref.RefereePhone,
		EventName:      eventName,
		EventTS:        time.Now().UTC(),
		Channel:        ChannelReferral,
		CampaignID:     ref.CampaignID,
		AmountNGN:      amount,
		IdempotencyKey: "referral:" + ref.ID.String() + ":" + eventName,
	}
	payload, err := json.Marshal(events.New("booking-service", leads.EventTypeFunnel, tenantSlug,
		ref.TenantID.String(), evt.Map()))
	if err != nil {
		s.log().Warn("referral funnel hook marshal failed; skipping emission", zap.Error(err))
		return
	}
	if err := s.Store.EnqueueOutbox(ctx, ref.ID, s.CACEventsTopic, payload); err != nil {
		s.log().Error("referral funnel hook enqueue failed; referral durable but cac.events row lost — reconcile",
			zap.String("referral_id", ref.ID.String()), zap.String("event_name", eventName), zap.Error(err))
	}
}
