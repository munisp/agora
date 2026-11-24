package search

// SPEC-W119 §5: query-builder table tests — pure, no cluster. The tenant_id
// term filter being the FIRST bool filter clause is asserted structurally
// (it is the isolation boundary).

import (
	"testing"
)

func boolClause(t *testing.T, q map[string]any) map[string]any {
	t.Helper()
	query, ok := q["query"].(map[string]any)
	if !ok {
		t.Fatalf("query missing: %#v", q)
	}
	boolQ, ok := query["bool"].(map[string]any)
	if !ok {
		t.Fatalf("bool missing: %#v", query)
	}
	return boolQ
}

func filters(t *testing.T, q map[string]any) []any {
	t.Helper()
	f, ok := boolClause(t, q)["filter"].([]any)
	if !ok || len(f) == 0 {
		t.Fatalf("filters missing/empty: %#v", q)
	}
	return f
}

// assertTenantFirst is THE structural isolation assertion.
func assertTenantFirst(t *testing.T, q map[string]any, tenantID string) {
	t.Helper()
	first, ok := filters(t, q)[0].(map[string]any)
	if !ok {
		t.Fatalf("first filter not an object")
	}
	term, ok := first["term"].(map[string]any)
	if !ok {
		t.Fatalf("first filter not a term: %#v", first)
	}
	if term["tenant_id"] != tenantID {
		t.Fatalf("first filter term = %#v, want tenant_id=%s", term, tenantID)
	}
}

func TestBuildBookingQueryTenantFirstAndFilters(t *testing.T) {
	q := BuildBookingQuery("t-1", Params{
		Q: "ada", Status: "confirmed", Source: "web",
		FromTS: "2026-01-01T00:00:00Z", ToTS: "2026-01-31T23:59:59Z",
		Limit: 50, Offset: 10,
	})
	assertTenantFirst(t, q, "t-1")
	fs := filters(t, q)
	if len(fs) != 4 { // tenant + status + source + range
		t.Fatalf("filters = %d, want 4: %#v", len(fs), fs)
	}
	if q["size"] != 50 || q["from"] != 10 {
		t.Fatalf("size/from = %v/%v", q["size"], q["from"])
	}
	if q["track_total_hits"] != true {
		t.Fatalf("track_total_hits missing")
	}
	must := boolClause(t, q)["must"].([]any)
	mm, ok := must[0].(map[string]any)["multi_match"].(map[string]any)
	if !ok {
		t.Fatalf("want multi_match must: %#v", must[0])
	}
	if mm["operator"] != "and" || mm["fuzziness"] != "AUTO" {
		t.Fatalf("multi_match opts = %#v", mm)
	}
	fields := mm["fields"].([]string)
	if len(fields) != 2 || fields[0] != "contact_name" || fields[1] != "public_ref" {
		t.Fatalf("fields = %#v", fields)
	}
}

func TestBuildBookingQueryDefaultsAndMatchAll(t *testing.T) {
	q := BuildBookingQuery("t-2", Params{})
	assertTenantFirst(t, q, "t-2")
	must := boolClause(t, q)["must"].([]any)
	if _, ok := must[0].(map[string]any)["match_all"]; !ok {
		t.Fatalf("want match_all: %#v", must[0])
	}
	if q["size"] != 1 { // Clamp: 0 -> 1
		t.Fatalf("size = %v, want 1 (clamped)", q["size"])
	}
	sort := q["sort"].([]any)
	if len(sort) != 2 {
		t.Fatalf("sort = %#v", sort)
	}
}

func TestBuildContactQueryHasNoDateRange(t *testing.T) {
	// Even if the caller passes dates, contacts have no timestamp columns —
	// no range clause may be emitted.
	q := BuildContactQuery("t-3", Params{
		Source: "crm", FromTS: "2026-01-01T00:00:00Z", Limit: 200,
	})
	assertTenantFirst(t, q, "t-3")
	fs := filters(t, q)
	if len(fs) != 2 { // tenant + source only
		t.Fatalf("filters = %d, want 2: %#v", len(fs), fs)
	}
	for _, f := range fs {
		if _, isRange := f.(map[string]any)["range"]; isRange {
			t.Fatalf("contacts must never emit a range filter: %#v", fs)
		}
	}
	if q["size"] != MaxLimit { // 200 clamped
		t.Fatalf("size = %v, want %d", q["size"], MaxLimit)
	}
	sort := q["sort"].([]any)
	first := sort[0].(map[string]any)
	if _, ok := first["name.keyword"]; !ok {
		t.Fatalf("contacts sort = %#v", sort)
	}
}

func TestBuildLeadQueryMapsChannelAndRange(t *testing.T) {
	q := BuildLeadQuery("t-4", Params{
		Q: "+23480", Status: "new", Channel: "whatsapp",
		FromTS: "2026-02-01T00:00:00Z",
		Limit:  25, Offset: 25,
	})
	assertTenantFirst(t, q, "t-4")
	fs := filters(t, q)
	if len(fs) != 4 { // tenant + status + channel_of_first_touch + range
		t.Fatalf("filters = %d, want 4: %#v", len(fs), fs)
	}
	term, ok := fs[2].(map[string]any)["term"].(map[string]any)
	if !ok || term["channel_of_first_touch"] != "whatsapp" {
		t.Fatalf("channel term = %#v", fs[2])
	}
	rng, ok := fs[3].(map[string]any)["range"].(map[string]any)
	if !ok {
		t.Fatalf("range missing: %#v", fs[3])
	}
	created := rng["created_at"].(map[string]any)
	if created["gte"] != "2026-02-01T00:00:00Z" {
		t.Fatalf("range = %#v", created)
	}
}

func TestUnknownFilterValuesPassThrough(t *testing.T) {
	// SPEC-W119 §3.2: unknown status/source/channel values become term
	// filters (matching nothing) — never rejected by the builder.
	q := BuildBookingQuery("t-5", Params{Status: "bogus", Limit: 5})
	fs := filters(t, q)
	term := fs[1].(map[string]any)["term"].(map[string]any)
	if term["status"] != "bogus" {
		t.Fatalf("status term = %#v", term)
	}
}

func TestClampBounds(t *testing.T) {
	p := Params{Limit: 0, Offset: -3}.Clamp()
	if p.Limit != 1 || p.Offset != 0 {
		t.Fatalf("clamp low = %#v", p)
	}
	p = Params{Limit: MaxLimit + 50}.Clamp()
	if p.Limit != MaxLimit {
		t.Fatalf("clamp high = %#v", p)
	}
}
