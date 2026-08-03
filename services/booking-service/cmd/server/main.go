// booking-service: catalog, availability engine, bookings + outbox,
// booking saga activities and the booking command consumer (SPEC §4/§6/§7).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/cache"
	"github.com/opendesk/booking-service/internal/config"
	"github.com/opendesk/booking-service/internal/consumer"
	"github.com/opendesk/booking-service/internal/daprc"
	"github.com/opendesk/booking-service/internal/devices"
	"github.com/opendesk/booking-service/internal/fieldcapture"
	"github.com/opendesk/booking-service/internal/geo"
	"github.com/opendesk/booking-service/internal/httpapi"
	"github.com/opendesk/booking-service/internal/incidents"
	"github.com/opendesk/booking-service/internal/leads"
	"github.com/opendesk/booking-service/internal/outbox"
	"github.com/opendesk/booking-service/internal/permify"
	"github.com/opendesk/booking-service/internal/referrals" // SPEC-W14 Agent B (additive import)
	"github.com/opendesk/booking-service/internal/store"
	"github.com/opendesk/booking-service/internal/temporalclient"
	"go.temporal.io/sdk/activity"
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

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, cfg.DatabaseURL, cfg.PGMaxConns)
	if err != nil {
		return err
	}
	defer st.Close()

	// SPEC-W14 Agent B — BEGIN (additive): the commission payout store,
	// shared by the payout/recon Temporal worker registration below and by
	// GET /v1/payouts. Gated on COMMISSIONS_ENABLED=true (contract §7).
	var payoutStore *referrals.PayoutStore
	if os.Getenv("COMMISSIONS_ENABLED") == "true" {
		ps, psErr := referrals.DialPayoutStore(ctx, cfg.DatabaseURL)
		if psErr != nil {
			logger.Error("commission payout store unavailable; payouts API + payout/recon workflows disabled", zap.Error(psErr))
		} else {
			payoutStore = ps
			defer payoutStore.Close()
		}
	}
	// SPEC-W14 Agent B — END

	daprClient := daprc.New(cfg.DaprHost, cfg.DaprHTTPPort)
	resolver := bookingops.NewTenantResolver(daprClient, cfg.IdentityAppID, cfg.IdentityCacheTTL, logger)

	// Temporal saga starter — optional at boot: when Temporal is unreachable
	// the service still accepts bookings (they stay `pending` and the saga
	// start is logged for reconciliation).
	var saga bookingops.SagaStarter
	var gdpr httpapi.GdprStarter
	var geoStarter geo.CampaignStarter
	var incidentStarter incidents.Starter
	tc, err := temporalclient.Dial(cfg.TemporalHostPort, cfg.TemporalNamespace, cfg.TemporalTaskQueue)
	if err != nil {
		logger.Warn("temporal unavailable at boot; saga starts will fail until redeploy",
			zap.String("host_port", cfg.TemporalHostPort), zap.Error(err))
	} else {
		defer tc.Close()
		saga = tc
		gdpr = tc
		geoStarter = tc
		incidentStarter = tc

		// SPEC-W8 A2: booking-service hosts the GeoCampaignWorkflow and its
		// DB activities on the shared opendesk-main task queue. Recipient
		// sends are scheduled as "NotifyPaced" activity tasks, which the
		// notification-worker picks up from the same queue (it owns the CPS
		// pacer + sender rotation).
		geoActs := &geo.CampaignActivities{Store: st, UsageTopic: cfg.UsageEventsTopic, Logger: logger}
		w := worker.New(tc.Underlying(), cfg.TemporalTaskQueue, worker.Options{})
		w.RegisterWorkflowWithOptions(geo.GeoCampaignWorkflow, workflow.RegisterOptions{Name: geo.WorkflowType})
		w.RegisterActivityWithOptions(geoActs.AudienceBatch, activity.RegisterOptions{Name: geo.ActivityGeoAudienceBatch})
		w.RegisterActivityWithOptions(geoActs.FilterUnsent, activity.RegisterOptions{Name: geo.ActivityGeoFilterUnsent})
		w.RegisterActivityWithOptions(geoActs.RecordSends, activity.RegisterOptions{Name: geo.ActivityGeoRecordSends})
		w.RegisterActivityWithOptions(geoActs.CompleteCampaign, activity.RegisterOptions{Name: geo.ActivityGeoCompleteCampaign})
		w.RegisterActivityWithOptions(geoActs.FailCampaign, activity.RegisterOptions{Name: geo.ActivityGeoFailCampaign})
		// SPEC-W11 Part B §5: incident outreach workflow — delegates the send
		// to the notification-worker paced fast-lane via NotifyPaced (kind
		// incident_alert, priority=true).
		w.RegisterWorkflowWithOptions(incidents.IncidentAlertWorkflow, workflow.RegisterOptions{Name: incidents.WorkflowTypeAlert})
		// SPEC-W14 Agent B — BEGIN (additive): commission payout + nightly
		// recon workflows on the same worker, plus the recon schedule
		// bootstrap (contract §5/§7). Uses the hoisted payoutStore (nil when
		// COMMISSIONS_ENABLED != true or the dial failed).
		// Ledger: Agent A's Postgres impl (reconciled after A landed).
		if payoutStore != nil {
			provider := referrals.ProviderFromEnv()
			payoutActs := &referrals.PayoutActivities{
				Store:      payoutStore,
				Ledger:     referrals.NewPostgresLedger(st), // Agent A's Postgres Ledger (SPEC-W14 reconciled)
				Provider:   provider,
				MinKobo:    referrals.MinPayoutFromEnv(),
				UsageTopic: cfg.UsageEventsTopic,
				Logger:     logger,
			}
			reconActs := &referrals.ReconActivities{
				Store:              payoutStore,
				Provider:           provider,
				UsageTopic:         cfg.UsageEventsTopic,
				NotificationsTopic: cfg.NotificationsTopic,
				Logger:             logger,
			}
			w.RegisterWorkflowWithOptions(referrals.CommissionPayoutWorkflow, workflow.RegisterOptions{Name: referrals.WorkflowTypePayout})
			w.RegisterWorkflowWithOptions(referrals.CommissionReconWorkflow, workflow.RegisterOptions{Name: referrals.WorkflowTypeRecon})
			w.RegisterActivityWithOptions(payoutActs.ExecuteTransfer, activity.RegisterOptions{Name: referrals.ActivityPayoutTransfer})
			w.RegisterActivityWithOptions(payoutActs.FinalizePaid, activity.RegisterOptions{Name: referrals.ActivityPayoutMarkPaid})
			w.RegisterActivityWithOptions(payoutActs.MarkFailed, activity.RegisterOptions{Name: referrals.ActivityPayoutMarkFailed})
			w.RegisterActivityWithOptions(reconActs.FetchCandidates, activity.RegisterOptions{Name: referrals.ActivityReconFetch})
			w.RegisterActivityWithOptions(reconActs.CheckTransfer, activity.RegisterOptions{Name: referrals.ActivityReconCheck})
			if err := tc.EnsureCommissionReconSchedule(ctx, os.Getenv("RECON_CRON")); err != nil {
				logger.Error("commission recon schedule bootstrap failed", zap.Error(err))
			} else {
				logger.Info("commission recon schedule ensured",
					zap.String("schedule_id", referrals.ReconScheduleID), zap.String("provider", provider.Name()))
			}
		}
		// SPEC-W14 Agent B — END
		if err := w.Start(); err != nil {
			logger.Error("geo campaign worker failed to start", zap.Error(err))
		} else {
			defer w.Stop()
			logger.Info("geo campaign worker started", zap.String("task_queue", cfg.TemporalTaskQueue))
		}
	}

	// SPEC-W8 A2 geospatial endpoints + optional Nominatim geocoding hook
	// (GEOCODE_ENABLED, off by default).
	geoHandlers := &geo.Handlers{
		Store:     st,
		Starter:   geoStarter,
		Geocoder:  geo.NewGeocoder(cfg.GeocodeEnabled, cfg.GeocodeBaseURL),
		BatchSize: cfg.GeoCampaignBatch,
		Log:       logger,
	}

	// Availability cache (SPEC-W3 §3) — nil when REDIS_ADDR is unset.
	availCache := cache.New(cfg.RedisAddr, cfg.CacheTTL, cfg.CacheStaleTTL, logger)
	if availCache.Enabled() {
		defer availCache.Close() //nolint:errcheck
		logger.Info("availability cache enabled", zap.String("redis_addr", cfg.RedisAddr), zap.Duration("ttl", cfg.CacheTTL))
	}

	ops := &bookingops.Service{
		Store:       st,
		Saga:        saga,
		EventsTopic: cfg.BookingEventsTopic,
		UsageTopic:  cfg.UsageEventsTopic,
		Logger:      logger,
		Cache:       availCache,
	}

	// Incidents service (SPEC-W11 Part B): ingest (Kafka consumer + webhook
	// ingest endpoint), signed dispatch via the Wave-5 delivery workflow and
	// critical/high auto-outreach via the paced priority lane.
	incidentSvc := &incidents.Service{
		Store:        st,
		Starter:      incidentStarter,
		AutoDispatch: cfg.IncidentAutoDispatch,
		UsageTopic:   cfg.UsageEventsTopic,
		Log:          logger,
	}

	// Leads service (SPEC-W13 Agent A): CAC lead capture with first-touch
	// attribution, the status machine emitting FunnelEvents to cac.events,
	// promo codes/redemption and campaign spend.
	leadSvc := &leads.Service{
		Store:          st,
		CACEventsTopic: cfg.CACEventsTopic,
		FirstTouchOnly: cfg.LeadAttributionFirstTouchOnly,
		Log:            logger,
	}

	// SPEC-W16 Agent B — BEGIN (additive): push device tokens (contract §1)
	// + the field offline-queue capture API (contract §4). Each dials a
	// small dedicated pool (PayoutStore idiom — the shared store.Store does
	// not expose its pool). A dial failure disables only these endpoints
	// (503), never the rest of the service.
	var deviceHandlers *devices.Handlers
	if ds, dsErr := devices.DialStore(ctx, cfg.DatabaseURL); dsErr != nil {
		logger.Error("device token store unavailable; /v1/devices + /internal/devices disabled", zap.Error(dsErr))
	} else {
		defer ds.Close()
		deviceHandlers = &devices.Handlers{Store: ds, Log: logger}
	}
	var fieldCaptureHandlers *fieldcapture.Handlers
	if fs, fsErr := fieldcapture.DialStore(ctx, cfg.DatabaseURL); fsErr != nil {
		logger.Error("field capture store unavailable; /v1/field/capture disabled", zap.Error(fsErr))
	} else {
		defer fs.Close()
		fieldCaptureHandlers = &fieldcapture.Handlers{
			Svc:        &fieldcapture.Service{Store: fs, Leads: leadSvc, Log: logger},
			BatchLimit: cfg.FieldCaptureBatchLimit,
			Log:        logger,
		}
	}
	// SPEC-W16 Agent B — END

	// Referrals + commissions service (SPEC-W14 Agent A): referral capture
	// with one-open-per-phone dedupe, the verify flow firing rules →
	// balanced double-entry postings (Postgres ledger today — the
	// TigerBeetle adapter seam is documented in referrals/ledger.go), the
	// §6 lead-conversion hook via the leads service and funnel hooks on
	// cac.events. COMMISSIONS_ENABLED=false disables the endpoints (503).
	var referralSvc *referrals.Service
	if cfg.CommissionsEnabled {
		referralSvc = &referrals.Service{
			Store:          st,
			Ledger:         referrals.NewPostgresLedger(st),
			Leads:          leadSvc,
			CACEventsTopic: cfg.CACEventsTopic,
			// SPEC-W14 Agent D (additive): referral_verified metering.
			UsageTopic: cfg.UsageEventsTopic,
			Log:        logger,
		}
	}

	// Outbox dispatcher goroutine: outbox → Dapr pubsub `pubsub-kafka` →
	// topic opendesk.booking.events.
	dispatcher := outbox.New(st, daprClient, cfg.PubSubName, cfg.OutboxPollInterval, logger)
	go dispatcher.Run(ctx)

	// Kafka command consumer (direct broker connection, NOT dapr, SPEC §4).
	var cmdConsumer *consumer.Consumer
	if cfg.ConsumerEnabled {
		cmdConsumer = consumer.New(cfg.KafkaBrokers, cfg.CommandsTopic, cfg.CommandsGroup, cfg.DLQTopic, ops, resolver, logger)
		go func() {
			if err := cmdConsumer.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("command consumer exited", zap.Error(err))
			}
		}()
		defer cmdConsumer.Close() //nolint:errcheck

		// GDPR erase consumer (SPEC-W3 §2): anonymizes contacts on
		// PrivacyEraseRequested tombstones from opendesk.privacy.events.
		privacyConsumer := consumer.NewPrivacy(cfg.KafkaBrokers, cfg.PrivacyEventsTopic, cfg.PrivacyGroup, cfg.DLQTopic, st, logger)
		go func() {
			if err := privacyConsumer.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("privacy consumer exited", zap.Error(err))
			}
		}()
		defer privacyConsumer.Close() //nolint:errcheck

		// SPEC-W11 Part B §2: IDP consumer (group booking-incidents) on
		// opendesk.incidents → idempotent persist + auto-dispatch/outreach.
		incidentsConsumer := incidents.NewConsumer(cfg.KafkaBrokers, cfg.IncidentsTopic, cfg.IncidentsGroup, cfg.DLQTopic, incidentSvc, logger)
		go func() {
			if err := incidentsConsumer.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("incidents consumer exited", zap.Error(err))
			}
		}()
		defer incidentsConsumer.Close() //nolint:errcheck
	}

	deps := httpapi.Deps{
		Store:             st,
		Ops:               ops,
		Resolver:          resolver,
		Authz:             permify.NewHTTPClient(cfg.PermifyURL),
		AuthzDisabled:     cfg.AuthzDisabled,
		AuthzOutagePolicy: cfg.AuthzOutagePolicy,
		Dapr:              daprClient,
		IdentityAppID:     cfg.IdentityAppID,
		Gdpr:              gdpr,
		Cache:             availCache,
		Logger:            logger,

		PortalSecret:       cfg.PortalSecret,
		PubSubName:         cfg.PubSubName,
		NotificationsTopic: cfg.NotificationsTopic,
		Geo:                geoHandlers,
		Incidents:          incidentSvc,
		Leads:              leadSvc,
		Referrals:          referralSvc,
		Payouts:            payoutStore,          // SPEC-W14 Agent B (additive): GET /v1/payouts
		Devices:            deviceHandlers,       // SPEC-W16 Agent B (additive): /v1/devices + /internal/devices
		FieldCapture:       fieldCaptureHandlers, // SPEC-W16 Agent B (additive): POST /v1/field/capture
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           httpapi.NewRouter(deps),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("booking-service listening", zap.Int("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	logger.Info("shutting down")
	return srv.Shutdown(shutCtx)
}
