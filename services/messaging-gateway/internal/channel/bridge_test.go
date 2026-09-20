package channel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opendesk/messaging-gateway/internal/metrics"
	"github.com/opendesk/messaging-gateway/internal/provider"
	"go.uber.org/zap"
)

// convFake fakes conversation-service: list (empty or hit), create, turns.
type convFake struct {
	mu        sync.Mutex
	existing  []string // conversation ids returned by the list endpoint
	created   []map[string]string
	turns     []turnCall
	nextID    int
	listQuery []string
}

type turnCall struct {
	convID  string
	tenant  string
	idemKey string
	role    string
	text    string
}

func (f *convFake) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/conversations", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			f.listQuery = append(f.listQuery, r.URL.RawQuery)
			ids := []map[string]string{}
			for _, id := range f.existing {
				ids = append(ids, map[string]string{"id": id})
			}
			json.NewEncoder(w).Encode(map[string]any{"conversations": ids}) //nolint:errcheck
		case http.MethodPost:
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
			f.created = append(f.created, body)
			f.nextID++
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"id": "conv-new-1"}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/v1/conversations/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/turns") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Role string `json:"role"`
			Text string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		convID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/conversations/"), "/turns")
		f.turns = append(f.turns, turnCall{
			convID:  convID,
			tenant:  r.Header.Get("X-Tenant-ID"),
			idemKey: r.Header.Get("Idempotency-Key"),
			role:    body.Role,
			text:    body.Text,
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"turn-1"}`)) //nolint:errcheck
	})
	return httptest.NewServer(mux)
}

// voiceFake fakes the voice runtime /voice/chat buffered path.
type voiceFake struct {
	mu    sync.Mutex
	calls []map[string]string
	reply string
}

func (f *voiceFake) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path != "/voice/chat" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]string
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body) //nolint:errcheck
		f.calls = append(f.calls, body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"conversation_id": body["conversation_id"],
			"reply":           f.reply,
			"tool_calls":      []any{},
		})
	}))
}

// providerFake captures outbound provider replies (telegram + whatsapp share
// it; the request path discriminates).
type providerFake struct {
	mu    sync.Mutex
	calls []providerCall
}

type providerCall struct {
	path   string
	chatID string
	text   string
	to     string
}

func (f *providerFake) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body) //nolint:errcheck
		call := providerCall{path: r.URL.Path}
		if v, ok := body["chat_id"].(string); ok {
			call.chatID = v
		}
		if v, ok := body["text"].(string); ok {
			call.text = v
		}
		if v, ok := body["to"].(string); ok {
			call.to = v
		}
		f.calls = append(f.calls, call)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	}))
}

func testProviders(baseURL string) (*provider.WhatsApp, *provider.Telegram) {
	wa := &provider.WhatsApp{
		Client:        provider.NewClient("whatsapp", metrics.New(), zap.NewNop()),
		BaseURL:       baseURL,
		Token:         "wa-token",
		PhoneNumberID: "10987654321",
	}
	tg := &provider.Telegram{
		Client:  provider.NewClient("telegram", metrics.New(), zap.NewNop()),
		BaseURL: baseURL,
		Token:   "tg-token",
	}
	return wa, tg
}

func TestBridgeFullFlowTelegram(t *testing.T) {
	conv := &convFake{}
	convSrv := conv.server()
	defer convSrv.Close()
	voice := &voiceFake{reply: "Sure, I can help with that."}
	voiceSrv := voice.server()
	defer voiceSrv.Close()
	prov := &providerFake{}
	provSrv := prov.server()
	defer provSrv.Close()

	wa, tg := testProviders(provSrv.URL)
	sites := map[string]Site{
		"telegram:opendesk_bot": {SiteSlug: "acme-ng", TenantID: "11111111-2222-3333-4444-555555555555"},
	}
	b := NewBridge(sites, convSrv.URL, voiceSrv.URL, wa, tg, zap.NewNop())

	msg := InboundMessage{
		Channel:   "telegram",
		From:      "42424242",
		MessageID: "555",
		Text:      "hello bot",
		Timestamp: 1733000100,
	}
	if err := b.Handle(context.Background(), msg, "opendesk_bot"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// 1. Conversation: list was empty → created with the mapped tenant/site.
	if len(conv.created) != 1 {
		t.Fatalf("expected 1 conversation create, got %d", len(conv.created))
	}
	created := conv.created[0]
	if created["tenant_id"] != "11111111-2222-3333-4444-555555555555" ||
		created["site_slug"] != "acme-ng" ||
		created["channel"] != "telegram" ||
		created["contact_phone"] != "42424242" {
		t.Fatalf("bad create payload: %v", created)
	}
	if len(conv.listQuery) != 1 ||
		!strings.Contains(conv.listQuery[0], "tenant=11111111-2222-3333-4444-555555555555") ||
		!strings.Contains(conv.listQuery[0], "contact=42424242") {
		t.Fatalf("bad list query: %v", conv.listQuery)
	}

	// 2. Turns: user turn with "<channel>:<message_id>", assistant with ":reply".
	if len(conv.turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(conv.turns))
	}
	user, assistant := conv.turns[0], conv.turns[1]
	if user.convID != "conv-new-1" || user.role != "user" || user.text != "hello bot" {
		t.Fatalf("bad user turn: %+v", user)
	}
	if user.idemKey != "telegram:555" {
		t.Fatalf("user turn idempotency key = %q, want telegram:555", user.idemKey)
	}
	if user.tenant != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("user turn missing X-Tenant-ID: %+v", user)
	}
	if assistant.role != "assistant" || assistant.text != "Sure, I can help with that." {
		t.Fatalf("bad assistant turn: %+v", assistant)
	}
	if assistant.idemKey != "telegram:555:reply" {
		t.Fatalf("assistant turn idempotency key = %q, want telegram:555:reply", assistant.idemKey)
	}

	// 3. Voice chat: called with the new conversation_id + channel.
	if len(voice.calls) != 1 {
		t.Fatalf("expected 1 voice call, got %d", len(voice.calls))
	}
	vc := voice.calls[0]
	if vc["conversation_id"] != "conv-new-1" || vc["site_slug"] != "acme-ng" ||
		vc["message"] != "hello bot" || vc["channel"] != "telegram" {
		t.Fatalf("bad voice payload: %v", vc)
	}

	// 4. Provider reply went to the correct chat via the Bot API path.
	if len(prov.calls) != 1 {
		t.Fatalf("expected 1 provider call, got %d", len(prov.calls))
	}
	pc := prov.calls[0]
	if pc.path != "/bottg-token/sendMessage" {
		t.Fatalf("unexpected provider path %q", pc.path)
	}
	if pc.chatID != "42424242" || pc.text != "Sure, I can help with that." {
		t.Fatalf("reply not sent to the correct chat: %+v", pc)
	}
}

func TestBridgeExistingConversationWhatsApp(t *testing.T) {
	conv := &convFake{existing: []string{"conv-existing-9"}}
	convSrv := conv.server()
	defer convSrv.Close()
	voice := &voiceFake{reply: "Your booking is confirmed."}
	voiceSrv := voice.server()
	defer voiceSrv.Close()
	prov := &providerFake{}
	provSrv := prov.server()
	defer provSrv.Close()

	wa, tg := testProviders(provSrv.URL)
	sites := map[string]Site{
		"whatsapp:10987654321": {SiteSlug: "acme-ng", TenantID: "tenant-1"},
	}
	b := NewBridge(sites, convSrv.URL, voiceSrv.URL, wa, tg, zap.NewNop())

	msg := InboundMessage{
		Channel:   "whatsapp",
		From:      "2348012345678",
		MessageID: "wamid.HBgLM",
		Text:      "confirm my booking",
		Timestamp: 1733000000,
	}
	if err := b.Handle(context.Background(), msg, "10987654321"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Existing conversation → no create.
	if n := len(conv.created); n != 0 {
		t.Fatalf("expected no create with an existing conversation, got %d", n)
	}
	if len(conv.turns) != 2 || conv.turns[0].convID != "conv-existing-9" {
		t.Fatalf("turns must target the existing conversation: %+v", conv.turns)
	}
	if conv.turns[0].idemKey != "whatsapp:wamid.HBgLM" {
		t.Fatalf("bad user idempotency key %q", conv.turns[0].idemKey)
	}
	if len(voice.calls) != 1 || voice.calls[0]["conversation_id"] != "conv-existing-9" {
		t.Fatalf("voice must use the existing conversation: %+v", voice.calls)
	}
	if len(prov.calls) != 1 ||
		prov.calls[0].path != "/10987654321/messages" ||
		prov.calls[0].to != "2348012345678" {
		t.Fatalf("whatsapp reply not sent to the contact: %+v", prov.calls)
	}
}

func TestBridgeUnmappedRouteDrops(t *testing.T) {
	conv := &convFake{}
	convSrv := conv.server()
	defer convSrv.Close()
	voice := &voiceFake{reply: "x"}
	voiceSrv := voice.server()
	defer voiceSrv.Close()
	prov := &providerFake{}
	provSrv := prov.server()
	defer provSrv.Close()

	wa, tg := testProviders(provSrv.URL)
	b := NewBridge(map[string]Site{}, convSrv.URL, voiceSrv.URL, wa, tg, zap.NewNop())

	err := b.Handle(context.Background(), InboundMessage{
		Channel: "telegram", From: "1", MessageID: "1", Text: "hi",
	}, "unknown_bot")
	if err != nil {
		t.Fatalf("unmapped route must drop without error, got %v", err)
	}
	if len(conv.turns) != 0 || len(conv.created) != 0 || len(voice.calls) != 0 || len(prov.calls) != 0 {
		t.Fatalf("unmapped route must not call any upstream")
	}
}

func TestBridgeInternalFailurePropagates(t *testing.T) {
	// Voice runtime down → Handle reports the error (the webhook layer
	// swallows it into a 200; tested in httpapi).
	conv := &convFake{}
	convSrv := conv.server()
	defer convSrv.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()
	prov := &providerFake{}
	provSrv := prov.server()
	defer provSrv.Close()

	wa, tg := testProviders(provSrv.URL)
	sites := map[string]Site{"telegram:b": {SiteSlug: "s", TenantID: "t"}}
	b := NewBridge(sites, convSrv.URL, dead.URL, wa, tg, zap.NewNop())

	err := b.Handle(context.Background(), InboundMessage{
		Channel: "telegram", From: "1", MessageID: "7", Text: "hi",
	}, "b")
	if err == nil {
		t.Fatal("expected error when the voice runtime fails")
	}
	// User turn was recorded, but no assistant turn and no provider reply.
	if len(conv.turns) != 1 || conv.turns[0].role != "user" {
		t.Fatalf("expected only the user turn, got %+v", conv.turns)
	}
	if len(prov.calls) != 0 {
		t.Fatalf("no provider reply expected on failure")
	}
}

func TestParseSiteMap(t *testing.T) {
	m, err := ParseSiteMap(`{"whatsapp:123":{"site_slug":"a","tenant_id":"t1"},"telegram:bot":{"site_slug":"b","tenant_id":"t2"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m["whatsapp:123"].SiteSlug != "a" || m["telegram:bot"].TenantID != "t2" {
		t.Fatalf("bad site map: %v", m)
	}
	if m, err := ParseSiteMap(""); err != nil || len(m) != 0 {
		t.Fatalf("empty map expected, got %v %v", m, err)
	}
	if _, err := ParseSiteMap("{not json"); err == nil {
		t.Fatal("expected parse error")
	}
}

// SPEC-W45 ORPH O2 base-resolution matrix: override > Dapr sidecar invoke
// (REGISTERED app-ids conversation/voice) > direct-base defaults.
func TestResolveBases(t *testing.T) {
	cases := []struct {
		name      string
		convOver  string
		voiceOver string
		port      int
		sidecar   bool
		wantConv  string
		wantVoice string
	}{
		{
			name: "sidecar uses registered app-ids",
			port: 3500, sidecar: true,
			wantConv:  "http://127.0.0.1:3500/v1.0/invoke/conversation/method",
			wantVoice: "http://127.0.0.1:3500/v1.0/invoke/voice/method",
		},
		{
			name: "sidecar on custom port",
			port: 3501, sidecar: true,
			wantConv:  "http://127.0.0.1:3501/v1.0/invoke/conversation/method",
			wantVoice: "http://127.0.0.1:3501/v1.0/invoke/voice/method",
		},
		{
			name: "no sidecar falls back to direct defaults",
			port: 3500, sidecar: false,
			wantConv:  DefaultConversationURL,
			wantVoice: DefaultVoiceURL,
		},
		{
			name:     "overrides win with sidecar",
			port:     3500,
			sidecar:  true,
			convOver: "http://conversation:7007", voiceOver: "http://voice:7006",
			wantConv: "http://conversation:7007", wantVoice: "http://voice:7006",
		},
		{
			name:     "overrides win without sidecar",
			port:     3500,
			sidecar:  false,
			convOver: "http://conv.internal:9000", voiceOver: "http://voice.internal:9001",
			wantConv: "http://conv.internal:9000", wantVoice: "http://voice.internal:9001",
		},
		{
			name:     "partial override: conv only, voice direct default",
			port:     3500,
			sidecar:  false,
			convOver: "http://conv.internal:9000",
			wantConv: "http://conv.internal:9000", wantVoice: DefaultVoiceURL,
		},
		{
			name:      "partial override: voice only, conv dapr",
			port:      3500,
			sidecar:   true,
			voiceOver: "http://voice.internal:9001",
			wantConv:  "http://127.0.0.1:3500/v1.0/invoke/conversation/method",
			wantVoice: "http://voice.internal:9001",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conv, voice := ResolveBases(tc.convOver, tc.voiceOver, tc.port, tc.sidecar)
			if conv != tc.wantConv || voice != tc.wantVoice {
				t.Fatalf("ResolveBases(%q, %q, %d, %v) = (%q, %q), want (%q, %q)",
					tc.convOver, tc.voiceOver, tc.port, tc.sidecar, conv, voice, tc.wantConv, tc.wantVoice)
			}
		})
	}
	// The compose-posture defaults must be the real service URLs.
	if DefaultConversationURL != "http://conversation:7007" || DefaultVoiceURL != "http://voice:7006" {
		t.Fatalf("direct defaults drifted: %q %q", DefaultConversationURL, DefaultVoiceURL)
	}
}
