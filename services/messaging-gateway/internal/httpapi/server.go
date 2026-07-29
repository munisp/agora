// Package httpapi exposes the messaging-gateway HTTP API: the provider
// send endpoints, the omnichannel inbound webhooks (SPEC-W6 Part A),
// /healthz and /metrics.
package httpapi

import (
	"encoding/json"
	"net/http"

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
	Bridge                Bridger // nil: inbound disabled, webhooks drop + 200
	WhatsAppVerifyToken   string  // WHATSAPP_VERIFY_TOKEN (Meta GET handshake)
	TelegramBotUsername   string  // TELEGRAM_BOT_USERNAME (site-map route key)
	TelegramWebhookSecret string  // TELEGRAM_WEBHOOK_SECRET (optional shared secret)

	// IoT incident ingest (SPEC-W11 Part B §6).
	IncidentSecrets map[string]string // INCIDENT_WEBHOOK_SECRETS parsed (tenant slug|id → secret)
	IncidentIngest  IncidentIngester  // nil: forward disabled, posts drop + 200

	Metrics *metrics.Registry
	Log     *zap.Logger
}

// Router builds the chi router.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
	})
	r.Get("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.Metrics.Render(w)
	})

	r.Route("/v1", func(r chi.Router) {
		r.Post("/termii/sms", s.handleTermiiSMS)
		r.Post("/africastalking/sms", s.handleATSMS)
		r.Post("/whatsapp/send", s.handleWhatsAppSend)
		r.Post("/telegram/send", s.handleTelegramSend)
	})

	// Omnichannel inbound webhooks (SPEC-W6 Part A). Public by design —
	// authentication is the Meta verify token / Telegram shared secret, and
	// handlers always answer 200 fast (providers retry-storm on non-200).
	r.Route("/webhooks", func(r chi.Router) {
		r.Get("/whatsapp", s.handleWhatsAppVerify)
		r.Post("/whatsapp", s.handleWhatsAppWebhook)
		r.Post("/telegram", s.handleTelegramWebhook)
		// IoT incident trigger (SPEC-W11 Part B §6): public via the same
		// APISIX /webhooks/* route, authenticated by the per-tenant shared
		// secret in the body.
		r.Post("/incidents", s.handleIncidentWebhook)
	})
	// Future: POST /v1/ussd/session (Termii / AT USSD gateways) — see
	// docs/integrations/messaging-channels.md.
	return r
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
