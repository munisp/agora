package channel

// SPEC-W44 N-05 tests: bridge MessageID dedupe — a redelivery after success
// skips agentReply+sendReply; a failure records no claim so the next
// redelivery retries the full pipeline.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestBridgeRedeliverySkipsCompletedWork(t *testing.T) {
	conv := &convFake{}
	convSrv := conv.server()
	defer convSrv.Close()
	voice := &voiceFake{reply: "ok"}
	voiceSrv := voice.server()
	defer voiceSrv.Close()
	prov := &providerFake{}
	provSrv := prov.server()
	defer provSrv.Close()

	wa, tg := testProviders(provSrv.URL)
	sites := map[string]Site{"telegram:b": {SiteSlug: "s", TenantID: "t"}}
	b := NewBridge(sites, convSrv.URL, voiceSrv.URL, wa, tg, zap.NewNop())
	msg := InboundMessage{Channel: "telegram", From: "1", MessageID: "77", Text: "hi"}

	if err := b.Handle(context.Background(), msg, "b"); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	if len(voice.calls) != 1 || len(prov.calls) != 1 {
		t.Fatalf("first delivery must reply once, voice=%d prov=%d", len(voice.calls), len(prov.calls))
	}

	// Redelivery after success: skipped entirely — no agentReply, no
	// sendReply, no extra turns.
	if err := b.Handle(context.Background(), msg, "b"); err != nil {
		t.Fatalf("redelivery Handle: %v", err)
	}
	if len(voice.calls) != 1 || len(prov.calls) != 1 || len(conv.turns) != 2 {
		t.Fatalf("redelivery must skip agentReply+sendReply, voice=%d prov=%d turns=%d",
			len(voice.calls), len(prov.calls), len(conv.turns))
	}

	// A message WITHOUT a provider id never dedupes (no idempotency anchor).
	msg2 := InboundMessage{Channel: "telegram", From: "1", Text: "again"}
	if err := b.Handle(context.Background(), msg2, "b"); err != nil {
		t.Fatal(err)
	}
	if err := b.Handle(context.Background(), msg2, "b"); err != nil {
		t.Fatal(err)
	}
	if len(voice.calls) != 3 {
		t.Fatalf("id-less messages must process every time, voice=%d", len(voice.calls))
	}
}

func TestBridgeFailureReleasesClaim(t *testing.T) {
	conv := &convFake{}
	convSrv := conv.server()
	defer convSrv.Close()
	// Voice runtime fails the FIRST attempt, succeeds after.
	voiceCalls := 0
	voice := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		voiceCalls++
		if voiceCalls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"conversation_id":"c1","reply":"recovered","tool_calls":[]}`)) //nolint:errcheck
	}))
	defer voice.Close()
	prov := &providerFake{}
	provSrv := prov.server()
	defer provSrv.Close()

	wa, tg := testProviders(provSrv.URL)
	sites := map[string]Site{"telegram:b": {SiteSlug: "s", TenantID: "t"}}
	b := NewBridge(sites, convSrv.URL, voice.URL, wa, tg, zap.NewNop())
	msg := InboundMessage{Channel: "telegram", From: "1", MessageID: "88", Text: "hi"}

	if err := b.Handle(context.Background(), msg, "b"); err == nil {
		t.Fatal("first attempt must fail (voice 500)")
	}
	if len(prov.calls) != 0 {
		t.Fatal("no reply on failure")
	}
	// Failure released the claim: the redelivery retries the FULL pipeline.
	if err := b.Handle(context.Background(), msg, "b"); err != nil {
		t.Fatalf("redelivery after failure must retry, got %v", err)
	}
	if voiceCalls != 2 || len(prov.calls) != 1 {
		t.Fatalf("redelivery must re-run agentReply+sendReply, voice=%d prov=%d", voiceCalls, len(prov.calls))
	}
	// And now the message is claimed.
	if err := b.Handle(context.Background(), msg, "b"); err != nil {
		t.Fatal(err)
	}
	if voiceCalls != 2 || len(prov.calls) != 1 {
		t.Fatalf("post-success redelivery must be skipped, voice=%d prov=%d", voiceCalls, len(prov.calls))
	}
}
