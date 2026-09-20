package referrals

// Referral agent registry (SPEC-W45 CODER-A items 6+7, STK O10/O18):
//   - referral_agents rows are the ONLY valid referrer_id targets for
//     referrer_type=agent (Create validates the referrer resolves — contact
//     → contacts row, staff → team_members row, agent → referral_agents
//     row; anything else is ErrUnknownReferrer, HTTP 422);
//   - commissions to an agent require status=approved AND beneficiary_id
//     set (the W44 K7 payout-beneficiary link) — enforced at verify time.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
)

// ReferralAgent is the registry row (canonical type lives in store).
type ReferralAgent = store.ReferralAgent

// Agent statuses (mirror store constants).
const (
	AgentPending   = store.AgentPending
	AgentApproved  = store.AgentApproved
	AgentSuspended = store.AgentSuspended
)

// ErrUnknownReferrer marks a referrer_id that resolves to no contact /
// team member / registered agent in the tenant (HTTP 422, STK O18).
var ErrUnknownReferrer = errors.New("unknown referrer")

// ErrAgentNotPayable rejects commissions to an agent that is not
// approved+beneficiary-linked (HTTP 409, STK O10).
var ErrAgentNotPayable = errors.New("agent is not payable (want status=approved with beneficiary_id)")

// validateReferrer enforces STK O18: the referrer must resolve inside the
// tenant — contact → contacts.id, staff → team_members.id, agent →
// referral_agents.id (any status; the PAYABLE gate lives in Verify).
func (s *Service) validateReferrer(ctx context.Context, tenantID uuid.UUID, referrerType, referrerID string) error {
	id, err := uuid.Parse(strings.TrimSpace(referrerID))
	if err != nil {
		return fmt.Errorf("%w: referrer_id must be the %s's uuid", ErrUnknownReferrer, referrerType)
	}
	switch referrerType {
	case ReferrerContact:
		_, err = s.Store.GetContact(ctx, tenantID, id)
	case ReferrerStaff:
		_, err = s.Store.GetTeamMember(ctx, tenantID, id)
	case ReferrerAgent:
		_, err = s.Store.GetReferralAgent(ctx, tenantID, id)
	}
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: no %s with id %s in this tenant", ErrUnknownReferrer, referrerType, id)
	}
	return err
}

// requireAgentPayable enforces STK O10 at commission time: the agent must
// be approved and carry a beneficiary_id (W44 K7 payout-beneficiary link).
func (s *Service) requireAgentPayable(ctx context.Context, tenantID uuid.UUID, agentID string) error {
	id, err := uuid.Parse(agentID)
	if err != nil {
		return fmt.Errorf("%w: referrer_id %q is not an agent uuid", ErrAgentNotPayable, agentID)
	}
	agent, err := s.Store.GetReferralAgent(ctx, tenantID, id)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: agent %s is not registered", ErrAgentNotPayable, id)
	}
	if err != nil {
		return err
	}
	if agent.Status != AgentApproved || agent.BeneficiaryID == nil {
		return fmt.Errorf("%w: agent %s is %s (beneficiary set: %t)",
			ErrAgentNotPayable, id, agent.Status, agent.BeneficiaryID != nil)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Agent registry CRUD (POST/GET/PATCH /v1/referrals/agents)
// ---------------------------------------------------------------------------

// CreateAgentInput is one POST /v1/referrals/agents call. Agents register
// as pending; approval is an admin-only PATCH.
type CreateAgentInput struct {
	Name          string
	Phone         string
	BeneficiaryID *uuid.UUID
}

// CreateAgent validates and registers one agent (status pending).
func (s *Service) CreateAgent(ctx context.Context, tenantID uuid.UUID, in CreateAgentInput) (ReferralAgent, error) {
	a := ReferralAgent{
		TenantID:      tenantID,
		Name:          strings.TrimSpace(in.Name),
		Phone:         strings.TrimSpace(in.Phone),
		BeneficiaryID: in.BeneficiaryID,
	}
	if a.Name == "" {
		return a, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if a.Phone == "" {
		return a, fmt.Errorf("%w: phone is required", ErrInvalidInput)
	}
	if err := s.Store.InsertReferralAgent(ctx, &a); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return a, fmt.Errorf("%w: an agent with phone %s already exists", ErrInvalidTransition, a.Phone)
		}
		return a, err
	}
	return a, nil
}

// ListAgents returns the tenant's agents (optional status filter,
// validated).
func (s *Service) ListAgents(ctx context.Context, tenantID uuid.UUID, status string) ([]ReferralAgent, error) {
	switch status {
	case "", AgentPending, AgentApproved, AgentSuspended:
	default:
		return nil, fmt.Errorf("%w: status filter %q (want pending|approved|suspended)", ErrInvalidInput, status)
	}
	return s.Store.ListReferralAgents(ctx, tenantID, status)
}

// UpdateAgentInput is the admin PATCH: a status transition and/or the
// beneficiary link (set or explicit clear).
type UpdateAgentInput struct {
	Status           *string
	BeneficiaryID    *uuid.UUID
	ClearBeneficiary bool
}

// UpdateAgent applies the admin PATCH (the admin ROLE gate lives in the
// httpapi layer — see referrals.go).
func (s *Service) UpdateAgent(ctx context.Context, tenantID, id uuid.UUID, in UpdateAgentInput) (ReferralAgent, error) {
	if in.Status != nil {
		switch strings.ToLower(strings.TrimSpace(*in.Status)) {
		case AgentPending, AgentApproved, AgentSuspended:
			v := strings.ToLower(strings.TrimSpace(*in.Status))
			in.Status = &v
		default:
			return ReferralAgent{}, fmt.Errorf("%w: status %q (want pending|approved|suspended)", ErrInvalidInput, *in.Status)
		}
	}
	if in.Status == nil && in.BeneficiaryID == nil && !in.ClearBeneficiary {
		return ReferralAgent{}, fmt.Errorf("%w: nothing to update (status and/or beneficiary_id)", ErrInvalidInput)
	}
	a, err := s.Store.UpdateReferralAgent(ctx, tenantID, id, in.Status, in.BeneficiaryID, in.ClearBeneficiary)
	if err != nil {
		return ReferralAgent{}, err
	}
	// Honest guard: approving an agent without a beneficiary link succeeds
	// (registration is complete) but commissions stay blocked — surface it.
	if a.Status == AgentApproved && a.BeneficiaryID == nil {
		s.log().Warn("referral agent approved without beneficiary_id — commissions stay blocked until linked")
	}
	return a, nil
}
