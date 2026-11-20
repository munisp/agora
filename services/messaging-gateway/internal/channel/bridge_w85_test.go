// SPEC-W85: Telegram contact-share identity pinning — chat_id -> phone
// binding store, per-turn pin of the bound phone (WhatsApp-equivalent),
// and the server-driven request_contact keyboard prompt. REDIRECTION
// PROOF: these tests fail on the base tree (BindTelegramContact absent,
// no pin logic, no keyboard branch) and pass on the candidate.
package channel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// voiceW85Fake captures /voice/chat bodies+headers and answers with a
// configurable request_contact flag.
type voiceW85Fake struct {
	mu             sync.Mutex
	bodies         []map[string]string
	tokens         []string
	hasHeader      []bool
	requestContact bool
}

func (f *voiceW85Fake) server() *httptest.Server {
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
		f.bodies = append(f.bodies, body)
		_, has := r.Header["X-Internal-Token"]
		f.hasHeader = append(f.hasHeader, has)
		f.tokens = append(f.tokens, r.Header.Get("X-Internal-Token"))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"conversation_id": body["conversation_id"],
			"reply":           "ok",
			"tool_calls":      []any{},
			"request_contact": f.requestContact,
		})
	}))
}

// tgRawFake captures RAW telegram provider payloads (reply_markup visible).
type tgRawFake struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (f *tgRawFake) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body) //nolint:errcheck
		f.bodies = append(f.bodies, body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	}))
}

func newW85Bridge(t *testing.T, provURL, voiceURL, convURL string) *Bridge {
	t.Helper()
	wa, tg := testProviders(provURL)
	sites := map[string]Site{
		"telegram:opendesk_bot": {SiteSlug: "acme-ng", TenantID: "11111111-2222-3333-4444-555555555555"},
	}
	b := NewBridge(sites, convURL, voiceURL, wa, tg, zap.NewNop())
	b.VoiceInternalToken = "test-voice-admin-token"
	return b
}

func tgMsg(id string) InboundMessage {
	return InboundMessage{
		Channel: "telegram", From: "42424242", MessageID: id,
		Text: "hello bot", Timestamp: 1733000100,
	}
}

func driveW85(t *testing.T, b *Bridge, conv *convFake, msg InboundMessage) {
	t.Helper()
	convSrv := conv.server()
	defer convSrv.Close()
	b.ConversationURL = convSrv.URL
	if err := b.Handle(context.Background(), msg, "opendesk_bot"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestBridgeTelegramBoundPhonePinned(t *testing.T) {
	voice := &voiceW85Fake{}
	voiceSrv := voice.server()
	defer voiceSrv.Close()
	prov := &tgRawFake{}
	provSrv := prov.server()
	defer provSrv.Close()
	b := newW85Bridge(t, provSrv.URL, voiceSrv.URL, "")
	b.BindTelegramContact("42424242", "+2348012345678")

	driveW85(t, b, &convFake{}, tgMsg("900"))

	voice.mu.Lock()
	defer voice.mu.Unlock()
	if len(voice.bodies) != 1 {
		t.Fatalf("expected 1 voice chat call, got %d", len(voice.bodies))
	}
	body := voice.bodies[0]
	if got := body["channel_identity"]; got != "+2348012345678" {
		t.Fatalf("bound telegram chat must pin the bound phone, got %q; body=%v", got, body)
	}
	if !voice.hasHeader[0] || voice.tokens[0] != "test-voice-admin-token" {
		t.Fatalf("pin must carry X-Internal-Token when configured, got %q (present=%v)",
			voice.tokens[0], voice.hasHeader[0])
	}
}

func TestBridgeTelegramUnboundStaysUnpinned(t *testing.T) {
	// W81 byte-compat: without a binding the telegram request is
	// byte-identical to the pre-W85 shape (no channel_identity, no header).
	voice := &voiceW85Fake{}
	voiceSrv := voice.server()
	defer voiceSrv.Close()
	prov := &tgRawFake{}
	provSrv := prov.server()
	defer provSrv.Close()
	b := newW85Bridge(t, provSrv.URL, voiceSrv.URL, "")

	driveW85(t, b, &convFake{}, tgMsg("901"))

	voice.mu.Lock()
	defer voice.mu.Unlock()
	body := voice.bodies[0]
	if _, ok := body["channel_identity"]; ok {
		t.Fatalf("unbound telegram must not send channel_identity; body=%v", body)
	}
	if voice.hasHeader[0] {
		t.Fatalf("unbound telegram must not send X-Internal-Token, got %q", voice.tokens[0])
	}
}

func TestBridgeTelegramBindingExpires(t *testing.T) {
	voice := &voiceW85Fake{}
	voiceSrv := voice.server()
	defer voiceSrv.Close()
	prov := &tgRawFake{}
	provSrv := prov.server()
	defer provSrv.Close()
	b := newW85Bridge(t, provSrv.URL, voiceSrv.URL, "")

	now := time.Now()
	clock := now
	b.SetClock(func() time.Time { return clock })
	b.BindTelegramContact("42424242", "+2348012345678")
	if got := b.telegramBoundPhone("42424242"); got != "+2348012345678" {
		t.Fatalf("binding must be live immediately, got %q", got)
	}
	// Past the 30-day TTL the binding is gone and the turn is unpinned.
	clock = now.Add(31 * 24 * time.Hour)
	if got := b.telegramBoundPhone("42424242"); got != "" {
		t.Fatalf("binding must expire after the TTL, got %q", got)
	}
	driveW85(t, b, &convFake{}, tgMsg("902"))
	voice.mu.Lock()
	defer voice.mu.Unlock()
	if _, ok := voice.bodies[0]["channel_identity"]; ok {
		t.Fatalf("expired binding must not pin; body=%v", voice.bodies[0])
	}
}

func TestBridgeRequestContactTriggersKeyboard(t *testing.T) {
	voice := &voiceW85Fake{requestContact: true}
	voiceSrv := voice.server()
	defer voiceSrv.Close()
	prov := &tgRawFake{}
	provSrv := prov.server()
	defer provSrv.Close()
	b := newW85Bridge(t, provSrv.URL, voiceSrv.URL, "")

	driveW85(t, b, &convFake{}, tgMsg("903"))

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.bodies) != 1 {
		t.Fatalf("expected 1 telegram send, got %d", len(prov.bodies))
	}
	markup, ok := prov.bodies[0]["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("request_contact=true must attach reply_markup: %v", prov.bodies[0])
	}
	rows := markup["keyboard"].([]any)
	btn := rows[0].([]any)[0].(map[string]any)
	if btn["request_contact"] != true {
		t.Fatalf("keyboard button must request contact: %v", prov.bodies[0])
	}
}

func TestBridgeRequestContactSkippedWhenBound(t *testing.T) {
	// A bound chat was pinned this turn — a residual request_contact flag
	// must not downgrade it to the keyboard path.
	voice := &voiceW85Fake{requestContact: true}
	voiceSrv := voice.server()
	defer voiceSrv.Close()
	prov := &tgRawFake{}
	provSrv := prov.server()
	defer provSrv.Close()
	b := newW85Bridge(t, provSrv.URL, voiceSrv.URL, "")
	b.BindTelegramContact("42424242", "+2348012345678")

	driveW85(t, b, &convFake{}, tgMsg("904"))

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if _, ok := prov.bodies[0]["reply_markup"]; ok {
		t.Fatalf("bound chat must get the plain send, not the keyboard: %v", prov.bodies[0])
	}
}

func TestBridgeRequestContactFalseSendsPlain(t *testing.T) {
	voice := &voiceW85Fake{requestContact: false}
	voiceSrv := voice.server()
	defer voiceSrv.Close()
	prov := &tgRawFake{}
	provSrv := prov.server()
	defer provSrv.Close()
	b := newW85Bridge(t, provSrv.URL, voiceSrv.URL, "")

	driveW85(t, b, &convFake{}, tgMsg("905"))

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if _, ok := prov.bodies[0]["reply_markup"]; ok {
		t.Fatalf("request_contact=false must send plain text: %v", prov.bodies[0])
	}
}

func TestBindTelegramContactGuards(t *testing.T) {
	b := NewBridge(map[string]Site{}, "", "", nil, nil, zap.NewNop())
	b.BindTelegramContact("", "+2348012345678") // empty chat: no-op
	b.BindTelegramContact("42424242", "")       // empty phone: no-op
	if got := b.telegramBoundPhone("42424242"); got != "" {
		t.Fatalf("guard violations must not bind, got %q", got)
	}
	// Zero-value Bridge (nil map) must not panic on bind.
	zero := &Bridge{Log: zap.NewNop(), done: map[string]time.Time{}, now: time.Now}
	zero.BindTelegramContact("42424242", "+2348012345678")
	if got := zero.telegramBoundPhone("42424242"); got != "+2348012345678" {
		t.Fatalf("zero-value bridge must bind safely, got %q", got)
	}
}
