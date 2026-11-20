package crm360

// SPEC-W83: keyset pagination for /v1/crm/contacts/search — store-level
// page walks, cursor validation, and handler-level next_cursor semantics.

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
)

// pagedNames walks all pages via SearchContactsPage and returns the
// flattened (name, id) sequence plus the number of pages seen.
func pagedNames(t *testing.T, st *Store, tenantID uuid.UUID, q, tag string, limit int) ([]ContactSearchResult, int) {
	t.Helper()
	ctx := context.Background()
	var all []ContactSearchResult
	cursor := ""
	pages := 0
	for {
		page, next, err := st.SearchContactsPage(ctx, tenantID, q, tag, limit, cursor)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		all = append(all, page...)
		if next == "" {
			return all, pages
		}
		if len(page) == 0 {
			t.Fatalf("empty page with non-empty next_cursor (page %d)", pages)
		}
		cursor = next
		if pages > 50 {
			t.Fatal("pagination did not terminate")
		}
	}
}

func TestSearchContactsPageWalkDuplicateNames(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()

	// 7 contacts, two sharing the name "Ada Obi" — the (name, id) keyset
	// must stay deterministic and complete across pages.
	names := []string{"Ada Obi", "Ada Obi", "Bola Ahmed", "Chidi Eze", "Dara Musa", "Efe Ojo", "Funke Alabi"}
	ids := map[string]bool{}
	for _, n := range names {
		ids[addContact(t, st, tenantID, n, "", "").String()] = true
	}
	_ = ctx

	all, pages := pagedNames(t, st, tenantID, "", "", 3)
	if pages != 3 || len(all) != 7 {
		t.Fatalf("pages=%d rows=%d, want 3 pages / 7 rows", pages, len(all))
	}
	seen := map[string]bool{}
	prevName, prevID := "", ""
	for i, r := range all {
		if seen[r.ID.String()] {
			t.Fatalf("duplicate row across pages: %v", r.ID)
		}
		seen[r.ID.String()] = true
		if i > 0 && (r.Name < prevName || (r.Name == prevName && r.ID.String() <= prevID)) {
			t.Fatalf("order broken at %d: (%q,%s) after (%q,%s)", i, r.Name, r.ID, prevName, prevID)
		}
		prevName, prevID = r.Name, r.ID.String()
	}
	for id := range ids {
		if !seen[id] {
			t.Fatalf("contact %s never returned", id)
		}
	}

	// Determinism: the same walk twice yields the identical id sequence.
	again, _ := pagedNames(t, st, tenantID, "", "", 3)
	for i := range all {
		if all[i].ID != again[i].ID {
			t.Fatalf("non-deterministic walk at %d", i)
		}
	}
}

func TestSearchContactsPageCursorValidation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	addContact(t, st, tenantID, "Ada Obi", "", "")

	bad := []string{
		"!!!not-base64!!!",
		base64.RawURLEncoding.EncodeToString([]byte("no-separator-here")),
		base64.RawURLEncoding.EncodeToString([]byte("name\x1fnot-a-uuid")),
		base64.RawURLEncoding.EncodeToString([]byte("\x1f")), // empty name + empty id
	}
	for _, c := range bad {
		if _, _, err := st.SearchContactsPage(ctx, tenantID, "", "", 10, c); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("cursor %q = %v, want ErrInvalidCursor", c, err)
		}
	}
	// Round-trip: encode -> decode returns the exact position.
	name, id := "Zuri Okonkwo", uuid.New()
	cur := encodeSearchCursor(name, id)
	gotName, gotID, err := decodeSearchCursor(cur)
	if err != nil || gotName != name || gotID != id {
		t.Fatalf("round-trip: (%q,%v,%v)", gotName, gotID, err)
	}
	// Names containing the separator byte cannot exist via encode, but a
	// hand-crafted one must still decode to the FIRST split only.
}

func TestSearchContactsPageFiltersAndEdges(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	vip1 := addContact(t, st, tenantID, "Ada Lovelace", "+234801", "")
	vip2 := addContact(t, st, tenantID, "Ada Byron", "+234802", "")
	_ = addContact(t, st, tenantID, "Ada Nonvip", "+234803", "")
	for _, id := range []uuid.UUID{vip1, vip2} {
		if _, err := st.AddTag(ctx, tenantID, id, "vip"); err != nil {
			t.Fatalf("tag: %v", err)
		}
	}

	// q + tag + cursor combined: 2 vip Adas, page size 1.
	all, pages := pagedNames(t, st, tenantID, "ada", "vip", 1)
	if pages != 2 || len(all) != 2 {
		t.Fatalf("q+tag pages=%d rows=%d, want 2/2", pages, len(all))
	}
	gotIDs := []uuid.UUID{all[0].ID, all[1].ID}
	wantIDs := []uuid.UUID{vip2, vip1} // "Ada Byron" < "Ada Lovelace"
	if gotIDs[0] != wantIDs[0] || gotIDs[1] != wantIDs[1] {
		t.Fatalf("q+tag order: %v want %v", gotIDs, wantIDs)
	}
	if len(all[0].Tags) != 1 || all[0].Tags[0] != "vip" {
		t.Fatalf("tags attached on paged row: %+v", all[0])
	}

	// Exact-multiple-of-limit: 2 rows, limit 2 -> next_cursor "".
	page, next, err := st.SearchContactsPage(ctx, tenantID, "ada", "vip", 2, "")
	if err != nil || len(page) != 2 || next != "" {
		t.Fatalf("exact-limit page: len=%d next=%q err=%v", len(page), next, err)
	}

	// Empty result set: first page, no cursor.
	page, next, err = st.SearchContactsPage(ctx, tenantID, "zzz-no-match", "", 10, "")
	if err != nil || len(page) != 0 || next != "" {
		t.Fatalf("empty: len=%d next=%q err=%v", len(page), next, err)
	}
}

func TestSearchContactsWrapperCompat(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	names := []string{"Ada Obi", "Bola Ahmed", "Chidi Eze", "Dara Musa"}
	for _, n := range names {
		addContact(t, st, tenantID, n, "", "")
	}
	// Pre-W83 behavior: single call, limit applied, (name,id) order.
	res, err := st.SearchContacts(ctx, tenantID, "", "", 2)
	if err != nil || len(res) != 2 || res[0].Name != "Ada Obi" || res[1].Name != "Bola Ahmed" {
		t.Fatalf("wrapper first page: %+v, %v", res, err)
	}
	res, err = st.SearchContacts(ctx, tenantID, "", "", 0) // default limit
	if err != nil || len(res) != 4 {
		t.Fatalf("wrapper default limit: %+v, %v", res, err)
	}
	if !sort.IsSorted(sort.StringSlice([]string{res[0].Name, res[1].Name, res[2].Name, res[3].Name})) {
		t.Fatalf("wrapper order: %+v", res)
	}
}

// Handler level: next_cursor key present only mid-walk; malformed cursor
// is a deterministic 400.
func TestSearchHandlerPagination(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme-w83"}
	r, st := testRouter(t, tenant)
	for _, n := range []string{"Ada Obi", "Bola Ahmed", "Chidi Eze"} {
		addContact(t, st, tenant.ID, n, "", "")
	}

	rec := do(t, r, tenant.Slug, http.MethodGet, "/v1/crm/contacts/search?limit=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("page 1 = %d (%s)", rec.Code, rec.Body.String())
	}
	env1 := decodeEnv[struct {
		Contacts []ContactSearchResult `json:"contacts"`
		Next     string                `json:"next_cursor"`
	}](t, rec)
	if len(env1.Contacts) != 2 || env1.Next == "" {
		t.Fatalf("page 1: %+v", env1)
	}
	rec = do(t, r, tenant.Slug, http.MethodGet, "/v1/crm/contacts/search?limit=2&cursor="+env1.Next, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("page 2 = %d (%s)", rec.Code, rec.Body.String())
	}
	env2 := decodeEnv[struct {
		Contacts []ContactSearchResult `json:"contacts"`
		Next     string                `json:"next_cursor"`
	}](t, rec)
	if len(env2.Contacts) != 1 || env2.Next != "" {
		t.Fatalf("page 2 (last): %+v", env2)
	}
	if env2.Contacts[0].ID == env1.Contacts[0].ID || env2.Contacts[0].ID == env1.Contacts[1].ID {
		t.Fatalf("page 2 repeats page 1: %+v vs %+v", env2, env1)
	}

	// Malformed cursor -> 400 (not a silent first page).
	rec = do(t, r, tenant.Slug, http.MethodGet, "/v1/crm/contacts/search?cursor=bogus!!!", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad cursor = %d (%s)", rec.Code, rec.Body.String())
	}
}
