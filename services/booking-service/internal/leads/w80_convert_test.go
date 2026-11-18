package leads

// SPEC-W80 B3: ConvertOpenLeadByPhone (the bookingops.OpenLeadConverter
// implementation) walks an open lead to `converted` through the status
// machine; a phone with NO open lead is a graceful no-op; the match is
// strictly tenant-scoped.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestConvertOpenLeadByPhoneWalksToConverted(t *testing.T) {
	st := newServiceTestStore(t)
	svc := &Service{Store: st, CACEventsTopic: "cac.events", Log: zap.NewNop()}
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()
	phone := "+234801111111"

	leadA, created, err := svc.Create(ctx, CreateInput{
		TenantID: tenantA, PhoneE164: phone, Channel: "web",
	}, "acme")
	if err != nil || !created {
		t.Fatalf("create lead A: created=%v err=%v", created, err)
	}
	// Same phone in ANOTHER tenant — the conversion must never leak.
	leadB, created, err := svc.Create(ctx, CreateInput{
		TenantID: tenantB, PhoneE164: phone, Channel: "web",
	}, "beta")
	if err != nil || !created {
		t.Fatalf("create lead B: created=%v err=%v", created, err)
	}

	svc.ConvertOpenLeadByPhone(ctx, tenantA, phone, "acme")

	gotA, err := st.GetLead(ctx, tenantA, leadA.ID)
	if err != nil {
		t.Fatalf("get lead A: %v", err)
	}
	if gotA.Status != StatusConverted {
		t.Fatalf("lead A status = %q, want converted", gotA.Status)
	}
	gotB, err := st.GetLead(ctx, tenantB, leadB.ID)
	if err != nil {
		t.Fatalf("get lead B: %v", err)
	}
	if gotB.Status != StatusNew {
		t.Fatalf("lead B status = %q, want new (cross-tenant untouched)", gotB.Status)
	}

	// Replay: the lead is already converted (FindOpenLeadByPhone filters
	// terminal statuses) — a graceful no-op, no error, no regression.
	svc.ConvertOpenLeadByPhone(ctx, tenantA, phone, "acme")
	gotA, err = st.GetLead(ctx, tenantA, leadA.ID)
	if err != nil {
		t.Fatalf("get lead A after replay: %v", err)
	}
	if gotA.Status != StatusConverted {
		t.Fatalf("lead A status after replay = %q, want converted", gotA.Status)
	}

	// No open lead for the phone at all: no-op, no panic.
	svc.ConvertOpenLeadByPhone(ctx, tenantA, "+234809999999", "acme")
	// Empty phone: defensive no-op.
	svc.ConvertOpenLeadByPhone(ctx, tenantA, "  ", "acme")
}
