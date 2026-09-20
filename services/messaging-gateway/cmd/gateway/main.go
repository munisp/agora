// messaging-gateway: outbound SMS/WhatsApp gateway for the Nigeria channel
// providers (Termii, Africa's Talking, WhatsApp Cloud API). The
// notification-worker reaches it through the Dapr HTTP output bindings
// bindings-termii / bindings-africastalking / bindings-whatsapp; this
// service owns the provider credentials, retry policy and error mapping.
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

	"github.com/opendesk/messaging-gateway/internal/channel"
	"github.com/opendesk/messaging-gateway/internal/config"
	"github.com/opendesk/messaging-gateway/internal/httpapi"
	"github.com/opendesk/messaging-gateway/internal/metrics"
	"github.com/opendesk/messaging-gateway/internal/provider"
	"go.uber.org/zap"
)

// defaultIdentityURL is the direct identity-service base for sidecar-less
// deployments (SPEC-W45 ORPH O2, compose service name/port).
const defaultIdentityURL = "http://identity:7001"

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
	reg := metrics.New()

	srv := &httpapi.Server{
		Termii: &provider.Termii{
			Client:   provider.NewClient("termii", reg, logger),
			BaseURL:  cfg.TermiiBaseURL,
			APIKey:   cfg.TermiiAPIKey,
			SenderID: cfg.TermiiSenderID,
		},
		AT: &provider.AfricasTalking{
			Client:   provider.NewClient("africastalking", reg, logger),
			BaseURL:  cfg.ATBaseURL,
			APIKey:   cfg.ATAPIKey,
			Username: cfg.ATUsername,
			From:     cfg.ATFrom,
		},
		WhatsApp: &provider.WhatsApp{
			Client:        provider.NewClient("whatsapp", reg, logger),
			BaseURL:       cfg.WhatsAppBaseURL,
			Token:         cfg.WhatsAppToken,
			PhoneNumberID: cfg.WhatsAppPhoneNumberID,
		},
		Telegram: &provider.Telegram{
			Client:  provider.NewClient("telegram", reg, logger),
			BaseURL: cfg.TelegramBaseURL,
			Token:   cfg.TelegramBotToken,
		},
		WhatsAppVerifyToken:   cfg.WhatsAppVerifyToken,
		WhatsAppAppSecret:     cfg.WhatsAppAppSecret,
		WhatsAppMock:          cfg.WhatsAppMock,
		TelegramBotUsername:   cfg.TelegramBotUsername,
		TelegramWebhookSecret: cfg.TelegramWebhookSecret,
		Metrics:               reg,
		Log:                   logger,
	}

	// SIM-007/SIM-008 posture: the inbound WhatsApp webhook verifies
	// X-Hub-Signature-256 against WHATSAPP_APP_SECRET, fail-closed. The
	// unsigned bypass exists ONLY via the explicit WHATSAPP_MOCK=1 dev
	// opt-in — surface both misconfigurations loudly at boot.
	switch {
	case cfg.WhatsAppMock:
		logger.Error("CRITICAL: MOCK WHATSAPP WEBHOOK — NOT FOR PRODUCTION: WHATSAPP_MOCK=1 accepts UNSIGNED inbound WhatsApp posts; unset it and configure WHATSAPP_APP_SECRET for production")
	case cfg.WhatsAppAppSecret == "":
		logger.Warn("WHATSAPP_APP_SECRET unset: inbound WhatsApp webhook rejects every post (401, fail-closed) until configured")
	}

	// Omnichannel inbound bridge (SPEC-W6 Part A): wired whenever
	// CHANNEL_SITE_MAP parses (empty map = inbound disabled, webhooks drop).
	siteMap, err := channel.ParseSiteMap(cfg.ChannelSiteMap)
	if err != nil {
		return err
	}
	// SPEC-W45 ORPH O2: resolve the internal upstream bases once and log
	// them — with no Dapr sidecar (the compose posture) these are the
	// direct service URLs; the Dapr invoke form uses the REGISTERED
	// app-ids conversation/voice/booking.
	convBase, voiceBase := channel.ResolveBases(cfg.ConversationURL, cfg.VoiceRuntimeURL, cfg.DaprHTTPPort, cfg.DaprSidecar)
	bookingBase := httpapi.ResolveIncidentBase(cfg.BookingURL, cfg.DaprHTTPPort, cfg.DaprSidecar)
	srv.Bridge = channel.NewBridge(siteMap, convBase, voiceBase, srv.WhatsApp, srv.Telegram, logger)
	logger.Info("inbound bridge bases resolved",
		zap.String("conversation_base", convBase),
		zap.String("voice_base", voiceBase),
		zap.String("booking_base", bookingBase),
		zap.Bool("dapr_sidecar", cfg.DaprSidecar))
	srv.Upstreams = []httpapi.UpstreamCheck{
		{Name: "conversation", Base: convBase},
		{Name: "voice", Base: voiceBase},
		{Name: "booking", Base: bookingBase},
	}
	// Boot-time reachability probe: fail-loud (WARN) but never fatal — the
	// gateway still serves provider webhooks and /healthz keeps reporting.
	for _, u := range srv.Upstreams {
		if err := httpapi.ProbeBase(context.Background(), u.Base); err != nil {
			logger.Warn("upstream base unreachable at boot (inbound paths degraded until it recovers)",
				zap.String("upstream", u.Name), zap.String("base", u.Base), zap.Error(err))
		}
	}

	// IoT incident ingest (SPEC-W11 Part B §6): per-tenant shared secrets +
	// the booking-service forwarder (BOOKING_URL override or Dapr invoke).
	incidentSecrets, err := httpapi.ParseIncidentSecrets(cfg.IncidentWebhookSecrets)
	if err != nil {
		return err
	}
	srv.IncidentSecrets = incidentSecrets
	srv.IncidentIngest = httpapi.NewIncidentIngester(bookingBase)
	logger.Info("incident webhook configured", zap.Int("tenant_secrets", len(incidentSecrets)))

	// NG SMS aggregator failover chain (SPEC-W12 Agent A): ordered chain
	// from SMS_PROVIDER_CHAIN with per-provider circuit breakers; the
	// price-tier annotations are relative cost hints used for reporting
	// only. Each provider's Client meters per-provider sends
	// (messaging_gateway_sends_total{provider,result}).
	ebulk := &provider.EBulkSMS{
		Client:   provider.NewClient("ebulksms", reg, logger),
		BaseURL:  cfg.EBulkBaseURL,
		APIKey:   cfg.EBulkAPIKey,
		Username: cfg.EBulkUsername,
		Sender:   cfg.EBulkSender,
	}
	srv.SMSChain = provider.NewFailover(map[string]provider.SMSProvider{
		"africastalking": srv.AT,
		"termii":         srv.Termii,
		"ebulksms":       ebulk,
	}, cfg.SMSProviderChain, logger)
	for _, e := range srv.SMSChain.Entries() {
		logger.Info("sms provider chain entry",
			zap.String("provider", e.Name),
			zap.Float64("price_tier", e.PriceTier),
			zap.Bool("configured", e.Provider.Configured()))
	}

	// USSD inbound (SPEC-W12 Agent A §1): session store + tenant menu fetch
	// + the synchronous conversation-service turn client. The conversation
	// base is the SAME Dapr invoke path the omnichannel bridge uses.
	var ussdStore channel.USSDSessionStore
	switch cfg.USSDSessionBackend {
	case "dapr":
		ussdStore = channel.NewDaprUSSDStore(cfg.USSDStateStore, cfg.DaprHTTPPort)
	default:
		if cfg.USSDSessionBackend != "memory" {
			logger.Warn("unknown USSD_SESSION_BACKEND, using memory",
				zap.String("backend", cfg.USSDSessionBackend))
		}
		ussdStore = channel.NewMemoryUSSDStore()
	}
	srv.USSD = &httpapi.USSDConfig{
		Sites:        siteMap,
		Store:        ussdStore,
		Menus:        channel.NewUSSDMenuFetcher(channel.ResolveInvokeBase(cfg.IdentityURL, "identity", defaultIdentityURL, cfg.DaprHTTPPort, cfg.DaprSidecar)),
		Conversation: channel.NewUSSDConversation(convBase),
		SessionTTL:   time.Duration(cfg.USSDSessionTTL) * time.Second,
		// SPEC-W45 K14/OOS-06: shared-secret path auth (fail-closed unset)
		// + per-phone sliding-window rate limit.
		CallbackSecret: cfg.ATCallbackSecret,
		RatePerMinute:  cfg.USSDRatePerMinute,
	}
	if cfg.ATCallbackSecret == "" {
		logger.Warn("AT_CALLBACK_SECRET unset: USSD callback rejects every post (503, fail-closed) until configured")
	}
	logger.Info("ussd channel configured",
		zap.String("session_backend", cfg.USSDSessionBackend),
		zap.Int("session_ttl_s", cfg.USSDSessionTTL),
		zap.Bool("callback_auth", cfg.ATCallbackSecret != ""),
		zap.Int("rate_per_minute", cfg.USSDRatePerMinute))

	logger.Info("messaging-gateway configured",
		zap.Bool("termii", srv.Termii.Configured()),
		zap.Bool("africastalking", srv.AT.Configured()),
		zap.Bool("whatsapp", srv.WhatsApp.Configured()),
		zap.Bool("telegram", srv.Telegram.Configured()),
		zap.Int("site_map_entries", len(siteMap)))

	hs := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           srv.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", zap.Int("port", cfg.Port))
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(shutdownCtx)
}
