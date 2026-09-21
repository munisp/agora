// Identity lifecycle emails (SPEC-W45 K8 + STK O16): MemberInvited →
// invite email with the app URL and sign-in instructions; TenantProvisioned
// → tenant welcome email. Both ride the existing Dapr SMTP binding.
package eventmail

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// Identity event types (produced by identity-service on
// opendesk.identity.events; both the bare and the CloudEvents type-prefix
// forms are accepted).
const (
	TypeMemberInvited     = "com.opendesk.identity.MemberInvited"
	TypeTenantProvisioned = "com.opendesk.identity.TenantProvisioned"
)

// IdentityDeps bundles the identity-consumer dependencies.
type IdentityDeps struct {
	Sender Sender
	// AppBaseURL is the user-facing app URL linked in the emails
	// (APP_BASE_URL env, PUBLIC_BASE_URL fallback).
	AppBaseURL string
	Log        *zap.Logger
}

// HandleIdentity is the eventmail.Handler for opendesk.identity.events.
// Unknown event types on the topic are acknowledged and skipped
// (forward-compatible).
func (d IdentityDeps) HandleIdentity(ctx context.Context, evt CloudEvent) error {
	switch evt.Type {
	case TypeMemberInvited, "MemberInvited":
		return d.memberInvited(ctx, evt)
	case TypeTenantProvisioned, "TenantProvisioned":
		return d.tenantProvisioned(ctx, evt)
	default:
		return nil
	}
}

// memberInvited sends the K8 invite email. Payload contract (identity,
// SPEC-W45 K8): tenant_slug, email, display_name, role, invited_by,
// invite_ts. Tenant slug falls back to the CloudEvent subject; an event
// without a recipient email can never be delivered → permanent (DLQ).
func (d IdentityDeps) memberInvited(ctx context.Context, evt CloudEvent) error {
	email := evt.DataString("email")
	if email == "" {
		return Permanent(fmt.Errorf("MemberInvited %s carries no email", evt.ID))
	}
	tenant := evt.DataString("tenant_slug", "tenant")
	if tenant == "" {
		tenant = evt.Subject
	}
	role := evt.DataString("role")
	if role == "" {
		role = "member"
	}
	inviter := evt.DataString("invited_by", "inviter")
	if inviter == "" {
		inviter = "your team administrator"
	}
	name := evt.DataString("display_name", "name")
	greeting := "Hello,"
	if name != "" {
		greeting = "Hello " + name + ","
	}
	appURL := strings.TrimRight(d.AppBaseURL, "/")
	subject := fmt.Sprintf("You've been invited to %s on OpenDesk", tenant)
	text := fmt.Sprintf(`%s

%s invited you to join the %s workspace on OpenDesk as %s.

Sign in here: %s

Your account has been created. If this is your first sign-in, use the
credentials email from the identity provider to set your password and
verify your address, then open the link above.

If you were not expecting this invitation you can ignore this email.
`, greeting, inviter, tenant, role, appURL)
	if err := d.Sender.Send(ctx, "email", email, subject, text); err != nil {
		return fmt.Errorf("send invite email: %w", err)
	}
	d.log().Info("member invite email sent",
		zap.String("event_id", evt.ID), zap.String("tenant", tenant),
		zap.String("role", role))
	return nil
}

// tenantProvisioned sends the STK O16 welcome email. Recipient contract:
// data.owner_email (preferred) or data.email — when the producer carries no
// owner address yet, the event is acknowledged with a warn (never an error:
// a missing optional recipient must not dead-letter provisioning events).
func (d IdentityDeps) tenantProvisioned(ctx context.Context, evt CloudEvent) error {
	email := evt.DataString("owner_email", "email", "billing_email")
	if email == "" {
		d.log().Warn("TenantProvisioned carries no owner email; welcome email skipped",
			zap.String("event_id", evt.ID), zap.String("tenant", evt.Subject))
		return nil
	}
	name := evt.DataString("name", "slug", "tenant_slug")
	if name == "" {
		name = evt.Subject
	}
	plan := evt.DataString("plan")
	if plan == "" {
		plan = "free"
	}
	appURL := strings.TrimRight(d.AppBaseURL, "/")
	subject := fmt.Sprintf("Welcome to OpenDesk — %s is ready", name)
	text := fmt.Sprintf(`Hello,

your workspace %s has been provisioned on OpenDesk (plan: %s).

Open your workspace: %s

Next steps:
  1. Sign in and invite your team (Settings → Members).
  2. Configure your booking site and offerings.
  3. Connect your notification channels.

Welcome aboard!
`, name, plan, appURL)
	if err := d.Sender.Send(ctx, "email", email, subject, text); err != nil {
		return fmt.Errorf("send welcome email: %w", err)
	}
	d.log().Info("tenant welcome email sent",
		zap.String("event_id", evt.ID), zap.String("tenant", name), zap.String("plan", plan))
	return nil
}

func (d IdentityDeps) log() *zap.Logger {
	if d.Log != nil {
		return d.Log
	}
	return zap.NewNop()
}
