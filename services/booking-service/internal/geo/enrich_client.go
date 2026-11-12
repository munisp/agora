package geo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Geo-service enrichment client (SPEC-W52 Scope B): reverse point-in-polygon
// lookup of administrative boundaries via the geo-service (Scope A, terrio).
//
// Contract (A↔B, MUST stay in exact sync with services/geo-service):
//
//	GET {GEO_SERVICE_URL}/v1/geo/enrich?lat=<f>&lng=<f>
//	X-Tenant-Slug: <tenant uuid>   (the geo-service parses this header as a
//	                                uuid — src/auth.rs tenant_uuid — and sets
//	                                the RLS GUC app.tenant_id on the read tx,
//	                                so tenant-scoped custom boundaries
//	                                participate; a slug here silently degrades
//	                                the answer to reference boundaries only.
//	                                Empty/uuid.Nil → header omitted →
//	                                reference-boundaries-only, documented)
//	200 {"state":..., "lga":..., "ward":..., "boundaries":[{"kind","name","code"}]}
//
// A documents "200 with nulls" for a no-hit: scalar fields arrive as JSON
// null (decoding null into a Go string leaves the zero value) and
// `boundaries` as null/[] — both decode cleanly into EnrichResult.
//
// FAIL-OPEN BY CONTRACT: enrichment is a provenance nicety on top of a
// business write, never a gate. Any transport error, timeout, non-200 or
// undecodable body is logged and reported as nil — callers MUST treat nil
// as "no enrichment" and let the business write stand. Disabled entirely
// when GEO_SERVICE_URL is empty (NewEnrichClient("") → Enabled() == false).

// EnrichTimeout bounds the whole enrich call (SPEC-W52: 3s). Enrichment runs
// synchronously after the row commits; this bound keeps a slow geo-service
// from stretching the business request beyond a small constant.
const EnrichTimeout = 3 * time.Second

// EnrichBoundary is one matched boundary of the geo-service enrich response.
type EnrichBoundary struct {
	Kind string `json:"kind"` // state | lga | ward | custom
	Name string `json:"name"`
	Code string `json:"code"`
}

// EnrichResult mirrors the geo-service GET /v1/geo/enrich response body
// exactly (A↔B contract). Empty strings + empty Boundaries = no hit.
type EnrichResult struct {
	State      string           `json:"state"`
	LGA        string           `json:"lga"`
	Ward       string           `json:"ward"`
	Boundaries []EnrichBoundary `json:"boundaries"`
}

// EnrichClient is the fail-open HTTP client for the geo-service enrich
// endpoint. Construct with NewEnrichClient; the zero/nil value is disabled.
type EnrichClient struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger
}

// NewEnrichClient builds the client for a geo-service base URL
// (GEO_SERVICE_URL, e.g. "http://geo-service:7010"). An empty baseURL yields
// a DISABLED client: Enabled() is false and Enrich returns nil without any
// network traffic — the documented off posture.
func NewEnrichClient(baseURL string, log *zap.Logger) *EnrichClient {
	if log == nil {
		log = zap.NewNop()
	}
	return &EnrichClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		client:  &http.Client{Timeout: EnrichTimeout},
		log:     log,
	}
}

// Enabled reports whether enrichment is configured (GEO_SERVICE_URL set).
func (c *EnrichClient) Enabled() bool { return c != nil && c.baseURL != "" }

// Enrich resolves the administrative boundaries containing (lat, lng).
// tenantID is the tenant UUID; it is forwarded verbatim as X-Tenant-Slug —
// the geo-service's tenant-context header — where it is parsed as a uuid and
// feeds the RLS GUC app.tenant_id on the tenant-boundaries read
// (services/geo-service src/enrich.rs pass 2). An empty or uuid.Nil tenantID
// omits the header and the geo-service answers from platform reference
// boundaries only (documented degradation). Fail-open: nil on any error,
// timeout, non-200 or undecodable body — the cause is logged (without
// coordinates: a precise fix is person-adjacent data and does not belong in
// logs).
func (c *EnrichClient) Enrich(ctx context.Context, tenantID string, lat, lng float64) *EnrichResult {
	if !c.Enabled() {
		return nil
	}
	u := c.baseURL + "/v1/geo/enrich?lat=" + url.QueryEscape(strconv.FormatFloat(lat, 'f', -1, 64)) +
		"&lng=" + url.QueryEscape(strconv.FormatFloat(lng, 'f', -1, 64))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		c.log.Warn("geo enrich request build failed (fail-open)", zap.Error(err))
		return nil
	}
	if tenantID != "" && tenantID != uuid.Nil.String() {
		req.Header.Set("X-Tenant-Slug", tenantID)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		c.log.Warn("geo enrich call failed (fail-open)", zap.String("tenant_id", tenantID), zap.Error(err))
		return nil
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		c.log.Warn("geo enrich non-200 (fail-open)", zap.String("tenant_id", tenantID), zap.Int("status", resp.StatusCode))
		return nil
	}
	var res EnrichResult
	// Bound the body so a misbehaving peer cannot stream forever; 1 MiB is
	// orders of magnitude past any real boundary list for one point.
	dec := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20))
	if err := dec.Decode(&res); err != nil {
		c.log.Warn("geo enrich undecodable body (fail-open)", zap.String("tenant_id", tenantID), zap.Error(err))
		return nil
	}
	if res.Boundaries == nil {
		res.Boundaries = []EnrichBoundary{}
	}
	return &res
}

// String describes the client for boot logs (never logs coordinates).
func (c *EnrichClient) String() string {
	if !c.Enabled() {
		return "geo-enrich disabled (GEO_SERVICE_URL unset)"
	}
	return fmt.Sprintf("geo-enrich via %s", c.baseURL)
}
