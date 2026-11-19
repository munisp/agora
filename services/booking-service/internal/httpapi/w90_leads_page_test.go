package httpapi

// SPEC-W90: GET /v1/leads keyset pagination — handler level. The
// response gains next_cursor only when another page exists (additive:
// pre-W90 clients see no change); a malformed cursor or a non-integer
// limit is a deterministic 400. Redirection: reverting the handler to
// Store.ListLeads fails the next_cursor round-trip; dropping the
// ErrInvalidCursor mapping fails the 400 case.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/opendesk/booking-service/internal/store"
)

func TestW90ListLeadsPagination(t *testing.T) {
	f := newW47FixtureOnPort(t, "w90-secret", 5590)
	ctx := context.Background()

	// Seed 5 leads through the store (dedupe keys distinct).
	for i, phone := range []string{"+2348031100001", "+2348031100002", "+2348031100003", "+2348031100004", "+2348031100005"} {
		l := store.Lead{
			TenantID:            f.tenant.ID,
			PhoneE164:           phone,
			ChannelOfFirstTouch: "web",
			DedupeKey:           "w90-http-" + phone[len(phone)-1:],
		}
		if _, err := f.st.InsertLead(ctx, &l); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	get := func(path string) (int, map[string]any) {
		rec := f.do(http.MethodGet, path, ``, nil)
		var body map[string]any
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code, body
	}
	leadIDs := func(body map[string]any) []string {
		list, _ := body["leads"].([]any)
		out := make([]string, 0, len(list))
		for _, it := range list {
			if m, _ := it.(map[string]any); m != nil {
				out = append(out, m["lead_id"].(string))
			}
		}
		return out
	}

	// Round-trip: limit=2 pages 2/2/1 via the returned next_cursor.
	code, p1 := get("/v1/leads?limit=2")
	if code != http.StatusOK {
		t.Fatalf("page1 = %d", code)
	}
	ids1 := leadIDs(p1)
	if len(ids1) != 2 {
		t.Fatalf("page1 rows = %d, want 2", len(ids1))
	}
	next, _ := p1["next_cursor"].(string)
	if next == "" {
		t.Fatalf("page1 missing next_cursor: %v", p1)
	}
	code, p2 := get("/v1/leads?limit=2&cursor=" + next)
	if code != http.StatusOK {
		t.Fatalf("page2 = %d", code)
	}
	ids2 := leadIDs(p2)
	if len(ids2) != 2 {
		t.Fatalf("page2 rows = %d, want 2", len(ids2))
	}
	next2, _ := p2["next_cursor"].(string)
	if next2 == "" || next2 == next {
		t.Fatalf("page2 next_cursor = %q (page1 %q)", next2, next)
	}
	code, p3 := get("/v1/leads?limit=2&cursor=" + next2)
	if code != http.StatusOK {
		t.Fatalf("page3 = %d", code)
	}
	ids3 := leadIDs(p3)
	if len(ids3) != 1 {
		t.Fatalf("page3 rows = %d, want 1", len(ids3))
	}
	if _, ok := p3["next_cursor"]; ok {
		t.Fatalf("last page must omit next_cursor: %v", p3)
	}
	// No dup / no skip across the round-trip.
	seen := map[string]bool{}
	for _, id := range append(append(ids1, ids2...), ids3...) {
		if seen[id] {
			t.Fatalf("duplicate lead across pages: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != 5 {
		t.Fatalf("round-trip saw %d leads, want 5", len(seen))
	}

	// Newest-first preserved (last inserted first on page 1).
	if ids1[0] == ids3[0] {
		t.Fatalf("order collapsed: page1 head %s == page3 tail", ids1[0])
	}

	// Malformed cursor -> deterministic 400 (never a silent first page).
	if code, _ := get("/v1/leads?cursor=!!!garbage!!!"); code != http.StatusBadRequest {
		t.Fatalf("bad cursor = %d, want 400", code)
	}
	// Non-integer limit -> 400; >500 clamps (all 5 fit, no next_cursor).
	if code, _ := get("/v1/leads?limit=abc"); code != http.StatusBadRequest {
		t.Fatalf("limit=abc = %d, want 400", code)
	}
	code, clamped := get("/v1/leads?limit=99999")
	if code != http.StatusOK || len(leadIDs(clamped)) != 5 {
		t.Fatalf("limit=99999 = %d rows=%v", code, leadIDs(clamped))
	}
	if _, ok := clamped["next_cursor"]; ok {
		t.Fatalf("clamped single page must omit next_cursor: %v", clamped)
	}
	// No params -> default page, additive shape intact.
	code, def := get("/v1/leads")
	if code != http.StatusOK || len(leadIDs(def)) != 5 {
		t.Fatalf("default = %d rows=%v", code, leadIDs(def))
	}
}
