// notification-worker: Temporal Go worker hosting the workflows of SPEC §6
// (BookingSagaWorkflow, ReminderWorkflow, NoShowFollowupWorkflow,
// TenantOnboardingWorkflow) plus the Go activities that reach
// booking/payments/identity via Dapr service invocation and send
// notifications via the Dapr smtp/twilio output bindings.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/activities"
	"github.com/opendesk/notification-worker/internal/config"
	"github.com/opendesk/notification-worker/internal/daprc"
	"github.com/opendesk/notification-worker/internal/httpapi"
	"github.com/opendesk/notification-worker/internal/notifyoutbox"
	"github.com/opendesk/notification-worker/internal/pacer"
	"github.com/opendesk/notification-worker/internal/packs"
	"github.com/opendesk/notification-worker/internal/signals"
	"github.com/opendesk/notification-worker/internal/store"
	"github.com/opendesk/notification-worker/internal/webhooks"
	"github.com/opendesk/notification-worker/internal/workflows"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"go.uber.org/zap"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	logger, err := zap.NewProduction()
	if err != nil {
		return err
	}
	defer logger.Sync() //nolint:errcheck

	cfg := config.Load()

	tc, err := client.Dial(client.Options{
		HostPort:  cfg.TemporalHostPort,
		Namespace: cfg.TemporalNamespace,
		Logger:    &temporalZapAdapter{log: logger},
	})
	if err != nil {
		return fmt.Errorf("dial temporal: %w", err)
	}
	defer tc.Close()

	// Industry workflow packs (SPEC-CRM §C): loaded + validated at boot.
	registry, err := packs.Load(cfg.IndustriesDir)
	if err != nil {
		return fmt.Errorf("load industry packs: %w", err)
	}
	logger.Info("industry packs loaded",
		zap.String("dir", cfg.IndustriesDir), zap.Strings("packs", registry.IDs()))

	daprClient := daprc.New(cfg.DaprHost, cfg.DaprHTTPPort)
	acts := activities.New(daprClient, cfg.BookingAppID, cfg.PaymentsAppID, cfg.IdentityAppID,
		cfg.SMTPBinding, cfg.TwilioBinding, cfg.SMTPFrom, cfg.TwilioFrom, cfg.OpenSearchURL,
		activities.IndustryDeps{
			Packs:          registry,
			KnowledgeAppID: cfg.KnowledgeAppID,
			CRMSyncAppID:   cfg.CRMSyncAppID,
			PubSubName:     cfg.PubSubName,
			CRMEventsTopic: cfg.CRMEventsTopic,
		}, logger)
	// Messaging channel routing (Nigeria channel): per-tenant provider
	// selection for the sms/email channels — see
	// docs/integrations/messaging-channels.md.
	channelRouter, err := activities.NewChannelRouter(cfg.MessagingChannels, cfg.TenantChannelMap)
	if err != nil {
		return fmt.Errorf("messaging channel routing: %w", err)
	}
	acts.Channels = channelRouter
	logger.Info("messaging channel routing configured",
		zap.String("email_provider", channelRouter.Provider("email", "")),
		zap.String("sms_provider", channelRouter.Provider("sms", "")))
	// User-facing base URL for waitlist claim links (SPEC-W3 §3).
	acts.PublicBaseURL = cfg.PublicBaseURL
	// Outbound CPS pacer + sender rotation (VOICE-SCALING §4 telephony):
	// paces every workflow-driven outbound send via NotifyPaced.
	acts.Pacer = pacer.New(pacer.Config{
		CPS:         cfg.OutboundCPS,
		Burst:       cfg.OutboundBurst,
		Backend:     cfg.PacerBackend,
		RedisAddr:   cfg.RedisAddr,
		FromNumbers: cfg.OutboundFromNumbers,
	}, logger)
	logger.Info("outbound pacer configured",
		zap.Float64("cps", cfg.OutboundCPS), zap.Int("burst", cfg.OutboundBurst),
		zap.String("backend", cfg.PacerBackend), zap.Int("from_numbers", len(cfg.OutboundFromNumbers)))
	// GDPR export/erase configuration (SPEC-W3 §2 innovation 13).
	acts.Gdpr = activities.GdprDeps{
		ConversationAppID: cfg.ConversationAppID,
		PubSubName:        cfg.PubSubName,
		PrivacyTopic:      cfg.PrivacyEventsTopic,
		S3Endpoint:        cfg.S3Endpoint,
		S3Region:          cfg.S3Region,
		S3AccessKey:       cfg.S3AccessKey,
		S3SecretKey:       cfg.S3SecretKey,
		S3ExportsBucket:   cfg.S3ExportsBucket,
	}

	w := worker.New(tc, cfg.TemporalTaskQueue, worker.Options{})

	// Workflows (SPEC §6)
	w.RegisterWorkflowWithOptions(workflows.BookingSagaWorkflow, workflow.RegisterOptions{Name: "BookingSagaWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.ReminderWorkflow, workflow.RegisterOptions{Name: "ReminderWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.NoShowFollowupWorkflow, workflow.RegisterOptions{Name: "NoShowFollowupWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.TenantOnboardingWorkflow, workflow.RegisterOptions{Name: "TenantOnboardingWorkflow"})
	// SPEC-W3 §3 innovation 7: waitlist backfill on BookingCancelled.
	w.RegisterWorkflowWithOptions(workflows.WaitlistBackfillWorkflow, workflow.RegisterOptions{Name: "WaitlistBackfillWorkflow"})
	// SPEC-W3 §3 innovation 12: digital-twin 24h cleanup.
	w.RegisterWorkflowWithOptions(workflows.TwinCleanupWorkflow, workflow.RegisterOptions{Name: "TwinCleanupWorkflow"})

	// Industry pack workflows (SPEC-CRM §C2)
	w.RegisterWorkflowWithOptions(workflows.ClinicIntakeWorkflow, workflow.RegisterOptions{Name: "ClinicIntakeWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.SalonDepositWorkflow, workflow.RegisterOptions{Name: "SalonDepositWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.ConsultancyFollowupWorkflow, workflow.RegisterOptions{Name: "ConsultancyFollowupWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.SupportEscalationWorkflow, workflow.RegisterOptions{Name: "SupportEscalationWorkflow"})

	// Activities
	w.RegisterActivityWithOptions(acts.ReserveSlot, activity.RegisterOptions{Name: workflows.ActivityReserveSlot})
	w.RegisterActivityWithOptions(acts.HoldDeposit, activity.RegisterOptions{Name: workflows.ActivityHoldDeposit})
	w.RegisterActivityWithOptions(acts.ConfirmBooking, activity.RegisterOptions{Name: workflows.ActivityConfirmBooking})
	w.RegisterActivityWithOptions(acts.SendConfirmation, activity.RegisterOptions{Name: workflows.ActivitySendConfirmation})
	w.RegisterActivityWithOptions(acts.SendReminder, activity.RegisterOptions{Name: workflows.ActivitySendReminder})
	w.RegisterActivityWithOptions(acts.ReleaseSlot, activity.RegisterOptions{Name: workflows.ActivityReleaseSlot})
	w.RegisterActivityWithOptions(acts.VoidHold, activity.RegisterOptions{Name: workflows.ActivityVoidHold})
	w.RegisterActivityWithOptions(acts.GetBookingStatus, activity.RegisterOptions{Name: workflows.ActivityGetBookingStatus})
	w.RegisterActivityWithOptions(acts.MarkNoShow, activity.RegisterOptions{Name: workflows.ActivityMarkNoShow})
	w.RegisterActivityWithOptions(acts.SendNoShowFollowup, activity.RegisterOptions{Name: workflows.ActivitySendNoShowFollow})
	w.RegisterActivityWithOptions(acts.EnsureKeycloakGroup, activity.RegisterOptions{Name: workflows.ActivityEnsureKeycloakGroup})
	w.RegisterActivityWithOptions(acts.EnsurePermifyTenant, activity.RegisterOptions{Name: workflows.ActivityEnsurePermifyTenant})
	w.RegisterActivityWithOptions(acts.SeedTenantData, activity.RegisterOptions{Name: workflows.ActivitySeedTenantData})
	w.RegisterActivityWithOptions(acts.EnsureSearchAlias, activity.RegisterOptions{Name: workflows.ActivityEnsureSearchAlias})
	// Waitlist backfill activities (SPEC-W3 §3 innovation 7)
	w.RegisterActivityWithOptions(acts.ListWaitlistEntries, activity.RegisterOptions{Name: workflows.ActivityListWaitlistEntries})
	w.RegisterActivityWithOptions(acts.SendWaitlistClaimNotification, activity.RegisterOptions{Name: workflows.ActivitySendWaitlistClaimNote})
	// Outbound pacing wrapper: all workflow sends go through it (VOICE-SCALING §4).
	w.RegisterActivityWithOptions(acts.NotifyPaced, activity.RegisterOptions{Name: workflows.ActivityNotifyPaced})
	// SPEC-W8 A2: geo-targeted campaign sends (scheduled by booking-service's
	// GeoCampaignWorkflow via NotifyPaced kind geo_campaign).
	w.RegisterActivityWithOptions(acts.SendGeoCampaignMessage, activity.RegisterOptions{Name: workflows.ActivitySendGeoCampaignMessage})
	// SPEC-W11 Part B §5: incident outreach sends (scheduled by
	// booking-service's IncidentAlertWorkflow via NotifyPaced kind
	// incident_alert with priority — the pacer fast-lane).
	w.RegisterActivityWithOptions(acts.SendIncidentAlert, activity.RegisterOptions{Name: workflows.ActivitySendIncidentAlert})
	// Digital-twin cleanup activity (SPEC-W3 §3 innovation 12)
	w.RegisterActivityWithOptions(acts.DeleteTwinTenant, activity.RegisterOptions{Name: workflows.ActivityDeleteTwinTenant})

	// Outbound webhook delivery (Wave 5 #10)
	w.RegisterWorkflowWithOptions(workflows.WebhookDeliveryWorkflow, workflow.RegisterOptions{Name: "WebhookDeliveryWorkflow"})
	w.RegisterActivityWithOptions(acts.DeliverWebhookHTTP, activity.RegisterOptions{Name: workflows.ActivityDeliverWebhookHTTP})
	w.RegisterActivityWithOptions(acts.UpdateWebhookDelivery, activity.RegisterOptions{Name: workflows.ActivityUpdateWebhookDelivery})

	// Industry pack activities (SPEC-CRM §C2)
	w.RegisterActivityWithOptions(acts.ApplyIndustryPack, activity.RegisterOptions{Name: workflows.ActivityApplyIndustryPack})
	w.RegisterActivityWithOptions(acts.VerifyDepositHold, activity.RegisterOptions{Name: workflows.ActivityVerifyDepositHold})
	w.RegisterActivityWithOptions(acts.SendDepositReminder, activity.RegisterOptions{Name: workflows.ActivitySendDepositReminder})
	w.RegisterActivityWithOptions(acts.ChargeNoShowFee, activity.RegisterOptions{Name: workflows.ActivityChargeNoShowFee})
	w.RegisterActivityWithOptions(acts.SendIntakeReminder, activity.RegisterOptions{Name: workflows.ActivitySendIntakeReminder})
	w.RegisterActivityWithOptions(acts.CreateStaffAlertTask, activity.RegisterOptions{Name: workflows.ActivityCreateStaffAlertTask})
	w.RegisterActivityWithOptions(acts.SendFollowupEmail, activity.RegisterOptions{Name: workflows.ActivitySendFollowupEmail})
	w.RegisterActivityWithOptions(acts.CreateCRMFollowupTask, activity.RegisterOptions{Name: workflows.ActivityCreateCRMFollowupTask})
	w.RegisterActivityWithOptions(acts.SendProposalReminder, activity.RegisterOptions{Name: workflows.ActivitySendProposalReminder})
	w.RegisterActivityWithOptions(acts.EscalateTicket, activity.RegisterOptions{Name: workflows.ActivityEscalateTicket})

	// Outbound webhook platform (Wave 5 #10): Postgres-backed subscriptions +
	// deliveries. Without DATABASE_URL the platform degrades to 503s on
	// /v1/webhooks and no dispatcher — the rest of the worker is unaffected.
	var webhookStore *store.Store
	if cfg.DatabaseURL != "" {
		st, err := store.New(context.Background(), cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("webhook store: %w", err)
		}
		webhookStore = st
		defer webhookStore.Close()
		acts.Webhooks = activities.WebhookDeps{Store: webhookStore, BookingAppID: cfg.BookingAppID}
		logger.Info("webhook platform enabled",
			zap.Bool("signing_required", cfg.WebhookSigningRequired))
	} else {
		logger.Warn("DATABASE_URL unset: webhook platform disabled (subscriptions/deliveries 503)")
	}

	// SPEC-W12 Agent B: DND 2442 + quiet-hours compliance guards. The DND
	// registry shares the notifications DB (no DATABASE_URL → store-less
	// guards pass marketing sends with a warn log, and /v1/dnd 503s).
	// Quiet-hours config is validated at boot and handed to marketing
	// workflows via their input (workflows.GuardedPacedSend).
	quietOverrides := map[string]string{}
	if cfg.QuietHoursOverrides != "" {
		if err := json.Unmarshal([]byte(cfg.QuietHoursOverrides), &quietOverrides); err != nil {
			return fmt.Errorf("QUIET_HOURS_OVERRIDES: invalid JSON object: %w", err)
		}
	}
	quietHours := pacer.QuietHoursConfig{
		DefaultWindow: cfg.QuietHoursDefault,
		Overrides:     quietOverrides,
		Timezone:      pacer.DefaultQuietHoursTimezone,
	}
	// Boot-time validation of the window + overrides (fail fast on typos).
	if _, _, err := pacer.QuietHoursOpenAt(time.Now(), "", quietHours); err != nil {
		return fmt.Errorf("quiet hours config: %w", err)
	}
	for ch := range quietOverrides {
		if _, _, err := pacer.QuietHoursOpenAt(time.Now(), ch, quietHours); err != nil {
			return fmt.Errorf("quiet hours override for channel %q: %w", ch, err)
		}
	}
	var dndChecker pacer.DNDChecker
	var dndStore httpapi.DNDStore
	if webhookStore != nil {
		dndChecker = webhookStore
		dndStore = webhookStore
	}
	acts.Guards = pacer.NewGuards(pacer.GuardConfig{
		DNDEnforcement: cfg.DNDEnforcement,
		DND:            dndChecker,
		QuietHours:     quietHours,
	}, logger)
	logger.Info("compliance guards configured (SPEC-W12)",
		zap.Bool("dnd_enforcement", cfg.DNDEnforcement),
		zap.Bool("dnd_store", dndChecker != nil),
		zap.String("quiet_hours_default", quietHours.DefaultWindow),
		zap.Int("quiet_hours_overrides", len(quietOverrides)))

	// HTTP sidecar: /healthz + /dev triggers + /v1/webhooks (Wave 5 #10)
	// + /v1/dnd (SPEC-W12).
	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.Port),
		Handler: httpapi.NewRouter(&httpapi.Server{
			Temporal:  tc,
			TaskQueue: cfg.TemporalTaskQueue,
			Log:       logger,
			Webhooks:  webhookStore,
			ResolveTenant: func(ctx context.Context, slug string) (httpapi.TenantRef, error) {
				var out struct {
					ID   string `json:"id"`
					Slug string `json:"slug"`
				}
				if err := daprClient.InvokeService(ctx, cfg.IdentityAppID, "v1/tenants/"+slug, nil, &out); err != nil {
					return httpapi.TenantRef{}, err
				}
				id, err := uuid.Parse(out.ID)
				if err != nil {
					return httpapi.TenantRef{}, fmt.Errorf("identity returned bad tenant id: %w", err)
				}
				return httpapi.TenantRef{ID: id, Slug: slug}, nil
			},
			WebhookSigningRequired: cfg.WebhookSigningRequired,
			DND:                    dndStore,
		}),
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("notification-worker http listening", zap.Int("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Booking-events → Temporal signal bridge (SPEC-CRM §C2): delivers
	// BookingCancelled/BookingNoShow to the pack + reminder child workflows.
	bridge := signals.New(strings.Split(cfg.KafkaBrokers, ","), cfg.BookingEventsTopic, cfg.SignalGroup, tc, logger,
		signals.WithBackfillStarter(tc, cfg.TemporalTaskQueue))
	defer bridge.Close() //nolint:errcheck
	go func() {
		if err := bridge.Run(ctx); err != nil {
			errCh <- fmt.Errorf("signal bridge: %w", err)
		}
	}()

	// Notifications outbox consumer (Wave 5 #7): delivers SendPortalCode and
	// future fire-and-forget notification commands via the smtp/twilio
	// bindings (booking-service publishes; this worker owns the bindings).
	outboxConsumer := notifyoutbox.New(
		strings.Split(cfg.KafkaBrokers, ","),
		cfg.NotificationsOutboxTopic, cfg.NotificationsOutboxGroup,
		notifyoutbox.BindingSender{
			Dapr: daprClient, SMTPBinding: cfg.SMTPBinding, TwilioBinding: cfg.TwilioBinding,
			SMTPFrom: cfg.SMTPFrom, TwilioFrom: cfg.TwilioFrom,
		}, logger)
	defer outboxConsumer.Close() //nolint:errcheck
	go func() {
		if err := outboxConsumer.Run(ctx); err != nil {
			errCh <- fmt.Errorf("notifications outbox consumer: %w", err)
		}
	}()

	// Outbound webhook dispatcher (Wave 5 #10): booking + conversation
	// events → matching subscriptions → WebhookDeliveryWorkflow per delivery.
	if webhookStore != nil {
		dispatcher := webhooks.New(
			strings.Split(cfg.KafkaBrokers, ","),
			[]string{cfg.BookingEventsTopic, cfg.ConversationEventsTopic},
			cfg.WebhookGroup, webhookStore, tc, cfg.TemporalTaskQueue, logger)
		defer dispatcher.Close() //nolint:errcheck
		go func() {
			if err := dispatcher.Run(ctx); err != nil {
				errCh <- fmt.Errorf("webhook dispatcher: %w", err)
			}
		}()
	}

	go func() {
		logger.Info("temporal worker starting",
			zap.String("task_queue", cfg.TemporalTaskQueue),
			zap.String("namespace", cfg.TemporalNamespace))
		if err := w.Run(worker.InterruptCh()); err != nil {
			errCh <- fmt.Errorf("worker run: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}

	logger.Info("shutting down")
	w.Stop()
	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

// temporalZapAdapter bridges Temporal's log.Logger to zap.
type temporalZapAdapter struct{ log *zap.Logger }

func (a *temporalZapAdapter) Debug(msg string, kv ...any) { a.log.Sugar().Debugw(msg, kv...) }
func (a *temporalZapAdapter) Info(msg string, kv ...any)  { a.log.Sugar().Infow(msg, kv...) }
func (a *temporalZapAdapter) Warn(msg string, kv ...any)  { a.log.Sugar().Warnw(msg, kv...) }
func (a *temporalZapAdapter) Error(msg string, kv ...any) { a.log.Sugar().Errorw(msg, kv...) }
