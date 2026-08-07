// Package permify implements the Authorizer interface against the Permify
// HTTP API v1 (REST instead of gRPC — documented deviation, see README).
package permify

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Schema is the OpenDesk ReBAC model (SPEC §8), embedded from schema.perm
// in this package — a copy of the canonical infra/permify/schema.perm
// (permify_test.go asserts the two stay identical). SPEC-W34 GF15: every
// per-tenant Permify tenant must have this schema written to it —
// booking-service permission checks against /v1/tenants/{id}/permissions/
// check fail-closed (502) on schema-less tenants.
//
//go:embed schema.perm
var Schema string

// Authorizer abstracts permission checks so handlers are testable and the
// Permify backend can be swapped.
type Authorizer interface {
	// Check reports whether subject (e.g. "user:u1") is allowed permission
	// (e.g. "manage_bookings") on resource (e.g. "organization:{tenantID}").
	Check(ctx context.Context, tenantID, subject, permission, resource string) (bool, error)
}

// RelationshipWriter writes Permify relationship tuples.
type RelationshipWriter interface {
	WriteRelationship(ctx context.Context, tenantID, entity, relation, subject string) error
}

// HTTPClient is a Permify HTTP API v1 client.
type HTTPClient struct {
	baseURL string
	hc      *http.Client
}

// NewHTTPClient builds a client for e.g. http://permify:3476.
func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		hc:      &http.Client{Timeout: 10 * time.Second},
	}
}

func splitRef(ref string) (typ, id string) {
	parts := strings.SplitN(ref, ":", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return ref, ""
}

// Check implements Authorizer via POST /v1/tenants/{t}/permissions/check.
func (c *HTTPClient) Check(ctx context.Context, tenantID, subject, permission, resource string) (bool, error) {
	st, si := splitRef(subject)
	et, ei := splitRef(resource)
	body := map[string]any{
		"metadata":   map[string]any{"snap_token": "", "schema_version": "", "depth": 20},
		"entity":     map[string]string{"type": et, "id": ei},
		"permission": permission,
		"subject":    map[string]string{"type": st, "id": si},
	}
	var out struct {
		Can string `json:"can"`
	}
	if err := c.post(ctx, "/v1/tenants/"+tenantID+"/permissions/check", body, &out); err != nil {
		return false, err
	}
	return out.Can == "RESULT_ALLOWED" || out.Can == "allowed", nil
}

// WriteRelationship implements RelationshipWriter via
// POST /v1/tenants/{t}/data/relationships/write.
// entity and subject are "type:id" references, relation is e.g. "owner".
func (c *HTTPClient) WriteRelationship(ctx context.Context, tenantID, entity, relation, subject string) error {
	et, ei := splitRef(entity)
	st, si := splitRef(subject)
	body := map[string]any{
		"metadata": map[string]any{"schema_version": ""},
		"tuple": map[string]any{
			"entity":   map[string]string{"type": et, "id": ei},
			"relation": relation,
			"subject":  map[string]string{"type": st, "id": si},
		},
	}
	return c.post(ctx, "/v1/tenants/"+tenantID+"/data/relationships/write", body, nil)
}

// CreateTenant provisions a Permify tenant for relationship isolation and
// writes the OpenDesk ReBAC schema to it (SPEC-W34 GF15 — a tenant without
// the schema fail-closes every booking-service permission check with 502).
// Fail-closed: a schema write error is returned to the caller.
func (c *HTTPClient) CreateTenant(ctx context.Context, tenantID, name string) error {
	body := map[string]any{"id": tenantID, "name": name}
	err := c.post(ctx, "/v1/tenants/create", body, nil)
	if err != nil && !strings.Contains(err.Error(), "status 409") {
		return err
	}
	// 409 = already exists — idempotent. The schema is (re)written on every
	// call so tenants provisioned before GF15 self-heal on the next
	// ensure-permify / onboarding call (schemas/write versions are additive).
	if err := c.WriteSchema(ctx, tenantID); err != nil {
		return fmt.Errorf("write schema for tenant %s: %w", tenantID, err)
	}
	return nil
}

// WriteSchema posts the embedded ReBAC schema to
// POST /v1/tenants/{t}/schemas/write (same base-path convention as Check
// and WriteRelationship; payload shape matches infra/permify/load-schema.sh).
func (c *HTTPClient) WriteSchema(ctx context.Context, tenantID string) error {
	body := map[string]any{"schema": Schema}
	return c.post(ctx, "/v1/tenants/"+tenantID+"/schemas/write", body, nil)
}

func (c *HTTPClient) post(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("permify %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("permify %s: status %d: %s", path, resp.StatusCode, string(rb))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			return fmt.Errorf("permify %s: decode: %w", path, err)
		}
	}
	return nil
}
