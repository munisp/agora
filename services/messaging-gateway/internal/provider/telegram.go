package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Telegram sends messages via the Telegram Bot API
// (https://core.telegram.org/bots/api#sendmessage —
// POST {base}/bot{token}/sendMessage). Plain text only (no parse_mode),
// matching the omnichannel inbound bridge (SPEC-W6 Part A).
type Telegram struct {
	Client  *Client
	BaseURL string // https://api.telegram.org
	Token   string // TELEGRAM_BOT_TOKEN (BotFather)
}

// Configured reports whether the provider has the credentials it needs.
func (t *Telegram) Configured() bool { return t.Token != "" }

// SendMessage delivers a plain-text message to a chat. chatID is the
// numeric chat id as a string (kept as string in JSON so 64-bit ids never
// lose precision through the gateway).
func (t *Telegram) SendMessage(ctx context.Context, chatID, message string) (int, []byte, error) {
	payload := map[string]any{
		"chat_id":    chatID,
		"text":       message,
		"parse_mode": "",
	}
	return t.Client.send(ctx, func(ctx context.Context) (*http.Request, error) {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal telegram payload: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			t.BaseURL+"/bot"+t.Token+"/sendMessage", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
}

// SendContactRequest delivers a plain-text message with a one-time reply
// keyboard whose single button asks the user's client to share the
// account's own phone number (Telegram Bot API KeyboardButton
// request_contact —
// https://core.telegram.org/bots/api#keyboardbutton). This is the
// canonical Telegram identity-verification mechanism: the resulting
// message.contact.phone_number is provider-verified (the account's own
// number), the same trust class as a WhatsApp wa_id. SPEC-W85.
func (t *Telegram) SendContactRequest(ctx context.Context, chatID, message, buttonText string) (int, []byte, error) {
	payload := map[string]any{
		"chat_id":    chatID,
		"text":       message,
		"parse_mode": "",
		"reply_markup": map[string]any{
			"keyboard": [][]map[string]any{
				{{"text": buttonText, "request_contact": true}},
			},
			"one_time_keyboard": true,
			"resize_keyboard":   true,
		},
	}
	return t.Client.send(ctx, func(ctx context.Context) (*http.Request, error) {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal telegram contact-request payload: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			t.BaseURL+"/bot"+t.Token+"/sendMessage", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
}
