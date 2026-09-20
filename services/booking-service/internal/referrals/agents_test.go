package referrals

// SPEC-W45 STK O10/O18 tests: the referral agent registry and the
// referrer-resolution / agent-payable gates.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// Create rejects referrer_ids that resolve to nothing in the tenant
// (unknown → ErrUnknownReferrer, which httpapi maps to 422).
func TestCreateRejectsUnknownReferrer(t *testing.T) {
	st := newServiceTestStore(t)
	svc := newService(st)
	ctx := context.Background()
	tenantID := uuid.New()

	for _, typ := range []string{ReferrerContact, ReferrerStaff, ReferrerAgent} {
		_, _, err := svc.Create(ctx, CreateInput{
			TenantID: tenantID, ReferrerType: typ,
			ReferrerID: uuid.NewString(), RefereePhone: "+2348011110001",
		})
		if !errors.Is(err, ErrUnknownReferrer) {
			t.Fatalf("%s referrer: err=%v, want ErrUnknownReferrer", typ, err)
		}
	}
	// Non-uuid referrer ids are unknown too (the registry keys on uuids).
	if _, _, err := svc.Create(ctx, CreateInput{
		TenantID: tenantID, ReferrerType: ReferrerContact,
		ReferrerID: "contact-1", RefereePhone: "+2348011110002",
	}); !errors.Is(err, ErrUnknownReferrer) {
		t.Fatalf("free-text referrer: err=%v, want ErrUnknownReferrer", err)
	}
	// A resolvable referrer passes.
	contactID := mkContactReferrer(t, st, tenantID)
	if _, created, err := svc.Create(ctx, CreateInput{
		TenantID: tenantID, ReferrerType: ReferrerContact,
		ReferrerID: contactID, RefereePhone: "+2348011110003",
	}); err != nil || !created {
		t.Fatalf("resolvable referrer: created=%v err=%v", created, err)
	}
	// A PENDING (registered, not yet payable) agent resolves at Create time
	// — the payable gate lives at Verify.
	agent, err := svc.CreateAgent(ctx, tenantID, CreateAgentInput{Name: "New Agent", Phone: "+2347011110001"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if agent.Status != AgentPending {
		t.Fatalf("new agent status = %q, want pending", agent.Status)
	}
	if _, created, err := svc.Create(ctx, CreateInput{
		TenantID: tenantID, ReferrerType: ReferrerAgent,
		ReferrerID: agent.ID.String(), RefereePhone: "+2348011110004",
	}); err != nil || !created {
		t.Fatalf("pending agent referrer: created=%v err=%v", created, err)
	}
}

// Commissions to an agent require status=approved AND beneficiary_id set —
// a pending/suspended agent (or one without the W44 K7 beneficiary link)
// cannot verify.
func TestVerifyAgentPayableGate(t *testing.T) {
	st := newServiceTestStore(t)
	svc := newService(st)
	ctx := context.Background()
	tenantID := uuid.New()
	mkSvcRule(t, svc, tenantID, "signup-flat", TriggerSignupVerified, BeneficiaryReferrer, AmountFlat, 50000, 0, nil, true, 1)

	mkRef := func(agentID string) Referral {
		ref, _, err := svc.Create(ctx, CreateInput{
			TenantID: tenantID, ReferrerType: ReferrerAgent,
			ReferrerID: agentID, RefereePhone: "+23480222" + uuid.NewString()[:7],
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return ref
	}

	// Pending agent → not payable.
	pending, err := svc.CreateAgent(ctx, tenantID, CreateAgentInput{Name: "P", Phone: "+2347033330001"})
	if err != nil {
		t.Fatal(err)
	}
	ref := mkRef(pending.ID.String())
	if _, err := svc.Verify(ctx, tenantID, ref.ID, TriggerSignupVerified, 0, "acme-ng"); !errors.Is(err, ErrAgentNotPayable) {
		t.Fatalf("pending agent verify: err=%v, want ErrAgentNotPayable", err)
	}

	// Approved but NO beneficiary link → still not payable.
	noBen, err := svc.CreateAgent(ctx, tenantID, CreateAgentInput{Name: "NB", Phone: "+2347033330002"})
	if err != nil {
		t.Fatal(err)
	}
	approved := AgentApproved
	if _, err := svc.UpdateAgent(ctx, tenantID, noBen.ID, UpdateAgentInput{Status: &approved}); err != nil {
		t.Fatal(err)
	}
	ref2 := mkRef(noBen.ID.String())
	if _, err := svc.Verify(ctx, tenantID, ref2.ID, TriggerSignupVerified, 0, "acme-ng"); !errors.Is(err, ErrAgentNotPayable) {
		t.Fatalf("beneficiary-less agent verify: err=%v, want ErrAgentNotPayable", err)
	}

	// Approved + beneficiary → payable.
	payable := mkPayableAgent(t, svc, tenantID)
	ref3 := mkRef(payable)
	res, err := svc.Verify(ctx, tenantID, ref3.ID, TriggerSignupVerified, 0, "acme-ng")
	if err != nil {
		t.Fatalf("payable agent verify: %v", err)
	}
	if res.AlreadyVerified || len(res.Awards) != 1 || res.Awards[0].BeneficiaryID != payable {
		t.Fatalf("payable verify result: %+v", res)
	}
}

// Agent registry CRUD: insert/list/update + the (tenant, phone) dedupe.
func TestAgentRegistryCRUD(t *testing.T) {
	st := newServiceTestStore(t)
	svc := newService(st)
	ctx := context.Background()
	tenantID, otherTenant := uuid.New(), uuid.New()

	a, err := svc.CreateAgent(ctx, tenantID, CreateAgentInput{Name: "Ada", Phone: "+2347044440001"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Duplicate (tenant, phone) → 409-mapped error; same phone in ANOTHER
	// tenant is fine.
	if _, err := svc.CreateAgent(ctx, tenantID, CreateAgentInput{Name: "Ada Clone", Phone: "+2347044440001"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("duplicate phone: err=%v, want ErrInvalidTransition(409)", err)
	}
	if _, err := svc.CreateAgent(ctx, otherTenant, CreateAgentInput{Name: "Other", Phone: "+2347044440001"}); err != nil {
		t.Fatalf("cross-tenant same phone must succeed: %v", err)
	}
	// Validation.
	if _, err := svc.CreateAgent(ctx, tenantID, CreateAgentInput{Name: "", Phone: "+2341"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty name: err=%v, want ErrInvalidInput", err)
	}
	if _, err := svc.CreateAgent(ctx, tenantID, CreateAgentInput{Name: "X", Phone: ""}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty phone: err=%v, want ErrInvalidInput", err)
	}

	// List (tenant-scoped, status filter).
	all, err := svc.ListAgents(ctx, tenantID, "")
	if err != nil || len(all) != 1 {
		t.Fatalf("list: %+v err=%v", all, err)
	}
	if none, _ := svc.ListAgents(ctx, tenantID, AgentApproved); len(none) != 0 {
		t.Fatalf("approved filter should be empty: %+v", none)
	}
	if _, err := svc.ListAgents(ctx, tenantID, "bogus"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bogus status filter: err=%v, want ErrInvalidInput", err)
	}

	// Update: approve + link beneficiary.
	ben := uuid.New()
	approved := AgentApproved
	got, err := svc.UpdateAgent(ctx, tenantID, a.ID, UpdateAgentInput{Status: &approved, BeneficiaryID: &ben})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Status != AgentApproved || got.BeneficiaryID == nil || *got.BeneficiaryID != ben {
		t.Fatalf("updated agent: %+v", got)
	}
	// Bad status / empty patch rejected; missing row → ErrNotFound.
	bogus := "bogus"
	if _, err := svc.UpdateAgent(ctx, tenantID, a.ID, UpdateAgentInput{Status: &bogus}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bogus status: err=%v, want ErrInvalidInput", err)
	}
	if _, err := svc.UpdateAgent(ctx, tenantID, a.ID, UpdateAgentInput{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty patch: err=%v, want ErrInvalidInput", err)
	}
}
