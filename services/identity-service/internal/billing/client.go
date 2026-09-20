// Package billing is a minimal std-lib HTTP client for billing-engine's
// internal tenant-plan surface (SPEC-W45 K contract note: after a successful
// plan change identity pushes the new plan to billing so plan-keyed rating
// stays in sync even if the TenantPlanChanged event is missed).
//
// The push is strictly best-effort at the call site: failures are logged and
// surfaced in the response warnings[], never rolled back.
package billing

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

// Client pushes tenant plan changes to billing-engine. A nil *Client
// disables the integration (BILLING_URL unset).
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// New builds a Client for baseURL (e.g. http://billing-engine:7010);
// token is the BILLING_INTERNAL_TOKEN sent as X-Internal-Token (billing
// gates every non-health surface on it, W44 K2 / RS-002).
func New(baseURL, token string) *Client {
	return &Client{
		base:  strings.TrimRight(baseURL, "/"),
		token: token,
		hc:    &http.Client{Timeout: 3 * time.Second},
	}
}

// PushPlan PUTs {base}/v1/tenants/{uuid}/plan {"plan": plan}. Any transport
// error or non-2xx status is returned as an error; the caller logs ERROR and
// appends a response warning (the identity change is never rolled back).
func (c *Client) PushPlan(ctx context.Context, tenantUUID, plan string) error {
	body, err := json.Marshal(map[string]string{"plan": plan})
	if err != nil {
		return fmt.Errorf("marshal plan push: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.base+"/v1/tenants/"+tenantUUID+"/plan", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build plan push request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("X-Internal-Token", c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("billing plan push: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("billing plan push: unexpected status %d", resp.StatusCode)
	}
	return nil
}
