// Billing event notifications (SPEC-W45 ORPH O8): InvoicePaid /
// InvoiceVoided on opendesk.billing.events → email to the tenant billing
// contact, idempotent by CloudEvent id (eventmail.Consumer dedupe).
//
// Recipient resolution order (billing event payloads today carry NO
// contact — routes.rs InvoicePaid/InvoiceVoided emit ids + amounts only):
//  1. the event payload (billing_email / billingEmail / email /
//     contact_email) — honored the moment the producer adds it;
//  2. the tenant owner/billing contact via identity's internal API
//     (X-Internal-Token): GET v1/tenants/{slug} (internauth accepts the
//     internal token, SPEC-W44 W-I-1) reading billing_email or
//     metadata.billing_email;
//  3. neither exists → permanent error → DLQ with a loud log (honest
//     failure, replayable once a contact source lands — CONTRACT NOTE:
//     billing-engine should add billingEmail to the payload, or identity
//     should expose an owner-email internal endpoint keyed by tenant_id;
//     billing events carry tenant_id (uuid), not the slug, so without (1)
//     only a slug-bearing payload can reach (2)).
package eventmail

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// Billing event types (billing-engine outbox relay).
const (
	TypeInvoicePaid   = "com.opendesk.billing.InvoicePaid"
	TypeInvoiceVoided = "com.opendesk.billing.InvoiceVoided"
)

// ContactResolver resolves the tenant billing contact email ("" = none
// configured) for the tenant slug. Implemented over Dapr service invocation
// against identity-service with the internal token.
type ContactResolver interface {
	ResolveBillingContact(ctx context.Context, tenantSlug string) (string, error)
}

// BillingDeps bundles the billing-consumer dependencies.
type BillingDeps struct {
	Sender   Sender
	Resolver ContactResolver // nil → payload-only resolution
	Log      *zap.Logger
}

// HandleBilling is the eventmail.Handler for opendesk.billing.events.
// Unknown event types on the topic are acknowledged and skipped.
func (d BillingDeps) HandleBilling(ctx context.Context, evt CloudEvent) error {
	switch evt.Type {
	case TypeInvoicePaid, "InvoicePaid":
		return d.notify(ctx, evt, true)
	case TypeInvoiceVoided, "InvoiceVoided":
		return d.notify(ctx, evt, false)
	default:
		return nil
	}
}

func (d BillingDeps) notify(ctx context.Context, evt CloudEvent, paid bool) error {
	email, err := d.recipient(ctx, evt)
	if err != nil {
		return err
	}
	kind := "InvoicePaid"
	verb := "was paid"
	if !paid {
		kind = "InvoiceVoided"
		verb = "was voided"
	}
	invoiceID := evt.DataString("invoiceId", "invoice_id")
	if invoiceID == "" {
		invoiceID = evt.Subject
	}
	period := evt.DataString("period")
	amount := formatMoney(evt.Data["subtotalCents"], evt.DataString("currency"))
	subject := fmt.Sprintf("OpenDesk invoice %s %s", invoiceID, verb)
	if amount != "" && period != "" {
		subject = fmt.Sprintf("OpenDesk invoice for %s (%s) %s", period, amount, verb)
	}
	text := fmt.Sprintf(`Hello,

this is a billing notification for your OpenDesk workspace.

  Event:   %s
  Invoice: %s
  Period:  %s
  Amount:  %s

You can review invoices in the admin console under Billing.
`, kind, invoiceID, period, amount)
	if err := d.Sender.Send(ctx, "email", email, subject, text); err != nil {
		return fmt.Errorf("send %s notification: %w", kind, err)
	}
	d.log().Info("billing notification sent",
		zap.String("event_id", evt.ID), zap.String("type", kind),
		zap.String("invoice_id", invoiceID), zap.String("tenant_id", evt.TenantID))
	return nil
}

// recipient resolves the billing contact email (see package doc).
func (d BillingDeps) recipient(ctx context.Context, evt CloudEvent) (string, error) {
	if email := evt.DataString("billing_email", "billingEmail", "email", "contact_email"); email != "" {
		return email, nil
	}
	slug := evt.DataString("tenant_slug", "tenantSlug", "slug")
	if slug != "" && d.Resolver != nil {
		email, err := d.Resolver.ResolveBillingContact(ctx, slug)
		if err != nil {
			return "", fmt.Errorf("resolve billing contact for %s: %w", slug, err)
		}
		if email != "" {
			return email, nil
		}
	}
	return "", Permanent(fmt.Errorf(
		"%s %s: no billing contact in payload and none resolvable via identity "+
			"(tenant_id=%s slug=%q) — configure billing_email on the tenant or event payload",
		evt.Type, evt.ID, evt.TenantID, slug))
}

// formatMoney renders cents + ISO currency ("₦"/symbol-free: "NGN 1,234.50").
// Returns "" when the payload carries no amount.
func formatMoney(cents any, currency string) string {
	v, ok := cents.(float64)
	if !ok {
		return ""
	}
	if currency == "" {
		currency = "NGN"
	}
	whole := int64(v) / 100
	frac := int64(v) % 100
	return fmt.Sprintf("%s %s.%02d", currency, groupThousands(whole), frac)
}

func groupThousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func (d BillingDeps) log() *zap.Logger {
	if d.Log != nil {
		return d.Log
	}
	return zap.NewNop()
}
