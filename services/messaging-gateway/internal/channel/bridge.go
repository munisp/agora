// Package channel implements the omnichannel inbound bridge (SPEC-W6
// Part A): a normalized inbound envelope, channel→tenant resolution via the
// CHANNEL_SITE_MAP env JSON, and the resolve-or-create-conversation →
// record-turn → agent-reply → record-turn → same-channel-reply flow against
// conversation-service and the voice runtime.
//
// Reliability contract (SPEC-W44 N-05): Handle reports internal failures to
// the caller, which answers 500 so the provider REDELIVERS. The bridge
// dedupes on <channel>:<message_id>: a redelivery of an already-completed
// message skips agentReply + sendReply (the whole downstream pipeline) —
// only a FAILED attempt retries, because the claim is recorded on success
// only (failure releases it implicitly).
package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/opendesk/messaging-gateway/internal/provider"
	"go.uber.org/zap"
)

// InboundMessage is the normalized inbound envelope (SPEC-W6 §A2 — exact
// shape, do not change).
type InboundMessage struct {
	Channel   string `json:"channel"`    // "whatsapp" | "telegram"
	From      string `json:"from"`       // whatsapp: E.164 phone; telegram: chat_id as string
	MessageID string `json:"message_id"` // provider message id (idempotency)
	Text      string `json:"text"`
	Timestamp int64  `json:"timestamp"` // unix seconds (telegram: message.date)
}

// Site is one CHANNEL_SITE_MAP entry: the tenant + site a channel identity
// routes to.
type Site struct {
	SiteSlug string `json:"site_slug"`
	TenantID string `json:"tenant_id"`
}

// ParseSiteMap decodes the CHANNEL_SITE_MAP env JSON:
//
//	{"whatsapp:<phone_number_id>": {"site_slug":"...","tenant_id":"<uuid>"},
//	 "telegram:<bot_username>":    {"site_slug":"...","tenant_id":"<uuid>"}}
//
// An empty string yields an empty map (inbound disabled, everything drops).
func ParseSiteMap(raw string) (map[string]Site, error) {
	m := map[string]Site{}
	if raw == "" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("parse CHANNEL_SITE_MAP: %w", err)
	}
	return m, nil
}

// Bridge routes normalized inbound messages through conversation-service
// and the voice runtime, then replies via the same-channel provider.
type Bridge struct {
	Sites map[string]Site

	// Base URLs. ConversationURL/VoiceURL are direct-base overrides
	// (CONVERSATION_URL / VOICE_RUNTIME_URL, used by tests and no-Dapr dev);
	// otherwise Dapr sidecar invoke bases
	// (http://127.0.0.1:{DAPR_HTTP_PORT}/v1.0/invoke/<app-id>/method).
	ConversationURL string
	VoiceURL        string

	WhatsApp *provider.WhatsApp
	Telegram *provider.Telegram

	HC  *http.Client
	Log *zap.Logger

	// done dedupes completed <channel>:<message_id> deliveries (N-05).
	mu       sync.Mutex
	done     map[string]time.Time
	now      func() time.Time // injectable for tests
	dedupeTTL time.Duration
}

// bridgeDedupeTTL bounds how long a completed message id is remembered
// (provider redelivery storms are minutes, not days).
const bridgeDedupeTTL = 24 * time.Hour

// SetClock injects the clock used by the dedupe window (tests).
func (b *Bridge) SetClock(now func() time.Time) { b.now = now }

// NewBridge builds a Bridge with a 10s-timeout HTTP client. convURL and
// voiceURL must already be fully resolved (direct base or Dapr invoke
// base) — see ResolveBases.
func NewBridge(sites map[string]Site, convURL, voiceURL string, wa *provider.WhatsApp, tg *provider.Telegram, log *zap.Logger) *Bridge {
	return &Bridge{
		Sites:           sites,
		ConversationURL: convURL,
		VoiceURL:        voiceURL,
		WhatsApp:        wa,
		Telegram:        tg,
		HC:              &http.Client{Timeout: 10 * time.Second},
		Log:             log,
		done:            map[string]time.Time{},
		now:             time.Now,
		dedupeTTL:       bridgeDedupeTTL,
	}
}

// ResolveBases maps the CONVERSATION_URL / VOICE_RUNTIME_URL overrides and
// the DAPR_HTTP_PORT onto the two base URLs the bridge calls. Direct-base
// overrides win; otherwise the Dapr sidecar invoke URL is used.
func ResolveBases(convOverride, voiceOverride string, daprHTTPPort int) (conv, voice string) {
	dapr := fmt.Sprintf("http://127.0.0.1:%d/v1.0/invoke", daprHTTPPort)
	conv = convOverride
	if conv == "" {
		conv = dapr + "/conversation-service/method"
	}
	voice = voiceOverride
	if voice == "" {
		voice = dapr + "/voice-agent-runtime/method"
	}
	return conv, voice
}

// Handle processes one normalized inbound message. routeID is the
// channel-routing identity: the WhatsApp metadata.phone_number_id or the
// Telegram bot username. Unmapped routes drop silently. A redelivery of an
// already-completed message (same channel+message_id) is a no-op (N-05
// dedupe); a failed attempt records nothing, so the next redelivery retries
// the full pipeline.
func (b *Bridge) Handle(ctx context.Context, msg InboundMessage, routeID string) error {
	site, ok := b.Sites[msg.Channel+":"+routeID]
	if !ok {
		b.Log.Info("inbound message dropped: no CHANNEL_SITE_MAP entry",
			zap.String("channel", msg.Channel), zap.String("route_id", routeID))
		return nil
	}
	if b.alreadyDone(msg) {
		b.Log.Info("inbound message redelivery skipped (already bridged)",
			zap.String("channel", msg.Channel), zap.String("message_id", msg.MessageID))
		return nil
	}
	log := b.Log.With(
		zap.String("channel", msg.Channel),
		zap.String("site_slug", site.SiteSlug),
		zap.String("message_id", msg.MessageID))

	convID, err := b.resolveConversation(ctx, site, msg)
	if err != nil {
		log.Warn("resolve conversation failed", zap.Error(err))
		return err
	}

	// Record the user turn; conversation-service dedupes on the
	// Idempotency-Key so provider webhook retries are safe.
	if err := b.recordTurn(ctx, convID, site.TenantID, msg.Channel+":"+msg.MessageID, "user", msg.Text); err != nil {
		log.Warn("record user turn failed", zap.Error(err))
		return err
	}

	reply, err := b.agentReply(ctx, site, convID, msg)
	if err != nil {
		log.Warn("agent reply failed", zap.Error(err))
		return err
	}

	if err := b.recordTurn(ctx, convID, site.TenantID, msg.Channel+":"+msg.MessageID+":reply", "assistant", reply); err != nil {
		log.Warn("record assistant turn failed", zap.Error(err))
		return err
	}

	if err := b.sendReply(ctx, msg, reply); err != nil {
		log.Warn("channel reply failed", zap.Error(err))
		return err
	}
	b.markDone(msg) // claim recorded on SUCCESS only — failure releases it
	log.Info("inbound message bridged", zap.String("conversation_id", convID))
	return nil
}

// alreadyDone reports whether the message was fully bridged within the
// dedupe window. Messages without a provider id cannot dedupe (processed
// every time).
func (b *Bridge) alreadyDone(msg InboundMessage) bool {
	if msg.MessageID == "" {
		return false
	}
	key := msg.Channel + ":" + msg.MessageID
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	// Lazy expiry sweep.
	for k, at := range b.done {
		if now.Sub(at) > b.dedupeTTL {
			delete(b.done, k)
		}
	}
	_, done := b.done[key]
	return done
}

// markDone records a completed delivery.
func (b *Bridge) markDone(msg InboundMessage) {
	if msg.MessageID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.done[msg.Channel+":"+msg.MessageID] = b.now()
}

// resolveConversation finds an existing conversation for the contact or
// creates one (SPEC-W6 §A3 step 2). The conversation UUID is the durable
// session/continuity key.
func (b *Bridge) resolveConversation(ctx context.Context, site Site, msg InboundMessage) (string, error) {
	q := url.Values{}
	q.Set("tenant", site.TenantID)
	q.Set("contact", msg.From)
	var list struct {
		Conversations []struct {
			ID string `json:"id"`
		} `json:"conversations"`
	}
	if err := b.doJSON(ctx, http.MethodGet, b.ConversationURL+"/v1/conversations?"+q.Encode(), nil, nil, &list); err != nil {
		return "", fmt.Errorf("list conversations: %w", err)
	}
	if len(list.Conversations) > 0 {
		return list.Conversations[0].ID, nil
	}
	var created struct {
		ID string `json:"id"`
	}
	body := map[string]string{
		"tenant_id":     site.TenantID,
		"site_slug":     site.SiteSlug,
		"channel":       msg.Channel,
		"contact_phone": msg.From,
	}
	if err := b.doJSON(ctx, http.MethodPost, b.ConversationURL+"/v1/conversations", body, nil, &created); err != nil {
		return "", fmt.Errorf("create conversation: %w", err)
	}
	if created.ID == "" {
		return "", fmt.Errorf("create conversation: empty id in response")
	}
	return created.ID, nil
}

// recordTurn appends one turn with tenant + idempotency headers (SPEC-W6
// §A3 steps 3/5).
func (b *Bridge) recordTurn(ctx context.Context, convID, tenantID, idemKey, role, text string) error {
	headers := map[string]string{
		"X-Tenant-ID":     tenantID,
		"Idempotency-Key": idemKey,
	}
	body := map[string]string{"role": role, "text": text}
	u := fmt.Sprintf("%s/v1/conversations/%s/turns", b.ConversationURL, url.PathEscape(convID))
	if err := b.doJSON(ctx, http.MethodPost, u, body, headers, nil); err != nil {
		return fmt.Errorf("record %s turn: %w", role, err)
	}
	return nil
}

// agentReply calls the voice runtime buffered chat path (SPEC-W6 §A3
// step 4 — /voice/chat, NOT the stream).
func (b *Bridge) agentReply(ctx context.Context, site Site, convID string, msg InboundMessage) (string, error) {
	body := map[string]string{
		"site_slug":       site.SiteSlug,
		"message":         msg.Text,
		"conversation_id": convID,
		"channel":         msg.Channel,
	}
	var resp struct {
		Reply string `json:"reply"`
	}
	if err := b.doJSON(ctx, http.MethodPost, b.VoiceURL+"/voice/chat", body, nil, &resp); err != nil {
		return "", fmt.Errorf("voice chat: %w", err)
	}
	if resp.Reply == "" {
		return "", fmt.Errorf("voice chat: empty reply")
	}
	return resp.Reply, nil
}

// sendReply delivers the agent reply via the same-channel provider
// (SPEC-W6 §A3 step 6).
func (b *Bridge) sendReply(ctx context.Context, msg InboundMessage, reply string) error {
	switch msg.Channel {
	case "whatsapp":
		if b.WhatsApp == nil || !b.WhatsApp.Configured() {
			return fmt.Errorf("whatsapp provider not configured")
		}
		_, _, err := b.WhatsApp.SendMessage(ctx, msg.From, reply, "")
		return err
	case "telegram":
		if b.Telegram == nil || !b.Telegram.Configured() {
			return fmt.Errorf("telegram provider not configured")
		}
		_, _, err := b.Telegram.SendMessage(ctx, msg.From, reply)
		return err
	default:
		return fmt.Errorf("unknown channel %q", msg.Channel)
	}
}

// doJSON performs one JSON request/response against an internal service.
// Non-2xx is an error with a truncated body excerpt.
func (b *Bridge) doJSON(ctx context.Context, method, u string, body, headers map[string]string, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := b.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode >= 300 {
		excerpt := raw
		if len(excerpt) > 512 {
			excerpt = excerpt[:512]
		}
		return fmt.Errorf("status %d: %s", resp.StatusCode, excerpt)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
