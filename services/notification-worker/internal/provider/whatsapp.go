package provider

// WhatsApp Cloud API provider — business-initiated TEMPLATE sends
// (SPEC-W21 Agent A: whatsapp_campaign paced kind).
//
// Configuration (env; read via NewWhatsAppFromEnv — the activities layer
// falls back to it when no provider is wired, so the zero-config default
// works WITHOUT main/config edits):
//
//	WHATSAPP_MOCK                 default OFF (SIM-011, KYC_MOCK idiom):
//	                              explicit dev/test opt-in ("1"/"true"/"on")
//	                              for the deterministic no-network mock.
//	                              NEVER enable in production.
//	WHATSAPP_CLOUD_API_TOKEN      Meta system-user access token
//	WHATSAPP_PHONE_NUMBER_ID      sender phone number id (not the number)
//	WHATSAPP_BUSINESS_ACCOUNT_ID  WhatsApp Business Account id (optional;
//	                              carried for boot logging/reporting only —
//	                              the Cloud API send path does not need it)
//	WHATSAPP_CLOUD_API_BASE_URL   default https://graph.facebook.com/v21.0
//	                              (tests/override)
//
// EXTERNAL PREREQUISITES (honest note, mirrored in
// docs/apps/campaign-studio.md): a template send only delivers when the
// template (name + language) is APPROVED in the WhatsApp Business Manager
// and the sender number's quality rating allows business-initiated traffic
// (new numbers ramp through Meta's messaging limits). Both are Meta-side
// processes outside this repo; the WHATSAPP_MOCK=1 opt-in exists so dev/test
// journeys are exercisable before they complete. With the mock off and no
// Cloud API credentials configured, sends fail closed with an explicit
// error — a WhatsApp send is never silently simulated.
//
// The shared Client machinery (retry/timeout/logging) is identical to the
// FCM provider's.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"go.uber.org/zap"
)

// DefaultWhatsAppBaseURL is the production Cloud API endpoint base.
const DefaultWhatsAppBaseURL = "https://graph.facebook.com/v21.0"

// MaxWhatsAppTemplateParams is the Cloud API positional body-parameter
// ceiling enforced by the whatsapp_campaign contract (SPEC-W21).
const MaxWhatsAppTemplateParams = 10

// WhatsAppConfig carries the WHATSAPP_* environment configuration (see
// file header).
type WhatsAppConfig struct {
	Mock              bool   // WHATSAPP_MOCK (default false, SIM-011): deterministic, no network — explicit dev/test opt-in
	Token             string // WHATSAPP_CLOUD_API_TOKEN
	PhoneNumberID     string // WHATSAPP_PHONE_NUMBER_ID
	BusinessAccountID string // WHATSAPP_BUSINESS_ACCOUNT_ID (optional; reporting only)
	BaseURL           string // WHATSAPP_CLOUD_API_BASE_URL override
}

// WhatsApp sends WhatsApp template messages via the Meta Cloud API.
type WhatsApp struct {
	Client            *Client
	BaseURL           string
	Token             string
	PhoneNumberID     string
	BusinessAccountID string
	Mock              bool
}

// NewWhatsApp builds the provider from the environment-derived config. An
// empty BaseURL selects the production default.
func NewWhatsApp(cfg WhatsAppConfig, log *zap.Logger) *WhatsApp {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultWhatsAppBaseURL
	}
	return &WhatsApp{
		Client:            NewClient("whatsapp", log),
		BaseURL:           base,
		Token:             cfg.Token,
		PhoneNumberID:     cfg.PhoneNumberID,
		BusinessAccountID: cfg.BusinessAccountID,
		Mock:              cfg.Mock,
	}
}

// NewWhatsAppFromEnv builds the provider from the WHATSAPP_* environment
// (file header). WHATSAPP_MOCK defaults OFF (SIM-011, KYC_MOCK idiom) — the
// zero-config posture is the LIVE Cloud API (credentials required; sends
// fail closed with an explicit error when they are absent); only an
// explicit truthy value opts into the deterministic mock.
func NewWhatsAppFromEnv(log *zap.Logger) *WhatsApp {
	mock := false
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WHATSAPP_MOCK"))) {
	case "1", "true", "yes", "on":
		mock = true
	}
	return NewWhatsApp(WhatsAppConfig{
		Mock:              mock,
		Token:             os.Getenv("WHATSAPP_CLOUD_API_TOKEN"),
		PhoneNumberID:     os.Getenv("WHATSAPP_PHONE_NUMBER_ID"),
		BusinessAccountID: os.Getenv("WHATSAPP_BUSINESS_ACCOUNT_ID"),
		BaseURL:           os.Getenv("WHATSAPP_CLOUD_API_BASE_URL"),
	}, log)
}

// Configured reports whether the provider can attempt a LIVE send (mock
// mode counts as configured: it needs no credentials).
func (w *WhatsApp) Configured() bool {
	return w.Mock || (w.Token != "" && w.PhoneNumberID != "")
}

// WhatsAppTemplateMessage is one business-initiated template send.
type WhatsAppTemplateMessage struct {
	To       string   // recipient E.164 (digits only is also accepted by Meta)
	Template string   // approved template name
	Language string   // template language code; "en" when empty (contract default)
	Params   []string // positional body parameters ({{1}}..{{n}}, max 10)
}

// SendTemplate delivers one template message, returning the provider HTTP
// status and (truncated) response body on success. The mock path returns a
// deterministic fake wamid; the live path POSTs
// {base}/{phoneNumberID}/messages.
func (w *WhatsApp) SendTemplate(ctx context.Context, msg WhatsAppTemplateMessage) (int, []byte, error) {
	if msg.To == "" {
		return 0, nil, &Error{StatusCode: 0, Body: "whatsapp: recipient phone is required"}
	}
	if msg.Template == "" {
		return 0, nil, &Error{StatusCode: 0, Body: "whatsapp: template name is required"}
	}
	if len(msg.Params) > MaxWhatsAppTemplateParams {
		return 0, nil, &Error{StatusCode: 0, Body: fmt.Sprintf("whatsapp: at most %d template params", MaxWhatsAppTemplateParams)}
	}
	if msg.Language == "" {
		msg.Language = "en"
	}
	if w.Mock {
		return w.sendMock(msg)
	}
	if !w.Configured() {
		return 0, nil, &Error{StatusCode: 0, Body: "whatsapp not configured: set WHATSAPP_CLOUD_API_TOKEN and WHATSAPP_PHONE_NUMBER_ID (or WHATSAPP_MOCK=1)"}
	}
	return w.sendLive(ctx, msg)
}

// ---------------------------------------------------------------------------
// Deterministic mock (WHATSAPP_MOCK=1 explicit opt-in; default OFF, SIM-011)
// ---------------------------------------------------------------------------

// sendMock mirrors the FCM mock idiom: no network, deterministic results,
// documented test hooks:
//   - phone "mock-fail" → provider 500 error
//   - anything else     → 200 with a deterministic message id in the Cloud
//     API response shape {"messages":[{"id":"wamid.mock-<sha256(to|template|lang)[:24]>"}]}
func (w *WhatsApp) sendMock(msg WhatsAppTemplateMessage) (int, []byte, error) {
	if msg.To == "mock-fail" {
		return 0, nil, &Error{StatusCode: http.StatusInternalServerError, Body: "whatsapp mock: send failed (phone mock-fail)"}
	}
	sum := sha256.Sum256([]byte(msg.To + "|" + msg.Template + "|" + msg.Language))
	body, _ := json.Marshal(map[string]any{
		"messaging_product": "whatsapp",
		"contacts":          []map[string]string{{"input": msg.To, "wa_id": strings.TrimPrefix(msg.To, "+")}},
		"messages":          []map[string]string{{"id": "wamid.mock-" + hex.EncodeToString(sum[:])[:24]}},
	})
	return http.StatusOK, body, nil
}

// ---------------------------------------------------------------------------
// Live Cloud API (WHATSAPP_MOCK=0)
// ---------------------------------------------------------------------------

// sendLive POSTs the template message envelope per the Meta Cloud API docs:
// {"messaging_product":"whatsapp","to","type":"template","template":{"name",
// "language":{"code"},"components":[{"type":"body","parameters":[{"type":"text",
// "text"}...]}]}}. The components block is omitted when there are no params.
func (w *WhatsApp) sendLive(ctx context.Context, msg WhatsAppTemplateMessage) (int, []byte, error) {
	template := map[string]any{
		"name":     msg.Template,
		"language": map[string]string{"code": msg.Language},
	}
	if len(msg.Params) > 0 {
		params := make([]map[string]string, 0, len(msg.Params))
		for _, p := range msg.Params {
			params = append(params, map[string]string{"type": "text", "text": p})
		}
		template["components"] = []map[string]any{{"type": "body", "parameters": params}}
	}
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                msg.To,
		"type":              "template",
		"template":          template,
	}
	return w.Client.send(ctx, func(ctx context.Context) (*http.Request, error) {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal whatsapp payload: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			w.BaseURL+"/"+w.PhoneNumberID+"/messages", strings.NewReader(string(body)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+w.Token)
		return req, nil
	})
}

// WhatsAppMessageID extracts the first message id (wamid) from a Cloud API
// success body ("" when absent — logging must tolerate that).
func WhatsAppMessageID(body []byte) string {
	var out struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Messages) == 0 {
		return ""
	}
	return out.Messages[0].ID
}
