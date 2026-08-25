package httpapi

// SPEC-W44 N-05 tests: Telegram route gated on the configured secret (404
// when unset), bridge failure fails loud (500 → provider redelivery).

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestTelegramSecretUnsetAnswers404(t *testing.T) {
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	s.TelegramWebhookSecret = "" // TELEGRAM_WEBHOOK_SECRET unset
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/telegram", tgTextPayload, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unconfigured telegram webhook = %d, want 404 (no open ingest surface)", rec.Code)
	}
	if n := len(fb.captured()); n != 0 {
		t.Fatalf("unconfigured webhook must not reach the bridge, got %d calls", n)
	}
}

func TestBridgeFailureAnswers500WhatsApp(t *testing.T) {
	fb := &fakeBridge{err: errors.New("conversation-service down")}
	s := newWebhookServer(fb)
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/whatsapp", waTextPayload, waSignedHeaders(waTestAppSecret, waTextPayload))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("bridge failure = %d, want 500 (fail loud → Meta redelivers)", rec.Code)
	}
}

func TestBridgeFailureAnswers500Telegram(t *testing.T) {
	fb := &fakeBridge{err: errors.New("voice runtime down")}
	s := newWebhookServer(fb)
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/telegram", tgTextPayload,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "s3cret"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("bridge failure = %d, want 500 (fail loud → Telegram redelivers)", rec.Code)
	}
}

func TestIncidentSecretConstantTimeShape(t *testing.T) {
	// Wrong secret → 403; the comparison is constant-time (subtle package,
	// see webhooks.go). Unknown tenants and empty configured secrets also
	// 403 — no oracle on tenant existence vs secret mismatch.
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	s.IncidentSecrets = map[string]string{"acme-ng": "s3cret"}
	body := `{"tenant_slug":"acme-ng","secret":"%s","incident":{"incident_type":"alarm"}}`
	for _, secret := range []string{"wrong", "s3cret-extra", ""} {
		rec := do(t, s.Router(), http.MethodPost, "/webhooks/incidents",
			fmt.Sprintf(body, secret), nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("secret %q = %d, want 403", secret, rec.Code)
		}
	}
}
