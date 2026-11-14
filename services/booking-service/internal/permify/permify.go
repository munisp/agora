// Package permify implements the Authorizer interface against the Permify
// HTTP API v1 (REST instead of gRPC — documented deviation, see README).
package permify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

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
	// Permify v1.x answers "CHECK_RESULT_ALLOWED" (the older "RESULT_ALLOWED" is kept for compatibility).
	return out.Can == "CHECK_RESULT_ALLOWED" || out.Can == "RESULT_ALLOWED" || out.Can == "allowed", nil
}

// WriteRelationship implements RelationshipWriter via
// POST /v1/tenants/{t}/data/write (Permify v1.x).
// entity and subject are "type:id" references, relation is e.g. "owner".
func (c *HTTPClient) WriteRelationship(ctx context.Context, tenantID, entity, relation, subject string) error {
	et, ei := splitRef(entity)
	st, si := splitRef(subject)
	body := map[string]any{
		"metadata": map[string]any{"schema_version": ""},
		"tuples": []map[string]any{{
			"entity":   map[string]string{"type": et, "id": ei},
			"relation": relation,
			"subject":  map[string]string{"type": st, "id": si},
		}},
	}
	return c.post(ctx, "/v1/tenants/"+tenantID+"/data/write", body, nil)
}

// CreateTenant provisions a Permify tenant for relationship isolation.
func (c *HTTPClient) CreateTenant(ctx context.Context, tenantID, name string) error {
	body := map[string]any{"id": tenantID, "name": name}
	err := c.post(ctx, "/v1/tenants/create", body, nil)
	if err != nil && strings.Contains(err.Error(), "status 409") {
		return nil // already exists — idempotent
	}
	return err
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
