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
	"github.com/opendesk/messaging-gateway/internal/media"
	"github.com/opendesk/messaging-gateway/internal/metrics"
	"github.com/opendesk/messaging-gateway/internal/provider"
	"go.uber.org/zap"
)

// defaultIdentityURL is the direct identity-service base for sidecar-less
// deployments (SPEC-W45 ORPH O2, compose service name/port).
const defaultIdentityURL = "http://identity:7001"

// defaultNotificationURL is the compose service address of
// notification-worker (SPEC-W47-C UIMOB-06: USSD menu-action confirmation
// SMS intake). Mirrors defaultIdentityURL.
const defaultNotificationURL = "http://notification:7003"

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

	// SPEC-W60: WhatsApp inbound media -> OCR funnel. The site map is shared
	// with the bridge (media resolves the tenant BEFORE any Meta fetch — the
	// P3 cost gate); the per-sender throttle mirrors the USSD
	// ratelimit.go precedent (P4/R3: APISIX limit-count keys Meta's egress
	// IPs, not senders). P7: flag off => media drops + logs exactly like
	// pre-W60; flag on with OCR envs unset => boot CRITICAL + fail-closed
	// per-message drop (receipt.go fail-visible posture).
	srv.Sites = siteMap
	srv.WhatsAppMediaEnabled = cfg.WhatsAppMediaEnabled
	srv.MediaRate = httpapi.NewPhoneRateLimiter(cfg.WhatsAppMediaRatePerMinute, time.Minute)
	srv.Media = media.NewOCRClient(cfg.OCRServiceURL, cfg.OCRInternalToken, logger)
	switch {
	case !cfg.WhatsAppMediaEnabled:
		logger.Info("whatsapp media ingest disabled (WHATSAPP_MEDIA_ENABLED=false) — media messages drop like pre-W60")
	case !srv.Media.Configured():
		logger.Error("CRITICAL: WHATSAPP_MEDIA_ENABLED=true but OCR_SERVICE_URL / OCR_INTERNAL_TOKEN unset — media messages will be DROPPED per-message (fail-closed)",
			zap.Bool("ocr_service_url", cfg.OCRServiceURL != ""),
			zap.Bool("ocr_internal_token", cfg.OCRInternalToken != ""))
	default:
		logger.Info("whatsapp media ingest enabled",
			zap.String("ocr_client", srv.Media.String()),
			zap.Int("media_rate_per_minute", cfg.WhatsAppMediaRatePerMinute))
		srv.Upstreams = append(srv.Upstreams, httpapi.UpstreamCheck{Name: "ocr", Base: cfg.OCRServiceURL})
	}

	// SPEC-W61: WhatsApp media -> booking anchor. The client reuses the
	// already-resolved booking base (ResolveIncidentBase idiom) and the K2
	// machine token (MESSAGING_BOOKING_INTERNAL_TOKEN). The flag also wires
	// the OCR expires_in_hours TTL (MEDIA_INBOX_TTL_HOURS — sent only when
	// the anchor flag is on, so flag-off stays byte-identical W60).
	srv.WhatsAppMediaAnchorEnabled = cfg.WhatsAppMediaAnchorEnabled
	srv.MediaAnchor = httpapi.NewAnchorClient(bookingBase, cfg.BookingInternalToken)
	if cfg.WhatsAppMediaAnchorEnabled {
		// expires_in_hours rides the extract call whenever the anchor flag
		// is on (SPEC-W61 §Gateway sequencing) — TTL bounds parked media
		// PII retention even if the anchor token is misconfigured.
		srv.Media.SetExpiresInHours(cfg.MediaInboxTTLHours)
		switch {
		case !cfg.WhatsAppMediaEnabled:
			logger.Warn("WHATSAPP_MEDIA_ANCHOR_ENABLED=true but WHATSAPP_MEDIA_ENABLED=false — anchor never fires (media messages drop before OCR)")
		case !srv.MediaAnchor.Configured():
			logger.Error("CRITICAL: WHATSAPP_MEDIA_ANCHOR_ENABLED=true but MESSAGING_BOOKING_INTERNAL_TOKEN unset — anchors DROP per-message after OCR (fail-closed)",
				zap.Bool("booking_internal_token", cfg.BookingInternalToken != ""))
		default:
			logger.Info("whatsapp media booking anchor enabled",
				zap.String("anchor_client", srv.MediaAnchor.String()),
				zap.Int("media_inbox_ttl_hours", cfg.MediaInboxTTLHours))
		}
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
		Sites: siteMap,
		Store: ussdStore,
		// SPEC-W46 PERF-10: 60s TTL menu cache (stale-on-error) around the
		// 5s-timeout identity fetcher — menus are near-static config.
		Menus: channel.NewCachedUSSDMenuFetcher(
			channel.NewUSSDMenuFetcher(channel.ResolveInvokeBase(cfg.IdentityURL, "identity", defaultIdentityURL, cfg.DaprHTTPPort, cfg.DaprSidecar)),
			0),
		Conversation: channel.NewUSSDConversation(convBase),
		SessionTTL:   time.Duration(cfg.USSDSessionTTL) * time.Second,
		// SPEC-W47-C (UIMOB-06): menu-action dispatch — booking-service
		// effects (helpdesk ticket for book/handoff, booking status lookup
		// for status) + the promised confirmation SMS via notification-worker
		// (paced send). Same ResolveInvokeBase posture as the other
		// upstreams: direct in-cluster bases sidecar-less, Dapr invoke when
		// a sidecar is present.
		Actions: httpapi.NewUSSDActionDispatcher(
			bookingBase,
			cfg.BookingInternalToken,
			channel.ResolveInvokeBase(cfg.NotificationURL, "notification", defaultNotificationURL, cfg.DaprHTTPPort, cfg.DaprSidecar),
			cfg.NotificationInternalToken,
			logger),
		// SPEC-W45 K14/OOS-06: shared-secret path auth (fail-closed unset)
		// + per-phone sliding-window rate limit.
		CallbackSecret: cfg.ATCallbackSecret,
		RatePerMinute:  cfg.USSDRatePerMinute,
	}
	if cfg.ATCallbackSecret == "" {
		logger.Warn("AT_CALLBACK_SECRET unset: USSD callback rejects every post (503, fail-closed) until configured")
	}
	// SPEC-W47-C (UIMOB-06): fail-loud boot signal when the machine tokens
	// for menu-action dispatch are unset (effects/confirmation SMS would
	// fail closed per dispatch, logged as errors per selection).
	if cfg.BookingInternalToken == "" || cfg.NotificationInternalToken == "" {
		logger.Warn("USSD menu-action dispatch degraded: MESSAGING_BOOKING_INTERNAL_TOKEN / MESSAGING_NOTIFICATION_INTERNAL_TOKEN unset",
			zap.Bool("booking_token", cfg.BookingInternalToken != ""),
			zap.Bool("notification_token", cfg.NotificationInternalToken != ""))
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
