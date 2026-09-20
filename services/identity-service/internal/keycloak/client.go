// Package keycloak implements a thin Keycloak Admin REST client.
// It obtains an access token via the client_credentials grant and uses it to
// manage tenant groups (/tenants/{slug}) and invited users.
package keycloak

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrUserExists is returned by CreateUser when Keycloak reports a conflict
// (HTTP 409 — username/e-mail already registered). Callers map this to a
// domain-level response (SPEC-W45 K16/STK O3: re-invite → 409 resend
// semantics instead of a 502 identity-provider error).
var ErrUserExists = errors.New("keycloak user already exists")

// Client is a Keycloak Admin REST client scoped to one realm.
type Client struct {
	baseURL      string
	realm        string
	clientID     string
	clientSecret string
	hc           *http.Client

	mu        sync.Mutex
	token     string
	tokenExpr time.Time
}

// New constructs the client.
func New(baseURL, realm, clientID, clientSecret string) *Client {
	return &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		realm:        realm,
		clientID:     clientID,
		clientSecret: clientSecret,
		hc:           &http.Client{Timeout: 15 * time.Second},
	}
}

// accessToken returns a cached client_credentials token, refreshing it when
// expired (with a 30s safety margin).
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExpr) {
		return c.token, nil
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
	}
	u := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", c.baseURL, c.realm)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("keycloak token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("keycloak token: status %d: %s", resp.StatusCode, string(b))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	c.token = tok.AccessToken
	c.tokenExpr = time.Now().Add(time.Duration(tok.ExpiresIn-30) * time.Second)
	return c.token, nil
}

// do performs an authenticated admin request and returns the response.
func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.hc.Do(req)
}

type groupRep struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}

// CreateTenantGroup ensures the group path /tenants/{slug} exists (SPEC §8:
// groups map to tenants). It creates the parent "tenants" group when missing.
// Returns the leaf group's ID.
func (c *Client) CreateTenantGroup(ctx context.Context, slug string) (string, error) {
	adminBase := "/admin/realms/" + c.realm

	// find-or-create the "tenants" top-level group
	tenantsID, err := c.findGroupByPath(ctx, "/tenants")
	if err != nil {
		return "", err
	}
	if tenantsID == "" {
		resp, err := c.do(ctx, http.MethodPost, adminBase+"/groups", groupRep{Name: "tenants"})
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return "", fmt.Errorf("create tenants group: status %d: %s", resp.StatusCode, string(b))
		}
		tenantsID, err = c.findGroupByPath(ctx, "/tenants")
		if err != nil || tenantsID == "" {
			return "", fmt.Errorf("tenants group lookup after create failed: %v", err)
		}
	}

	// find-or-create /tenants/{slug}
	path := "/tenants/" + slug
	id, err := c.findGroupByPath(ctx, path)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	resp, err := c.do(ctx, http.MethodPost, adminBase+"/groups/"+tenantsID+"/children", groupRep{Name: slug})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("create tenant group %s: status %d: %s", path, resp.StatusCode, string(b))
	}
	id, err = c.findGroupByPath(ctx, path)
	if err != nil || id == "" {
		return "", fmt.Errorf("tenant group lookup after create failed: %v", err)
	}
	return id, nil
}

// findGroupByPath returns the group ID for an exact path or "" if absent.
func (c *Client) findGroupByPath(ctx context.Context, path string) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/admin/realms/"+c.realm+"/group-by-path/"+path, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("group-by-path %s: status %d: %s", path, resp.StatusCode, string(b))
	}
	var g struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		return "", fmt.Errorf("decode group: %w", err)
	}
	return g.ID, nil
}

// CreateUserInput describes an invited member.
type CreateUserInput struct {
	Email     string
	FirstName string
	LastName  string
}

// CreateUser creates a Keycloak user with a temporary password-less invite
// (UPDATE_PASSWORD required action sends setup email flows in the realm) and
// adds the user to the tenant group /tenants/{slug}. Returns the user ID.
func (c *Client) CreateUser(ctx context.Context, slug string, in CreateUserInput) (string, error) {
	adminBase := "/admin/realms/" + c.realm
	rep := map[string]any{
		"username":        in.Email,
		"email":           in.Email,
		"firstName":       in.FirstName,
		"lastName":        in.LastName,
		"enabled":         true,
		"emailVerified":   false,
		"requiredActions": []string{"UPDATE_PASSWORD", "VERIFY_EMAIL"},
	}
	resp, err := c.do(ctx, http.MethodPost, adminBase+"/users", rep)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return "", fmt.Errorf("%w: %s", ErrUserExists, in.Email)
	}
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("create user: status %d: %s", resp.StatusCode, string(b))
	}
	loc := resp.Header.Get("Location")
	userID := loc[strings.LastIndex(loc, "/")+1:]
	if userID == "" {
		return "", fmt.Errorf("create user: missing Location header")
	}

	groupID, err := c.findGroupByPath(ctx, "/tenants/"+slug)
	if err != nil {
		return "", err
	}
	if groupID == "" {
		return "", fmt.Errorf("tenant group /tenants/%s not found", slug)
	}
	jresp, err := c.do(ctx, http.MethodPut, adminBase+"/users/"+userID+"/groups/"+groupID, nil)
	if err != nil {
		return "", err
	}
	defer jresp.Body.Close()
	if jresp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(jresp.Body, 512))
		return "", fmt.Errorf("join group: status %d: %s", jresp.StatusCode, string(b))
	}
	return userID, nil
}

// expectStatus reads + closes resp and errors unless the status is one of the
// accepted codes (Keycloak admin writes answer 204; a few answer 200).
func expectStatus(resp *http.Response, op string, accepted ...int) error {
	defer resp.Body.Close()
	for _, code := range accepted {
		if resp.StatusCode == code {
			return nil
		}
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("%s: status %d: %s", op, resp.StatusCode, string(b))
}

// SendExecuteActionsEmail asks Keycloak to e-mail the user its required
// actions (SPEC-W45 K8: UPDATE_PASSWORD + VERIFY_EMAIL after an invite so
// Keycloak's own credentials e-mail fires when realm SMTP is live). Fails
// when the realm has no SMTP configured — callers treat this as fail-soft
// (the MemberInvited notification-worker path is the primary rail).
func (c *Client) SendExecuteActionsEmail(ctx context.Context, userID string, actions []string) error {
	path := "/admin/realms/" + c.realm + "/users/" + userID + "/execute-actions-email"
	resp, err := c.do(ctx, http.MethodPut, path, actions)
	if err != nil {
		return err
	}
	return expectStatus(resp, "execute-actions-email", http.StatusNoContent, http.StatusOK)
}

// DisableUser sets enabled=false on the user (SPEC-W45 K16 member removal:
// the account is disabled, never hard-deleted, so audit history survives).
func (c *Client) DisableUser(ctx context.Context, userID string) error {
	path := "/admin/realms/" + c.realm + "/users/" + userID
	resp, err := c.do(ctx, http.MethodPut, path, map[string]any{"enabled": false})
	if err != nil {
		return err
	}
	return expectStatus(resp, "disable user", http.StatusNoContent)
}

// LogoutUserSessions revokes every session/refresh token of the user
// (POST .../users/{id}/logout — SPEC-W45 K16).
func (c *Client) LogoutUserSessions(ctx context.Context, userID string) error {
	path := "/admin/realms/" + c.realm + "/users/" + userID + "/logout"
	resp, err := c.do(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	return expectStatus(resp, "logout user", http.StatusNoContent, http.StatusOK)
}

// roleRep is the Keycloak realm-role representation used by role-mappings.
type roleRep struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// realmRole resolves a realm role by name. Returns ErrRoleNotFound when the
// realm does not define it — role assignment is fail-soft for callers (the
// Permify relationship is the authorization source of truth).
func (c *Client) realmRole(ctx context.Context, name string) (roleRep, error) {
	var r roleRep
	path := "/admin/realms/" + c.realm + "/roles/" + url.PathEscape(name)
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return r, fmt.Errorf("%w: %s", ErrRoleNotFound, name)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return r, fmt.Errorf("get realm role %s: status %d: %s", name, resp.StatusCode, string(b))
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return r, fmt.Errorf("decode realm role %s: %w", name, err)
	}
	return r, nil
}

// ErrRoleNotFound marks an unknown realm role (role-mappings fail-soft).
var ErrRoleNotFound = errors.New("keycloak realm role not found")

// AssignRealmRole adds a realm role to the user via role-mappings
// (SPEC-W45 K16/STK O4: admin/staff/viewer map to same-named realm roles).
func (c *Client) AssignRealmRole(ctx context.Context, userID, roleName string) error {
	role, err := c.realmRole(ctx, roleName)
	if err != nil {
		return err
	}
	path := "/admin/realms/" + c.realm + "/users/" + userID + "/role-mappings/realm"
	resp, err := c.do(ctx, http.MethodPost, path, []roleRep{role})
	if err != nil {
		return err
	}
	return expectStatus(resp, "assign realm role "+roleName, http.StatusNoContent)
}

// RemoveRealmRole removes a realm role from the user via role-mappings
// (SPEC-W45 K16: role change revokes the previous realm role).
func (c *Client) RemoveRealmRole(ctx context.Context, userID, roleName string) error {
	role, err := c.realmRole(ctx, roleName)
	if err != nil {
		return err
	}
	path := "/admin/realms/" + c.realm + "/users/" + userID + "/role-mappings/realm"
	resp, err := c.do(ctx, http.MethodDelete, path, []roleRep{role})
	if err != nil {
		return err
	}
	return expectStatus(resp, "remove realm role "+roleName, http.StatusNoContent)
}

// DeleteTenantGroup removes the Keycloak group /tenants/{slug} (SPEC-W45 K9
// tenant-deletion cascade). Idempotent: an absent group is a no-op.
func (c *Client) DeleteTenantGroup(ctx context.Context, slug string) error {
	id, err := c.findGroupByPath(ctx, "/tenants/"+slug)
	if err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	resp, err := c.do(ctx, http.MethodDelete, "/admin/realms/"+c.realm+"/groups/"+id, nil)
	if err != nil {
		return err
	}
	return expectStatus(resp, "delete tenant group /tenants/"+slug, http.StatusNoContent)
}

// RealmSMTPConfig carries the realm smtpServer settings applied at bootstrap
// (SPEC-W45 K10; env KC_REALM_SMTP_*).
type RealmSMTPConfig struct {
	Host     string
	Port     string
	From     string
	User     string
	Password string
}

// ApplyRealmSMTP PATCHes the realm's smtpServer map via the admin API
// (GET realm rep → merge smtpServer → PUT). Existing non-SMTP realm
// attributes are preserved (full-rep round trip, not a partial PATCH).
func (c *Client) ApplyRealmSMTP(ctx context.Context, cfg RealmSMTPConfig) error {
	if cfg.Host == "" {
		return fmt.Errorf("smtp host is required")
	}
	path := "/admin/realms/" + c.realm
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	var realm map[string]any
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return fmt.Errorf("get realm: status %d: %s", resp.StatusCode, string(b))
	}
	if err := json.NewDecoder(resp.Body).Decode(&realm); err != nil {
		resp.Body.Close()
		return fmt.Errorf("decode realm: %w", err)
	}
	resp.Body.Close()

	smtp := map[string]string{}
	if existing, ok := realm["smtpServer"].(map[string]any); ok {
		for k, v := range existing {
			if s, ok := v.(string); ok {
				smtp[k] = s
			}
		}
	}
	smtp["host"] = cfg.Host
	if cfg.Port != "" {
		smtp["port"] = cfg.Port
	}
	if cfg.From != "" {
		smtp["from"] = cfg.From
	}
	if cfg.User != "" {
		smtp["user"] = cfg.User
		smtp["auth"] = "true"
	}
	if cfg.Password != "" {
		smtp["password"] = cfg.Password
	}
	realm["smtpServer"] = smtp

	put, err := c.do(ctx, http.MethodPut, path, realm)
	if err != nil {
		return err
	}
	return expectStatus(put, "update realm smtpServer", http.StatusNoContent, http.StatusOK)
}
