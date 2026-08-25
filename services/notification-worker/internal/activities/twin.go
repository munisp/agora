package activities

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/opendesk/notification-worker/internal/workflows"
)

// DeleteTwinTenant deletes an expired digital-twin tenant via
// identity-service's internauth-guarded DELETE /internal/tenants/{slug}
// (W44 contract: X-Internal-Token = IDENTITY_INTERNAL_TOKEN; 200
// {"deleted"} on success; 404 is ALSO success — the twin may already have
// been removed manually). Identity decides "is a twin" from the store
// (S1-F7-06); the slug marker check below stays as local defence in depth.
func (a *Activities) DeleteTwinTenant(ctx context.Context, in workflows.TwinCleanupInput) error {
	if !strings.Contains(in.Slug, "-twin-") {
		// Defence in depth: this activity must never delete a real tenant.
		return fmt.Errorf("refusing to delete non-twin tenant %q", in.Slug)
	}
	var out struct {
		Deleted string `json:"deleted"`
	}
	err := a.Dapr.InvokeServiceMethod(ctx, http.MethodDelete, a.IdentityAppID, "internal/tenants/"+in.Slug,
		nil, internalHeaders(a.IdentityInternalToken), &out)
	if err != nil && strings.Contains(err.Error(), "404") {
		return nil // already gone — treat as success (W44 contract)
	}
	return err
}
