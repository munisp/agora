// USSD channel support (SPEC-W12 Agent A): session state (180s TTL), the
// pack-driven numeric menu model, the tenant menu fetch and the synchronous
// request/reply client to conversation-service.
//
// Contract (SPEC-W12 §1): Africa's Talking posts form fields sessionId,
// serviceCode, phoneNumber, text (text is the CUMULATIVE "1*2*3" input,
// empty on the first request of a session). The gateway answers text/plain
// prefixed "CON " (session continues) or "END " (session terminates).
//
// Request/reply contract with conversation-service (DELIVERED by Agent D,
// app/ussd.py + routes.py): every callback is forwarded synchronously via
// the same Dapr invoke path the other inbound channels use
// (http://127.0.0.1:{DAPR_HTTP_PORT}/v1.0/invoke/conversation-service/method,
// overridable with CONVERSATION_URL) as POST {base}/v1/ussd/turns with the
// tenant pack's resolved ussd.menu attached when defined (menu mode is
// driven conversation-side: numeric selection, 0=back, 00=main menu).
// conversation-service returns {conversation_id, reply, continue, mode,
// selection, action} in the invoke response body; the gateway renders
// "CON reply" when continue=true, "END reply" otherwise.
package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// USSDSessionTTL is the session state TTL (SPEC-W12 §1: 180s).
const USSDSessionTTL = 180 * time.Second

// USSDMenuItem is one entry of a tenant pack's ussd.menu list
// ({key,label,action}).
type USSDMenuItem struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Action string `json:"action"`
}

// ParseUSSDMenu extracts pack.ussd.menu from an identity tenant payload
// (GET /v1/tenants/{slug} → {"pack": {...}}).
//
// ASSUMPTION: identity's pack Summary passes the pack yaml `ussd:` block
// through as JSON {"ussd": {"menu": [...]}}. That passthrough does not exist
// in identity-service yet (flagged for a follow-up), so today every tenant
// resolves to an empty menu and the gateway runs in pass-through text mode —
// the SPEC-mandated fallback. Items without key/label are skipped.
func ParseUSSDMenu(tenantPayload map[string]any) []USSDMenuItem {
	pack, ok := tenantPayload["pack"].(map[string]any)
	if !ok {
		return nil
	}
	ussd, ok := pack["ussd"].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := ussd["menu"].([]any)
	if !ok {
		return nil
	}
	menu := make([]USSDMenuItem, 0, len(raw))
	for _, entry := range raw {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		item := USSDMenuItem{
			Key:    asString(m["key"]),
			Label:  asString(m["label"]),
			Action: asString(m["action"]),
		}
		if item.Key == "" || item.Label == "" {
			continue
		}
		menu = append(menu, item)
	}
	return menu
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// NOTE: menu rendering and the numeric navigation state machine
// (1/2/… select, 0=back, 00=main menu) live conversation-side in Agent D's
// app/ussd.py — the gateway resolves the pack menu once per session and
// attaches it to every turn (USSDTurnRequest.Menu).

// ---------------------------------------------------------------------------
// Session state
// ---------------------------------------------------------------------------

// USSDSession is the per-sessionId state kept between callbacks.
type USSDSession struct {
	ID          string         `json:"id"`           // provider sessionId
	ServiceCode string         `json:"service_code"` // e.g. *384*123#
	Phone       string         `json:"phone"`        // E.164
	SiteSlug    string         `json:"site_slug"`    // CHANNEL_SITE_MAP resolution
	TenantID    string         `json:"tenant_id"`
	Menu        []USSDMenuItem `json:"menu,omitempty"` // resolved pack menu; nil = pass-through text mode
	UpdatedAt   time.Time      `json:"updated_at"`
}

func (s *USSDSession) clone() *USSDSession {
	out := *s
	if s.Menu != nil {
		out.Menu = make([]USSDMenuItem, len(s.Menu))
		copy(out.Menu, s.Menu)
	}
	return &out
}

// USSDSessionStore keeps session state for the callback round-trips.
type USSDSessionStore interface {
	// Get returns the session, or (nil, nil) when absent or expired.
	Get(ctx context.Context, id string) (*USSDSession, error)
	Save(ctx context.Context, s *USSDSession, ttl time.Duration) error
	Delete(ctx context.Context, id string) error
}

// MemoryUSSDStore is the default store: in-process map with lazy TTL
// expiry. The messaging-gateway is deployed as a single replica per
// environment (see internal/metrics), so process-local state matches the
// established scaling model; USSD_SESSION_BACKEND=dapr switches to the Dapr
// state store for multi-replica deployments.
type MemoryUSSDStore struct {
	mu  sync.Mutex
	m   map[string]memoryUSSDEntry
	now func() time.Time // injectable for tests
}

type memoryUSSDEntry struct {
	sess      *USSDSession
	expiresAt time.Time
}

// NewMemoryUSSDStore builds an empty in-memory store.
func NewMemoryUSSDStore() *MemoryUSSDStore {
	return &MemoryUSSDStore{m: map[string]memoryUSSDEntry{}, now: time.Now}
}

// Get returns the session or (nil, nil) when missing/expired.
func (s *MemoryUSSDStore) Get(_ context.Context, id string) (*USSDSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok {
		return nil, nil
	}
	if s.now().After(e.expiresAt) {
		delete(s.m, id)
		return nil, nil
	}
	return e.sess.clone(), nil
}

// Save upserts the session with a fresh TTL.
func (s *MemoryUSSDStore) Save(_ context.Context, sess *USSDSession, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[sess.ID] = memoryUSSDEntry{sess: sess.clone(), expiresAt: s.now().Add(ttl)}
	return nil
}

// Delete drops the session (session end / explicit reset).
func (s *MemoryUSSDStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
	return nil
}

// DaprUSSDStore keeps sessions in a Dapr state store (mirrors the daprc
// net/http state pattern used by booking/identity/notification — no Dapr
// SDK). The TTL is delegated to the state store via the ttlInSeconds
// metadata key.
//
// INFRA NOTE: infra/dapr/components/statestore.redis.yaml does not list the
// messaging-gateway app-id in its scopes today (flagged for Agent D); the
// store name is configurable via USSD_STATE_STORE.
type DaprUSSDStore struct {
	base  string // http://127.0.0.1:{DAPR_HTTP_PORT}
	store string // state store component name
	hc    *http.Client
}

// NewDaprUSSDStore builds the store client; store defaults to "statestore".
func NewDaprUSSDStore(store string, daprHTTPPort int) *DaprUSSDStore {
	if store == "" {
		store = "statestore"
	}
	return &DaprUSSDStore{
		base:  fmt.Sprintf("http://127.0.0.1:%d", daprHTTPPort),
		store: store,
		hc:    &http.Client{Timeout: 5 * time.Second},
	}
}

func (s *DaprUSSDStore) key(id string) string { return "ussd:" + id }

// Get returns the session or (nil, nil) when missing/expired (the state
// store enforces the TTL and answers 204 No Content).
func (s *DaprUSSDStore) Get(ctx context.Context, id string) (*USSDSession, error) {
	u := fmt.Sprintf("%s/v1.0/state/%s/%s", s.base, s.store, s.key(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("state get %s: %w", s.key(id), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("state get %s: status %d: %s", s.key(id), resp.StatusCode, b)
	}
	var sess USSDSession
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		return nil, fmt.Errorf("state get %s: decode: %w", s.key(id), err)
	}
	return &sess, nil
}

// Save upserts the session with the store-side TTL.
func (s *DaprUSSDStore) Save(ctx context.Context, sess *USSDSession, ttl time.Duration) error {
	body, err := json.Marshal([]map[string]any{{
		"key":      s.key(sess.ID),
		"value":    sess,
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
		return fmt.Errorf("state save %s: %w", s.key(sess.ID), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("state save %s: status %d: %s", s.key(sess.ID), resp.StatusCode, b)
	}
	return nil
}

// Delete drops the session.
func (s *DaprUSSDStore) Delete(ctx context.Context, id string) error {
	u := fmt.Sprintf("%s/v1.0/state/%s/%s", s.base, s.store, s.key(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("state delete %s: %w", s.key(id), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("state delete %s: status %d: %s", s.key(id), resp.StatusCode, b)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Upstream clients
// ---------------------------------------------------------------------------

// ResolveInvokeBase maps a direct-base override onto the Dapr sidecar
// invoke base for an app-id (same convention as ResolveBases).
func ResolveInvokeBase(override, appID string, daprHTTPPort int) string {
	if override != "" {
		return override
	}
	return fmt.Sprintf("http://127.0.0.1:%d/v1.0/invoke/%s/method", daprHTTPPort, appID)
}

// USSDMenuFetcher loads the tenant pack's ussd.menu (nil = pass-through
// text mode). Implemented by HTTPUSSDMenuFetcher in prod, faked in tests.
type USSDMenuFetcher interface {
	USSDMenu(ctx context.Context, tenantSlug string) ([]USSDMenuItem, error)
}

// HTTPUSSDMenuFetcher fetches the menu via the identity packs summary
// endpoint (GET {base}/v1/tenants/{slug}, same client pattern as the voice
// runtime's tenant-context fetch — Dapr invoke app-id identity, overridable
// with IDENTITY_URL). tenantSlug is the CHANNEL_SITE_MAP site_slug for the
// USSD service code (ASSUMPTION: for USSD deployments the site slug is the
// tenant slug — single-site tenants, the common case for the NG channel).
type HTTPUSSDMenuFetcher struct {
	Base string
	HC   *http.Client
}

// NewUSSDMenuFetcher builds the fetcher with a 5s-timeout client.
func NewUSSDMenuFetcher(identityBase string) *HTTPUSSDMenuFetcher {
	return &HTTPUSSDMenuFetcher{Base: identityBase, HC: &http.Client{Timeout: 5 * time.Second}}
}

// USSDMenu returns the parsed menu, or nil when the pack defines none.
func (f *HTTPUSSDMenuFetcher) USSDMenu(ctx context.Context, tenantSlug string) ([]USSDMenuItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		f.Base+"/v1/tenants/"+tenantSlug, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.HC.Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity tenant fetch: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("identity tenant fetch: status %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("identity tenant fetch: decode: %w", err)
	}
	return ParseUSSDMenu(payload), nil
}

// USSDTurnRequest is the synchronous request/reply contract with
// conversation-service (POST {base}/v1/ussd/turns — DELIVERED by Agent D,
// app/ussd.py UssdTurnRequest; keep the field set exactly in sync).
type USSDTurnRequest struct {
	TenantID    string `json:"tenant_id"`  // uuid — conversation key is uuid5(tenant, sessionId)
	SiteSlug    string `json:"site_slug"`  // required by conversation-service
	SessionID   string `json:"session_id"` // provider sessionId
	ServiceCode string `json:"service_code,omitempty"`
	PhoneNumber string `json:"phone_number,omitempty"`
	Text        string `json:"text"` // cumulative "1*2*3" input as received ("" on first request)
	// Menu is the tenant pack's resolved ussd.menu; its presence switches
	// the session to menu mode conversation-side. Omitted = pass-through
	// text mode.
	Menu []USSDMenuItem `json:"menu,omitempty"`
}

// USSDTurnResponse is what conversation-service returns in the invoke
// response body (Agent D response_payload); the gateway renders
// "CON reply" when Continue is true, "END reply" otherwise.
type USSDTurnResponse struct {
	ConversationID string `json:"conversation_id"`
	Reply          string `json:"reply"`
	Continue       bool   `json:"continue"` // true → session stays open (CON)
	Mode           string `json:"mode"`     // "menu" | "text"
	Selection      string `json:"selection"`
	Action         string `json:"action"`
}

// USSDConversation is the conversation-service USSD turn contract
// (implemented by HTTPUSSDConversation in prod, faked in tests).
type USSDConversation interface {
	Turn(ctx context.Context, req USSDTurnRequest) (USSDTurnResponse, error)
}

// HTTPUSSDConversation invokes conversation-service via the same Dapr
// invoke path the other inbound channels use (CONVERSATION_URL override or
// {DAPR}/v1.0/invoke/conversation-service/method).
type HTTPUSSDConversation struct {
	Base string
	HC   *http.Client
}

// NewUSSDConversation builds the client with a 10s-timeout HTTP client
// (matches the bridge's client).
func NewUSSDConversation(conversationBase string) *HTTPUSSDConversation {
	return &HTTPUSSDConversation{Base: conversationBase, HC: &http.Client{Timeout: 10 * time.Second}}
}

// Turn posts one USSD interaction and decodes the reply.
func (c *HTTPUSSDConversation) Turn(ctx context.Context, turn USSDTurnRequest) (USSDTurnResponse, error) {
	var out USSDTurnResponse
	body, err := json.Marshal(turn)
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/v1/ussd/turns", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HC.Do(req)
	if err != nil {
		return out, fmt.Errorf("conversation ussd turn: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("conversation ussd turn: status %d: %s", resp.StatusCode, truncateBody(raw))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("conversation ussd turn: decode: %w", err)
	}
	return out, nil
}

func truncateBody(b []byte) string {
	if len(b) > 512 {
		return string(b[:512])
	}
	return string(b)
}
