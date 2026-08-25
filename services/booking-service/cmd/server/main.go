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

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/appgate"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/cache"
	"github.com/opendesk/booking-service/internal/campaignstudio" // SPEC-W19 integrator (additive import)
	"github.com/opendesk/booking-service/internal/civic"          // SPEC-W32 WS-A (additive import)
	"github.com/opendesk/booking-service/internal/config"
	"github.com/opendesk/booking-service/internal/consumer"
	"github.com/opendesk/booking-service/internal/crm360" // SPEC-W20 integrator (additive import)
	"github.com/opendesk/booking-service/internal/daprc"
	"github.com/opendesk/booking-service/internal/devices"
	"github.com/opendesk/booking-service/internal/fieldcapture"
	"github.com/opendesk/booking-service/internal/geo"
	"github.com/opendesk/booking-service/internal/helpdesk" // SPEC-W19 integrator (additive import)
	"github.com/opendesk/booking-service/internal/httpapi"
	"github.com/opendesk/booking-service/internal/incidents"
	"github.com/opendesk/booking-service/internal/leads"
	"github.com/opendesk/booking-service/internal/lending" // SPEC-W20 integrator (additive import)
	"github.com/opendesk/booking-service/internal/loyalty" // SPEC-W19 integrator (additive import)
	"github.com/opendesk/booking-service/internal/outbox"
	"github.com/opendesk/booking-service/internal/permify"
	"github.com/opendesk/booking-service/internal/referrals" // SPEC-W14 Agent B (additive import)
	"github.com/opendesk/booking-service/internal/socialpub"
	"github.com/opendesk/booking-service/internal/socialpub/provider" // SPEC-W21 integrator (additive imports)
	"github.com/opendesk/booking-service/internal/store"
	"github.com/opendesk/booking-service/internal/surveys" // SPEC-W20 integrator (additive import)
	"github.com/opendesk/booking-service/internal/temporalclient"
	"github.com/opendesk/booking-service/internal/workforce"  // SPEC-W20 integrator (additive import)
	"github.com/opendesk/booking-service/internal/workorders" // SPEC-W19 integrator (additive import)
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

	// SPEC-W19 integrator — BEGIN (additive): campaign-studio store, hoisted
	// like payoutStore because the Temporal worker block below registers the
	// StudioSendWorkflow + its outcome activity on the shared worker
	// (booking-service DOES run an in-process Temporal worker — see below).
	// STUDIO_DATABASE_URL falls back to DATABASE_URL (routes.go sketch).
	// A dial failure disables only /v1/studio (routes stay unregistered).
	var studioStore *campaignstudio.Store
	studioURL := cfg.StudioDatabaseURL
	if studioURL == "" {
		studioURL = cfg.DatabaseURL
	}
	if ss, ssErr := campaignstudio.DialStore(ctx, studioURL); ssErr != nil {
		logger.Error("campaign studio store unavailable; /v1/studio disabled", zap.Error(ssErr))
	} else {
		defer ss.Close()
		studioStore = ss
	}
	// SPEC-W19 integrator — END (hoisted half; the remaining three app
	// stores are dialed next to the W16 stores below)

	daprClient := daprc.New(cfg.DaprHost, cfg.DaprHTTPPort)
	// SPEC-W42 Coder G: IDENTITY_BASE_URL (default empty) switches tenant
	// resolution to a direct HTTP GET, removing the hard daprd dependency
	// from the booking write path (used by tests and no-Dapr dev). Empty =
	// unchanged Dapr behavior.
	// SPEC-W44 W-B (K2): identity's internal endpoints + tenant-resolution
	// service call are internal-token gated (CODER-I2). Unset is a boot WARN
	// — the identity calls then answer 503/401 — never a startup error
	// (other gates own that).
	if cfg.IdentityInternalToken == "" {
		logger.Warn("IDENTITY_INTERNAL_TOKEN unset — booking→identity service calls (entitlement check, tenant resolution) will fail once identity enforces internauth (503/401)")
	}
	resolver := bookingops.NewTenantResolver(daprClient, cfg.IdentityAppID, cfg.IdentityCacheTTL, logger,
		bookingops.WithIdentityBaseURL(cfg.IdentityBaseURL),
		bookingops.WithInternalToken(cfg.IdentityInternalToken))
	if cfg.IdentityBaseURL != "" {
		logger.Info("IDENTITY_BASE_URL set: tenant resolution uses direct HTTP (no Dapr sidecar required)",
			zap.String("identity_base_url", cfg.IdentityBaseURL))
	}

	// SPEC-W18 Agent D — BEGIN (additive): app entitlement gate (contract
	// §4). APP_GATE_ENABLED=false is the DEFAULT and keeps the gate a pure
	// pass-through — production behavior is UNCHANGED unless opted in. When
	// enabled, /v1/leads (catalog app_id "cac") is gated against identity's
	// GET /internal/entitlements/check via the Dapr sidecar with a 60s TTL
	// cache + singleflight, failing closed (503 + Retry-After) on
	// entitlement outages (docs/app-developer-guide.md).
	appGate := appgate.New(appgate.Options{
		Enabled:       cfg.AppGateEnabled,
		IdentityAppID: cfg.IdentityAppID,
		BaseURL:       fmt.Sprintf("http://%s:%d", cfg.DaprHost, cfg.DaprHTTPPort),
		CacheTTL:      cfg.AppGateCacheTTL,
		Logger:        logger,
		// SPEC-W44 W-B (K2): X-Internal-Token on GET /internal/entitlements/check.
		InternalToken: cfg.IdentityInternalToken,
	})
	if cfg.AppGateEnabled {
		logger.Info("app entitlement gate ENABLED (APP_GATE_ENABLED=true)",
			zap.String("identity_app_id", cfg.IdentityAppID), zap.Duration("cache_ttl", cfg.AppGateCacheTTL))
	}
	// SPEC-W18 Agent D — END

	// Temporal saga starter — optional at boot: when Temporal is unreachable
	// the service still accepts bookings (they stay `pending` and the saga
	// start is logged for reconciliation).
	var saga bookingops.SagaStarter
	var gdpr httpapi.GdprStarter
	var geoStarter geo.CampaignStarter
	var incidentStarter incidents.Starter
	// SPEC-W19 integrator (additive): campaign-studio send starter. nil when
	// Temporal is unreachable → journey send steps defer (sends_deferred)
	// instead of erroring (campaignstudio.Handlers contract).
	var studioStarter campaignstudio.SendStarter
	// SPEC-W43 K-08: retry the Temporal dial with backoff — a slow Temporal
	// pod must not permanently disable saga starts at boot.
	tc, err := temporalclient.DialWithRetry(ctx, cfg.TemporalHostPort, cfg.TemporalNamespace, cfg.TemporalTaskQueue, 5, time.Second)
	if err != nil {
		logger.Warn("temporal unavailable at boot (after retries); saga starts will fail until redeploy",
			zap.String("host_port", cfg.TemporalHostPort), zap.Error(err))
	} else {
		defer tc.Close()
		saga = tc
		gdpr = tc
		geoStarter = tc
		incidentStarter = tc
		// SPEC-W19 integrator (additive): the studio step endpoint starts
		// StudioSendWorkflow batches via this starter.
		studioStarter = campaignstudio.TemporalStarter{Client: tc.Underlying(), TaskQueue: cfg.TemporalTaskQueue}

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
			// W39 SIM-002: the payout provider fails closed — PAYOUT_MOCK
			// defaults OFF; without a real rail (PAYOUT_PROVIDER_BASE_URL /
			// PAYOUT_PROVIDER_SECRET) the payout + recon workflows are NOT
			// registered (no payout may be marked sent without a real
			// rail) instead of silently simulating money movement.
			provider, providerErr := referrals.ProviderFromEnv()
			if providerErr != nil {
				logger.Error("payout provider not configured; commission payout + recon workflows disabled (set PAYOUT_PROVIDER_BASE_URL/PAYOUT_PROVIDER_SECRET, or PAYOUT_MOCK=1 for the dev simulation)",
					zap.Error(providerErr))
			} else {
				payoutActs := &referrals.PayoutActivities{
					Store:      payoutStore,
					Ledger:     referrals.NewPostgresLedger(st), // Agent A's Postgres Ledger (SPEC-W14 reconciled)
					Provider:   provider,
					MinKobo:    referrals.MinPayoutFromEnv(),
					UsageTopic: cfg.UsageEventsTopic,
					Logger:     logger,
					// SPEC-W44 W-B/S1-F7-08: COMMISSION_MATURITY_SECONDS (default 0).
					Maturity: time.Duration(cfg.CommissionMaturitySeconds) * time.Second,
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
				// SPEC-W44 W-B/S1-F7-08: the nightly recon feeds matured
				// balances into the payout queue first (non-fatal on failure).
				w.RegisterActivityWithOptions(payoutActs.FeedMatured, activity.RegisterOptions{Name: referrals.ActivityPayoutFeed})
				w.RegisterActivityWithOptions(reconActs.FetchCandidates, activity.RegisterOptions{Name: referrals.ActivityReconFetch})
				w.RegisterActivityWithOptions(reconActs.CheckTransfer, activity.RegisterOptions{Name: referrals.ActivityReconCheck})
				if err := tc.EnsureCommissionReconSchedule(ctx, os.Getenv("RECON_CRON")); err != nil {
					logger.Error("commission recon schedule bootstrap failed", zap.Error(err))
				} else {
					logger.Info("commission recon schedule ensured",
						zap.String("schedule_id", referrals.ReconScheduleID), zap.String("provider", provider.Name()))
				}
			}
		}
		// SPEC-W14 Agent B — END
		// SPEC-W19 integrator — BEGIN (additive): campaign-studio send
		// workflow + outcome activity on the same in-process worker (the
		// step endpoint works via the Starter even without this; the
		// registration is required for queued sends to actually dispatch).
		if studioStore != nil {
			campaignstudio.RegisterWorker(w, &campaignstudio.SendActivities{Store: studioStore, Logger: logger})
		}
		// SPEC-W19 integrator — END
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

	// SPEC-W32 WS-A — BEGIN (additive): the civic reporting module — public
	// citizen intake (throttled, honeypot-guarded), operator triage console
	// and CloudEvents on opendesk.civic.events.v1 via the outbox. Shares the
	// main store (civic tables are bootstrapped by store.New).
	civicSvc := &civic.Service{
		Store:       st,
		EventsTopic: cfg.CivicEventsTopic,
		RatePerHour: cfg.CivicPublicRatePerHour,
		RatePerDay:  cfg.CivicPublicRatePerDay,
		Log:         logger,
	}
	// SPEC-W32 WS-A — END

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
			Svc:        &fieldcapture.Service{Store: fs, Leads: leadSvc, Civic: civicSvc, Log: logger},
			BatchLimit: cfg.FieldCaptureBatchLimit,
			Log:        logger,
		}
	}
	// SPEC-W16 Agent B — END

	// SPEC-W19 integrator — BEGIN (additive): the helpdesk, field-service
	// and loyalty app stores (campaign-studio is hoisted above for the
	// Temporal worker registration). Same DialStore idiom as W16 — small
	// dedicated pools; a dial failure disables only that app's routes
	// (httpapi leaves the group unregistered on nil Deps), never the rest
	// of the service. The tenant/user accessors and perms middleware are
	// attached by httpapi.NewRouter (its context keys are package-private).
	var helpdeskDeps *helpdesk.Deps
	if hs, hsErr := helpdesk.DialStore(ctx, cfg.DatabaseURL, cfg.HelpdeskDBMaxConns); hsErr != nil {
		logger.Error("helpdesk store unavailable; /v1/helpdesk disabled", zap.Error(hsErr))
	} else {
		defer hs.Close()
		helpdeskDeps = &helpdesk.Deps{
			Store:       hs,
			Log:         logger,
			EventsTopic: cfg.HelpdeskEventsTopic,
			UsageTopic:  cfg.HelpdeskUsageTopic,
		}
	}
	var workordersDeps *workorders.Deps
	if ws, wsErr := workorders.DialStore(ctx, cfg.DatabaseURL); wsErr != nil {
		logger.Error("work orders store unavailable; /v1/field-service disabled", zap.Error(wsErr))
	} else {
		defer ws.Close()
		workordersDeps = &workorders.Deps{
			Store:              ws,
			Resolver:           resolver,
			Logger:             logger,
			NotificationsTopic: cfg.WorkordersNotificationsTopic,
			UsageTopic:         cfg.WorkordersUsageTopic,
			FSMEventsTopic:     cfg.WorkordersFSMEventsTopic,
		}
	}
	var loyaltyDeps *loyalty.Deps
	if ls, lsErr := loyalty.DialStore(ctx, cfg.DatabaseURL); lsErr != nil {
		logger.Error("loyalty store unavailable; /v1/loyalty disabled", zap.Error(lsErr))
	} else {
		defer ls.Close()
		loyaltyDeps = &loyalty.Deps{
			Store:       ls,
			Log:         logger,
			EventsTopic: cfg.LoyaltyEventsTopic,
			UsageTopic:  cfg.UsageEventsTopic, // SPEC-W19 Agent C: metering rides USAGE_EVENTS_TOPIC
		}
	}
	var studioDeps *campaignstudio.Deps
	if studioStore != nil {
		studioDeps = &campaignstudio.Deps{
			Store:         studioStore,
			Log:           logger,
			Starter:       studioStarter, // nil when Temporal was unreachable at boot → send steps defer
			UsageTopic:    cfg.UsageEventsTopic,
			EventsTopic:   cfg.StudioEventsTopic,
			StepBatchSize: cfg.StudioStepBatch,
		}
	}
	// SPEC-W19 integrator — END

	// SPEC-W20 integrator — BEGIN (additive): the crm-360, surveys, lending
	// and workforce app stores. Same DialStore idiom as W19 — small
	// dedicated pools; a dial failure disables only that app's routes
	// (httpapi leaves the group unregistered on nil Deps), never the rest
	// of the service. The tenant/user accessors and perms middleware are
	// attached by httpapi.NewRouter (its context keys are package-private).
	var crm360Deps *crm360.Deps
	if cs, csErr := crm360.DialStore(ctx, cfg.DatabaseURL); csErr != nil {
		logger.Error("crm360 store unavailable; /v1/crm disabled", zap.Error(csErr))
	} else {
		defer cs.Close()
		crm360Deps = &crm360.Deps{
			Store:           cs,
			Resolver:        resolver,
			Logger:          logger,
			CRMEventsTopic:  cfg.CRM360EventsTopic,
			ConsentResolver: nil, // consent lives in identity's DB — no clean seam yet; profiles degrade to consent=null by design
		}
	}
	var surveysDeps *surveys.Deps
	surveysURL := cfg.SurveysDatabaseURL
	if surveysURL == "" {
		surveysURL = cfg.DatabaseURL
	}
	if ss, ssErr := surveys.DialStore(ctx, surveysURL); ssErr != nil {
		logger.Error("surveys store unavailable; /v1/surveys disabled", zap.Error(ssErr))
	} else {
		defer ss.Close()
		surveysDeps = &surveys.Deps{
			Store:              ss,
			Resolver:           resolver,
			Logger:             logger,
			NotificationsTopic: cfg.SurveysNotificationsTopic,
			UsageTopic:         cfg.UsageEventsTopic, // SPEC-W20 Agent B: metering rides USAGE_EVENTS_TOPIC
			EventsTopic:        cfg.SurveysEventsTopic,
			PublicBaseURL:      cfg.SurveysPublicBaseURL,
		}
	}
	var lendingDeps *lending.Deps
	if ls, lsErr := lending.DialStore(ctx, cfg.DatabaseURL); lsErr != nil {
		logger.Error("lending store unavailable; /v1/lending disabled", zap.Error(lsErr))
	} else {
		defer ls.Close()
		// SPEC-W44 W-B/F16-1: credit-bureau fallback visibility — wire the
		// logger and log the scoring posture once at boot (WARN when
		// CREDIT_BUREAU_URL is unset; the per-fallback WARN/DEBUG rides the
		// score path).
		lending.SetSidecarLogger(logger)
		lending.LogCreditBureauBootStatus()
		lendingDeps = &lending.Deps{
			Store:       ls,
			Resolver:    resolver,
			Logger:      logger,
			EventsTopic: cfg.LendingEventsTopic,
			UsageTopic:  cfg.UsageEventsTopic, // SPEC-W20 Agent C: metering rides USAGE_EVENTS_TOPIC
			KYCURL:      cfg.LendingKYCURL,    // empty = override-only approval mode (documented in docs/apps/lending.md)
			// SPEC-W44 W-B/S1-F7-07: kyc_override approve role gate.
			KYCOverrideRoles: cfg.LendingKYCOverrideRoles,
			// W39 SIM-001: the live rail is "configured" only when the TB
			// bridge URL is set; otherwise Disburse fails closed unless
			// ALLOW_MOCK_RAILS=1 opts into the simulation.
			RealRailConfigured: os.Getenv(consumer.EnvLendingTBBridgeURL) != "",
		}
	}
	var workforceDeps *workforce.Deps
	if ws, wsErr := workforce.DialStore(ctx, cfg.DatabaseURL); wsErr != nil {
		logger.Error("workforce store unavailable; /v1/workforce disabled", zap.Error(wsErr))
	} else {
		defer ws.Close()
		workforceDeps = &workforce.Deps{
			Store:       ws,
			Resolver:    resolver,
			Logger:      logger,
			EventsTopic: cfg.WorkforceEventsTopic,
		}
	}
	// SPEC-W20 integrator — END

	// SPEC-W21 integrator — BEGIN (additive): the social-publisher store.
	// Same DialStore idiom as W19/W20 — a small dedicated pool; a dial
	// failure disables only this app's routes (httpapi leaves the group
	// unregistered on nil Deps), never the rest of the service. Providers
	// resolve through the mock switches: the deterministic mock is the
	// zero-config default (both switches truthy); *_MOCK=0 selects the
	// honest real-API stub ("not configured" until the credential
	// follow-up lands — docs/apps/social-publisher.md).
	var socialDeps *socialpub.Deps
	if sp, spErr := socialpub.DialStore(ctx, cfg.DatabaseURL); spErr != nil {
		logger.Error("socialpub store unavailable; /v1/social disabled", zap.Error(spErr))
	} else {
		defer sp.Close()
		publishers := map[string]provider.Publisher{}
		for id, mockEnv := range map[string]string{
			"meta":   cfg.MetaMock,
			"tiktok": cfg.TikTokMock,
			"x":      cfg.XMock,
		} {
			if pub, ok := provider.New(id, provider.MockEnabled(cfg.SocialMock, mockEnv)); ok {
				publishers[id] = pub
			}
		}
		socialDeps = &socialpub.Deps{
			Store:       sp,
			Log:         logger,
			EventsTopic: cfg.SocialEventsTopic,
			UsageTopic:  cfg.UsageEventsTopic, // SPEC-W21 Agent B: social_ad_launched metering rides USAGE_EVENTS_TOPIC
			Publishers:  publishers,
		}
	}
	// SPEC-W21 integrator — END

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

	// SPEC-W43 K-08: pending-booking sweeper — periodically re-drives
	// StartBookingSaga for `pending` rows whose saga start failed at create
	// (idempotent by deterministic workflow id). On by default; requires a
	// Temporal client.
	if cfg.BookingSweeperEnabled {
		if saga != nil {
			sweeper := &bookingops.Sweeper{
				Store:    st,
				Saga:     saga,
				Log:      logger,
				Interval: cfg.BookingSweeperInterval,
				ResolveSlug: func(ctx context.Context, tenantID uuid.UUID) string {
					site, err := st.GetSiteByTenant(ctx, tenantID)
					if err != nil {
						return "" // slug unknown; the saga start is still idempotent
					}
					return site.TenantSlug
				},
			}
			go sweeper.Run(ctx)
			logger.Info("pending-booking saga sweeper enabled",
				zap.Duration("interval", cfg.BookingSweeperInterval))
		} else {
			logger.Warn("pending-booking sweeper enabled but Temporal is unavailable; sweeper inert until redeploy")
		}
	}

	// SPEC-W43 K-09: per-consumer liveness registry read by /healthz — a
	// fatally exited consumer (Run returned with the root context alive)
	// clears its flag, turning /healthz into a 503 so the pod is restarted.
	consumerHealth := httpapi.NewConsumerHealth()

	// Outbox dispatcher goroutine: outbox → Dapr pubsub `pubsub-kafka` →
	// topic opendesk.booking.events. SPEC-W44 W-B/F15-13: three consecutive
	// failed dispatch cycles clear the health flag → /healthz 503 (a wedged
	// sidecar must page, not sit silently while the outbox backs up);
	// recovery restores it.
	dispatcher := outbox.New(st, daprClient, cfg.PubSubName, cfg.OutboxPollInterval, logger).
		WithHealth(consumerHealth.Register("outbox-dispatcher"))
	go dispatcher.Run(ctx)

	// Kafka command consumer (direct broker connection, NOT dapr, SPEC §4).
	var cmdConsumer *consumer.Consumer
	if cfg.ConsumerEnabled {
		cmdConsumer = consumer.New(cfg.KafkaBrokers, cfg.CommandsTopic, cfg.CommandsGroup, cfg.DLQTopic, ops, resolver, logger)
		cmdHealth := consumerHealth.Register("commands")
		go func() {
			if err := cmdConsumer.Run(ctx); err != nil && ctx.Err() == nil {
				cmdHealth.Store(false)
				logger.Error("command consumer exited; liveness flag cleared", zap.Error(err))
			}
		}()
		defer cmdConsumer.Close() //nolint:errcheck

		// GDPR erase consumer (SPEC-W3 §2): anonymizes contacts on
		// PrivacyEraseRequested tombstones from opendesk.privacy.events.
		privacyConsumer := consumer.NewPrivacy(cfg.KafkaBrokers, cfg.PrivacyEventsTopic, cfg.PrivacyGroup, cfg.DLQTopic, st, logger)
		privacyHealth := consumerHealth.Register("privacy")
		go func() {
			if err := privacyConsumer.Run(ctx); err != nil && ctx.Err() == nil {
				privacyHealth.Store(false)
				logger.Error("privacy consumer exited; liveness flag cleared", zap.Error(err))
			}
		}()
		defer privacyConsumer.Close() //nolint:errcheck

		// SPEC-W11 Part B §2: IDP consumer (group booking-incidents) on
		// opendesk.incidents → idempotent persist + auto-dispatch/outreach.
		incidentsConsumer := incidents.NewConsumer(cfg.KafkaBrokers, cfg.IncidentsTopic, cfg.IncidentsGroup, cfg.DLQTopic, incidentSvc, logger)
		incidentsHealth := consumerHealth.Register("incidents")
		go func() {
			if err := incidentsConsumer.Run(ctx); err != nil && ctx.Err() == nil {
				incidentsHealth.Store(false)
				logger.Error("incidents consumer exited; liveness flag cleared", zap.Error(err))
			}
		}()
		defer incidentsConsumer.Close() //nolint:errcheck

		// SPEC-W24 Agent B (WS-B2) — BEGIN (additive): lending
		// DisbursementIntent → TigerBeetle rail consumer (group
		// booking-lending-disbursements) on LENDING_EVENTS_TOPIC. Rail
		// posture (W39 SIM-001, fail closed): LENDING_TB_BRIDGE_URL selects
		// the live payments-service bridge; ALLOW_MOCK_RAILS=1 opts into
		// the deterministic mock (no money movement, dev only); with
		// neither configured the consumer is NOT started (intents stay
		// unconsumed for the operator to wire the rail — never silently
		// simulated). Exactly-once under redelivery via the deterministic
		// transfer ID (anchor: intent ref_id = application id). Skipped
		// when lending events are disabled (empty topic).
		if cfg.LendingEventsTopic != "" {
			lendingGroup := os.Getenv("LENDING_DISBURSEMENTS_GROUP")
			if lendingGroup == "" {
				lendingGroup = consumer.DefaultLendingDisbursementsGroup
			}
			lendingRail, railErr := consumer.RailFromEnv(logger)
			if railErr != nil {
				logger.Error("lending disbursement rail not configured; consumer NOT started (fail closed)",
					zap.Error(railErr), zap.String("topic", cfg.LendingEventsTopic))
			} else {
				lendingConsumer := consumer.NewLendingDisbursements(cfg.KafkaBrokers, cfg.LendingEventsTopic, lendingGroup, cfg.DLQTopic, lendingRail, logger)
				lendingHealth := consumerHealth.Register("lending-disbursements")
				go func() {
					if err := lendingConsumer.Run(ctx); err != nil && ctx.Err() == nil {
						lendingHealth.Store(false)
						logger.Error("lending disbursement consumer exited; liveness flag cleared", zap.Error(err))
					}
				}()
				defer lendingConsumer.Close() //nolint:errcheck
				logger.Info("lending disbursement consumer started",
					zap.String("topic", cfg.LendingEventsTopic), zap.String("group", lendingGroup),
					zap.String("rail", lendingRail.Name()))
			}
		}
		// SPEC-W24 Agent B (WS-B2) — END
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
		// SPEC-W44 W-B/S1-F7-05 (K2): internauth on the civic sla-breach
		// callback (BOOKING_INTERNAL_TOKEN, no default — fail closed).
		InternalToken: cfg.InternalToken,
		Geo:           geoHandlers,
		Incidents:     incidentSvc,
		Leads:         leadSvc,
		Referrals:     referralSvc,
		Payouts:       payoutStore,          // SPEC-W14 Agent B (additive): GET /v1/payouts
		Devices:       deviceHandlers,       // SPEC-W16 Agent B (additive): /v1/devices + /internal/devices
		FieldCapture:  fieldCaptureHandlers, // SPEC-W16 Agent B (additive): POST /v1/field/capture
		AppGate:       appGate,              // SPEC-W18 Agent D (additive): /v1/leads gated behind app "cac" (opt-in)
		Helpdesk:      helpdeskDeps,         // SPEC-W19 integrator (additive): /v1/helpdesk gated behind app "helpdesk" (opt-in)
		Workorders:    workordersDeps,       // SPEC-W19 integrator (additive): /v1/field-service gated behind app "field-service" (opt-in)
		Loyalty:       loyaltyDeps,          // SPEC-W19 integrator (additive): /v1/loyalty gated behind app "loyalty-wallet" (opt-in)
		Studio:        studioDeps,           // SPEC-W19 integrator (additive): /v1/studio gated behind app "campaign-studio" (opt-in)
		CRM360:        crm360Deps,           // SPEC-W20 integrator (additive): /v1/crm gated behind app "crm-360" (opt-in)
		Surveys:       surveysDeps,          // SPEC-W20 integrator (additive): /v1/surveys gated behind app "surveys-voc" (opt-in; respond stays public)
		Lending:       lendingDeps,          // SPEC-W20 integrator (additive): /v1/lending gated behind app "lending" (opt-in)
		Workforce:     workforceDeps,        // SPEC-W20 integrator (additive): /v1/workforce gated behind app "workforce" (opt-in)
		Social:        socialDeps,           // SPEC-W21 integrator (additive): /v1/social gated behind app "social-publisher" (opt-in)
		Civic:         civicSvc,             // SPEC-W32 WS-A (additive): /v1/civic public intake + operator console
		Health:        consumerHealth,       // SPEC-W43 K-09: consumer liveness flags read by /healthz
	}

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: httpapi.NewRouter(deps),
		// SPEC-W43 K-10: full server timeouts — a slow-loris or stalled
		// handler goroutine must not pin connections/goroutines forever.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
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
