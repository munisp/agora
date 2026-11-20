// SPEC-W85: Telegram contact-share ingestion — self-share enforcement,
// phone canonicalization, bridge binding, and the synthetic shared-contact
// turn. REDIRECTION PROOF: fails on the base tree (contact field absent ->
// share ignored, no BindTelegramContact) and passes on the candidate.
package httpapi

import (
	"net/http"
	"testing"

	"github.com/opendesk/messaging-gateway/internal/channel"
)

const tgContactPayload = `{
  "update_id": 9002,
  "message": {
    "message_id": 556,
    "date": 1733000200,
    "chat": {"id": 42424242, "type": "private"},
    "from": {"id": 42424242, "first_name": "Ada"},
    "contact": {"phone_number": "2348012345678", "user_id": 42424242, "first_name": "Ada"}
  }
}`

func TestTelegramContactSelfShareBoundAndBridged(t *testing.T) {
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/telegram", tgContactPayload,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "s3cret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	binds := fb.capturedBinds()
	if len(binds) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(binds))
	}
	if binds[0].chatID != "42424242" || binds[0].phone != "+2348012345678" {
		t.Fatalf("binding = %+v, want chat 42424242 -> +2348012345678", binds[0])
	}
	calls := fb.captured()
	if len(calls) != 1 {
		t.Fatalf("expected 1 bridge call, got %d", len(calls))
	}
	want := channel.InboundMessage{
		Channel:   "telegram",
		From:      "42424242",
		MessageID: "556",
		Text:      channel.TgContactSharedText,
		Timestamp: 1733000200,
	}
	if calls[0].msg != want {
		t.Fatalf("envelope mismatch:\n got %+v\nwant %+v", calls[0].msg, want)
	}
}

func TestTelegramContactForwardedShareRejected(t *testing.T) {
	// contact.user_id != from.id: a forwarded contact must NEVER pin
	// someone else's phone (impersonation).
	spoofed := `{
	  "update_id": 9003,
	  "message": {
	    "message_id": 557,
	    "date": 1733000300,
	    "chat": {"id": 42424242, "type": "private"},
	    "from": {"id": 42424242, "first_name": "Ada"},
	    "contact": {"phone_number": "2348099999999", "user_id": 777, "first_name": "Victim"}
	  }
	}`
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/telegram", spoofed,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "s3cret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("spoofed share must be ignored with 200 (no redelivery loop), got %d", rec.Code)
	}
	if n := len(fb.capturedBinds()); n != 0 {
		t.Fatalf("spoofed share must not bind, got %d binds", n)
	}
	if n := len(fb.captured()); n != 0 {
		t.Fatalf("spoofed share must not bridge, got %d calls", n)
	}
}

func TestTelegramContactMissingFromRejected(t *testing.T) {
	noFrom := `{
	  "update_id": 9004,
	  "message": {
	    "message_id": 558,
	    "date": 1733000400,
	    "chat": {"id": 42424242, "type": "private"},
	    "contact": {"phone_number": "2348012345678", "user_id": 42424242}
	  }
	}`
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/telegram", noFrom,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "s3cret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("from-less share must be ignored with 200, got %d", rec.Code)
	}
	if n := len(fb.capturedBinds()); n != 0 {
		t.Fatalf("from-less share must not bind, got %d binds", n)
	}
	if n := len(fb.captured()); n != 0 {
		t.Fatalf("from-less share must not bridge, got %d calls", n)
	}
}

func TestTelegramContactEmptyPhoneRejected(t *testing.T) {
	emptyPhone := `{
	  "update_id": 9005,
	  "message": {
	    "message_id": 559,
	    "date": 1733000500,
	    "chat": {"id": 42424242, "type": "private"},
	    "from": {"id": 42424242, "first_name": "Ada"},
	    "contact": {"phone_number": "+--()", "user_id": 42424242}
	  }
	}`
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/telegram", emptyPhone,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "s3cret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("empty-phone share must be ignored with 200, got %d", rec.Code)
	}
	if n := len(fb.capturedBinds()); n != 0 {
		t.Fatalf("empty-phone share must not bind, got %d binds", n)
	}
	if n := len(fb.captured()); n != 0 {
		t.Fatalf("empty-phone share must not bridge, got %d calls", n)
	}
}

func TestTelegramContactPlusAndPrefixNormalized(t *testing.T) {
	// phone.Digits strips separators and ONE leading 00 prefix.
	prefixed := `{
	  "update_id": 9006,
	  "message": {
	    "message_id": 560,
	    "date": 1733000600,
	    "chat": {"id": 42424242, "type": "private"},
	    "from": {"id": 42424242, "first_name": "Ada"},
	    "contact": {"phone_number": "00234 801 234 5678", "user_id": 42424242}
	  }
	}`
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/telegram", prefixed,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "s3cret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	binds := fb.capturedBinds()
	if len(binds) != 1 || binds[0].phone != "+2348012345678" {
		t.Fatalf("normalized binding = %+v, want +2348012345678", binds)
	}
}

func TestTelegramTextLessNonContactStillIgnored(t *testing.T) {
	// Byte-compat: a text-less update without a contact is still ignored.
	sticker := `{"update_id": 9007, "message": {"message_id": 561, "date": 1733000700, "chat": {"id": 42424242}, "from": {"id": 42424242}}}`
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/telegram", sticker,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "s3cret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if n := len(fb.captured()); n != 0 {
		t.Fatalf("text-less non-contact update must be ignored, got %d calls", n)
	}
	if n := len(fb.capturedBinds()); n != 0 {
		t.Fatalf("text-less non-contact update must not bind, got %d binds", n)
	}
}

func TestTelegramContactWithTextKeepsText(t *testing.T) {
	// A contact share that also carries text bridges the REAL text (the
	// synthetic placeholder is only for text-less shares).
	withText := `{
	  "update_id": 9008,
	  "message": {
	    "message_id": 562,
	    "date": 1733000800,
	    "text": "here is my number",
	    "chat": {"id": 42424242, "type": "private"},
	    "from": {"id": 42424242, "first_name": "Ada"},
	    "contact": {"phone_number": "2348012345678", "user_id": 42424242}
	  }
	}`
	fb := &fakeBridge{}
	s := newWebhookServer(fb)
	rec := do(t, s.Router(), http.MethodPost, "/webhooks/telegram", withText,
		map[string]string{"X-Telegram-Bot-Api-Secret-Token": "s3cret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	calls := fb.captured()
	if len(calls) != 1 || calls[0].msg.Text != "here is my number" {
		t.Fatalf("share with text must bridge the real text, got %+v", calls)
	}
	if n := len(fb.capturedBinds()); n != 1 {
		t.Fatalf("share with text must still bind, got %d binds", n)
	}
}
