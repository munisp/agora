package channel

// SPEC-W87: durable Telegram bindings — L1/L2 bridge behavior.
// REDIRECTION PROOF: fails on the base tree (SetTgBindStore /
// telegramBoundPhoneCtx / TgBindStore undefined).

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeTgBindStore is an in-memory TgBindStore with call counters and an
// injectable error.
type fakeTgBindStore struct {
	mu    sync.Mutex
	binds map[string]string
	err   error
	saves int
	gets  int
	ttl   time.Duration
}

func (f *fakeTgBindStore) GetTgBind(_ context.Context, chatID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.err != nil {
		return "", f.err
	}
	return f.binds[chatID], nil
}

func (f *fakeTgBindStore) SaveTgBind(_ context.Context, chatID, phone string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	f.ttl = ttl
	if f.err != nil {
		return f.err
	}
	if f.binds == nil {
		f.binds = map[string]string{}
	}
	f.binds[chatID] = phone
	return nil
}

func TestW87BindPersistsToStore(t *testing.T) {
	b := newW85Bridge(t, "http://127.0.0.1:1", "http://127.0.0.1:1", "")
	fs := &fakeTgBindStore{}
	b.SetTgBindStore(fs)

	b.BindTelegramContact("42424242", "+2348012345678")

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.saves != 1 {
		t.Fatalf("expected 1 L2 save, got %d", fs.saves)
	}
	if fs.binds["42424242"] != "+2348012345678" {
		t.Fatalf("L2 bind = %v", fs.binds)
	}
	if fs.ttl != bridgeTgBindTTL {
		t.Fatalf("save TTL = %v, want %v", fs.ttl, bridgeTgBindTTL)
	}
	// L1 holds the binding too (same behavior as pre-W87).
	if got := b.telegramBoundPhone("42424242"); got != "+2348012345678" {
		t.Fatalf("L1 bind = %q", got)
	}
}

func TestW87L2BackfillsL1(t *testing.T) {
	b := newW85Bridge(t, "http://127.0.0.1:1", "http://127.0.0.1:1", "")
	fs := &fakeTgBindStore{binds: map[string]string{"42424242": "+2348012345678"}}
	b.SetTgBindStore(fs)

	// L1 is empty (fresh bridge, e.g. after a restart): the ctx variant
	// must find the L2 binding exactly once...
	if got := b.telegramBoundPhoneCtx(context.Background(), "42424242"); got != "+2348012345678" {
		t.Fatalf("L2 lookup = %q", got)
	}
	// ...and backfill L1, so the second lookup stays in-memory.
	if got := b.telegramBoundPhoneCtx(context.Background(), "42424242"); got != "+2348012345678" {
		t.Fatalf("L1-backfilled lookup = %q", got)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.gets != 1 {
		t.Fatalf("expected exactly 1 L2 get (then L1 hits), got %d", fs.gets)
	}
}

func TestW87L2MissAndErrorFailOpen(t *testing.T) {
	b := newW85Bridge(t, "http://127.0.0.1:1", "http://127.0.0.1:1", "")
	fs := &fakeTgBindStore{}
	b.SetTgBindStore(fs)

	// Miss -> unbound.
	if got := b.telegramBoundPhoneCtx(context.Background(), "42424242"); got != "" {
		t.Fatalf("miss = %q, want unbound", got)
	}
	// Error -> fail-open to unbound (re-prompt path, safe direction).
	fs.mu.Lock()
	fs.err = errors.New("dapr sidecar down")
	fs.mu.Unlock()
	if got := b.telegramBoundPhoneCtx(context.Background(), "42424242"); got != "" {
		t.Fatalf("error = %q, want unbound (fail-open)", got)
	}
	// Save error is equally fail-open: the L1 binding still lands.
	b.BindTelegramContact("42424242", "+2348012345678")
	if got := b.telegramBoundPhone("42424242"); got != "+2348012345678" {
		t.Fatalf("L1 bind after failed L2 save = %q", got)
	}
}

func TestW87NilStoreZeroValueCompat(t *testing.T) {
	// No SetTgBindStore call: the ctx variant must behave byte-identically
	// to the pre-W87 in-memory path.
	b := newW85Bridge(t, "http://127.0.0.1:1", "http://127.0.0.1:1", "")
	if got := b.telegramBoundPhoneCtx(context.Background(), "42424242"); got != "" {
		t.Fatalf("nil store, unbound = %q", got)
	}
	b.BindTelegramContact("42424242", "+2348012345678")
	if got := b.telegramBoundPhoneCtx(context.Background(), "42424242"); got != "+2348012345678" {
		t.Fatalf("nil store, bound = %q", got)
	}
}

func TestW87EndToEndPinFromStore(t *testing.T) {
	voice := &voiceW85Fake{}
	voiceSrv := voice.server()
	defer voiceSrv.Close()
	prov := &tgRawFake{}
	provSrv := prov.server()
	defer provSrv.Close()
	b := newW85Bridge(t, provSrv.URL, voiceSrv.URL, "")

	// Binding exists ONLY in L2 (gateway restarted, L1 empty).
	fs := &fakeTgBindStore{binds: map[string]string{"42424242": "+2348012345678"}}
	b.SetTgBindStore(fs)

	driveW85(t, b, &convFake{}, tgMsg("901"))

	voice.mu.Lock()
	defer voice.mu.Unlock()
	if len(voice.bodies) != 1 {
		t.Fatalf("expected 1 voice chat call, got %d", len(voice.bodies))
	}
	body := voice.bodies[0]
	if got := body["channel_identity"]; got != "+2348012345678" {
		t.Fatalf("L2-backed telegram chat must pin the stored phone, got %q; body=%v", got, body)
	}
	if !voice.hasHeader[0] || voice.tokens[0] != "test-voice-admin-token" {
		t.Fatalf("pin must carry X-Internal-Token when configured, got %q (present=%v)",
			voice.tokens[0], voice.hasHeader[0])
	}
}
