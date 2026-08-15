// Omnichannel inbound webhooks (SPEC-W6 Part A): Meta WhatsApp Cloud API
// verification + message ingestion and Telegram Bot API updates.
//
// Reliability contract: these handlers ALWAYS answer 200 fast to the
// provider (Meta/Telegram retry-storm on non-200). The only non-200 answers
// are authentication failures, never internal processing errors: 403 for a
// bad verify token (GET) or a bad shared secret (Telegram POST), and 401
// for a missing/invalid WhatsApp X-Hub-Signature-256 (SIM-007/SIM-008).
// Processing is synchronous but bounded by a 25s context.
package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/opendesk/messaging-gateway/internal/channel"
	"go.uber.org/zap"
)

// webhookTimeout bounds the synchronous bridge processing per message
// (SPEC-W6 §A1: process synchronously but bounded).
const webhookTimeout = 25 * time.Second

// Bridger is the inbound bridge contract used by the webhook handlers
// (implemented by *channel.Bridge; faked in tests).
type Bridger interface {
	Handle(ctx context.Context, msg channel.InboundMessage, routeID string) error
}

// ---------------------------------------------------------------------------
// WhatsApp (Meta Cloud API)
// ---------------------------------------------------------------------------

// handleWhatsAppVerify implements the Meta webhook verification handshake:
// hub.mode=subscribe + matching hub.verify_token → 200 with the raw
// challenge body; anything else → 403 (SPEC-W6 §A1).
func (s *Server) handleWhatsAppVerify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("hub.mode") == "subscribe" &&
		s.WhatsAppVerifyToken != "" &&
		q.Get("hub.verify_token") == s.WhatsAppVerifyToken {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(q.Get("hub.challenge"))) //nolint:errcheck
		return
	}
	writeError(w, http.StatusForbidden, "webhook verification failed")
}

// waWebhook is the Meta Cloud API webhook payload shape (only the fields
// the bridge needs).
type waWebhook struct {
	Entry []struct {
		Changes []struct {
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				Messages []struct {
					From      string `json:"from"` // E.164 without '+'
					ID        string `json:"id"`
					Timestamp string `json:"timestamp"` // unix seconds, string-typed by Meta
					Type      string `json:"type"`
					Text      struct {
						Body string `json:"body"`
					} `json:"text"`
				} `json:"messages"`
				Statuses []json.RawMessage `json:"statuses"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// verifyWhatsAppSignature enforces the Meta WhatsApp webhook signature
// (SIM-007/SIM-008, fail-closed): X-Hub-Signature-256 must be
// "sha256=" + hex HMAC-SHA256(WHATSAPP_APP_SECRET, raw body). A missing or
// invalid signature — or a missing app-secret configuration — rejects the
// post with 401 and NOTHING is processed. The ONLY bypass is the explicit
// dev opt-in WHATSAPP_MOCK=1 (Server.WhatsAppMock, default false), which
// exists so local/dev journeys can post unsigned payloads.
func (s *Server) verifyWhatsAppSignature(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		s.Log.Warn("whatsapp webhook: unreadable body, dropping", zap.Error(err))
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		return nil, false
	}
	if s.WhatsAppMock {
		// Dev/test opt-in (WHATSAPP_MOCK=1): unsigned payloads accepted.
		return body, true
	}
	if s.WhatsAppAppSecret == "" {
		s.Log.Error("whatsapp webhook: WHATSAPP_APP_SECRET not configured, rejecting (fail-closed)")
		writeError(w, http.StatusUnauthorized, "webhook signature verification not configured")
		return nil, false
	}
	sig := r.Header.Get("X-Hub-Signature-256")
	mac := hmac.New(sha256.New, []byte(s.WhatsAppAppSecret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if sig == "" || !hmac.Equal([]byte(sig), []byte(want)) {
		s.Log.Warn("whatsapp webhook: missing or invalid X-Hub-Signature-256, rejecting")
		writeError(w, http.StatusUnauthorized, "invalid webhook signature")
		return nil, false
	}
	return body, true
}

// handleWhatsAppWebhook ingests inbound WhatsApp messages. Text messages
// are normalized and bridged; statuses[] delivery receipts and non-text
// message types are ignored. Always 200 (SPEC-W6 §A1) except 401 for a
// missing/invalid X-Hub-Signature-256 (authentication failure — Meta does
// not retry-storm on those).
func (s *Server) handleWhatsAppWebhook(w http.ResponseWriter, r *http.Request) {
	body, ok := s.verifyWhatsAppSignature(w, r)
	if !ok {
		return
	}
	var payload waWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		// Malformed payload: still 200 — Meta retries non-200 forever and a
		// poison payload would loop. Log and drop.
		s.Log.Warn("whatsapp webhook: invalid JSON body, dropping", zap.Error(err))
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		return
	}
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			v := change.Value
			// v.Statuses (delivery receipts) are ignored silently — only
			// messages[] below produce bridge work.
			for _, m := range v.Messages {
				if m.Type != "text" {
					s.Log.Info("whatsapp webhook: ignoring non-text message",
						zap.String("type", m.Type), zap.String("message_id", m.ID))
					continue
				}
				ts, _ := strconv.ParseInt(m.Timestamp, 10, 64)
				s.bridge(r.Context(), channel.InboundMessage{
					Channel:   "whatsapp",
					From:      m.From,
					MessageID: m.ID,
					Text:      m.Text.Body,
					Timestamp: ts,
				}, v.Metadata.PhoneNumberID)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// ---------------------------------------------------------------------------
// Telegram (Bot API)
// ---------------------------------------------------------------------------

// tgUpdate is the Telegram Bot API Update shape (message-only subset).
type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64  `json:"message_id"`
		Date      int64  `json:"date"` // unix seconds
		Text      string `json:"text"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From *struct {
			ID int64 `json:"id"`
		} `json:"from"`
	} `json:"message"`
}

// handleTelegramWebhook ingests Telegram Bot API updates. When
// TELEGRAM_WEBHOOK_SECRET is set the X-Telegram-Bot-Api-Secret-Token header
// must match (else 403). Updates without message.text are ignored.
// Always 200 otherwise (SPEC-W6 §A1).
func (s *Server) handleTelegramWebhook(w http.ResponseWriter, r *http.Request) {
	if s.TelegramWebhookSecret != "" &&
		r.Header.Get("X-Telegram-Bot-Api-Secret-Token") != s.TelegramWebhookSecret {
		writeError(w, http.StatusForbidden, "bad webhook secret")
		return
	}
	var upd tgUpdate
	if err := json.NewDecoder(r.Body).Decode(&upd); err != nil {
		s.Log.Warn("telegram webhook: invalid JSON body, dropping", zap.Error(err))
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		return
	}
	if upd.Message == nil || upd.Message.Text == "" {
		// Edits, stickers, join events, …: nothing to bridge.
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		return
	}
	m := upd.Message
	s.bridge(r.Context(), channel.InboundMessage{
		Channel:   "telegram",
		From:      strconv.FormatInt(m.Chat.ID, 10), // chat_id as string
		MessageID: strconv.FormatInt(m.MessageID, 10),
		Text:      m.Text,
		Timestamp: m.Date,
	}, s.TelegramBotUsername)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// bridge runs the inbound bridge with the bounded 25s context and swallows
// the outcome (already logged by the bridge) — the provider always gets 200.
func (s *Server) bridge(parent context.Context, msg channel.InboundMessage, routeID string) {
	if s.Bridge == nil {
		s.Log.Warn("inbound bridge not configured, dropping message",
			zap.String("channel", msg.Channel))
		return
	}
	ctx, cancel := context.WithTimeout(parent, webhookTimeout)
	defer cancel()
	if err := s.Bridge.Handle(ctx, msg, routeID); err != nil {
		s.Log.Warn("inbound bridge failed",
			zap.String("channel", msg.Channel), zap.Error(err))
	}
}

// ---------------------------------------------------------------------------
// IoT incident ingest (SPEC-W11 Part B §6)
// ---------------------------------------------------------------------------
//
// POST /webhooks/incidents is the public IoT trigger: an alarm panel /
// sensor gateway posts a PARTIAL Incident Data Packet; the gateway
// authenticates the tenant via the per-tenant shared secret
// (INCIDENT_WEBHOOK_SECRETS env JSON) and forwards to booking-service's
// internal POST /v1/incidents/ingest via Dapr service invocation, which
// completes the IDP (channel=webhook), persists it and triggers
// auto-dispatch + critical/high outreach.
//
// Response contract mirrors the provider webhooks: 400 only for garbage
// JSON, 403 for an unknown tenant / bad secret (authentication failure),
// 200 otherwise — internal forward failures are logged, not propagated.

// incidentWebhookBody is the accepted payload shape. Exactly one of
// tenant_slug / tenant_id addresses the tenant.
type incidentWebhookBody struct {
	TenantSlug string          `json:"tenant_slug,omitempty"`
	TenantID   string          `json:"tenant_id,omitempty"`
	Secret     string          `json:"secret,omitempty"`
	Incident   json.RawMessage `json:"incident"`
}

// ParseIncidentSecrets decodes the INCIDENT_WEBHOOK_SECRETS env JSON:
//
//	{"acme-ng": "s3cret", "9f1c…-uuid": "other-secret"}
//
// Keys may be tenant slugs or tenant ids (either addresses the tenant in the
// webhook body). An empty string yields an empty map (ingest disabled —
// every post is 403).
func ParseIncidentSecrets(raw string) (map[string]string, error) {
	m := map[string]string{}
	if raw == "" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("parse INCIDENT_WEBHOOK_SECRETS: %w", err)
	}
	return m, nil
}

// IncidentIngester forwards a validated incident to booking-service
// (httpIncidentIngester in prod; faked in tests).
type IncidentIngester interface {
	Ingest(ctx context.Context, body []byte) error
}

// httpIncidentIngester posts the ingest envelope to booking-service:
// {base}/v1/incidents/ingest, where base is the BOOKING_URL override or the
// Dapr sidecar invoke base (…/v1.0/invoke/booking/method).
type httpIncidentIngester struct {
	base string
	hc   *http.Client
}

// NewIncidentIngester builds the ingester; base must already be resolved
// (direct base or Dapr invoke base) — see ResolveIncidentBase.
func NewIncidentIngester(base string) IncidentIngester {
	return &httpIncidentIngester{base: base, hc: &http.Client{Timeout: 10 * time.Second}}
}

// ResolveIncidentBase maps the BOOKING_URL override / DAPR_HTTP_PORT onto
// the booking-service base URL (same convention as channel.ResolveBases).
func ResolveIncidentBase(bookingOverride string, daprHTTPPort int) string {
	if bookingOverride != "" {
		return bookingOverride
	}
	return fmt.Sprintf("http://127.0.0.1:%d/v1.0/invoke/booking/method", daprHTTPPort)
}

func (c *httpIncidentIngester) Ingest(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/incidents/ingest", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("invoke booking ingest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("booking ingest: status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// incidentSignal is the minimal validation subset of the partial IDP: at
// least one signal field must be present, otherwise the post is junk
// telemetry and ignored (200, not forwarded).
type incidentSignal struct {
	IncidentType     string          `json:"incident_type"`
	NarrativeSummary string          `json:"narrative_summary"`
	CallbackNumber   *string         `json:"callback_number"`
	Location         json.RawMessage `json:"location"`
}

func (s *Server) handleIncidentWebhook(w http.ResponseWriter, r *http.Request) {
	var body incidentWebhookBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// Garbage JSON is the ONE client error we report (400): unlike the
		// provider webhooks there is no retry-storm risk from IoT gateways.
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	tenantKey := body.TenantSlug
	if tenantKey == "" {
		tenantKey = body.TenantID
	}
	if tenantKey == "" {
		writeError(w, http.StatusForbidden, "tenant_slug or tenant_id is required")
		return
	}
	want, known := s.IncidentSecrets[tenantKey]
	if !known || want == "" || body.Secret != want {
		writeError(w, http.StatusForbidden, "bad incident webhook secret")
		return
	}
	if len(body.Incident) == 0 || string(body.Incident) == "null" {
		// Valid JSON but nothing to ingest.
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		return
	}
	var sig incidentSignal
	if err := json.Unmarshal(body.Incident, &sig); err != nil {
		writeError(w, http.StatusBadRequest, "incident must be a JSON object")
		return
	}
	if sig.IncidentType == "" && sig.NarrativeSummary == "" &&
		(sig.CallbackNumber == nil || *sig.CallbackNumber == "") && len(sig.Location) == 0 {
		s.Log.Info("incident webhook: no signal fields, ignoring",
			zap.String("tenant", tenantKey))
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		return
	}
	if s.IncidentIngest == nil {
		s.Log.Warn("incident ingest not configured, dropping", zap.String("tenant", tenantKey))
		writeJSON(w, http.StatusOK, map[string]any{"status": "dropped"})
		return
	}
	forward, err := json.Marshal(map[string]any{
		"tenant_slug": body.TenantSlug,
		"tenant_id":   body.TenantID,
		"incident":    body.Incident,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid incident payload")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), webhookTimeout)
	defer cancel()
	if err := s.IncidentIngest.Ingest(ctx, forward); err != nil {
		// Internal failure: logged, still 200 (the caller must not retry-storm;
		// booking-side ingest is idempotent on incident_id for safe manual replays).
		s.Log.Warn("incident ingest forward failed", zap.String("tenant", tenantKey), zap.Error(err))
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// ---------------------------------------------------------------------------
// Telegram send endpoint (outbound parity with the other providers, §A4)
// ---------------------------------------------------------------------------

type telegramRequest struct {
	To      string `json:"to"` // chat_id as string
	Message string `json:"message"`
}

func (s *Server) handleTelegramSend(w http.ResponseWriter, r *http.Request) {
	var req telegramRequest
	if !decodeJSON(w, r, &req) || !requireToMessage(w, req.To, req.Message) {
		return
	}
	if !s.Telegram.Configured() {
		writeError(w, http.StatusServiceUnavailable, "telegram provider not configured (TELEGRAM_BOT_TOKEN)")
		return
	}
	status, body, err := s.Telegram.SendMessage(r.Context(), req.To, req.Message)
	s.respond(w, r, status, body, err)
}
