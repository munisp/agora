// Package search — SPEC-W119: OpenSearch-backed advanced search for the
// bookings, contacts and leads admin surfaces, served by booking-service.
//
// Contract mirrored from W117 (conversation-service app/search.py):
//   - the tenant_id term filter is ALWAYS the first bool filter clause —
//     that is the isolation boundary and must never be conditional;
//   - optional term filters pass through verbatim (unknown values simply
//     match nothing — consistent with the list endpoints, never a 400);
//   - q becomes a match with operator=and + fuzziness=AUTO over the entity
//     text fields (multi_match when there is more than one), else match_all;
//   - size/from with limit clamped to [1, MaxLimit]; the caller (httpapi)
//     enforces limit+offset <= MaxWindow with a 400;
//   - track_total_hits=true so the panel can page honestly.
//
// Query builders are pure — unit-tested without a cluster.
package search

import "strings"

const (
	// MaxLimit mirrors OpenSearch index.max_result_window paging practice.
	MaxLimit = 100
	// MaxWindow is the default OpenSearch result window; the HTTP layer
	// rejects limit+offset beyond it with a 400.
	MaxWindow = 10000
	// DefaultLimit is the page size when the caller passes no limit.
	DefaultLimit = 25
)

// Params is the normalized filter set for all three entity searches. Not
// every entity uses every field: contacts have no date range (the table
// carries no timestamp columns); bookings ignore Channel; leads ignore
// Source.
type Params struct {
	Q       string
	Status  string
	Source  string
	Channel string
	FromTS  string // normalized RFC3339 UTC (httpapi.parseSearchTS) or ""
	ToTS    string // normalized RFC3339 UTC or ""
	Limit   int
	Offset  int
}

// Clamp bounds Limit to [1, MaxLimit] and Offset to >= 0. The result-window
// rule (Limit+Offset <= MaxWindow) stays with the HTTP layer because it is
// a 400, not a silent clamp.
func (p Params) Clamp() Params {
	if p.Limit < 1 {
		p.Limit = 1
	}
	if p.Limit > MaxLimit {
		p.Limit = MaxLimit
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	return p
}

// termFilter renders one {"term": {field: value}} clause.
func termFilter(field, value string) map[string]any {
	return map[string]any{"term": map[string]any{field: value}}
}

// sortSpec renders one {"field": {"order": ord}} sort clause.
func sortSpec(field, ord string) map[string]any {
	return map[string]any{field: map[string]any{"order": ord}}
}

// buildQuery is the shared builder. termFilters are (field, value) pairs —
// empty values are skipped. rangeField "" disables the date range clause
// (contacts). sort must be pre-built per entity.
func buildQuery(tenantID string, termFilters [][2]string, textFields []string, rangeField string, p Params, sort []any, source []string) map[string]any {
	p = p.Clamp()
	// The tenant_id term filter is ALWAYS the first filter clause.
	filters := []any{termFilter("tenant_id", tenantID)}
	for _, tf := range termFilters {
		if tf[1] != "" {
			filters = append(filters, termFilter(tf[0], tf[1]))
		}
	}
	if rangeField != "" && (p.FromTS != "" || p.ToTS != "") {
		rng := map[string]any{}
		if p.FromTS != "" {
			rng["gte"] = p.FromTS
		}
		if p.ToTS != "" {
			rng["lte"] = p.ToTS
		}
		filters = append(filters, map[string]any{"range": map[string]any{rangeField: rng}})
	}

	var must []any
	if q := strings.TrimSpace(p.Q); q != "" {
		if len(textFields) == 1 {
			must = []any{map[string]any{"match": map[string]any{textFields[0]: map[string]any{
				"query": q, "operator": "and", "fuzziness": "AUTO",
			}}}}
		} else {
			must = []any{map[string]any{"multi_match": map[string]any{
				"query": q, "fields": textFields, "operator": "and", "fuzziness": "AUTO",
			}}}
		}
	} else {
		must = []any{map[string]any{"match_all": map[string]any{}}}
	}

	return map[string]any{
		"size":             p.Limit,
		"from":             p.Offset,
		"track_total_hits": true,
		"sort":             sort,
		"query": map[string]any{"bool": map[string]any{
			"filter": filters,
			"must":   must,
		}},
		"_source": source,
	}
}

// bookingSource is the _source projection for the bookings index (mirrors
// the bookings index template in infra/opensearch/setup-indices.sh).
var bookingSource = []string{
	"booking_id", "public_ref", "status", "source",
	"contact_name", "contact_phone", "offering_id", "team_member_id",
	"starts_at", "ends_at", "created_at", "updated_at",
}

// BuildBookingQuery: terms status + source, range starts_at, text over
// contact_name + public_ref; newest-start first, _doc tiebreak.
func BuildBookingQuery(tenantID string, p Params) map[string]any {
	return buildQuery(tenantID,
		[][2]string{{"status", p.Status}, {"source", p.Source}},
		[]string{"contact_name", "public_ref"},
		"starts_at", p,
		[]any{sortSpec("starts_at", "desc"), sortSpec("_doc", "desc")},
		bookingSource)
}

// contactSource is the _source projection for the contacts index.
var contactSource = []string{
	"contact_id", "name", "phone", "email", "notes",
	"source", "external_id", "synced_at",
}

// BuildContactQuery: term source, NO date range (the contacts table has no
// timestamp columns), text over name + email + notes; alphabetical by
// name.keyword with contact_id tiebreak (no timestamp to sort on).
func BuildContactQuery(tenantID string, p Params) map[string]any {
	return buildQuery(tenantID,
		[][2]string{{"source", p.Source}},
		[]string{"name", "email", "notes"},
		"", p,
		[]any{sortSpec("name.keyword", "asc"), sortSpec("contact_id", "asc")},
		contactSource)
}

// leadSource is the _source projection for the leads index.
var leadSource = []string{
	"lead_id", "phone_e164", "channel_of_first_touch", "status",
	"campaign_id", "promo_code", "created_at", "updated_at",
}

// BuildLeadQuery: terms status + channel_of_first_touch (the HTTP param is
// "channel"), range created_at, text over phone_e164 + promo_code;
// newest-captured first, _doc tiebreak.
func BuildLeadQuery(tenantID string, p Params) map[string]any {
	return buildQuery(tenantID,
		[][2]string{{"status", p.Status}, {"channel_of_first_touch", p.Channel}},
		[]string{"phone_e164", "promo_code"},
		"created_at", p,
		[]any{sortSpec("created_at", "desc"), sortSpec("_doc", "desc")},
		leadSource)
}
