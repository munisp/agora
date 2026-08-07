// notification-worker: Temporal worker + Dapr subscribers + outbound
// notifiers (SPEC §6). Boots the worker on task queue `opendesk-main`,
// registers the saga/reminder/no-show/onboarding workflows and activities,
// and subscribes to booking events for signal fan-out.
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
	"github.com/opendesk/notification-worker/internal/civicoutbox"
	"github.com/opendesk/notification-worker/internal/config"
	"github.com/opendesk/notification-worker/internal/daprc"
	"github.com/opendesk/notification-worker/internal/httpapi"
	"github.com/opendesk/notification-worker/internal/notifyoutbox"
	"github.com/opendesk/notification-worker/internal/pacer"
	"github.com/opendesk/notification-worker/internal/provider"
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

	daprClient := daprc.New(cfg.DaprHost, cfg.DaprHTTPPort)

	tc, err := client.Dial(client.Options{
		HostPort:  cfg.TemporalHostPort,
		Namespace: cfg.TemporalNamespace,
		Logger:    &temporalZapAdapter{log: logger},
	})
	if err != nil {
		return fmt.Errorf("temporal dial: %w", err)
	}
	defer tc.Close()

	acts := activities.New(activities.Deps{
		Dapr:           daprClient,
		Log:            logger,
		SearchAliasURL: cfg.OpenSearchURL,
	})

	w := worker.New(tc, cfg.TemporalTaskQueue, worker.Options{})

	// Workflows (SPEC §6 + SPEC-CRM §C2 industry packs).
	w.RegisterWorkflowWithOptions(workflows.BookingSagaWorkflow, workflow.RegisterOptions{Name: "BookingSagaWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.ReminderWorkflow, workflow.RegisterOptions{Name: "ReminderWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.NoShowFollowupWorkflow, workflow.RegisterOptions{Name: "NoShowFollowupWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.TenantOnboardingWorkflow, workflow.RegisterOptions{Name: "TenantOnboardingWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.SalonDepositWorkflow, workflow.RegisterOptions{Name: "SalonDepositWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.ClinicIntakeWorkflow, workflow.RegisterOptions{Name: "ClinicIntakeWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.ConsultancyFollowupWorkflow, workflow.RegisterOptions{Name: "ConsultancyFollowupWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.SupportEscalationWorkflow, workflow.RegisterOptions{Name: "SupportEscalationWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.WebhookDeliveryWorkflow, workflow.RegisterOptions{Name: "WebhookDeliveryWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.PacedSendWorkflow, workflow.RegisterOptions{Name: "PacedSendWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.CivicSLAWorkflow, workflow.RegisterOptions{Name: "CivicSLAWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.CivicStatusWorkflow, workflow.RegisterOptions{Name: "CivicStatusWorkflow"})
	w.RegisterWorkflowWithOptions(workflows.AudienceCampaignWorkflow, workflow.RegisterOptions{Name: "AudienceCampaignWorkflow"})

	// Activities.
	w.RegisterActivityWithOptions(acts.ReserveSlot, activity.RegisterOptions{Name: workflows.ActivityReserveSlot})
	w.RegisterActivityWithOptions(acts.HoldDeposit, activity.RegisterOptions{Name: workflows.ActivityHoldDeposit})
	w.RegisterActivityWithOptions(acts.ConfirmBooking, activity.RegisterOptions{Name: workflows.ActivityConfirmBooking})
	w.RegisterActivityWithOptions(acts.SendConfirmation, activity.RegisterOptions{Name: workflows.ActivitySendConfirmation})
	w.RegisterActivityWithOptions(acts.SendReminder, activity.RegisterOptions{Name: workflows.ActivitySendReminder})
	w.RegisterActivityWithOptions(acts.ReleaseSlot, activity.RegisterOptions{Name: workflows.ActivityReleaseSlot})
	w.RegisterActivityWithOptions(acts.VoidHold, activity.RegisterOptions{Name: workflows.ActivityVoidHold})
	w.RegisterActivityWithOptions(acts.GetBookingStatus, activity.RegisterOptions{Name: workflows.ActivityGetBookingStatus})
	w.RegisterActivityWithOptions(acts.EmitOpsAlert, activity.RegisterOptions{Name: workflows.ActivityEmitOpsAlert})
	w.RegisterActivityWithOptions(acts.MarkNoShow, activity.RegisterOptions{Name: workflows.ActivityMarkNoShow})
	w.RegisterActivityWithOptions(acts.SendNoShowFollowup, activity.RegisterOptions{Name: workflows.ActivitySendNoShowFollow})
	w.RegisterActivityWithOptions(acts.EnsureKeycloakGroup, activity.RegisterOptions{Name: workflows.ActivityEnsureKeycloakGroup})
	w.RegisterActivityWithOptions(acts.EnsurePermifyTenant, activity.RegisterOptions{Name: workflows.ActivityEnsurePermifyTenant})
	w.RegisterActivityWithOptions(acts.SeedTenantData, activity.RegisterOptions{Name: workflows.ActivitySeedTenantData})
	w.RegisterActivityWithOptions(acts.EnsureSearchAlias, activity.RegisterOptions{Name: workflows.ActivityEnsureSearchAlias})
	w.RegisterActivityWithOptions(acts.NotifyPaced, activity.RegisterOptions{Name: workflows.ActivityNotifyPaced})
	w.RegisterActivityWithOptions(acts.DeliverWebhook, activity.RegisterOptions{Name: workflows.ActivityDeliverWebhook})
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

	// SPEC-W32 WS-B: civic delivery ledger + SLA-breach escalation producer.
	// No DATABASE_URL → ledger degrades to log-only (same posture as the
	// webhook platform); CIVIC_ESCALATION_TOPIC="off" disables emission.
	acts.Civic = activities.CivicDeps{EscalationTopic: civicoutbox.TopicEnabled(cfg.CivicEscalationTopic)}
	if webhookStore != nil {
		acts.Civic.Ledger = webhookStore
	}
	if acts.Civic.EscalationTopic != "" {
		if brokers := strings.Split(cfg.KafkaBrokers, ","); len(brokers) > 0 && brokers[0] != "" {
			acts.Civic.Escalations = activities.NewKafkaTrajectoryProducer(brokers)
		} else {
			logger.Warn("CIVIC_ESCALATION_TOPIC set but no Kafka brokers; civic escalation emission disabled")
		}
	}

	// SPEC-W34 GF16: ops alert producer for saga compensation exhaustion
	// (orphaned deposit holds). Same posture as civic escalations:
	// OPS_ALERTS_TOPIC="off" or no brokers → CRITICAL-log-only.
	acts.Ops = activities.OpsAlertDeps{Topic: civicoutbox.TopicEnabled(cfg.OpsAlertsTopic)}
	if acts.Ops.Topic != "" {
		if brokers := strings.Split(cfg.KafkaBrokers, ","); len(brokers) > 0 && brokers[0] != "" {
			acts.Ops.Producer = activities.NewKafkaTrajectoryProducer(brokers)
		} else {
			logger.Warn("OPS_ALERTS_TOPIC set but no Kafka brokers; ops alerts degrade to CRITICAL-log-only")
		}
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

	// SPEC-W16 §1: push notification providers. FCM_MOCK=1 (default) is a
	// deterministic no-network mock; FCM_CREDENTIALS_JSON selects HTTP v1,
	// FCM_SERVER_KEY the legacy API. APNs is a documented STUB (interface +
	// config only): iOS tokens surface honest "not implemented" per-token
	// failures until the provider/apns.go TODO lands.
	fcm, err := provider.NewFCM(provider.FCMConfig{
		Mock:            cfg.FCMMock,
		ServerKey:       cfg.FCMServerKey,
		CredentialsJSON: cfg.FCMCredentialsJSON,
		ProjectID:       cfg.FCMProjectID,
		BaseURL:         cfg.FCMBaseURL,
	}, logger)
	if err != nil {
		return fmt.Errorf("fcm provider: %w", err)
	}
	apns := &provider.APNS{
		KeyID:  cfg.APNSKeyID,
		TeamID: cfg.APNSTeamID,
		KeyP8:  cfg.APNSKeyP8,
		Topic:  cfg.APNSTopic,
	}
	acts.Push = activities.PushDeps{Providers: map[string]provider.PushProvider{
		"fcm":  fcm,
		"apns": apns,
	}}
	logger.Info("push providers configured (SPEC-W16)",
		zap.Bool("fcm_mock", cfg.FCMMock),
		zap.Bool("fcm_configured", fcm.Configured()),
		zap.String("fcm_mode", fcmMode(cfg)),
		zap.Bool("apns_configured", apns.Configured()))

	// HTTP sidecar: /healthz + /dev triggers + /v1/webhooks (Wave 5 #10)
	// + /v1/dnd (SPEC-W12).
	audienceIntake := activities.NewAudienceIntake(daprClient, cfg.BookingAppID, tc, cfg.TemporalTaskQueue, strings.Split(cfg.KafkaBrokers, ","), logger)
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
			AudienceIntake:         audienceIntake,
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
	// SPEC-W19 integrator (additive): PacedSend commands (e.g. field-service
	// dispatch push) start one PacedSendWorkflow each on this task queue —
	// the send rides the NotifyPaced activity (CPS pacer + sender rotation
	// + SPEC-W12 guards), never a raw binding call.
	outboxConsumer := notifyoutbox.New(
		strings.Split(cfg.KafkaBrokers, ","),
		cfg.NotificationsOutboxTopic, cfg.NotificationsOutboxGroup,
		notifyoutbox.BindingSender{
			Dapr: daprClient, SMTPBinding: cfg.SMTPBinding, TwilioBinding: cfg.TwilioBinding,
			SMTPFrom: cfg.SMTPFrom, TwilioFrom: cfg.TwilioFrom,
		}, logger,
		notifyoutbox.WithStarter(tc, cfg.TemporalTaskQueue))
	defer outboxConsumer.Close() //nolint:errcheck
	go func() {
		if err := outboxConsumer.Run(ctx); err != nil {
			errCh <- fmt.Errorf("notifications outbox consumer: %w", err)
		}
	}()

	// Civic events consumer (SPEC-W32 WS-B): ReportReceived starts the
	// case's CivicSLAWorkflow; StatusChanged signals it and starts the
	// citizen status notification workflow; Merged cancels the merged
	// case's timers. CIVIC_EVENTS_TOPIC empty/"off" disables the consumer.
	if topic := civicoutbox.TopicEnabled(cfg.CivicEventsTopic); topic != "" {
		civicConsumer := civicoutbox.New(
			strings.Split(cfg.KafkaBrokers, ","),
			topic, cfg.CivicEventsGroup, tc, cfg.TemporalTaskQueue, logger,
			civicoutbox.WithNotifyChannel(cfg.CivicStatusChannel))
		defer civicConsumer.Close() //nolint:errcheck
		go func() {
			if err := civicConsumer.Run(ctx); err != nil {
				errCh <- fmt.Errorf("civic events consumer: %w", err)
			}
		}()
		logger.Info("civic events consumer enabled", zap.String("topic", topic),
			zap.String("group", cfg.CivicEventsGroup))
	} else {
		logger.Info("civic events consumer disabled (CIVIC_EVENTS_TOPIC off)")
	}

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

// fcmMode describes the active FCM auth mode for the boot log (SPEC-W16).
func fcmMode(cfg config.Config) string {
	switch {
	case cfg.FCMMock:
		return "mock"
	case cfg.FCMCredentialsJSON != "":
		return "http-v1"
	case cfg.FCMServerKey != "":
		return "legacy-server-key"
	default:
		return "unconfigured"
	}
}

// temporalZapAdapter bridges Temporal's log.Logger to zap.
type temporalZapAdapter struct{ log *zap.Logger }

func (a *temporalZapAdapter) Debug(msg string, kv ...any) { a.log.Sugar().Debugw(msg, kv...) }
func (a *temporalZapAdapter) Info(msg string, kv ...any)  { a.log.Sugar().Infow(msg, kv...) }
func (a *temporalZapAdapter) Warn(msg string, kv ...any)  { a.log.Sugar().Warnw(msg, kv...) }
func (a *temporalZapAdapter) Error(msg string, kv ...any) { a.log.Sugar().Errorw(msg, kv...) }
