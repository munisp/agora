// SPEC-W81 (C1/C3): the inbound bridge pins the provider-verified WhatsApp
// sender identity (wa_id) on POST /voice/chat via channel_identity +
// X-Internal-Token, so the runtime can treat the session as verified
// (K15(c) soft gate — token mismatch/unset => pin ignored, fail-safe).
package channel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// voicePinFake captures the /voice/chat body AND headers (the plain
// voiceFake in bridge_test.go does not record headers).
type voicePinFake struct {
	mu        sync.Mutex
	bodies    []map[string]string
	tokens    []string // X-Internal-Token header per call ("" = absent)
	hasHeader []bool
	reply     string
}

func (f *voicePinFake) server() *httptest.Server {
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
			"reply":           f.reply,
			"tool_calls":      []any{},
		})
	}))
}

// handleOne drives one inbound message through the full bridge against
// fake conversation/voice/provider servers and returns the voice capture.
func handleOne(t *testing.T, msg InboundMessage, routeID string, voiceToken string) *voicePinFake {
	t.Helper()
	conv := &convFake{}
	convSrv := conv.server()
	defer convSrv.Close()
	voice := &voicePinFake{reply: "ok"}
	voiceSrv := voice.server()
	defer voiceSrv.Close()
	prov := &providerFake{}
	provSrv := prov.server()
	defer provSrv.Close()

	wa, tg := testProviders(provSrv.URL)
	sites := map[string]Site{
		msg.Channel + ":" + routeID: {SiteSlug: "acme-ng", TenantID: "11111111-2222-3333-4444-555555555555"},
	}
	b := NewBridge(sites, convSrv.URL, voiceSrv.URL, wa, tg, zap.NewNop())
	b.VoiceInternalToken = voiceToken

	if err := b.Handle(context.Background(), msg, routeID); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	voice.mu.Lock()
	defer voice.mu.Unlock()
	if len(voice.bodies) != 1 {
		t.Fatalf("expected 1 voice chat call, got %d", len(voice.bodies))
	}
	return voice
}

// C1 happy path: WhatsApp + token configured -> channel_identity (wa_id) in
// the body AND X-Internal-Token on the request. REDIRECTION PROOF: fails on
// base (body carries no channel_identity), passes on candidate.
func TestBridgeAgentReplyWhatsAppPinsChannelIdentity(t *testing.T) {
	msg := InboundMessage{
		Channel:   "whatsapp",
		From:      "+2348012345678",
		MessageID: "wamid.1",
		Text:      "hello",
		Timestamp: 1733000100,
	}
	voice := handleOne(t, msg, "10987654321", "test-voice-admin-token")

	body := voice.bodies[0]
	if got := body["channel_identity"]; got != "+2348012345678" {
		t.Fatalf("channel_identity = %q, want the wa_id From; body=%v", got, body)
	}
	if got := body["channel"]; got != "whatsapp" {
		t.Fatalf("channel = %q, want whatsapp", got)
	}
	if !voice.hasHeader[0] || voice.tokens[0] != "test-voice-admin-token" {
		t.Fatalf("X-Internal-Token header = %q (present=%v), want the configured token",
			voice.tokens[0], voice.hasHeader[0])
	}
}

// C1 fail-safe: WhatsApp but the token env is unset -> the pin body is
// still sent (harmless — the peer ignores it) but NO X-Internal-Token
// header, so the runtime refuses the pin instead of ever spoofing it.
func TestBridgeAgentReplyWhatsAppNoTokenNoHeader(t *testing.T) {
	msg := InboundMessage{
		Channel:   "whatsapp",
		From:      "+2348012345678",
		MessageID: "wamid.2",
		Text:      "hello",
		Timestamp: 1733000100,
	}
	voice := handleOne(t, msg, "10987654321", "")

	body := voice.bodies[0]
	if got := body["channel_identity"]; got != "+2348012345678" {
		t.Fatalf("channel_identity = %q, want the wa_id From", got)
	}
	if voice.hasHeader[0] {
		t.Fatalf("X-Internal-Token must NOT be sent when the env is unset, got %q", voice.tokens[0])
	}
}

// C1 scope: non-whatsapp channels are byte-identical — no channel_identity,
// no token header (even with the token configured). Telegram's From is a
// chat_id, not a phone, so pinning it would be a runtime no-op + warning.
func TestBridgeAgentReplyTelegramUnpinned(t *testing.T) {
	msg := InboundMessage{
		Channel:   "telegram",
		From:      "42424242",
		MessageID: "555",
		Text:      "hello bot",
		Timestamp: 1733000100,
	}
	voice := handleOne(t, msg, "opendesk_bot", "test-voice-admin-token")

	body := voice.bodies[0]
	if _, ok := body["channel_identity"]; ok {
		t.Fatalf("telegram must not send channel_identity; body=%v", body)
	}
	if voice.hasHeader[0] {
		t.Fatalf("telegram must not send X-Internal-Token, got %q", voice.tokens[0])
	}
	if got := body["channel"]; got != "telegram" {
		t.Fatalf("channel = %q, want telegram", got)
	}
}
