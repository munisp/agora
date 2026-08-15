// Package activities implements the Temporal activities invoked by the
// workflows of SPEC §6. Cross-service calls go through Dapr service
// invocation; notifications go through the Dapr output bindings
// bindings-smtp and bindings-twilio (operation create).
package activities

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/opendesk/notification-worker/internal/daprc"
	"github.com/opendesk/notification-worker/internal/pacer"
	"github.com/opendesk/notification-worker/internal/packs"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.uber.org/zap"
)

// IndustryDeps bundles the SPEC-CRM §C2 dependencies: the loaded industry
// packs and the Dapr app-ids/topics the pack activities talk to.
type IndustryDeps struct {
	Packs          *packs.Registry // loaded from INDUSTRIES_DIR (may be empty)
	KnowledgeAppID string          // Dapr app-id of knowledge-service
	CRMSyncAppID   string          // Dapr app-id of crm-sync-service
	PubSubName     string          // Dapr pubsub component for CRM events
	CRMEventsTopic string          // topic for escalation priority flags
}

// Activities bundles the dependencies shared by all activity methods.
type Activities struct {
	Dapr          *daprc.Client
	BookingAppID  string
	PaymentsAppID string
	IdentityAppID string
	SMTPBinding   string
	TwilioBinding string
	SMTPFrom      string
	TwilioFrom    string
	OpenSearchURL string
	Industry      IndustryDeps
	// Gdpr holds the GDPR export/erase configuration; set by main after New.
	Gdpr GdprDeps
	// PublicBaseURL is the user-facing base for claim links
	// (PUBLIC_BASE_URL); set by main after New.
	PublicBaseURL string
	// Pacer is the outbound CPS limiter + sender rotator (VOICE-SCALING §4);
	// set by main after New. Nil disables pacing/rotation (tests).
	Pacer *pacer.Pacer
	// Guards is the SPEC-W12 §3 compliance guard set (DND 2442 suppression +
	// quiet-hours config); set by main after New. Nil disables the DND
	// pre-send guard (tests / no DATABASE_URL).
	Guards *pacer.Guards
	// Webhooks holds the outbound webhook delivery dependencies (Wave 5 #10);
	// set by main after New.
	Webhooks WebhookDeps
	// Push holds the push notification provider set (SPEC-W16 §1: fcm +
	// apns stub); set by main after New. Nil Providers disables push —
	// tokens resolve to unroutable per-token failures, never panics.
	Push PushDeps
	// WhatsApp holds the WhatsApp campaign provider (SPEC-W21 Agent A);
	// set by main after New. A nil Provider falls back to the env-derived
	// provider at send time (WHATSAPP_MOCK default OFF, SIM-011 — see
	// activities/whatsapp.go).
	WhatsApp WhatsAppDeps
	// Channels resolves the messaging provider per channel + tenant
	// (MESSAGING_CHANNELS / TENANT_CHANNEL_MAP); set by main after New.
	// Nil keeps the built-in defaults (email→smtp, sms→twilio).
	Channels *ChannelRouter
	// Civic holds the SPEC-W32 civic dependencies (delivery ledger +
	// SLA-breach escalation producer); set by main after New. Zero value
	// degrades to log-only ledger + no escalation emission.
	Civic CivicDeps
	// Ops holds the SPEC-W34 GF16 ops-alert dependencies (saga compensation
	// exhaustion alerts); set by main after New. Zero value degrades to
	// CRITICAL-log-only alerting.
	Ops OpsAlertDeps
	Log *zap.Logger

	hc *http.Client
}

// New builds the activity set.
func New(d *daprc.Client, bookingAppID, paymentsAppID, identityAppID, smtpBinding, twilioBinding, smtpFrom, twilioFrom, osURL string, ind IndustryDeps, log *zap.Logger) *Activities {
	return &Activities{
		Dapr:          d,
		BookingAppID:  bookingAppID,
		PaymentsAppID: paymentsAppID,
		IdentityAppID: identityAppID,
		SMTPBinding:   smtpBinding,
		TwilioBinding: twilioBinding,
		SMTPFrom:      smtpFrom,
		TwilioFrom:    twilioFrom,
		OpenSearchURL: strings.TrimRight(osURL, "/"),
		Industry:      ind,
		Log:           log,
		hc:            &http.Client{Timeout: 15 * time.Second},
	}
}

// ---------------------------------------------------------------------------
// Booking saga activities (booking-svc / payments-svc via Dapr invocation)
// ---------------------------------------------------------------------------

type activityCallback struct {
	BookingID  string `json:"booking_id"`
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Reason     string `json:"reason,omitempty"`
}

// ReserveSlot asks booking-service to hold the pending booking's slot.
func (a *Activities) ReserveSlot(ctx context.Context, in workflows.SagaInput) error {
	return a.Dapr.InvokeService(ctx, a.BookingAppID, "activities/reserve-slot", activityCallback{
		BookingID: in.BookingID, TenantID: in.TenantID, TenantSlug: in.TenantSlug,
	}, nil)
}

// ReleaseSlot is the compensation of ReserveSlot.
func (a *Activities) ReleaseSlot(ctx context.Context, in workflows.SagaInput, reason string) error {
	return a.Dapr.InvokeService(ctx, a.BookingAppID, "activities/release-slot", activityCallback{
		BookingID: in.BookingID, TenantID: in.TenantID, TenantSlug: in.TenantSlug, Reason: reason,
	}, nil)
}

// ConfirmBooking marks the booking confirmed (emits BookingConfirmed).
func (a *Activities) ConfirmBooking(ctx context.Context, in workflows.SagaInput) error {
	return a.Dapr.InvokeService(ctx, a.BookingAppID, "activities/confirm-booking", activityCallback{
		BookingID: in.BookingID, TenantID: in.TenantID, TenantSlug: in.TenantSlug,
	}, nil)
}

// HoldDeposit places a deposit hold in payments-service; returns the hold ID.
// Calls the real payments-service activity route (Dapr app-id `payments`).
// The held amount is the pack deposit (ceil(price * depositPercent/100)) when
// the tenant's bookingPolicy was resolved, otherwise the full price
// (SPEC-CRM §C3).
func (a *Activities) HoldDeposit(ctx context.Context, in workflows.SagaInput) (string, error) {
	var out struct {
		HoldID string `json:"hold_id"`
	}
	err := a.Dapr.InvokeService(ctx, a.PaymentsAppID, "activities/hold-deposit", map[string]any{
		"booking_id":   in.BookingID,
		"tenant_id":    in.TenantID,
		"amount_cents": depositAmountCents(in),
		"currency":     in.Currency,
	}, &out)
	if err != nil {
		return "", err
	}
	if out.HoldID == "" {
		return "", fmt.Errorf("payments hold-deposit returned empty hold_id")
	}
	return out.HoldID, nil
}

// depositAmountCents resolves the amount to hold: the pack deposit when the
// bookingPolicy was resolved by booking-service, full price otherwise.
func depositAmountCents(in workflows.SagaInput) int64 {
	if in.DepositKnown {
		return in.DepositCents
	}
	return in.PriceCents
}

// VoidHold is the compensation of HoldDeposit.
func (a *Activities) VoidHold(ctx context.Context, in workflows.SagaInput, holdID string) error {
	return a.Dapr.InvokeService(ctx, a.PaymentsAppID, "activities/void-hold", map[string]any{
		"hold_id":    holdID,
		"booking_id": in.BookingID,
		"tenant_id":  in.TenantID,
	}, nil)
}

// GetBookingStatus fetches the current booking status from booking-service.
// Uses HTTP GET on the invoke path (booking's route is GET /v1/bookings/{id}).
func (a *Activities) GetBookingStatus(ctx context.Context, bookingID, tenantSlug string) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	err := a.Dapr.InvokeServiceMethod(ctx, http.MethodGet, a.BookingAppID, "v1/bookings/"+bookingID, nil,
		map[string]string{"X-Tenant-Slug": tenantSlug}, &out)
	if err != nil {
		return "", err
	}
	return out.Status, nil
}

// MarkNoShow flips the booking to no_show via the booking activity endpoint.
func (a *Activities) MarkNoShow(ctx context.Context, in workflows.NoShowInput) error {
	return a.Dapr.InvokeService(ctx, a.BookingAppID, "activities/mark-no-show", activityCallback{
		BookingID: in.BookingID, TenantID: in.TenantID, TenantSlug: in.TenantSlug,
	}, nil)
}

// ---------------------------------------------------------------------------
// Notification activities (Dapr output bindings)
// ---------------------------------------------------------------------------

type templateData struct {
	Name     string
	StartsAt string
	Kind     string
	Tenant   string
}

var confirmationEmail = template.Must(template.New("confirm").Parse(
	`Hi {{.Name}}, your appointment with {{.Tenant}} is confirmed for {{.StartsAt}}. We look forward to seeing you!`))

var reminderEmail = template.Must(template.New("reminder").Parse(
	`Hi {{.Name}}, this is a reminder ({{.Kind}}) of your appointment with {{.Tenant}} at {{.StartsAt}}.`))

var noShowEmail = template.Must(template.New("noshow").Parse(
	`Hi {{.Name}}, we missed you at your appointment with {{.Tenant}} today. Reply to rebook at a time that suits you.`))

// Industry pack templates (SPEC-CRM §C2). Kind carries the per-template
// payload (intake form link, deposit amount, SLA, ...).
var intakeEmail = template.Must(template.New("intake").Parse(
	`Hi {{.Name}}, ahead of your visit with {{.Tenant}} on {{.StartsAt}}, please complete your intake form (about 5 minutes): {{.Kind}}`))

var depositReminderEmail = template.Must(template.New("deposit").Parse(
	`Hi {{.Name}}, your appointment with {{.Tenant}} on {{.StartsAt}} is inside the cancellation window and the deposit is still outstanding. Please complete the deposit to keep your slot.`))

var followupEmail = template.Must(template.New("followup").Parse(
	`Hi {{.Name}}, thank you for your session with {{.Tenant}}. We will follow up with notes and a tailored proposal within 7 days. Reply to this email with anything you would like us to include.`))

var proposalReminderEmail = template.Must(template.New("proposal").Parse(
	`Reminder ({{.Tenant}}): the tailored proposal for {{.Name}} (session on {{.StartsAt}}) is due. Please send it today.`))

var escalationEmail = template.Must(template.New("escalation").Parse(
	`SLA breach ({{.Tenant}}): the ticket from {{.Name}} opened at {{.StartsAt}} has passed the {{.Kind}} first-response SLA without a reply. It has been flagged as priority in the CRM.`))

// SendConfirmation sends the booking confirmation via email + SMS bindings.
func (a *Activities) SendConfirmation(ctx context.Context, in workflows.SagaInput) error {
	td := templateData{Name: in.ContactName, StartsAt: in.StartsAt.Format(time.RFC1123), Kind: "confirmation", Tenant: in.TenantSlug}
	return a.notify(ctx, td, in.ContactEmail, in.ContactPhone, "Appointment confirmed", confirmationEmail)
}

// SendReminder sends a T-24h / T-1h reminder.
func (a *Activities) SendReminder(ctx context.Context, in workflows.ReminderInput, kind string) error {
	td := templateData{Name: in.ContactName, StartsAt: in.StartsAt.Format(time.RFC1123), Kind: kind, Tenant: in.TenantSlug}
	return a.notify(ctx, td, in.ContactEmail, in.ContactPhone, "Appointment reminder "+kind, reminderEmail)
}

// SendNoShowFollowup sends the no-show follow-up message.
func (a *Activities) SendNoShowFollowup(ctx context.Context, in workflows.NoShowInput) error {
	td := templateData{Name: "there", StartsAt: in.EndsAt.Format(time.RFC1123), Kind: "no-show", Tenant: in.TenantSlug}
	return a.notify(ctx, td, in.ContactEmail, in.ContactPhone, "We missed you", noShowEmail)
}

// notify renders the template and dispatches to the output bindings
// resolved by the channel router (email → bindings-smtp; sms →
// bindings-twilio or the messaging-gateway HTTP bindings termii /
// africastalking / whatsapp per tenant). A missing recipient channel is
// skipped.
//
// Sender rotation (VOICE-SCALING §4): when the pacer has an
// OUTBOUND_FROM_NUMBERS pool, the next round-robin number replaces the
// default Twilio sender and is recorded in the binding payload metadata of
// both channels ("senderNumber"), so every outbound message carries the
// originating number for reputation tracing.
func (a *Activities) notify(ctx context.Context, td templateData, email, phone, subject string, tpl *template.Template) error {
	var body bytes.Buffer
	if err := tpl.Execute(&body, td); err != nil {
		return fmt.Errorf("render template: %w", err)
	}
	text := body.String()

	sender := a.TwilioFrom
	if a.Pacer != nil {
		if n := a.Pacer.NextSender(ctx); n != "" {
			sender = n
		}
	}

	if email != "" {
		// Channel routing (MESSAGING_CHANNELS / TENANT_CHANNEL_MAP): email
		// currently has a single provider (smtp).
		emailProvider := a.Channels.Provider(ChannelEmail, td.Tenant)
		if err := a.Dapr.InvokeBinding(ctx, a.BindingName(emailProvider), "create", text, map[string]string{
			"emailTo":      email,
			"emailFrom":    a.SMTPFrom,
			"subject":      subject,
			"senderNumber": sender,
		}); err != nil {
			return fmt.Errorf("%s binding: %w", emailProvider, err)
		}
		a.Log.Info("notification sent", zap.String("subject", subject), zap.String("channel", ChannelEmail),
			zap.String("provider", emailProvider), zap.String("email", email))
	}
	if phone != "" {
		smsProvider := a.Channels.Provider(ChannelSMS, td.Tenant)
		if err := a.sendSMS(ctx, smsProvider, phone, text, sender); err != nil {
			return fmt.Errorf("%s binding: %w", smsProvider, err)
		}
		a.Log.Info("notification sent", zap.String("subject", subject), zap.String("channel", ChannelSMS),
			zap.String("provider", smsProvider), zap.String("phone", phone),
			zap.String("sender_number", sender))
	}
	return nil
}

// sendSMS dispatches one SMS/WhatsApp message to the resolved provider. The
// Nigeria providers (termii/africastalking/whatsapp) are reached through the
// messaging-gateway HTTP bindings ("bindings-"+provider, operation "post",
// {"to","message"} data); twilio keeps the native binding + sender rotation.
func (a *Activities) sendSMS(ctx context.Context, provider, phone, text, sender string) error {
	if provider == "twilio" {
		return a.Dapr.InvokeBinding(ctx, a.TwilioBinding, "create", text, map[string]string{
			"toNumber":     phone,
			"fromNumber":   sender,
			"senderNumber": sender,
		})
	}
	return a.Dapr.InvokeBinding(ctx, a.BindingName(provider), "post", map[string]string{
		"to":      phone,
		"message": text,
	}, nil)
}

// ---------------------------------------------------------------------------
// Tenant onboarding activities
// ---------------------------------------------------------------------------

// EnsureKeycloakGroup (idempotent) asks identity-service to ensure
// /tenants/{slug} exists.
func (a *Activities) EnsureKeycloakGroup(ctx context.Context, in workflows.OnboardingInput) error {
	return a.Dapr.InvokeService(ctx, a.IdentityAppID, "internal/tenants/"+in.Slug+"/ensure-group", map[string]any{}, nil)
}

// EnsurePermifyTenant (idempotent) ensures the Permify tenant exists.
func (a *Activities) EnsurePermifyTenant(ctx context.Context, in workflows.OnboardingInput) error {
	return a.Dapr.InvokeService(ctx, a.IdentityAppID, "internal/tenants/"+in.Slug+"/ensure-permify", map[string]any{}, nil)
}

// SeedTenantData asks booking-service to seed the tenant's default public
// site row (the /p/{slug} booking page).
func (a *Activities) SeedTenantData(ctx context.Context, in workflows.OnboardingInput) error {
	return a.Dapr.InvokeService(ctx, a.BookingAppID, "internal/sites", map[string]any{
		"tenant_id":    in.TenantID,
		"tenant_slug":  in.Slug,
		"slug":         in.Slug,
		"display_name": in.Name,
	}, nil)
}

// EnsureSearchAlias creates the per-tenant OpenSearch alias kb-{slug} over
// the shared kb-chunks index (SPEC §10).
func (a *Activities) EnsureSearchAlias(ctx context.Context, in workflows.OnboardingInput) error {
	alias := "kb-" + in.Slug
	url := fmt.Sprintf("%s/kb-chunks/_alias/%s", a.OpenSearchURL, alias)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, nil)
	if err != nil {
		return err
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("opensearch alias %s: %w", alias, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("opensearch alias %s: status %d", alias, resp.StatusCode)
	}
	return nil
}
