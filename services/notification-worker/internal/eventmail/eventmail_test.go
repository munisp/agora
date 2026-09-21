package eventmail

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"
)

type fakeSender struct {
	calls []sentMail
	err   error
}

type sentMail struct {
	channel, to, subject, text string
}

func (f *fakeSender) Send(_ context.Context, channel, to, subject, text string) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, sentMail{channel, to, subject, text})
	return nil
}

func evtJSON(t *testing.T, evt CloudEvent) []byte {
	t.Helper()
	raw, err := json.Marshal(evt)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestMemberInvited_SendsInviteEmail(t *testing.T) {
	sender := &fakeSender{}
	d := IdentityDeps{Sender: sender, AppBaseURL: "https://app.opendesk.example/", Log: zap.NewNop()}
	evt := CloudEvent{
		ID:      "evt-invite-1",
		Type:    TypeMemberInvited,
		Subject: "acme",
		Data: map[string]any{
			"tenant_slug":  "acme",
			"email":        "jane@acme.test",
			"display_name": "Jane",
			"role":         "staff",
			"invited_by":   "owner@acme.test",
		},
	}
	if err := d.HandleIdentity(context.Background(), evt); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 email, got %d", len(sender.calls))
	}
	m := sender.calls[0]
	if m.channel != "email" || m.to != "jane@acme.test" {
		t.Errorf("bad recipient: %+v", m)
	}
	if !strings.Contains(m.text, "https://app.opendesk.example") {
		t.Errorf("invite email lacks app URL: %q", m.text)
	}
	if !strings.Contains(m.text, "owner@acme.test") || !strings.Contains(m.text, "acme") || !strings.Contains(m.text, "staff") {
		t.Errorf("invite email lacks inviter/tenant/role: %q", m.text)
	}
}

func TestMemberInvited_NoEmail_Permanent(t *testing.T) {
	d := IdentityDeps{Sender: &fakeSender{}, AppBaseURL: "x", Log: zap.NewNop()}
	err := d.HandleIdentity(context.Background(), CloudEvent{ID: "e2", Type: TypeMemberInvited, Data: map[string]any{}})
	if !errors.Is(err, errPermanent) {
		t.Fatalf("expected permanent error, got %v", err)
	}
}

func TestTenantProvisioned_WelcomeEmail(t *testing.T) {
	sender := &fakeSender{}
	d := IdentityDeps{Sender: sender, AppBaseURL: "https://app.opendesk.example", Log: zap.NewNop()}
	evt := CloudEvent{
		ID:   "evt-prov-1",
		Type: TypeTenantProvisioned,
		Data: map[string]any{
			"slug": "acme", "name": "Acme Ltd", "plan": "pro",
			"owner_email": "owner@acme.test",
		},
	}
	if err := d.HandleIdentity(context.Background(), evt); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(sender.calls) != 1 || sender.calls[0].to != "owner@acme.test" {
		t.Fatalf("bad welcome email: %+v", sender.calls)
	}
	if !strings.Contains(sender.calls[0].text, "Acme Ltd") || !strings.Contains(sender.calls[0].text, "pro") {
		t.Errorf("welcome email lacks tenant/plan: %q", sender.calls[0].text)
	}
}

func TestTenantProvisioned_NoRecipient_Acks(t *testing.T) {
	sender := &fakeSender{}
	d := IdentityDeps{Sender: sender, AppBaseURL: "x", Log: zap.NewNop()}
	evt := CloudEvent{ID: "e4", Type: TypeTenantProvisioned, Data: map[string]any{"slug": "acme"}}
	if err := d.HandleIdentity(context.Background(), evt); err != nil {
		t.Fatalf("missing recipient must ack (nil), got %v", err)
	}
	if len(sender.calls) != 0 {
		t.Fatalf("no email expected, got %+v", sender.calls)
	}
}

type fakeResolver struct {
	email string
	err   error
	seen  []string
}

func (f *fakeResolver) ResolveBillingContact(_ context.Context, slug string) (string, error) {
	f.seen = append(f.seen, slug)
	return f.email, f.err
}

func TestInvoicePaid_PayloadContactWins(t *testing.T) {
	sender := &fakeSender{}
	res := &fakeResolver{email: "owner@acme.test"}
	d := BillingDeps{Sender: sender, Resolver: res, Log: zap.NewNop()}
	evt := CloudEvent{
		ID:       "evt-pay-1",
		Type:     TypeInvoicePaid,
		TenantID: "t-1",
		Data: map[string]any{
			"invoiceId": "inv-1", "period": "2025-01",
			"subtotalCents": float64(150000), "currency": "NGN",
			"billing_email": "billing@acme.test",
		},
	}
	if err := d.HandleBilling(context.Background(), evt); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(sender.calls) != 1 || sender.calls[0].to != "billing@acme.test" {
		t.Fatalf("bad billing email: %+v", sender.calls)
	}
	if len(res.seen) != 0 {
		t.Errorf("resolver must not be consulted when payload carries the contact")
	}
	if !strings.Contains(sender.calls[0].text, "NGN 1,500.00") {
		t.Errorf("amount formatting wrong: %q", sender.calls[0].text)
	}
}

func TestInvoiceVoided_ResolverFallback(t *testing.T) {
	sender := &fakeSender{}
	res := &fakeResolver{email: "owner@acme.test"}
	d := BillingDeps{Sender: sender, Resolver: res, Log: zap.NewNop()}
	evt := CloudEvent{
		ID:       "evt-void-1",
		Type:     TypeInvoiceVoided,
		TenantID: "t-1",
		Data: map[string]any{
			"invoiceId": "inv-2", "period": "2025-01",
			"subtotalCents": float64(9900), "currency": "NGN",
			"tenant_slug": "acme",
		},
	}
	if err := d.HandleBilling(context.Background(), evt); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(res.seen) != 1 || res.seen[0] != "acme" {
		t.Fatalf("resolver not consulted with slug: %+v", res.seen)
	}
	if len(sender.calls) != 1 || sender.calls[0].to != "owner@acme.test" {
		t.Fatalf("bad void email: %+v", sender.calls)
	}
	if !strings.Contains(sender.calls[0].text, "InvoiceVoided") {
		t.Errorf("void email lacks kind: %q", sender.calls[0].text)
	}
}

func TestInvoicePaid_NoContact_Permanent(t *testing.T) {
	d := BillingDeps{Sender: &fakeSender{}, Resolver: &fakeResolver{}, Log: zap.NewNop()}
	evt := CloudEvent{ID: "evt-pay-2", Type: TypeInvoicePaid, TenantID: "t-1",
		Data: map[string]any{"invoiceId": "inv-3", "tenant_slug": "acme"}}
	err := d.HandleBilling(context.Background(), evt)
	if !errors.Is(err, errPermanent) {
		t.Fatalf("expected permanent error (DLQ), got %v", err)
	}
}

func TestUnknownTypes_Ack(t *testing.T) {
	d := IdentityDeps{Sender: &fakeSender{}, Log: zap.NewNop()}
	if err := d.HandleIdentity(context.Background(), CloudEvent{ID: "x", Type: "com.opendesk.identity.TenantDeleted", Data: map[string]any{}}); err != nil {
		t.Fatalf("unknown identity type must ack: %v", err)
	}
	b := BillingDeps{Sender: &fakeSender{}, Log: zap.NewNop()}
	if err := b.HandleBilling(context.Background(), CloudEvent{ID: "x", Type: "com.opendesk.billing.InvoiceIssued", Data: map[string]any{}}); err != nil {
		t.Fatalf("unknown billing type must ack: %v", err)
	}
}

func TestProcess_IdempotentByEventID(t *testing.T) {
	sender := &fakeSender{}
	d := IdentityDeps{Sender: sender, AppBaseURL: "x", Log: zap.NewNop()}
	c := &Consumer{handler: d.HandleIdentity, log: zap.NewNop(), seen: map[string]struct{}{}}
	evt := CloudEvent{ID: "dup-1", Type: TypeMemberInvited, Data: map[string]any{"email": "a@b.c"}}
	raw := evtJSON(t, evt)
	if err := c.Process(context.Background(), raw); err != nil {
		t.Fatalf("first process: %v", err)
	}
	if err := c.Process(context.Background(), raw); err != nil {
		t.Fatalf("second process: %v", err)
	}
	if len(sender.calls) != 1 {
		t.Fatalf("redelivery must not re-send (idempotency), got %d sends", len(sender.calls))
	}
}

func TestProcess_MalformedDropped(t *testing.T) {
	c := &Consumer{handler: func(context.Context, CloudEvent) error {
		t.Fatal("handler must not run on poison payload")
		return nil
	}, log: zap.NewNop(), seen: map[string]struct{}{}}
	if err := c.Process(context.Background(), []byte("{not json")); err != nil {
		t.Fatalf("poison payload must be dropped (nil), got %v", err)
	}
}

func TestFormatMoney(t *testing.T) {
	if got := formatMoney(float64(150000), "NGN"); got != "NGN 1,500.00" {
		t.Errorf("got %q", got)
	}
	if got := formatMoney("nope", "NGN"); got != "" {
		t.Errorf("non-numeric amount must render empty, got %q", got)
	}
}
