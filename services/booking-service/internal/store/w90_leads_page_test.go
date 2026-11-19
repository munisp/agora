package store

// SPEC-W90: leads keyset pagination — store level. The cursor is bound to
// the (created_at, id) position of the previous page's last row, so pages
// are stable under timestamp ties and concurrent inserts (no offset
// drift). Redirection: removing the keyset predicate makes page 2 repeat
// page 1 rows; removing the id tiebreaker makes the 3-way tie order
// nondeterministic.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedW90Leads inserts 7 leads and pins created_at explicitly: a 3-way
// tie at t1, plus t2/t3/t4/t5 — newest first that's t5, t4, t3, then the
// t1 trio (id DESC inside the tie), then t2.
func seedW90Leads(t *testing.T, st *Store, tenantID uuid.UUID) []Lead {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	t1 := base                     // 3-way tie
	t2 := base.Add(-1 * time.Hour) // oldest
	t3 := base.Add(2 * time.Hour)  //
	t4 := base.Add(3 * time.Hour)  //
	t5 := base.Add(4 * time.Hour)  // newest
	stamps := []time.Time{t1, t1, t1, t2, t3, t4, t5}
	out := make([]Lead, len(stamps))
	for i, ts := range stamps {
		l := mkLead(tenantID, fmt.Sprintf("+23480310%06d", i), "web", fmt.Sprintf("w90-%d", i))
		if _, err := st.InsertLead(ctx, &l); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		execTenant(t, st, tenantID, `UPDATE leads SET created_at=$2::timestamptz WHERE tenant_id=$1 AND id=$3`, ts, l.ID)
		l.CreatedAt = ts
		out[i] = l
	}
	return out
}

// wantW90Order is the exact (created_at DESC, id DESC) total order.
// Index mapping from the stamp table: 0,1,2=t1 (tie); 3=t2; 4=t3; 5=t4;
// 6=t5.
func wantW90Order(seeded []Lead) []uuid.UUID {
	// The t1 trio must be sorted by id DESC, not by seed order.
	trio := []Lead{seeded[0], seeded[1], seeded[2]}
	for i := 0; i < len(trio); i++ {
		for j := i + 1; j < len(trio); j++ {
			if trio[j].ID.String() > trio[i].ID.String() {
				trio[i], trio[j] = trio[j], trio[i]
			}
		}
	}
	order := []Lead{seeded[6], seeded[5], seeded[4], trio[0], trio[1], trio[2], seeded[3]}
	ids := make([]uuid.UUID, len(order))
	for i, l := range order {
		ids[i] = l.ID
	}
	return ids
}

func TestListLeadsPageKeyset(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	seeded := seedW90Leads(t, st, tenantID)
	want := wantW90Order(seeded)

	// Page through with limit=3: pages of 3, 3, 1 — no dup, no skip.
	var got []uuid.UUID
	cursor := ""
	pages := 0
	for {
		rows, next, err := st.ListLeadsPage(ctx, tenantID, "", "", nil, nil, nil, "", cursor, 3)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if len(rows) == 0 {
			t.Fatalf("page %d: empty before exhaustion", pages)
		}
		if len(rows) > 3 {
			t.Fatalf("page %d: %d rows > limit 3", pages, len(rows))
		}
		for _, r := range rows {
			got = append(got, r.ID)
		}
		pages++
		if next == "" {
			break
		}
		if pages > 5 {
			t.Fatal("cursor never terminates")
		}
		cursor = next
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 3", pages)
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %s, want %s (full got %v)", i, got[i], want[i], got)
		}
	}

	// A cursor bound to the very last row returns empty — no dup of the
	// tail row, no error.
	last := seeded[3] // oldest (t2), final element of the total order
	endCur := encodeLeadCursor(last.CreatedAt, last.ID)
	rows, next, err := st.ListLeadsPage(ctx, tenantID, "", "", nil, nil, nil, "", endCur, 3)
	if err != nil {
		t.Fatalf("exhausted: %v", err)
	}
	if len(rows) != 0 || next != "" {
		t.Fatalf("exhausted page = %d rows, next %q", len(rows), next)
	}
}

func TestListLeadsPageCursorValidationAndClamp(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	seedW90Leads(t, st, tenantID)

	// Malformed cursors are ErrInvalidCursor — never a silent first page.
	for _, bad := range []string{"!!!not-base64!!!", "aGVsbG8", "MjAyNi0xMS0yMFQxMjowMDowMFo"} {
		if _, _, err := st.ListLeadsPage(ctx, tenantID, "", "", nil, nil, nil, "", bad, 10); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("cursor %q: err = %v, want ErrInvalidCursor", bad, err)
		}
	}

	// limit <=0 and >500 fall back to the default (100): all 7 fit.
	rows, next, err := st.ListLeadsPage(ctx, tenantID, "", "", nil, nil, nil, "", "", 0)
	if err != nil || len(rows) != 7 || next != "" {
		t.Fatalf("limit 0: %d rows next=%q err=%v", len(rows), next, err)
	}
	rows, next, err = st.ListLeadsPage(ctx, tenantID, "", "", nil, nil, nil, "", "", 99999)
	if err != nil || len(rows) != 7 || next != "" {
		t.Fatalf("limit 99999: %d rows next=%q err=%v", len(rows), next, err)
	}
}

func TestListLeadsPageComposesWithFilters(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	seeded := seedW90Leads(t, st, tenantID)

	// Flip three leads to contacted: newest (t5), one of the t1 tie trio,
	// and the oldest (t2) — so the filtered result also spans a page
	// boundary at limit=2 and exercises the tie inside the filter.
	for _, idx := range []int{6, 2, 3} {
		if _, err := st.UpdateLeadStatus(ctx, tenantID, seeded[idx].ID, "contacted"); err != nil {
			t.Fatalf("status %d: %v", idx, err)
		}
	}
	var got []uuid.UUID
	cursor := ""
	for {
		rows, next, err := st.ListLeadsPage(ctx, tenantID, "contacted", "", nil, nil, nil, "", cursor, 2)
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		for _, r := range rows {
			if r.Status != "contacted" {
				t.Fatalf("filter leaked status %q", r.Status)
			}
			got = append(got, r.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(got) != 3 || got[0] != seeded[6].ID || got[1] != seeded[2].ID || got[2] != seeded[3].ID {
		t.Fatalf("filtered order = %v, want [%s %s %s]", got, seeded[6].ID, seeded[2].ID, seeded[3].ID)
	}

	// Cursor + phone-prefix q compose: "+2348031" matches all 7, so paging
	// under the filter must follow the same total order as unfiltered.
	want := wantW90Order(seeded)
	p1, next1, err := st.ListLeadsPage(ctx, tenantID, "", "", nil, nil, nil, "+2348031", "", 2)
	if err != nil || len(p1) != 2 || next1 == "" {
		t.Fatalf("q page1 = %v next=%q err=%v", p1, next1, err)
	}
	p2, next2, err := st.ListLeadsPage(ctx, tenantID, "", "", nil, nil, nil, "+2348031", next1, 2)
	if err != nil || len(p2) != 2 {
		t.Fatalf("q page2 = %v err=%v", p2, err)
	}
	gotQ := []uuid.UUID{p1[0].ID, p1[1].ID, p2[0].ID, p2[1].ID}
	for i := range gotQ {
		if gotQ[i] != want[i] {
			t.Fatalf("q paged row %d = %s, want %s", i, gotQ[i], want[i])
		}
	}
	if next2 == "" {
		t.Fatal("q page2 must still have more pages")
	}
}
