package referrals

// SPEC-W80 B2: the referral_converted loyalty earn rule — a NON-idempotent
// verify of a contact-referrer referral accrues loyalty points for the
// referrer through the optional Loyalty seam; idempotent replays never
// double-award; a nil seam leaves Verify untouched (existing posture).

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/loyalty"
)

// fakeAccruer is the ReferralConvertedAccruer test double.
type fakeAccruer struct {
	mu    sync.Mutex
	calls []accrueCall
	err   error
}

type accrueCall struct {
	tenantID, contactID uuid.UUID
	event, refID        string
}

func (f *fakeAccruer) Accrue(_ context.Context, tenantID, contactID uuid.UUID, event, refID string) (loyalty.AccrueResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, accrueCall{tenantID, contactID, event, refID})
	return loyalty.AccrueResult{}, f.err
}

func (f *fakeAccruer) got() []accrueCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]accrueCall(nil), f.calls...)
}

func TestVerifyAccruesReferralConverted(t *testing.T) {
	st := newServiceTestStore(t)
	svc := newService(st)
	acc := &fakeAccruer{}
	svc.Loyalty = acc
	ctx := context.Background()
	tenantID := uuid.New()

	mkSvcRule(t, svc, tenantID, "signup-flat", TriggerSignupVerified, BeneficiaryReferrer, AmountFlat, 50000, 0, nil, true, 1)
	referrerID := mkContactReferrer(t, st, tenantID)
	ref, created, err := svc.Create(ctx, CreateInput{
		TenantID: tenantID, ReferrerType: ReferrerContact, ReferrerID: referrerID, RefereePhone: "+2348099990002",
	})
	if err != nil || !created {
		t.Fatalf("create referral: created=%v err=%v", created, err)
	}

	if _, err := svc.Verify(ctx, tenantID, ref.ID, TriggerSignupVerified, 0, "acme-ng"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	calls := acc.got()
	if len(calls) != 1 {
		t.Fatalf("accrue calls = %d, want 1 (%+v)", len(calls), calls)
	}
	c := calls[0]
	if c.tenantID != tenantID || c.contactID.String() != referrerID {
		t.Fatalf("accrue target = %s/%s, want %s/%s", c.tenantID, c.contactID, tenantID, referrerID)
	}
	if c.event != loyalty.EventReferralConverted {
		t.Fatalf("accrue event = %q, want referral_converted", c.event)
	}
	if c.refID != "referral:"+ref.ID.String() {
		t.Fatalf("accrue ref_id = %q, want referral:%s", c.refID, ref.ID)
	}

	// Idempotent replay: no second accrual.
	res, err := svc.Verify(ctx, tenantID, ref.ID, TriggerSignupVerified, 0, "acme-ng")
	if err != nil {
		t.Fatalf("replay verify: %v", err)
	}
	if !res.AlreadyVerified {
		t.Fatal("second verify must be an idempotent replay")
	}
	if got := acc.got(); len(got) != 1 {
		t.Fatalf("accrue calls after replay = %d, want still 1", len(got))
	}

	// A failing loyalty seam must NOT fail the verify (best-effort
	// post-commit posture — the referral is already durable).
	acc2 := &fakeAccruer{err: loyalty.ErrNoActiveProgram}
	svc.Loyalty = acc2
	referrer2 := mkContactReferrer(t, st, tenantID)
	ref2, created, err := svc.Create(ctx, CreateInput{
		TenantID: tenantID, ReferrerType: ReferrerContact, ReferrerID: referrer2, RefereePhone: "+2348099990003",
	})
	if err != nil || !created {
		t.Fatalf("create referral 2: created=%v err=%v", created, err)
	}
	if _, err := svc.Verify(ctx, tenantID, ref2.ID, TriggerSignupVerified, 0, "acme-ng"); err != nil {
		t.Fatalf("verify with no-program loyalty must succeed: %v", err)
	}
	if got := acc2.got(); len(got) != 1 {
		t.Fatalf("accrue calls (no program) = %d, want 1 (attempted, swallowed)", len(got))
	}
}
