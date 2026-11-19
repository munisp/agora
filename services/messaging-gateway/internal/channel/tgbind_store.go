package channel

// SPEC-W87 Part B: durable Telegram chat_id→phone bindings. The W85 bridge
// keeps bindings in a process-local map (L1); a gateway restart re-prompts
// every bound chat with the contact keyboard. TgBindStore is the optional
// L2: when wired (TG_BIND_BACKEND=dapr) bindings survive restarts in the
// Dapr state store, exactly like USSD sessions (DaprUSSDStore).
//
// Fail-open everywhere: an L2 error only logs — a lost binding re-prompts
// the contact keyboard (safe direction, identical to today's restart
// behavior).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// TgBindStore persists Telegram chat_id→phone bindings outside the process.
type TgBindStore interface {
	// GetTgBind returns the bound phone or ("", nil) when missing/expired.
	GetTgBind(ctx context.Context, chatID string) (string, error)
	// SaveTgBind upserts the binding with the store-side TTL.
	SaveTgBind(ctx context.Context, chatID, phone string, ttl time.Duration) error
}

// tgBindRecord is the stored payload (json-shaped for forward-compat).
type tgBindRecord struct {
	Phone string `json:"phone"`
}

// DaprTgBindStore keeps bindings in a Dapr state store (mirrors
// DaprUSSDStore — net/http state pattern, no Dapr SDK). The TTL is
// delegated to the state store via the ttlInSeconds metadata key.
//
// INFRA NOTE: same statestore.scopes caveat as DaprUSSDStore — the store
// name is configurable via TG_BIND_STATE_STORE.
type DaprTgBindStore struct {
	base  string // http://127.0.0.1:{DAPR_HTTP_PORT}
	store string // state store component name
	hc    *http.Client
}

// NewDaprTgBindStore builds the store client; store defaults to
// "statestore".
func NewDaprTgBindStore(store string, daprHTTPPort int) *DaprTgBindStore {
	if store == "" {
		store = "statestore"
	}
	return &DaprTgBindStore{
		base:  fmt.Sprintf("http://127.0.0.1:%d", daprHTTPPort),
		store: store,
		hc:    &http.Client{Timeout: 2 * time.Second},
	}
}

func (s *DaprTgBindStore) key(chatID string) string { return "tgbind:" + chatID }

// GetTgBind returns the bound phone or ("", nil) when missing/expired (the
// state store enforces the TTL and answers 204 No Content).
func (s *DaprTgBindStore) GetTgBind(ctx context.Context, chatID string) (string, error) {
	u := fmt.Sprintf("%s/v1.0/state/%s/%s", s.base, s.store, s.key(chatID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("state get %s: %w", s.key(chatID), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("state get %s: status %d: %s", s.key(chatID), resp.StatusCode, b)
	}
	var rec tgBindRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return "", fmt.Errorf("state get %s: decode: %w", s.key(chatID), err)
	}
	return rec.Phone, nil
}

// SaveTgBind upserts the binding with the store-side TTL.
func (s *DaprTgBindStore) SaveTgBind(ctx context.Context, chatID, phone string, ttl time.Duration) error {
	body, err := json.Marshal([]map[string]any{{
		"key":      s.key(chatID),
		"value":    tgBindRecord{Phone: phone},
		"metadata": map[string]string{"ttlInSeconds": strconv.Itoa(int(ttl.Seconds()))},
	}})
	if err != nil {
		return err
	}
	u := fmt.Sprintf("%s/v1.0/state/%s", s.base, s.store)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("state save %s: %w", s.key(chatID), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("state save %s: status %d: %s", s.key(chatID), resp.StatusCode, b)
	}
	return nil
}
