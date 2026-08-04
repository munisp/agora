package campaignstudio

// Segment definition evaluation (SPEC-W19 Agent D).
//
// Two evaluators share the SAME whitelist + operator semantics:
//
//   - buildSegmentQuery compiles a definition to a parameterized SQL
//     predicate over booking.contacts (lead_* fields via an EXISTS
//     subquery on booking.leads joined by phone — leads carry no contact
//     FK). Used by POST /segments/{id}/count (read-only, RLS-safe: it
//     runs inside withTenant and always filters tenant_id=$1).
//   - EvaluateCondition evaluates a definition in Go against one
//     contact's attributes (branch steps). Semantics mirror the SQL
//     builder (contains is case-insensitive both sides; gte/lte compare
//     numerically when both sides parse as floats, else as strings —
//     documented approximation for timestamps).
//
// PERFORMANCE CEILING (documented): segment counts scan at most
// segmentCountRowCeiling (100k) contacts per evaluation — the count runs
// over a LIMIT-ed subquery and reports truncated=true when the ceiling
// is hit. Larger audiences must be narrowed with filters.

import (
	"fmt"
	"strconv"
	"strings"
)

// segmentCountRowCeiling is the documented 100k-row scan ceiling for one
// segment count evaluation.
const segmentCountRowCeiling = 100000

// contactFieldSQL maps whitelisted contact fields to SQL expressions on
// the contacts alias c. Nullable text columns are COALESCEd so eq/neq/
// contains behave predictably on NULLs.
var contactFieldSQL = map[string]string{
	FieldName:       "c.name",
	FieldPhone:      "COALESCE(c.phone,'')",
	FieldEmail:      "COALESCE(c.email,'')",
	FieldSource:     "COALESCE(c.source,'')",
	FieldExternalID: "COALESCE(c.external_id,'')",
}

// leadFieldSQL maps whitelisted lead fields to SQL expressions on the
// leads alias l (inside the EXISTS subquery).
var leadFieldSQL = map[string]string{
	FieldLeadStatus:     "l.status",
	FieldLeadChannel:    "l.channel_of_first_touch",
	FieldLeadCampaignID: "COALESCE(l.campaign_id::text,'')",
	FieldLeadCreatedAt:  "l.created_at",
}

// buildSegmentQuery assembles the WHERE fragment (AND-joined predicates)
// plus bound args for a validated definition. $1 is reserved for
// tenant_id; filter placeholders start at $2. Values are ALWAYS bound
// parameters — never interpolated — and every field re-checked against
// the whitelist so a definition can never inject SQL.
func buildSegmentQuery(def *SegmentDefinition) (string, []any, error) {
	if def == nil || len(def.Filters) == 0 {
		return "", nil, fmt.Errorf("%w: segment definition has no filters", ErrInvalidInput)
	}
	preds := make([]string, 0, len(def.Filters))
	args := make([]any, 0, len(def.Filters))
	for i, f := range def.Filters {
		pos := i + 2
		pred, err := buildPredicate(f, pos)
		if err != nil {
			return "", nil, err
		}
		arg, err := bindArg(f)
		if err != nil {
			return "", nil, err
		}
		preds = append(preds, pred)
		args = append(args, arg)
	}
	return strings.Join(preds, " AND "), args, nil
}

// buildPredicate renders one filter. Contact fields become direct
// predicates; lead fields become an EXISTS subquery over leads joined on
// phone (l.tenant_id=$1 reuses the outer tenant binding).
func buildPredicate(f SegmentFilter, pos int) (string, error) {
	ph := fmt.Sprintf("$%d", pos)
	if expr, ok := contactFieldSQL[f.Field]; ok {
		return renderPredicate(expr, f.Op, ph, false)
	}
	if expr, ok := leadFieldSQL[f.Field]; ok {
		inner, err := renderPredicate(expr, f.Op, ph, f.Field == FieldLeadCreatedAt)
		if err != nil {
			return "", err
		}
		return `EXISTS (SELECT 1 FROM leads l WHERE l.tenant_id=$1 AND c.phone IS NOT NULL AND l.phone_e164=c.phone AND ` + inner + `)`, nil
	}
	return "", fmt.Errorf("%w: unsupported filter field %q", ErrInvalidInput, f.Field)
}

// renderPredicate maps an operator to SQL. timeField casts the bound
// value to timestamptz for ordering comparisons.
func renderPredicate(expr, op, ph string, timeField bool) (string, error) {
	if timeField {
		ph = ph + "::timestamptz"
	}
	switch op {
	case OpEq:
		return expr + " = " + ph, nil
	case OpNeq:
		return expr + " <> " + ph, nil
	case OpGte:
		return expr + " >= " + ph, nil
	case OpLte:
		return expr + " <= " + ph, nil
	case OpContains:
		return expr + " ILIKE '%' || " + ph + " || '%'", nil
	case OpIn:
		return expr + " = ANY(" + ph + ")", nil
	}
	return "", fmt.Errorf("%w: unsupported op %q", ErrInvalidInput, op)
}

// bindArg produces the bound value for a validated filter: scalars are
// stringified (the SQL casts back where needed); op in requires a
// non-empty string array (pgx encodes []string to text[]).
func bindArg(f SegmentFilter) (any, error) {
	if f.Op == OpIn {
		vals, ok := toStringSlice(f.Value)
		if !ok || len(vals) == 0 {
			return nil, fmt.Errorf("%w: op in requires a non-empty string array", ErrInvalidInput)
		}
		return vals, nil
	}
	s, ok := scalarString(f.Value)
	if !ok {
		return nil, fmt.Errorf("%w: unsupported filter value type %T", ErrInvalidInput, f.Value)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Go-side condition evaluation (branch steps)
// ---------------------------------------------------------------------------

// ContactAttrs is the attribute view of one contact (+ its latest lead)
// that branch conditions evaluate against. Keys are the whitelisted
// filter fields.
type ContactAttrs map[string]string

// EvaluateCondition reports whether attrs satisfy EVERY filter of def
// (AND semantics, mirroring buildSegmentQuery). Semantics:
//
//	eq/neq   string equality
//	in       membership in a string array
//	gte/lte  numeric comparison when BOTH sides parse as floats, else
//	         lexicographic (documented approximation — RFC3339 timestamps
//	         compare correctly lexicographically)
//	contains case-insensitive substring (mirrors ILIKE)
func EvaluateCondition(def *SegmentDefinition, attrs ContactAttrs) bool {
	if def == nil {
		return false
	}
	for _, f := range def.Filters {
		if !evalFilter(f, attrs[f.Field]) {
			return false
		}
	}
	return true
}

func evalFilter(f SegmentFilter, got string) bool {
	switch f.Op {
	case OpEq:
		want, ok := scalarString(f.Value)
		return ok && got == want
	case OpNeq:
		want, ok := scalarString(f.Value)
		return ok && got != want
	case OpIn:
		vals, ok := toStringSlice(f.Value)
		if !ok {
			return false
		}
		for _, v := range vals {
			if got == v {
				return true
			}
		}
		return false
	case OpGte, OpLte:
		want, ok := scalarString(f.Value)
		if !ok {
			return false
		}
		cmp := compareValues(got, want)
		if f.Op == OpGte {
			return cmp >= 0
		}
		return cmp <= 0
	case OpContains:
		want, ok := scalarString(f.Value)
		return ok && strings.Contains(strings.ToLower(got), strings.ToLower(want))
	}
	return false
}

func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

// compareValues orders two scalar strings: numerically when both parse as
// floats, else lexicographically.
func compareValues(a, b string) int {
	fa, errA := strconv.ParseFloat(a, 64)
	fb, errB := strconv.ParseFloat(b, 64)
	if errA == nil && errB == nil {
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		}
		return 0
	}
	return strings.Compare(a, b)
}
