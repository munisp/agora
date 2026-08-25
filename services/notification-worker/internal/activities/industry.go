package activities

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// Industry pack activities (SPEC-CRM §C2)
// ---------------------------------------------------------------------------

// ApplyIndustryPack seeds the tenant with its industry pack: offerings in
// booking-service (idempotent by name), knowledge documents in
// knowledge-service (skipped when the exact title is already indexed) and the
// pack terminology via identity's internal merge endpoint. Every step is
// safe to retry, so the onboarding workflow can re-run the activity after
// transient failures without duplicating data.
func (a *Activities) ApplyIndustryPack(ctx context.Context, in workflows.OnboardingInput) error {
	if a.Industry.Packs == nil {
		a.Log.Warn("no industry packs loaded; skipping pack application", zap.String("slug", in.Slug))
		return nil
	}
	pack, ok := a.Industry.Packs.Get(in.IndustryOrDefault())
	if !ok {
		a.Log.Warn("unknown industry pack; skipping",
			zap.String("slug", in.Slug), zap.String("industry", in.IndustryOrDefault()))
		return nil
	}
	tenantHeader := map[string]string{"X-Tenant-Slug": in.Slug}

	// 1. Offerings — idempotent by name.
	var catalog struct {
		Offerings []struct {
			Name string `json:"name"`
		} `json:"offerings"`
	}
	if err := a.Dapr.InvokeServiceMethod(ctx, http.MethodGet, a.BookingAppID, "v1/offerings", nil, tenantHeader, &catalog); err != nil {
		return fmt.Errorf("list existing offerings: %w", err)
	}
	existing := make(map[string]bool, len(catalog.Offerings))
	for _, o := range catalog.Offerings {
		existing[o.Name] = true
	}
	for _, o := range pack.Offerings {
		if existing[o.Name] {
			continue
		}
		body := map[string]any{
			"name":         o.Name,
			"duration_min": o.DurationMin,
			"buffer_min":   o.BufferMin,
			"price_cents":  o.PriceCents,
			"capacity":     o.Capacity,
			"bookable":     true,
		}
		if err := a.Dapr.InvokeServiceMethod(ctx, http.MethodPost, a.BookingAppID, "v1/offerings", body, tenantHeader, nil); err != nil {
			return fmt.Errorf("seed offering %q: %w", o.Name, err)
		}
		a.Log.Info("pack offering seeded", zap.String("slug", in.Slug), zap.String("offering", o.Name))
	}

	// 2. Knowledge seed documents — skipped when the exact title is already
	// indexed for the tenant (best-effort idempotency; the knowledge service
	// has no list endpoint).
	for _, doc := range pack.KnowledgeSeed {
		var found struct {
			Results []struct {
				Title string `json:"title"`
			} `json:"results"`
		}
		searchPath := "v1/search?q=" + url.QueryEscape(doc.Title) + "&tenant=" + url.QueryEscape(in.Slug)
		if err := a.Dapr.InvokeServiceMethod(ctx, http.MethodGet, a.Industry.KnowledgeAppID, searchPath, nil, nil, &found); err != nil {
			return fmt.Errorf("search knowledge for %q: %w", doc.Title, err)
		}
		dup := false
		for _, r := range found.Results {
			if r.Title == doc.Title {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		body := map[string]any{"tenant": in.Slug, "title": doc.Title, "body": doc.Body}
		if err := a.Dapr.InvokeService(ctx, a.Industry.KnowledgeAppID, "v1/documents", body, nil); err != nil {
			return fmt.Errorf("seed knowledge doc %q: %w", doc.Title, err)
		}
		a.Log.Info("pack knowledge doc seeded", zap.String("slug", in.Slug), zap.String("title", doc.Title))
	}

	// 3. Terminology — merge-patch into the tenant record.
	if len(pack.Terminology) > 0 {
		if err := a.Dapr.InvokeService(ctx, a.IdentityAppID, "internal/tenants/"+in.Slug+"/terminology", pack.Terminology, nil); err != nil {
			return fmt.Errorf("apply pack terminology: %w", err)
		}
	}
	a.Log.Info("industry pack applied",
		zap.String("slug", in.Slug), zap.String("industry", pack.ID),
		zap.Int("offerings", len(pack.Offerings)), zap.Int("knowledge_docs", len(pack.KnowledgeSeed)))
	return nil
}

// VerifyDepositHold asks payments-service for the tenant's ledger balance and
// reports whether an open (pending) deposit hold exists. K2: the call
// carries X-Internal-Token = PAYMENTS_INTERNAL_TOKEN (Dapr passthrough, or
// direct HTTP when PAYMENTS_URL is set); K5: the account is addressed by the
// tenant SLUG (uuid fallback with WARN).
func (a *Activities) VerifyDepositHold(ctx context.Context, in workflows.SalonDepositInput) (bool, error) {
	slug := a.k5TenantSlug(in.BookingID, in.TenantID, in.TenantSlug)
	var bal struct {
		Accounts []struct {
			DebitsPending  int64 `json:"debits_pending"`
			CreditsPending int64 `json:"credits_pending"`
		} `json:"accounts"`
	}
	if err := a.invokePaymentsMethod(ctx, http.MethodGet,
		"v1/accounts/"+slug+"/balance", nil, &bal); err != nil {
		return false, fmt.Errorf("payments balance: %w", err)
	}
	for _, acc := range bal.Accounts {
		if acc.DebitsPending > 0 || acc.CreditsPending > 0 {
			return true, nil
		}
	}
	return false, nil
}

// ChargeNoShowFee captures the pack no-show fee from the deposit hold via
// payments-service (idempotent by deposit id on the payments side). K2:
// X-Internal-Token = PAYMENTS_INTERNAL_TOKEN via the shared payments invoke
// helper (Dapr passthrough / PAYMENTS_URL fallback); K5: tenant namespace is
// the slug (uuid fallback with WARN). The deterministic idempotency_key
// (noshowfee-{booking_id}) makes activity retries safe even if the payments
// side dedupe by deposit id ever changes.
func (a *Activities) ChargeNoShowFee(ctx context.Context, in workflows.SalonDepositInput) error {
	if in.HoldID == "" {
		return fmt.Errorf("no deposit hold to charge the no-show fee from")
	}
	slug := a.k5TenantSlug(in.BookingID, in.TenantID, in.TenantSlug)
	return a.invokePayments(ctx, "v1/no-show-fee", map[string]any{
		"tenant_id":       slug,
		"tenant_slug":     slug,
		"deposit_id":      in.HoldID,
		"amount_cents":    in.NoShowFeeCents,
		"booking_id":      in.BookingID,
		"idempotency_key": "noshowfee-" + in.BookingID,
	}, nil)
}

// SendIntakeReminder emails the patient their intake form link at T-72h.
func (a *Activities) SendIntakeReminder(ctx context.Context, in workflows.ClinicIntakeInput) error {
	link := fmt.Sprintf("https://forms.opendesk.local/intake/%s/%s", in.TenantSlug, in.BookingID)
	td := templateData{
		Name:     in.ContactName,
		StartsAt: in.StartsAt.Format(time.RFC1123),
		Kind:     link,
		Tenant:   in.TenantSlug,
	}
	return a.notify(ctx, td, in.ContactEmail, "", "Intake form for your upcoming visit", intakeEmail)
}

// SendDepositReminder nudges the client about the missing deposit inside the
// cancellation window.
func (a *Activities) SendDepositReminder(ctx context.Context, in workflows.SalonDepositInput) error {
	td := templateData{
		Name:     in.ContactName,
		StartsAt: in.StartsAt.Format(time.RFC1123),
		Kind:     fmt.Sprintf("%.2f", float64(in.DepositCents)/100.0),
		Tenant:   in.TenantSlug,
	}
	return a.notify(ctx, td, in.ContactEmail, in.ContactPhone, "Deposit required for your appointment", depositReminderEmail)
}

// SendFollowupEmail sends the post-session follow-up (consultancy pack).
func (a *Activities) SendFollowupEmail(ctx context.Context, in workflows.ConsultancyFollowupInput) error {
	td := templateData{
		Name:     in.ContactName,
		StartsAt: in.EndsAt.Format(time.RFC1123),
		Kind:     "follow-up",
		Tenant:   in.TenantSlug,
	}
	return a.notify(ctx, td, in.ContactEmail, "", "Thank you — next steps after our session", followupEmail)
}

// SendProposalReminder reminds the consultancy team (T+7d after the session)
// that the tailored proposal is due. The staff notification address is
// derived from the tenant slug; in dev the SMTP binding (mailhog) captures
// all outbound mail.
func (a *Activities) SendProposalReminder(ctx context.Context, in workflows.ConsultancyFollowupInput) error {
	td := templateData{
		Name:     in.ContactName,
		StartsAt: in.EndsAt.Format(time.RFC1123),
		Kind:     "proposal-due",
		Tenant:   in.TenantSlug,
	}
	return a.notify(ctx, td, staffAddress(in.TenantSlug), "", "Proposal due for "+in.ContactName, proposalReminderEmail)
}

// CreateStaffAlertTask files a CRM task for the clinic staff when a patient's
// intake form is still incomplete at T-2h (Dapr invoke app-id `crm-sync`).
func (a *Activities) CreateStaffAlertTask(ctx context.Context, in workflows.ClinicIntakeInput) error {
	return a.createCRMTask(ctx, in.TenantSlug, in.TenantID, map[string]any{
		"kind":       "staff_alert",
		"title":      "Intake form incomplete — visit " + in.BookingID,
		"body":       fmt.Sprintf("Patient %s has not completed the intake form 2 hours before their visit (%s).", in.ContactName, in.StartsAt.Format(time.RFC1123)),
		"booking_id": in.BookingID,
		"due_at":     in.StartsAt.UTC().Format(time.RFC3339),
	})
}

// CreateCRMFollowupTask files the post-session follow-up task in Twenty via
// crm-sync (consultancy pack: "proposal follow-up task in CRM").
func (a *Activities) CreateCRMFollowupTask(ctx context.Context, in workflows.ConsultancyFollowupInput) error {
	return a.createCRMTask(ctx, in.TenantSlug, in.TenantID, map[string]any{
		"kind":       "follow_up",
		"title":      "Follow up with " + in.ContactName + " (session " + in.BookingID + ")",
		"body":       "Discovery/strategy session completed. Send call notes and prepare the tailored proposal (due within 7 days).",
		"booking_id": in.BookingID,
		"due_at":     in.EndsAt.Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339),
	})
}

// createCRMTask posts a task to crm-sync's helper endpoint.
func (a *Activities) createCRMTask(ctx context.Context, tenantSlug, tenantID string, task map[string]any) error {
	task["tenant_slug"] = tenantSlug
	task["tenant_id"] = tenantID
	if err := a.Dapr.InvokeService(ctx, a.Industry.CRMSyncAppID, "v1/tasks", task, nil); err != nil {
		return fmt.Errorf("crm-sync create task: %w", err)
	}
	return nil
}

// EscalateTicket handles a first-response SLA breach (support-desk pack):
// emails the tenant owner and publishes a priority-flag event to
// opendesk.crm.events for the CRM.
func (a *Activities) EscalateTicket(ctx context.Context, in workflows.SupportEscalationInput) error {
	sla := time.Duration(in.FirstResponseSLAHours) * time.Hour
	if sla <= 0 {
		sla = 4 * time.Hour
	}
	// Owner notification email (staff address derived from the tenant slug;
	// mailhog captures it in dev).
	td := templateData{
		Name:     in.ContactName,
		StartsAt: in.CreatedAt.Format(time.RFC1123),
		Kind:     sla.String(),
		Tenant:   in.TenantSlug,
	}
	if err := a.notify(ctx, td, staffAddress(in.TenantSlug), "", "SLA breach — ticket "+in.BookingID+" escalated", escalationEmail); err != nil {
		return fmt.Errorf("escalation email: %w", err)
	}
	// Priority flag event for the CRM pipeline.
	evt := map[string]any{
		"specversion": "1.0",
		"id":          uuid.NewString(),
		"source":      "notification-worker",
		"type":        "com.opendesk.crm.TicketEscalated",
		"subject":     in.TenantSlug,
		"time":        time.Now().UTC().Format(time.RFC3339),
		"data": map[string]any{
			"tenant_slug": in.TenantSlug,
			"tenant_id":   in.TenantID,
			"ticket_id":   in.BookingID,
			"priority":    "high",
			"reason":      "first_response_sla_breach",
			"sla":         sla.String(),
		},
	}
	if err := a.Dapr.PublishEvent(ctx, a.Industry.PubSubName, a.Industry.CRMEventsTopic, evt); err != nil {
		return fmt.Errorf("publish escalation event: %w", err)
	}
	return nil
}

// staffAddress derives the tenant staff notification mailbox from the slug.
// In dev all outbound mail is captured by the SMTP binding (mailhog); in
// production the binding maps this alias to the tenant owner's address.
func staffAddress(slug string) string {
	return "staff+" + slug + "@opendesk.local"
}
