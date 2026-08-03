package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/kyc-service/internal/daprc"
)

// ErrConsentDenied is returned when identity reports no active consent for
// (subject, purpose) — mapped to 403 by the handler (SPEC-W12 §5).
var ErrConsentDenied = fmt.Errorf("no active consent")

// ConsentChecker is the identity-service consent gate
// (GET /internal/consents/check). ConsentClient is the production
// implementation; tests substitute a fake.
type ConsentChecker interface {
	// CheckConsent returns the canonical tenant uuid when an active consent
	// exists, ErrConsentDenied on a 403, or a wrapped error on transport
	// failure (mapped to 502 — the gate itself is down).
	CheckConsent(ctx context.Context, tenantRef, subject, purpose string) (uuid.UUID, error)
}

// ConsentClient calls identity-service, either directly (IdentityBaseURL
// set — tests / no-Dapr dev) or via Dapr service invocation against
// IdentityAppID (production default).
type ConsentClient struct {
	dapr    *daprc.Client
	appID   string
	baseURL string // when set, Dapr is bypassed
	hc      *http.Client
}

// NewConsentClient builds the consent gate client.
func NewConsentClient(d *daprc.Client, appID, baseURL string) *ConsentClient {
	return &ConsentClient{
		dapr:    d,
		appID:   appID,
		baseURL: strings.TrimRight(baseURL, "/"),
		hc:      &http.Client{Timeout: 10 * time.Second},
	}
}

// CheckConsent implements ConsentChecker. The tenant reference is forwarded
// as X-Tenant-ID (uuid) or X-Tenant-Slug header, matching identity's
// /internal/consents/check contract.
func (c *ConsentClient) CheckConsent(ctx context.Context, tenantRef, subject, purpose string) (uuid.UUID, error) {
	q := url.Values{"subject": {subject}, "purpose": {purpose}}
	headers := map[string]string{}
	if _, err := uuid.Parse(tenantRef); err == nil {
		headers["X-Tenant-ID"] = tenantRef
	} else {
		headers["X-Tenant-Slug"] = tenantRef
	}

	var status int
	var body []byte
	var err error
	if c.baseURL != "" {
		status, body, err = daprc.DoGET(ctx, c.hc,
			c.baseURL+"/internal/consents/check?"+q.Encode(), headers)
	} else {
		status, body, err = c.dapr.InvokeGET(ctx, c.appID, "internal/consents/check", q, headers)
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("consent check transport: %w", err)
	}
	if status == http.StatusForbidden {
		return uuid.Nil, ErrConsentDenied
	}
	if status != http.StatusOK {
		return uuid.Nil, fmt.Errorf("consent check: unexpected status %d: %s", status, string(body))
	}
	var out struct {
		Allowed  bool   `json:"allowed"`
		TenantID string `json:"tenant_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return uuid.Nil, fmt.Errorf("consent check decode: %w", err)
	}
	if !out.Allowed {
		return uuid.Nil, ErrConsentDenied
	}
	tenantID, err := uuid.Parse(out.TenantID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("consent check: bad tenant_id %q: %w", out.TenantID, err)
	}
	return tenantID, nil
}
