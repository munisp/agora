package store

// SPEC-W83: leads phone-prefix search (q) — store level.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestListLeadsPhoneSearch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	a := mkLead(tenantID, "+2348031111111", "whatsapp", "w83-a")
	b := mkLead(tenantID, "+2348032222222", "web", "w83-b")
	c := mkLead(tenantID, "+2347013333333", "qr", "w83-c")
	for i, l := range []*Lead{&a, &b, &c} {
		if _, err := st.InsertLead(ctx, l); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if _, err := st.UpdateLeadStatus(ctx, tenantID, b.ID, "contacted"); err != nil {
		t.Fatalf("status: %v", err)
	}

	// Prefix match narrows to the +234803 family.
	rows, err := st.ListLeads(ctx, tenantID, "", "", nil, nil, nil, "+234803")
	if err != nil || len(rows) != 2 {
		t.Fatalf("prefix: %+v, %v", rows, err)
	}
	for _, r := range rows {
		if !strings.HasPrefix(r.PhoneE164, "+234803") {
			t.Fatalf("non-prefix row leaked: %+v", r)
		}
	}

	// Combined with status.
	rows, err = st.ListLeads(ctx, tenantID, "contacted", "", nil, nil, nil, "+234803")
	if err != nil || len(rows) != 1 || rows[0].ID != b.ID {
		t.Fatalf("q+status: %+v, %v", rows, err)
	}

	// Full number pins exactly one lead.
	rows, err = st.ListLeads(ctx, tenantID, "", "", nil, nil, nil, "+2347013333333")
	if err != nil || len(rows) != 1 || rows[0].ID != c.ID {
		t.Fatalf("exact: %+v, %v", rows, err)
	}

	// No match -> empty, not error.
	rows, err = st.ListLeads(ctx, tenantID, "", "", nil, nil, nil, "+999")
	if err != nil || len(rows) != 0 {
		t.Fatalf("no match: %+v, %v", rows, err)
	}

	// Defence in depth: >32 chars is hard-capped (no error, no match).
	long := "+234803" + strings.Repeat("1", 40)
	rows, err = st.ListLeads(ctx, tenantID, "", "", nil, nil, nil, long)
	if err != nil || len(rows) != 0 {
		t.Fatalf("cap: %+v, %v", rows, err)
	}

	// Empty q preserves pre-W83 behavior (all three, newest first).
	rows, err = st.ListLeads(ctx, tenantID, "", "", nil, nil, nil, "")
	if err != nil || len(rows) != 3 {
		t.Fatalf("empty q: %+v, %v", rows, err)
	}

	// Cross-tenant: another tenant's leads never leak through q.
	other := mkLead(uuid.New(), "+2348039999999", "web", "w83-other")
	if _, err := st.InsertLead(ctx, &other); err != nil {
		t.Fatalf("insert other: %v", err)
	}
	rows, err = st.ListLeads(ctx, tenantID, "", "", nil, nil, nil, "+234803")
	if err != nil || len(rows) != 2 {
		t.Fatalf("cross-tenant leak: %+v, %v", rows, err)
	}
}
