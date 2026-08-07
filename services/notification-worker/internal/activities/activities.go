// Package activities hosts the Go activities that implement the saga steps
// (SPEC §6) via Dapr service invocation, plus outbound notification sends
// through Dapr output bindings (smtp/twilio). Activities are registered on
// the worker in cmd/worker/main.go.
package activities

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/opendesk/notification-worker/internal/daprc"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.temporal.io/sdk/activity"
	"go.uber.org/zap"
)

// Deps bundles activity dependencies: the Dapr client, the OpenSearch
// alias ensurer, and the identity HMAC signer used by gdpr_export.
type Deps struct {
	Dapr *daprc.Client
	Log  *zap.Logger
	// SearchAliasURL is the OpenSearch base URL used by EnsureSearchAlias.
	SearchAliasURL string
	// Signer computes the GDPR export bundle HMAC (canonical request shape).
	// Injected from main; see hmacsign.
	Signer func(payload []byte) string
}

// Activities is the receiver registered with the Temporal worker.
type Activities struct {
	Dapr *daprc.Client
	Log  *zap.Logger
	Deps Deps
	// Ops carries the SPEC-W34 GF16 ops-alert dependencies (producer/topic);
	// see ops_alerts.go. Zero value = CRITICAL log-only degradation.
	Ops OpsAlertDeps
}

// New constructs the activities set.
func New(deps Deps) *Activities {
	if deps.Log == nil {
		deps.Log = zap.NewNop()
	}
	return &Activities{Dapr: deps.Dapr, Log: deps.Log, Deps: deps}
}

// ---------------------------------------------------------------------------
// Booking saga steps (SPEC §6)
// ---------------------------------------------------------------------------

// ReserveSlot invokes booking-svc POST /internal/bookings/{id}/reserve.
func (a *Activities) ReserveSlot(ctx context.Context, in workflows.SagaInput) error {
	return a.invokeBooking(ctx, "reserve", in.BookingID, map[string]any{
		"tenant_id": in.TenantID,
	})
}

// ReleaseSlot invokes booking-svc POST /internal/bookings/{id}/release (compensation).
func (a *Activities) ReleaseSlot(ctx context.Context, in workflows.SagaInput, reason string) error {
	return a.invokeBooking(ctx, "release", in.BookingID, map[string]any{
		"tenant_id": in.TenantID,
		"reason":    reason,
	})
}

// HoldDeposit invokes payments-svc POST /internal/holds (TigerBeetle two-phase
// transfer). Returns the hold id.
func (a *Activities) HoldDeposit(ctx context.Context, in workflows.SagaInput) (string, error) {
	amount := in.PriceCents
	if in.DepositKnown {
		amount = in.DepositCents
	}
	body := map[string]any{
		"tenant_id":    in.TenantID,
		"booking_id":   in.BookingID,
		"amount_cents": amount,
		"currency":     in.Currency,
	}
	var out struct {
		HoldID string `json:"hold_id"`
	}
	if err := a.Dapr.InvokeJSON(ctx, "payments", "internal/holds", body, &out); err != nil {
		return "", fmt.Errorf("HoldDeposit: %w", err)
	}
	return out.HoldID, nil
}

// VoidHold invokes payments-svc POST /internal/holds/{id}/void (compensation).
func (a *Activities) VoidHold(ctx context.Context, in workflows.SagaInput, holdID string) error {
	body := map[string]any{
		"tenant_id": in.TenantID,
		"reason":    "saga_compensation",
	}
	if err := a.Dapr.InvokeJSON(ctx, "payments", "internal/holds/"+holdID+"/void", body, nil); err != nil {
		return fmt.Errorf("VoidHold: %w", err)
	}
	return nil
}

// ConfirmBooking invokes booking-svc POST /internal/bookings/{id}/confirm.
func (a *Activities) ConfirmBooking(ctx context.Context, in workflows.SagaInput) error {
	return a.invokeBooking(ctx, "confirm", in.BookingID, map[string]any{
		"tenant_id": in.TenantID,
	})
}

// GetBookingStatus reads booking-svc GET /internal/bookings/{id} — used by
// Reminder/NoShowFollowup to re-check state before firing.
func (a *Activities) GetBookingStatus(ctx context.Context, bookingID, tenantID string) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	path := fmt.Sprintf("internal/bookings/%s?tenant_id=%s", bookingID, tenantID)
	if err := a.Dapr.InvokeGetJSON(ctx, "booking", path, &out); err != nil {
		return "", fmt.Errorf("GetBookingStatus: %w", err)
	}
	return out.Status, nil
}

// MarkNoShow invokes booking-svc POST /internal/bookings/{id}/no-show.
func (a *Activities) MarkNoShow(ctx context.Context, in workflows.NoShowInput) error {
	return a.invokeBooking(ctx, "no-show", in.BookingID, map[string]any{
		"tenant_id": in.TenantID,
	})
}

func (a *Activities) invokeBooking(ctx context.Context, action, bookingID string, body map[string]any) error {
	path := fmt.Sprintf("internal/bookings/%s/%s", bookingID, action)
	if err := a.Dapr.InvokeJSON(ctx, "booking", path, body, nil); err != nil {
		return fmt.Errorf("booking %s: %w", action, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Notification sends (Dapr output bindings)
// ---------------------------------------------------------------------------

// SendInput is one outbound notification. Channel: "email" | "sms".
type SendInput struct {
	Channel string `json:"channel"`
	To      string `json:"to"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
}

// SendConfirmation emails/SMSes the booking confirmation.
func (a *Activities) SendConfirmation(ctx context.Context, in workflows.SagaInput) error {
	text := fmt.Sprintf("Your booking %s is confirmed for %s.", in.BookingID, in.StartsAt.Format(time.RFC1123))
	return a.sendBoth(ctx, in.ContactEmail, in.ContactPhone, "Booking confirmed", text)
}

// SendReminder sends the T-24h / T-1h reminders.
func (a *Activities) SendReminder(ctx context.Context, in workflows.ReminderInput, label string) error {
	text := fmt.Sprintf("Reminder (%s): booking %s at %s.", label, in.BookingID, in.StartsAt.Format(time.RFC1123))
	return a.sendBoth(ctx, in.ContactEmail, in.ContactPhone, "Booking reminder", text)
}

// SendNoShowFollowup sends the post-no-show follow-up message.
func (a *Activities) SendNoShowFollowup(ctx context.Context, in workflows.NoShowInput) error {
	text := fmt.Sprintf("We missed you for booking %s — reply to rebook.", in.BookingID)
	return a.sendBoth(ctx, in.ContactEmail, in.ContactPhone, "We missed you", text)
}

func (a *Activities) sendBoth(ctx context.Context, email, phone, subject, body string) error {
	if email != "" {
		if err := a.SendEmail(ctx, SendInput{Channel: "email", To: email, Subject: subject, Body: body}); err != nil {
			return err
		}
	}
	if phone != "" {
		if err := a.SendSMS(ctx, SendInput{Channel: "sms", To: phone, Body: body}); err != nil {
			return err
		}
	}
	return nil
}

// SendEmail invokes the Dapr smtp output binding (bindings-smtp).
func (a *Activities) SendEmail(ctx context.Context, in SendInput) error {
	meta := map[string]string{
		"emailTo":   in.To,
		"subject":   in.Subject,
		"emailFrom": "no-reply@opendesk.local",
	}
	return a.Dapr.InvokeBinding(ctx, "bindings-smtp", "create", []byte(in.Body), meta)
}

// SendSMS invokes the Dapr twilio output binding (bindings-twilio).
func (a *Activities) SendSMS(ctx context.Context, in SendInput) error {
	meta := map[string]string{
		"toNumber":   in.To,
		"fromNumber": "+10000000000",
	}
	return a.Dapr.InvokeBinding(ctx, "bindings-twilio", "create", []byte(in.Body), meta)
}

// ---------------------------------------------------------------------------
// Tenant onboarding steps
// ---------------------------------------------------------------------------

// EnsureKeycloakGroup creates the /tenants/{slug} group in the opendesk realm
// via the Keycloak admin REST (through Dapr invoke on identity-svc, which owns
// realm admin).
func (a *Activities) EnsureKeycloakGroup(ctx context.Context, in workflows.OnboardingInput) error {
	body := map[string]any{
		"tenant_id": in.TenantID,
		"slug":      in.Slug,
		"name":      in.Name,
	}
	if err := a.Dapr.InvokeJSON(ctx, "identity", "internal/tenants/ensure-keycloak", body, nil); err != nil {
		return fmt.Errorf("EnsureKeycloakGroup: %w", err)
	}
	return nil
}

// EnsurePermifyTenant creates the Permify tenant + writes the org relationships.
func (a *Activities) EnsurePermifyTenant(ctx context.Context, in workflows.OnboardingInput) error {
	body := map[string]any{
		"tenant_id": in.TenantID,
		"slug":      in.Slug,
	}
	if err := a.Dapr.InvokeJSON(ctx, "identity", "internal/tenants/ensure-permify", body, nil); err != nil {
		return fmt.Errorf("EnsurePermifyTenant: %w", err)
	}
	return nil
}

// SeedTenantData asks booking-svc to seed a starter offering/team/site.
func (a *Activities) SeedTenantData(ctx context.Context, in workflows.OnboardingInput) error {
	body := map[string]any{
		"tenant_id": in.TenantID,
		"slug":      in.Slug,
		"plan":      in.Plan,
	}
	if err := a.Dapr.InvokeJSON(ctx, "booking", "internal/tenants/seed", body, nil); err != nil {
		return fmt.Errorf("SeedTenantData: %w", err)
	}
	return nil
}

// EnsureSearchAlias creates the tenant-scoped OpenSearch alias (filtered on
// tenant_id) so queries are physically unable to cross tenants (SPEC §6,
// multi-tenant search hardening).
func (a *Activities) EnsureSearchAlias(ctx context.Context, in workflows.OnboardingInput) error {
	log := activity.GetLogger(ctx)
	base := a.Deps.SearchAliasURL
	if base == "" {
		base = "http://opensearch:9200"
	}
	alias := fmt.Sprintf("tenant-%s-conversations", in.TenantID)
	body := map[string]any{
		"actions": []map[string]any{
			{
				"add": map[string]any{
					"index": "conversations",
					"alias": alias,
					"filter": map[string]any{
						"term": map[string]any{"tenant_id": in.TenantID},
					},
				},
			},
		},
	}
	payload, _ := json.Marshal(body)
	req, err := httpNewRequest(ctx, "POST", base+"/_aliases", payload)
	if err != nil {
		return fmt.Errorf("EnsureSearchAlias: %w", err)
	}
	resp, err := httpDefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("EnsureSearchAlias: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("EnsureSearchAlias: opensearch alias %s: status %d", alias, resp.StatusCode)
	}
	log.Info("tenant search alias ensured", "alias", alias)
	return nil
}
