// crm-sync-service: bidirectional OpenDesk <-> Twenty CRM sync. Forward:
// identity/booking/conversation event consumers -> Twenty REST. Reverse:
// HMAC-verified webhook intake -> opendesk.crm.events -> reverse worker ->
// booking-service internal endpoints. Plus the /v1/tasks helper used by
// Temporal activities (SPEC-CRM §B).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/opendesk/crm-sync-service/internal/callsummary"
	"github.com/opendesk/crm-sync-service/internal/config"
	"github.com/opendesk/crm-sync-service/internal/consumer"
	"github.com/opendesk/crm-sync-service/internal/daprc"
	"github.com/opendesk/crm-sync-service/internal/httpapi"
	"github.com/opendesk/crm-sync-service/internal/metrics"
	"github.com/opendesk/crm-sync-service/internal/syncmap"
	"github.com/opendesk/crm-sync-service/internal/twentyc"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func main() {
	// SPEC-W80 A8: the runtime image is distroless (no shell, curl or
	// wget), so the compose healthcheck execs the binary itself in this
	// probe mode: GET /healthz on the configured PORT, exit 0 on 2xx.
	for _, a := range os.Args[1:] {
		if a == "--healthcheck" || a == "-healthcheck" {
			os.Exit(healthcheck())
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

// healthcheck probes the local /healthz endpoint (compose healthcheck
// CMD). Returns 0 healthy, 1 otherwise.
func healthcheck() int {
	port := 7010
	if v := os.Getenv("PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/healthz", port), nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return 0
	}
	fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
	return 1
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
	if cfg.TwentyWebhookSecret == "" {
		logger.Warn("TWENTY_WEBHOOK_SECRET is empty; /webhooks/twenty will reject all requests")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// sync_map store (bootstraps its DDL idempotently).
	st, err := syncmap.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	reg := metrics.New()
	twenty := twentyc.New(cfg.TwentyAPIURL, cfg.TwentyAPIKey, cfg.TwentyRatePerMin)
	twenty.Observe = reg.ObserveTwentyCall
	daprClient := daprc.New(cfg.DaprHost, cfg.DaprHTTPPort)
	// 2026-11-23: businesses with their own Twenty instance (TWENTY_ROUTES).
	router := twentyc.NewRouter(twenty)
	var extraSecrets []string
	for _, rt := range cfg.TwentyRoutes {
		c := twentyc.New(rt.URL, rt.APIKey, cfg.TwentyRatePerMin)
		c.Observe = reg.ObserveTwentyCall
		router.Add(rt.Tenants, rt.WorkspaceID, c)
		extraSecrets = append(extraSecrets, rt.WebhookSecret)
		logger.Info("twenty route", zap.String("name", rt.Name), zap.String("url", rt.URL),
			zap.Strings("tenants", rt.Tenants), zap.String("workspace_id", rt.WorkspaceID))
	}

	syncer := &consumer.Syncer{Twenty: twenty, Router: router, Map: st, Metrics: reg, Log: logger}
	if cfg.ConversationURL != "" && cfg.SummaryLLMURL != "" {
		syncer.Summary = callsummary.New(cfg.ConversationURL, cfg.SummaryLLMURL, cfg.SummaryLLMModel, cfg.SummaryTimeout)
		logger.Info("call-summary sentences enabled", zap.String("model", cfg.SummaryLLMModel))
	}

	// Reverse worker (Twenty -> OpenDesk): consumes the webhook CloudEvents
	// from opendesk.crm.events in its own group so it scales/offsets
	// independently of the forward consumers.
	reverse := &consumer.ReverseSyncer{
		Twenty:               twenty,
		Router:               router,
		Map:                  st,
		Invoker:              daprClient,
		BookingAppID:         cfg.BookingAppID,
		EchoWindow:           cfg.EchoWindow,
		BookingInternalToken: cfg.BookingInternalToken,
		Metrics:              reg,
		Log:                  logger,
	}

	g, gctx := errgroup.WithContext(ctx)

	// Event consumers (direct Kafka, shared group `crm-sync`, DLQ after 3 attempts).
	if cfg.ConsumerEnabled {
		consumers := []*consumer.Consumer{
			consumer.New(cfg.KafkaBrokers, cfg.IdentityTopic, cfg.ConsumerGroup, cfg.DLQTopic, syncer.HandleIdentity, reg, logger),
			consumer.New(cfg.KafkaBrokers, cfg.BookingTopic, cfg.ConsumerGroup, cfg.DLQTopic, syncer.HandleBooking, reg, logger),
			consumer.New(cfg.KafkaBrokers, cfg.ConversationTopic, cfg.ConsumerGroup, cfg.DLQTopic, syncer.HandleConversation, reg, logger),
			// Wave 5 #2: sentiment-enriched call-quality events (preferred
			// call-summary note path; plain SessionEnded stays as fallback).
			consumer.New(cfg.KafkaBrokers, cfg.QualityTopic, cfg.ConsumerGroup, cfg.DLQTopic, syncer.HandleQuality, reg, logger),
			// SPEC-W38 F3: post-call captured fields -> Twenty note (own
			// group `crm-sync-capture`; dedupe via sync_map capture_note).
			consumer.New(cfg.KafkaBrokers, cfg.CapturesTopic, cfg.CaptureGroup, cfg.DLQTopic, syncer.HandleCapture, reg, logger),
			// GDPR erase tombstones (SPEC-W3 §2): deletes the Twenty person.
			consumer.New(cfg.KafkaBrokers, cfg.PrivacyTopic, cfg.ConsumerGroup, cfg.DLQTopic, syncer.HandlePrivacy, reg, logger),
			// Reverse direction: Twenty webhook events -> OpenDesk.
			consumer.New(cfg.KafkaBrokers, cfg.CRMEventsTopic, cfg.ReverseGroup, cfg.DLQTopic, reverse.HandleCRMEvent, reg, logger),
		}
		for _, c := range consumers {
			c := c
			g.Go(func() error { return c.Run(gctx) })
			defer c.Close() //nolint:errcheck
		}
	} else {
		logger.Warn("CONSUMER_ENABLED=false; Kafka consumers disabled")
	}

	if cfg.InternalToken == "" {
		// Runtime fail-closed (503 on the /v1 surface, K2 pattern) — logged
		// loudly here so the misconfig is obvious at boot.
		logger.Error("CRM_SYNC_INTERNAL_TOKEN unset — /v1/tasks + /v1/people/lookup will refuse all requests (503 fail-closed, SPEC-W45 K21)")
	}
	if cfg.BookingInternalToken == "" {
		// B7 (SPEC-W80 A11): booking-service's internAuth-guarded internal
		// routes reject tokenless invokes (503/401) — every reverse-sync
		// contacts/upsert + crm-note call will fail until this is set.
		logger.Error("BOOKING_INTERNAL_TOKEN unset — reverse-sync invokes of booking /internal/* will fail closed at the peer (SPEC-W80 B7)")
	}

	// HTTP server.
	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.Port),
		Handler: (&httpapi.Server{
			Twenty:              twenty,
			TwentyRouter:        router,
			ExtraWebhookSecrets: extraSecrets,
			Dapr:                daprClient,
			PubSubName:          cfg.PubSubName,
			CRMEventsTopic:      cfg.CRMEventsTopic,
			WebhookSecret:       cfg.TwentyWebhookSecret,
			DB:                  st,
			Map:                 st,
			Dedupe:              st, // SPEC-W43 K-13: webhook_events_seen replay guard
			Metrics:             reg,
			InternalToken:       cfg.InternalToken,
			Log:                 logger,
		}).Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	g.Go(func() error {
		logger.Info("http listening", zap.Int("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shCtx)
	})

	logger.Info("crm-sync-service started",
		zap.String("twenty_api", cfg.TwentyAPIURL), zap.Int("rate_per_min", cfg.TwentyRatePerMin))
	return g.Wait()
}
