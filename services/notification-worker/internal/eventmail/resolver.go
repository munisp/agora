// Dapr-backed ContactResolver: identity tenant lookup with the K2 internal
// token (GET v1/tenants/{slug}, X-Internal-Token). Reads billing_email from
// the top level or metadata.billing_email — both are forward-compatible
// contracts: identity's getTenant currently exposes neither, so resolution
// returns "" (no contact) until the producer/identity side lands.
package eventmail

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/opendesk/notification-worker/internal/daprc"
)

// DaprContactResolver resolves tenant billing contacts via identity-service.
type DaprContactResolver struct {
	Dapr  *daprc.Client
	AppID string // identity Dapr app-id
	Token string // IDENTITY_INTERNAL_TOKEN (K2)
}

// ResolveBillingContact implements ContactResolver.
func (r *DaprContactResolver) ResolveBillingContact(ctx context.Context, tenantSlug string) (string, error) {
	if strings.TrimSpace(tenantSlug) == "" {
		return "", fmt.Errorf("tenant slug is required")
	}
	var out struct {
		BillingEmail string `json:"billing_email"`
		ContactEmail string `json:"contact_email"`
		Metadata     struct {
			BillingEmail string `json:"billing_email"`
			ContactEmail string `json:"contact_email"`
		} `json:"metadata"`
	}
	err := r.Dapr.InvokeServiceMethod(ctx, http.MethodGet, r.AppID,
		"v1/tenants/"+url.PathEscape(tenantSlug), nil,
		map[string]string{"X-Internal-Token": r.Token}, &out)
	if err != nil {
		return "", err
	}
	for _, c := range []string{out.BillingEmail, out.ContactEmail, out.Metadata.BillingEmail, out.Metadata.ContactEmail} {
		if strings.TrimSpace(c) != "" {
			return strings.TrimSpace(c), nil
		}
	}
	return "", nil
}
