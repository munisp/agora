// Filter encoding for the Twenty REST API (SPEC-CRM §B).
// Twenty expects filters as `?filter=<field>[<op>]:<value>` e.g.
//
//	/rest/people?filter=emails.primaryEmail[eq]:"jane@example.com"
//
// Values are quoted JSON strings; the whole expression is URL-encoded.
package twentyc

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// FilterOp is a Twenty REST filter operator. Only the operators we use are
// declared; see https://twenty.com/developers for the full list.
type FilterOp string

const (
	OpEq    FilterOp = "eq"
	OpIn    FilterOp = "in"
	OpIlike FilterOp = "ilike"
)

// Expr renders one filter expression: field[op]:"value".
func Expr(field string, op FilterOp, value string) string {
	return fmt.Sprintf("%s[%s]:%s", field, op, strconv.Quote(value))
}

// AndExpr ANDs several expressions with Twenty's `and(...)` combinator.
func AndExpr(exprs ...string) string {
	if len(exprs) == 1 {
		return exprs[0]
	}
	return "and(" + strings.Join(exprs, ",") + ")"
}

// Query returns url.Values carrying the encoded filter (and optional limit).
func Query(filter string, limit int) url.Values {
	q := url.Values{}
	if filter != "" {
		q.Set("filter", filter)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return q
}

// PersonEmailFilter matches a Person by primary email.
func PersonEmailFilter(email string) string {
	return Expr("emails.primaryEmail", OpEq, email)
}

// PersonPhoneFilter matches a Person by primary phone number.
func PersonPhoneFilter(phone string) string {
	national, _, _ := NationalPhone(phone) // stored national, see NationalPhone
	return Expr("phones.primaryPhoneNumber", OpEq, national)
}

// CompanyNameFilter matches a Company by exact name.
func CompanyNameFilter(name string) string {
	return Expr("name", OpEq, name)
}
