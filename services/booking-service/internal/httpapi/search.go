package httpapi

// SPEC-W119 §3.2: OpenSearch advanced-search HTTP surface for bookings,
// contacts and leads. Mirrors the W117 conversation-search contract:
//   - tenant comes from the request context (tenant middleware) — NEVER
//     from query params;
//   - nil Searcher -> every endpoint answers 503 search_unavailable
//     (fail-closed; there is deliberately no silent SQL fallback, which
//     would return different semantics under the same URL);
//   - unknown status/source/channel values pass through as term filters
//     (they simply match nothing) — not a 400;
//   - junk dates are a 400; limit clamps to [1,100]; limit+offset beyond
//     the OpenSearch result window (10000) is a 400.

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/opendesk/booking-service/internal/search"
)

// Searcher is the search backend contract (satisfied by *search.Handlers;
// faked in handler tests). Nil in Deps -> fail-closed 503.
type Searcher interface {
	SearchBookings(ctx context.Context, tenantID string, p search.Params) (search.Response, error)
	SearchContacts(ctx context.Context, tenantID string, p search.Params) (search.Response, error)
	SearchLeads(ctx context.Context, tenantID string, p search.Params) (search.Response, error)
}

// parseSearchTS accepts RFC3339 or a bare yyyy-mm-dd date (normalized to
// midnight UTC) and returns a canonical "2006-01-02T15:04:05Z" string.
// Anything else is an error (400 at the handler).
func parseSearchTS(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC().Format("2006-01-02T15:04:05Z"), nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.UTC().Format("2006-01-02T15:04:05Z"), nil
	}
	return "", strconv.ErrSyntax
}

// searchIntParam parses a non-negative int query param; absent -> def.
func searchIntParam(q interface{ Get(string) string }, name string, def int) (int, error) {
	raw := q.Get(name)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// serveSearch is the shared plumbing for all three entity handlers; call
// dispatches to the entity-specific backend method.
func (s *server) serveSearch(w http.ResponseWriter, r *http.Request, call func(ctx context.Context, tenantID string, p search.Params) (search.Response, error)) {
	if s.d.Searcher == nil {
		writeError(w, http.StatusServiceUnavailable, "search_unavailable")
		return
	}
	q := r.URL.Query()
	text := q.Get("q")
	if len(text) > 500 {
		writeError(w, http.StatusBadRequest, "q too long (max 500 chars)")
		return
	}
	status, source, channel := q.Get("status"), q.Get("source"), q.Get("channel")
	for _, v := range []string{status, source, channel} {
		if len(v) > 40 {
			writeError(w, http.StatusBadRequest, "filter value too long (max 40 chars)")
			return
		}
	}
	fromTS, err := parseSearchTS(q.Get("from_ts"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "from_ts must be ISO-8601 (RFC3339 or yyyy-mm-dd)")
		return
	}
	toTS, err := parseSearchTS(q.Get("to_ts"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "to_ts must be ISO-8601 (RFC3339 or yyyy-mm-dd)")
		return
	}
	limit, err := searchIntParam(q, "limit", search.DefaultLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "limit must be an integer")
		return
	}
	offset, err := searchIntParam(q, "offset", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "offset must be an integer")
		return
	}
	p := (search.Params{
		Q:       text,
		Status:  status,
		Source:  source,
		Channel: channel,
		FromTS:  fromTS,
		ToTS:    toTS,
		Limit:   limit,
		Offset:  offset,
	}).Clamp()
	if p.Limit+p.Offset > search.MaxWindow {
		writeError(w, http.StatusBadRequest, "limit+offset must be <= 10000 (result window)")
		return
	}
	tenant := tenantFrom(r.Context())
	resp, err := call(r.Context(), tenant.ID.String(), p)
	if err != nil {
		s.log().Error("entity search failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "search_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) log() *zap.Logger {
	if s.d.Logger != nil {
		return s.d.Logger
	}
	return zap.NewNop()
}

// NOTE: the call sites are closures, not s.d.Searcher.SearchX method
// values — evaluating a method value on a nil interface panics BEFORE
// serveSearch's fail-closed nil check can run.
func (s *server) searchBookings(w http.ResponseWriter, r *http.Request) {
	s.serveSearch(w, r, func(ctx context.Context, tenantID string, p search.Params) (search.Response, error) {
		return s.d.Searcher.SearchBookings(ctx, tenantID, p)
	})
}

func (s *server) searchContacts(w http.ResponseWriter, r *http.Request) {
	s.serveSearch(w, r, func(ctx context.Context, tenantID string, p search.Params) (search.Response, error) {
		return s.d.Searcher.SearchContacts(ctx, tenantID, p)
	})
}

func (s *server) searchLeads(w http.ResponseWriter, r *http.Request) {
	s.serveSearch(w, r, func(ctx context.Context, tenantID string, p search.Params) (search.Response, error) {
		return s.d.Searcher.SearchLeads(ctx, tenantID, p)
	})
}
