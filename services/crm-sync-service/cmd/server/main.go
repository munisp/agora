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
	"syscall"
	"time"

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

	syncer := &consumer.Syncer{Twenty: twenty, Map: st, Metrics: reg, Log: logger}

	// Reverse worker (Twenty -> OpenDesk): consumes the webhook CloudEvents
	// from opendesk.crm.events in its own group so it scales/offsets
	// independently of the forward consumers.
	reverse := &consumer.ReverseSyncer{
		Twenty:       twenty,
		Map:          st,
		Invoker:      daprClient,
		BookingAppID: cfg.BookingAppID,
		EchoWindow:   cfg.EchoWindow,
		Metrics:      reg,
		Log:          logger,
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

	// HTTP server.
	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.Port),
		Handler: (&httpapi.Server{
			Twenty:         twenty,
			Dapr:           daprClient,
			PubSubName:     cfg.PubSubName,
			CRMEventsTopic: cfg.CRMEventsTopic,
			WebhookSecret:  cfg.TwentyWebhookSecret,
			DB:             st,
			Map:            st,
			Metrics:        reg,
			Log:            logger,
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
