// Package httpapi exposes the messaging-gateway HTTP API: the provider
// send endpoints, the omnichannel inbound webhooks (SPEC-W6 Part A),
// /healthz and /metrics.
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/opendesk/messaging-gateway/internal/metrics"
	"github.com/opendesk/messaging-gateway/internal/provider"
	"go.uber.org/zap"
)

// Server bundles the HTTP dependencies.
type Server struct {
	Termii   *provider.Termii
	AT       *provider.AfricasTalking
	WhatsApp *provider.WhatsApp
	Telegram *provider.Telegram

	// Omnichannel inbound (SPEC-W6 Part A).
	Bridge              Bridger // nil: inbound disabled, webhooks drop + 200
	WhatsAppVerifyToken string  // WHATSAPP_VERIFY_TOKEN (Meta GET handshake)
	// WhatsAppAppSecret (WHATSAPP_APP_SECRET) authenticates inbound WhatsApp
	// posts via X-Hub-Signature-256 HMAC (SIM-007/SIM-008, fail-closed: an
	// empty secret rejects every post with 401). WhatsAppMock
	// (WHATSAPP_MOCK, default false) is the explicit dev/test opt-in that
	// accepts unsigned posts.
	WhatsAppAppSecret     string
	WhatsAppMock          bool
	TelegramBotUsername   string // TELEGRAM_BOT_USERNAME (site-map route key)
	TelegramWebhookSecret string // TELEGRAM_WEBHOOK_SECRET (optional shared secret)

	// IoT incident ingest (SPEC-W11 Part B §6).
	IncidentSecrets map[string]string // INCIDENT_WEBHOOK_SECRETS parsed (tenant slug|id → secret)
	IncidentIngest  IncidentIngester  // nil: forward disabled, posts drop + 200

	// USSD inbound (SPEC-W12 Agent A + SPEC-W45 K14): POST
	// /ussd/callback/{secret}.
	USSD *USSDConfig // nil/empty secret: USSD disabled, callbacks fail-closed 503

	// Upstreams are the resolved internal bases (conversation / voice /
	// booking) probed by /healthz (SPEC-W45 ORPH O2): reachability is
	// REPORTED (warn-level) but never changes the 200 — the gateway itself
	// is healthy and serving provider webhooks regardless.
	Upstreams []UpstreamCheck

	// NG SMS aggregator failover chain (SPEC-W12 Agent A): POST /v1/sms/send.
	SMSChain *provider.Failover // nil: chain endpoint disabled (503)

	Metrics *metrics.Registry
	Log     *zap.Logger
}

// Router builds the chi router.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", s.handleHealthz)
	r.Get("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.Metrics.Render(w)
	})

	r.Route("/v1", func(r chi.Router) {
		r.Post("/termii/sms", s.handleTermiiSMS)
		r.Post("/africastalking/sms", s.handleATSMS)
		r.Post("/whatsapp/send", s.handleWhatsAppSend)
		r.Post("/telegram/send", s.handleTelegramSend)
		// SPEC-W12 Agent A: failover chain across the NG SMS aggregators
		// (SMS_PROVIDER_CHAIN order, per-provider circuit breaker).
		r.Post("/sms/send", s.handleChainSMS)
	})

	// Omnichannel inbound webhooks (SPEC-W6 Part A). Public by design —
	// authentication is the Meta verify token (GET) / X-Hub-Signature-256
	// HMAC (WhatsApp POST, SIM-007/SIM-008) / Telegram shared secret, and
	// handlers answer 200 fast for everything except authentication
	// failures (providers retry-storm on non-200).
	r.Route("/webhooks", func(r chi.Router) {
		r.Get("/whatsapp", s.handleWhatsAppVerify)
		r.Post("/whatsapp", s.handleWhatsAppWebhook)
		r.Post("/telegram", s.handleTelegramWebhook)
		// IoT incident trigger (SPEC-W11 Part B §6): public via the same
		// APISIX /webhooks/* route, authenticated by the per-tenant shared
		// secret in the body.
		r.Post("/incidents", s.handleIncidentWebhook)
	})

	// USSD session callback (SPEC-W12 Agent A §1 + SPEC-W45 K14/OOS-06):
	// Africa's Talking posts the session form here; the answer is the
	// text/plain CON/END line shown to the subscriber. The shared secret
	// lives in the path (the AT dashboard configures a bare callback URL —
	// no custom headers); AT_CALLBACK_SECRET unset fails closed (503), a
	// wrong secret gets 401 (constant-time compare). See
	// docs/channels-ussd.md.
	r.Post("/ussd/callback/{secret}", s.handleUSSDCallback)
	return r
}

// UpstreamCheck is one resolved internal base reported by /healthz.
type UpstreamCheck struct {
	Name string // "conversation" | "voice" | "booking"
	Base string // fully resolved base (direct or Dapr invoke)
}

// handleHealthz reports liveness plus the reachability of the resolved
// internal bases (SPEC-W45 ORPH O2). An unreachable base is a WARN-level
// signal in the body + logs — never a non-200 (the gateway still serves
// provider webhooks; the inbound bridge degrades per-message instead).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	status := map[string]string{}
	for _, u := range s.Upstreams {
		if err := ProbeBase(r.Context(), u.Base); err != nil {
			s.Log.Warn("healthz: upstream base unreachable",
				zap.String("upstream", u.Name), zap.String("base", u.Base), zap.Error(err))
			status[u.Name] = "unreachable"
		} else {
			status[u.Name] = "ok"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "upstreams": status})
}

type smsRequest struct {
	To       string `json:"to"`
	Message  string `json:"message"`
	SenderID string `json:"sender_id,omitempty"` // termii
	From     string `json:"from,omitempty"`      // africastalking
}

type whatsappRequest struct {
	To       string `json:"to"`
	Message  string `json:"message"`
	Template string `json:"template,omitempty"`
}

func (s *Server) handleTermiiSMS(w http.ResponseWriter, r *http.Request) {
	var req smsRequest
	if !decodeJSON(w, r, &req) || !requireToMessage(w, req.To, req.Message) {
		return
	}
	if !s.Termii.Configured() {
		writeError(w, http.StatusServiceUnavailable, "termii provider not configured (TERMII_API_KEY)")
		return
	}
	status, body, err := s.Termii.SendSMS(r.Context(), req.To, req.Message, req.SenderID)
	s.respond(w, r, status, body, err)
}

func (s *Server) handleATSMS(w http.ResponseWriter, r *http.Request) {
	var req smsRequest
	if !decodeJSON(w, r, &req) || !requireToMessage(w, req.To, req.Message) {
		return
	}
	if !s.AT.Configured() {
		writeError(w, http.StatusServiceUnavailable, "africastalking provider not configured (AT_API_KEY/AT_USERNAME)")
		return
	}
	status, body, err := s.AT.SendSMS(r.Context(), req.To, req.Message, req.From)
	s.respond(w, r, status, body, err)
}

func (s *Server) handleWhatsAppSend(w http.ResponseWriter, r *http.Request) {
	var req whatsappRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.To == "" {
		writeError(w, http.StatusBadRequest, "to is required")
		return
	}
	if !s.WhatsApp.Configured() {
		writeError(w, http.StatusServiceUnavailable, "whatsapp provider not configured (WHATSAPP_TOKEN/WHATSAPP_PHONE_NUMBER_ID)")
		return
	}
	if req.Template == "" && req.Message == "" {
		writeError(w, http.StatusBadRequest, "message or template is required")
		return
	}
	status, body, err := s.WhatsApp.SendMessage(r.Context(), req.To, req.Message, req.Template)
	s.respond(w, r, status, body, err)
}

// handleChainSMS (SPEC-W12 Agent A) sends via the SMS_PROVIDER_CHAIN
// failover chain. The response names the provider that accepted the send
// (reporting); provider 4xx maps to 400 (caller fault, no failover
// happened), chain exhaustion to 502.
func (s *Server) handleChainSMS(w http.ResponseWriter, r *http.Request) {
	var req smsRequest
	if !decodeJSON(w, r, &req) || !requireToMessage(w, req.To, req.Message) {
		return
	}
	if s.SMSChain == nil || len(s.SMSChain.Entries()) == 0 {
		writeError(w, http.StatusServiceUnavailable, "sms provider chain not configured (SMS_PROVIDER_CHAIN)")
		return
	}
	name, status, body, err := s.SMSChain.SendSMS(r.Context(), req.To, req.Message, req.SenderID)
	if err != nil {
		if provider.ClientError(err) {
			pe := err.(*provider.Error) //nolint:errcheck // guaranteed by ClientError
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":           "provider rejected the request",
				"provider":        name,
				"provider_status": status,
				"provider_body":   pe.Body,
			})
			return
		}
		s.Log.Warn("sms failover chain exhausted", zap.String("path", r.URL.Path), zap.Error(err))
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "all sms providers failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider":        name,
		"provider_status": status,
		"provider_body":   string(body),
	})
}

// respond maps a provider outcome onto the gateway response: provider 4xx →
// 400 with the provider body (no retry happened), persistent 5xx/transport
// failures → 502, success → 200 with the provider body.
func (s *Server) respond(w http.ResponseWriter, r *http.Request, status int, body []byte, err error) {
	if err != nil {
		if provider.ClientError(err) {
			pe := err.(*provider.Error) //nolint:errcheck // guaranteed by ClientError
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":           "provider rejected the request",
				"provider_status": status,
				"provider_body":   pe.Body,
			})
			return
		}
		s.Log.Warn("provider send failed", zap.String("path", r.URL.Path), zap.Error(err))
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "provider send failed after retries"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(body) //nolint:errcheck
}

// decodeJSON parses the request body as JSON.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// requireToMessage validates the common {to, message} envelope.
func requireToMessage(w http.ResponseWriter, to, message string) bool {
	if to == "" || message == "" {
		writeError(w, http.StatusBadRequest, "to and message are required")
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// probeTimeout bounds one upstream reachability probe (health endpoint and
// the boot-time warn probe share it).
const probeTimeout = 2 * time.Second

// ProbeBase reports whether a resolved internal base answers HTTP at all
// (GET {base}/healthz; for Dapr invoke bases this invokes the target app's
// /healthz through the sidecar). ANY HTTP response — even a 4xx/5xx — means
// reachable; only a transport failure counts as unreachable.
func ProbeBase(ctx context.Context, base string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: probeTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", base, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
