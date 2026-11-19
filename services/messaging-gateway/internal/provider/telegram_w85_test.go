package provider

import (
	"context"
	"encoding/json"
	"testing"
)

// SPEC-W85: SendContactRequest — sendMessage with a one-time
// request_contact reply keyboard. This file FAILS on the base tree
// (method absent) and passes on the candidate (redirection proof).

func TestTelegramSendContactRequestPayload(t *testing.T) {
	var last *fakeProvider
	runCase(t, func(baseURL string, f *fakeProvider) error {
		last = f
		p := &Telegram{Client: testClient("telegram"), BaseURL: baseURL, Token: "123456:ABC-def"}
		_, _, err := p.SendContactRequest(context.Background(), "42424242", "Please share your number", "Share my number")
		return err
	})

	f := last
	if f.path != "/bot123456:ABC-def/sendMessage" {
		t.Fatalf("unexpected path %q", f.path)
	}
	if f.authHeader != "" {
		t.Fatalf("bot API must not send an Authorization header, got %q", f.authHeader)
	}
	var payload map[string]any
	if err := json.Unmarshal(f.body, &payload); err != nil {
		t.Fatalf("decode telegram payload: %v", err)
	}
	// Same plain-text contract as SendMessage...
	if payload["chat_id"] != "42424242" || payload["text"] != "Please share your number" {
		t.Fatalf("bad sendMessage payload: %s", f.body)
	}
	if pm, ok := payload["parse_mode"]; !ok || pm != "" {
		t.Fatalf("plain text only: parse_mode must be present and empty: %s", f.body)
	}
	// ...plus the request_contact keyboard.
	markup, ok := payload["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("reply_markup missing: %s", f.body)
	}
	if markup["one_time_keyboard"] != true || markup["resize_keyboard"] != true {
		t.Fatalf("keyboard must be one-time + resized: %s", f.body)
	}
	rows, ok := markup["keyboard"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("keyboard must have exactly one row: %s", f.body)
	}
	row, ok := rows[0].([]any)
	if !ok || len(row) != 1 {
		t.Fatalf("row must have exactly one button: %s", f.body)
	}
	btn, ok := row[0].(map[string]any)
	if !ok {
		t.Fatalf("button must be an object: %s", f.body)
	}
	if btn["text"] != "Share my number" || btn["request_contact"] != true {
		t.Fatalf("button must request contact: %s", f.body)
	}
}

func TestTelegramSendMessageHasNoKeyboard(t *testing.T) {
	// Byte-compat: the plain SendMessage payload is unchanged by W85.
	var last *fakeProvider
	runCase(t, func(baseURL string, f *fakeProvider) error {
		last = f
		p := &Telegram{Client: testClient("telegram"), BaseURL: baseURL, Token: "tok"}
		_, _, err := p.SendMessage(context.Background(), "42424242", "hello")
		return err
	})
	var payload map[string]any
	if err := json.Unmarshal(last.body, &payload); err != nil {
		t.Fatalf("decode telegram payload: %v", err)
	}
	if _, ok := payload["reply_markup"]; ok {
		t.Fatalf("SendMessage must NOT carry reply_markup: %s", last.body)
	}
}
