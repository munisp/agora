package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opendesk/messaging-gateway/internal/channel"
	"github.com/opendesk/messaging-gateway/internal/metrics"
	"github.com/opendesk/messaging-gateway/internal/provider"
	"go.uber.org/zap"
)

// fakeBridge captures every bridged envelope.
type fakeBridge struct {
	mu    sync.Mutex
	calls []bridgeCall
	err   error
}

type bridgeCall struct {
	msg     channel.InboundMessage
	routeID string
}

func (f *fakeBridge) Handle(_ context.Context, msg channel.InboundMessage, routeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, bridgeCall{msg: msg, routeID: routeID})
	return f.err
}

func (f *fakeBridge) captured() []bridgeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]bridgeCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// waTestAppSecret is the WHATSAPP_APP_SECRET the test servers run with; the
// WhatsApp POST tests sign their payloads with it (SIM-007/SIM-008).
const waTestAppSecret = "test-app-secret"

func newWebhookServer(fb Bridger) *Server {
	return &Server{
		WhatsApp:              &provider.WhatsApp{},
		Telegram:              &provider.Telegram{},
		Bridge:                fb,
		WhatsAppVerifyToken:   "verify-me",
		WhatsAppAppSecret:     waTestAppSecret,
		TelegramBotUsername:   "opendesk_bot",
		TelegramWebhookSecret: "s3cret",
		Metrics:               metrics.New(),
		Log:                   zap.NewNop(),
	}
}

// waSignedHeaders computes the Meta X-Hub-Signature-256 header for a body.
func waSignedHeaders(secret, body string) map[string]string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return map[string]string{"X-Hub-Signature-256": "sha256=" + hex.EncodeToString(mac.Sum(nil))}
}

func do(t *testing.T, h http.Handler, method, target, body string, hdrs map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- WhatsApp verification handshake (SPEC-W6 §A1) ---

func TestWhatsAppVerifyOK(t *testing.T) {
	s := newWebhookServer(&fakeBridge{})
	rec := do(t, s.Router(), http.MethodGet,
		"/webhooks/whatsapp?hub.mode=subscribe&hub.verify_token=verify-me&hub.challenge=challenge-123", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if body, _ := io.ReadAll(rec.Result().Body); string(body) != "challenge-123" {
		t.Fatalf("expected raw challenge body, got %q", body)
	}
}

func TestWhatsAppVerifyBadToken(t *testing.T) {
	s := newWebhookServer(&fakeBridge{})
	for _, target := range []string{
		"/webhooks/whatsapp?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=x",
		"/webhooks/whatsapp?hub.mode=publish&hub.verify_token=verify-me&hub.challenge=x",
	} {
		rec := do(t, s.Router(), http.MethodGet, target, "", nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: expected 403, got %d", target, rec.Code)
		}
	}
}

// --- WhatsApp POST ingestion ---

const waTextPayload = `{
  "entry": [{
    "changes": [{
      "value": {
        "metadata": {"phone_number_id": "10987654321"},
        "messages": [{
          "from": "2348012345678",
          "id": "wamid.HBgLM",
          "timestamp": "1733000000",
          "type": "text",
          "text": {"body": "I want to book an appointment"}
        }]
      }
    }]
  }]
}`

func TestWhatsAppTextIngestion(t *testing.T) {
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/whatsapp", waTextPayload, waSignedHeaders(waTestAppSecret, waTextPayload))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	calls := fb.captured()
	if len(calls) != 1 {
		t.Fatalf("expected 1 bridge call, got %d", len(calls))
	}
	want := channel.InboundMessage{
		Channel:   "whatsapp",
		From:      "2348012345678",
		MessageID: "wamid.HBgLM",
		Text:      "I want to book an appointment",
		Timestamp: 1733000000,
	}
	if calls[0].msg != want {
		t.Fatalf("envelope mismatch:\n got %+v\nwant %+v", calls[0].msg, want)
	}
	if calls[0].routeID != "10987654321" {
		t.Fatalf("expected route id = phone_number_id, got %q", calls[0].routeID)
	}
}

func TestWhatsAppStatusesIgnored(t *testing.T) {
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	payload := `{"entry":[{"changes":[{"value":{
		"metadata":{"phone_number_id":"10987654321"},
		"statuses":[{"id":"wamid.HBgLM","status":"delivered","timestamp":"1733000001"}]
	}}]}]}`
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/whatsapp", payload, waSignedHeaders(waTestAppSecret, payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if n := len(fb.captured()); n != 0 {
		t.Fatalf("statuses must not reach the bridge, got %d calls", n)
	}
}

func TestWhatsAppNonTextIgnored(t *testing.T) {
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	payload := `{"entry":[{"changes":[{"value":{
		"metadata":{"phone_number_id":"10987654321"},
		"messages":[{"from":"2348012345678","id":"wamid.Img","timestamp":"1733000000","type":"image","image":{"id":"img1"}}]
	}}]}]}`
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/whatsapp", payload, waSignedHeaders(waTestAppSecret, payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if n := len(fb.captured()); n != 0 {
		t.Fatalf("non-text messages must not reach the bridge, got %d calls", n)
	}
}

// --- WhatsApp signature enforcement (SIM-007/SIM-008, fail-closed) ---

// A missing or invalid X-Hub-Signature-256 must be rejected with 401 and
// NOTHING reaches the bridge.
func TestWhatsAppSignatureMissingOrInvalidRejected(t *testing.T) {
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	for _, hdrs := range []map[string]string{
		nil, // missing header
		{"X-Hub-Signature-256": "sha256=deadbeef"},          // wrong HMAC
		{"X-Hub-Signature-256": "deadbeef"},                 // missing sha256= prefix
		waSignedHeaders("wrong-secret", waTextPayload),      // wrong secret
		waSignedHeaders(waTestAppSecret, waTextPayload+" "), // signed a different body
	} {
		rec := do(t, s.Router(), http.MethodPost, "/webhooks/whatsapp", waTextPayload, hdrs)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("headers %v: expected 401, got %d (%s)", hdrs, rec.Code, rec.Body.String())
		}
	}
	if n := len(fb.captured()); n != 0 {
		t.Fatalf("rejected posts must not reach the bridge, got %d calls", n)
	}
}

// Fail-closed at the config layer too: with no WHATSAPP_APP_SECRET and no
// mock opt-in, even a well-formed post is 401.
func TestWhatsAppSignatureFailClosedWithoutAppSecret(t *testing.T) {
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	s.WhatsAppAppSecret = ""
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/whatsapp", waTextPayload,
		waSignedHeaders(waTestAppSecret, waTextPayload))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if n := len(fb.captured()); n != 0 {
		t.Fatalf("unconfigured signature verification must not process, got %d calls", n)
	}
}

// The unsigned bypass exists ONLY behind the explicit WHATSAPP_MOCK=1
// dev/test opt-in (Server.WhatsAppMock).
func TestWhatsAppSignatureMockBypassOptIn(t *testing.T) {
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	s.WhatsAppMock = true
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/whatsapp", waTextPayload, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("WHATSAPP_MOCK=1 must accept unsigned posts, got %d", rec.Code)
	}
	if n := len(fb.captured()); n != 1 {
		t.Fatalf("mock bypass must still process the payload, got %d calls", n)
	}
}

// --- Telegram POST ingestion ---

const tgTextPayload = `{
  "update_id": 9001,
  "message": {
    "message_id": 555,
    "date": 1733000100,
    "text": "hello bot",
    "chat": {"id": 42424242, "type": "private"},
    "from": {"id": 42424242, "first_name": "Ada"}
  }
}`

func TestTelegramSecretOK(t *testing.T) {
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/telegram", tgTextPayload,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "s3cret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	calls := fb.captured()
	if len(calls) != 1 {
		t.Fatalf("expected 1 bridge call, got %d", len(calls))
	}
	want := channel.InboundMessage{
		Channel:   "telegram",
		From:      "42424242", // chat_id as string
		MessageID: "555",
		Text:      "hello bot",
		Timestamp: 1733000100,
	}
	if calls[0].msg != want {
		t.Fatalf("envelope mismatch:\n got %+v\nwant %+v", calls[0].msg, want)
	}
	if calls[0].routeID != "opendesk_bot" {
		t.Fatalf("expected route id = bot username, got %q", calls[0].routeID)
	}
}

func TestTelegramSecretBad(t *testing.T) {
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	for _, hdrs := range []map[string]string{
		{"X-Telegram-Bot-Api-Secret-Token": "nope"},
		{}, // missing header
	} {
		rec := do(t, s.Router(), http.MethodPost, "/webhooks/telegram", tgTextPayload, hdrs)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", rec.Code)
		}
	}
	if n := len(fb.captured()); n != 0 {
		t.Fatalf("bad secret must not reach the bridge, got %d calls", n)
	}
}

func TestTelegramNonTextIgnored(t *testing.T) {
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	payload := `{"update_id": 9002, "message": {"message_id": 556, "date": 1733000101,
		"chat": {"id": 42424242}, "photo": [{"file_id": "p1"}]}}`
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/telegram", payload,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "s3cret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if n := len(fb.captured()); n != 0 {
		t.Fatalf("updates without message.text must not reach the bridge, got %d calls", n)
	}
}

// --- Telegram send endpoint (outbound parity, SPEC-W6 §A4) ---

func TestTelegramSendEndpointMaps5xxTo502(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"ok":false}`)) //nolint:errcheck
	}))
	defer fake.Close()
	s := newWebhookServer(&fakeBridge{})
	s.Telegram = &provider.Telegram{
		Client:  provider.NewClient("telegram", metrics.New(), zap.NewNop()),
		BaseURL: fake.URL,
		Token:   "bot-token",
	}
	rec := do(t, s.Router(), http.MethodPost, "/v1/telegram/send",
		`{"to":"42424242","message":"hi"}`, nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("persistent provider 5xx must map to 502, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestTelegramSendEndpointNotConfigured(t *testing.T) {
	s := newWebhookServer(&fakeBridge{})
	rec := do(t, s.Router(), http.MethodPost, "/v1/telegram/send",
		`{"to":"42424242","message":"hi"}`, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured telegram must map to 503, got %d", rec.Code)
	}
}
