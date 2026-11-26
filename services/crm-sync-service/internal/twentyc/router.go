package twentyc

import (
	"strings"

	"github.com/google/uuid"
)

// Router picks the Twenty instance a business's records belong to (2026-11-23).
//
// One Twenty workspace for every business let each client's staff see every
// other client's customers. A business can now have its own Twenty instance:
// its tenant id (and slug) map to that instance's client, and the instance's
// workspace id maps back to it for webhooks coming the other way. Businesses
// without a route keep using the default (shared) instance, so nothing changes
// for them.
type Router struct {
	Default     *Client
	byTenant    map[string]*Client
	byWorkspace map[string]*Client
	all         []*Client
	fields      map[string]*PersonFields // tenant id/slug -> the business's own CRM fields (2026-11-23)
}

// NewRouter returns a router that sends everything to def until routes are added.
func NewRouter(def *Client) *Router {
	r := &Router{Default: def, byTenant: map[string]*Client{}, byWorkspace: map[string]*Client{}}
	if def != nil {
		r.all = append(r.all, def)
	}
	return r
}

// SetFields records which CRM fields the given tenants' contacts carry (TWENTY_ROUTES "person_fields").
func (r *Router) SetFields(tenants []string, f *PersonFields) {
	if f == nil {
		return
	}
	if r.fields == nil {
		r.fields = map[string]*PersonFields{}
	}
	for _, t := range tenants {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			r.fields[t] = f
		}
	}
}

// FieldsFor returns the tenant's CRM field mapping, or nil (no business-specific fields).
func (r *Router) FieldsFor(tenantID *uuid.UUID) *PersonFields {
	if r == nil || tenantID == nil || r.fields == nil {
		return nil
	}
	return r.fields[strings.ToLower(tenantID.String())]
}

// Add routes the given tenant ids/slugs (and the instance's workspace id, when known) to c.
func (r *Router) Add(tenants []string, workspaceID string, c *Client) {
	for _, t := range tenants {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			r.byTenant[t] = c
		}
	}
	if ws := strings.TrimSpace(workspaceID); ws != "" {
		r.byWorkspace[ws] = c
	}
	r.all = append(r.all, c)
}

// ForTenant returns the client for a tenant id (nil or unrouted: the default).
func (r *Router) ForTenant(tenantID *uuid.UUID) *Client {
	if r == nil {
		return nil
	}
	if tenantID != nil {
		if c, ok := r.byTenant[tenantID.String()]; ok {
			return c
		}
	}
	return r.Default
}

// ForTenantKey returns the client for a tenant id or slug given as text ("" or unrouted: the default).
func (r *Router) ForTenantKey(key string) *Client {
	if r == nil {
		return nil
	}
	if c, ok := r.byTenant[strings.ToLower(strings.TrimSpace(key))]; ok {
		return c
	}
	return r.Default
}

// ForWorkspace returns the client whose instance owns a webhook's workspace id ("" or unknown: the default).
func (r *Router) ForWorkspace(workspaceID string) *Client {
	if r == nil {
		return nil
	}
	if c, ok := r.byWorkspace[strings.TrimSpace(workspaceID)]; ok {
		return c
	}
	return r.Default
}

// All returns every instance's client, the default first (used by the legacy erase fallback).
func (r *Router) All() []*Client {
	if r == nil {
		return nil
	}
	return r.all
}
