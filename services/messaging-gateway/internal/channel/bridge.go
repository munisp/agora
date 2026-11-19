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
	"container/heap"
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

	// VoiceInternalToken (MESSAGING_VOICE_INTERNAL_TOKEN) authorizes the
	// WhatsApp channel-identity pin on POST /voice/chat (SPEC-W81 C1): the
	// runtime pins the provider-verified wa_id as the session's verified
	// phone ONLY when X-Internal-Token matches its
	// VOICE_ADMIN_INTERNAL_TOKEN (K15(c) soft gate — fail-closed when
	// unset). Empty here = the header is not sent and the peer ignores the
	// pin (fail-safe, no spoofing surface opened).
	VoiceInternalToken string

	WhatsApp *provider.WhatsApp
	Telegram *provider.Telegram

	HC  *http.Client
	Log *zap.Logger

	// done dedupes completed <channel>:<message_id> deliveries (N-05).
	// doneExp is a min-heap of the same completions ordered by time:
	// expiry/eviction pops the OLDEST entries (O(log n) amortized) instead
	// of the previous O(n) sweep of every key on every inbound message
	// (SPEC-W46 PERF-18), and the map stays bounded at
	// bridgeDedupeMaxEntries.
	mu      sync.Mutex
	done    map[string]time.Time
	doneExp doneHeap
	// tgBinds maps a Telegram chat_id to the provider-verified phone the
	// chat's user shared via a request_contact keyboard (SPEC-W85).
	// Telegram's From is a chat_id, not a phone, so unlike WhatsApp (wa_id
	// IS the phone, pinned per-turn) the bridge must remember the binding
	// and pin it on subsequent turns. tgBindExp mirrors doneExp: a min-heap
	// for O(log n) expiry/eviction; the map is bounded at
	// bridgeTgBindMaxEntries with a bridgeTgBindTTL lifetime. In-memory by
	// design (restart drops bindings -> affected chats are re-prompted via
	// request_contact); durable bindings are a registered follow-up.
	tgBinds   map[string]tgBind
	tgBindExp doneHeap
	now       func() time.Time // injectable for tests
	dedupeTTL time.Duration
}

// tgBind is one Telegram chat_id -> verified-phone binding.
type tgBind struct {
	phone string
	at    time.Time
}

// doneEntry is one heap element: a completed delivery key + completion time.
type doneEntry struct {
	key string
	at  time.Time
}

// doneHeap is a min-heap of doneEntry ordered by completion time (oldest
// first) — container/heap.Interface.
type doneHeap []doneEntry

func (h doneHeap) Len() int           { return len(h) }
func (h doneHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h doneHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *doneHeap) Push(x any)        { *h = append(*h, x.(doneEntry)) }
func (h *doneHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = doneEntry{}
	*h = old[:n-1]
	return e
}

// bridgeDedupeTTL bounds how long a completed message id is remembered
// (provider redelivery storms are minutes, not days).
const bridgeDedupeTTL = 24 * time.Hour

// bridgeDedupeMaxEntries bounds the dedupe map (SPEC-W46 PERF-18: every
// cache needs a bound). At capacity the OLDEST completions are evicted —
// 100k ids ≈ a full day of traffic at >1 msg/s, so a capacity eviction
// cannot open a realistic redelivery replay window.
const bridgeDedupeMaxEntries = 100_000

// bridgeTgBindTTL bounds how long a Telegram chat_id -> verified-phone
// binding lives (SPEC-W85). 30 days: long enough that an active chat never
// re-prompts, short enough that a number change is eventually re-verified.
const bridgeTgBindTTL = 30 * 24 * time.Hour

// bridgeTgBindMaxEntries bounds the binding map (same PERF-18 posture as
// the dedupe map: every cache needs a bound; oldest-first eviction).
const bridgeTgBindMaxEntries = 100_000

// TgContactSharedText is the fixed synthetic user-turn text bridged when a
// Telegram contact share arrives (shares carry no text). Fixed by design:
// no user-controlled content is synthesized into the agent turn. Exported
// for the webhook handler (httpapi), which synthesizes the turn.
const TgContactSharedText = "[shared contact]"

// tgContactButtonText is the request_contact keyboard button label sent
// when the runtime asks for a contact share (request_contact=true).
const tgContactButtonText = "Share my number"

// BindTelegramContact records a provider-verified phone for a Telegram
// chat (called by the webhook handler after the self-share check; SPEC-W85).
// Safe on a zero-value Bridge (tests): nil map is initialized lazily.
func (b *Bridge) BindTelegramContact(chatID, phone string) {
	if chatID == "" || phone == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tgBinds == nil {
		b.tgBinds = map[string]tgBind{}
	}
	now := b.now()
	b.sweepTgBindsLocked(now)
	b.tgBinds[chatID] = tgBind{phone: phone, at: now}
	heap.Push(&b.tgBindExp, doneEntry{key: chatID, at: now})
	for len(b.tgBinds) > bridgeTgBindMaxEntries {
		b.evictOldestTgBindLocked()
	}
	b.Log.Info("telegram contact bound", zap.String("chat_id", chatID))
}

// telegramBoundPhone returns the live bound phone for a Telegram chat_id,
// or "" when unbound/expired.
func (b *Bridge) telegramBoundPhone(chatID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweepTgBindsLocked(b.now())
	bind, ok := b.tgBinds[chatID]
	if !ok {
		return ""
	}
	return bind.phone
}

// sweepTgBindsLocked expires bindings past the TTL (oldest-first heap pops;
// stale duplicates dropped without touching the newer entry). Caller holds
// b.mu.
func (b *Bridge) sweepTgBindsLocked(now time.Time) {
	for len(b.tgBindExp) > 0 {
		top := b.tgBindExp[0]
		if now.Sub(top.at) <= bridgeTgBindTTL {
			return
		}
		heap.Pop(&b.tgBindExp)
		if bind, ok := b.tgBinds[top.key]; ok && bind.at.Equal(top.at) {
			delete(b.tgBinds, top.key)
		}
	}
}

// evictOldestTgBindLocked drops the oldest binding (capacity bound).
// Caller holds b.mu.
func (b *Bridge) evictOldestTgBindLocked() {
	for len(b.tgBindExp) > 0 {
		e := heap.Pop(&b.tgBindExp).(doneEntry)
		if bind, ok := b.tgBinds[e.key]; ok && bind.at.Equal(e.at) {
			delete(b.tgBinds, e.key)
			return
		}
	}
}

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
		doneExp:         doneHeap{},
		tgBinds:         map[string]tgBind{},
		now:             time.Now,
		dedupeTTL:       bridgeDedupeTTL,
	}
}

// Direct-base defaults for sidecar-less deployments (SPEC-W45 ORPH O2):
// the compose service names/ports. messaging-gateway runs WITHOUT a daprd
// sidecar in compose, so these are the primary path; the Dapr invoke form
// is used only when a sidecar is actually injected (DAPR_HTTP_PORT/DAPR_HOST
// present).
const (
	DefaultConversationURL = "http://conversation:7007"
	DefaultVoiceURL        = "http://voice:7006"
)

// ResolveBases maps the CONVERSATION_URL / VOICE_RUNTIME_URL overrides onto
// the two base URLs the bridge calls (SPEC-W45 ORPH O2). Precedence:
// explicit override wins; with a Dapr sidecar (sidecar=true) the invoke API
// is used with the REGISTERED app-ids `conversation` / `voice` (see
// docker-compose DAPR_APP_ID — the previous `conversation-service` /
// `voice-agent-runtime` app-ids matched no sidecar and made the bridge a
// permanent 500); without a sidecar the direct-base defaults above.
func ResolveBases(convOverride, voiceOverride string, daprHTTPPort int, sidecar bool) (conv, voice string) {
	dapr := fmt.Sprintf("http://127.0.0.1:%d/v1.0/invoke", daprHTTPPort)
	conv = convOverride
	if conv == "" {
		if sidecar {
			conv = dapr + "/conversation/method"
		} else {
			conv = DefaultConversationURL
		}
	}
	voice = voiceOverride
	if voice == "" {
		if sidecar {
			voice = dapr + "/voice/method"
		} else {
			voice = DefaultVoiceURL
		}
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

	reply, requestContact, err := b.agentReply(ctx, site, convID, msg)
	if err != nil {
		log.Warn("agent reply failed", zap.Error(err))
		return err
	}

	if err := b.recordTurn(ctx, convID, site.TenantID, msg.Channel+":"+msg.MessageID+":reply", "assistant", reply); err != nil {
		log.Warn("record assistant turn failed", zap.Error(err))
		return err
	}

	if err := b.sendReply(ctx, msg, reply, requestContact); err != nil {
		log.Warn("channel reply failed", zap.Error(err))
		return err
	}
	b.markDone(msg) // claim recorded on SUCCESS only — failure releases it
	log.Info("inbound message bridged", zap.String("conversation_id", convID))
	return nil
}

// sweepDoneLocked expires completions past the TTL by popping the heap's
// OLDEST entries until the top is still inside the window (O(log n)
// amortized per expired entry — no per-message O(n) map sweep, PERF-18).
// A heap element whose timestamp no longer matches the map is a stale
// duplicate of a re-completed key; it is dropped without touching the
// newer map entry. Caller holds b.mu.
func (b *Bridge) sweepDoneLocked(now time.Time) {
	for len(b.doneExp) > 0 {
		top := b.doneExp[0]
		if now.Sub(top.at) <= b.dedupeTTL {
			return // heap is ordered: nothing older remains
		}
		heap.Pop(&b.doneExp)
		if at, ok := b.done[top.key]; ok && at.Equal(top.at) {
			delete(b.done, top.key)
		}
	}
}

// evictOldestDoneLocked drops the oldest completion (capacity bound);
// stale heap duplicates are skipped first. Caller holds b.mu.
func (b *Bridge) evictOldestDoneLocked() {
	for len(b.doneExp) > 0 {
		e := heap.Pop(&b.doneExp).(doneEntry)
		if at, ok := b.done[e.key]; ok && at.Equal(e.at) {
			delete(b.done, e.key)
			return
		}
	}
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
	b.sweepDoneLocked(b.now())
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
	now := b.now()
	key := msg.Channel + ":" + msg.MessageID
	b.sweepDoneLocked(now)
	b.done[key] = now
	heap.Push(&b.doneExp, doneEntry{key: key, at: now})
	for len(b.done) > bridgeDedupeMaxEntries {
		b.evictOldestDoneLocked()
	}
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
// step 4 — /voice/chat, NOT the stream). The second return reports the
// runtime's additive request_contact flag (SPEC-W85): true when a telegram
// turn hit a step-up gate and the user should be prompted to share their
// contact.
func (b *Bridge) agentReply(ctx context.Context, site Site, convID string, msg InboundMessage) (string, bool, error) {
	body := map[string]string{
		"site_slug":       site.SiteSlug,
		"message":         msg.Text,
		"conversation_id": convID,
		"channel":         msg.Channel,
	}
	// SPEC-W81 C1: pin the provider-verified WhatsApp sender identity
	// (wa_id — the E.164 From) so the runtime can treat the session as
	// verified (K15(c)). The runtime honors channel_identity ONLY with a
	// valid X-Internal-Token (VOICE_ADMIN_INTERNAL_TOKEN server-side);
	// without the token env the header is omitted and the peer ignores the
	// pin (fail-safe).
	// SPEC-W85: Telegram pins the BOUND phone (recorded by
	// BindTelegramContact from a self-shared contact). An unbound telegram
	// chat pins nothing — the request stays byte-identical to the pre-W85
	// shape, so the W81 telegram byte-compat guarantee holds.
	pin := ""
	if msg.Channel == "whatsapp" {
		pin = msg.From
	} else if msg.Channel == "telegram" {
		pin = b.telegramBoundPhone(msg.From)
	}
	var headers map[string]string
	if pin != "" {
		body["channel_identity"] = pin
		if b.VoiceInternalToken != "" {
			headers = map[string]string{"X-Internal-Token": b.VoiceInternalToken}
		}
	}
	var resp struct {
		Reply          string `json:"reply"`
		RequestContact bool   `json:"request_contact"`
	}
	if err := b.doJSON(ctx, http.MethodPost, b.VoiceURL+"/voice/chat", body, headers, &resp); err != nil {
		return "", false, fmt.Errorf("voice chat: %w", err)
	}
	if resp.Reply == "" {
		return "", false, fmt.Errorf("voice chat: empty reply")
	}
	return resp.Reply, resp.RequestContact, nil
}

// sendReply delivers the agent reply via the same-channel provider
// (SPEC-W6 §A3 step 6). SPEC-W85: on telegram with requestContact=true and
// no live phone binding the reply carries a one-time request_contact
// keyboard so the user can verify their number natively; every other case
// is the pre-W85 plain-text send.
func (b *Bridge) sendReply(ctx context.Context, msg InboundMessage, reply string, requestContact bool) error {
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
		if requestContact && b.telegramBoundPhone(msg.From) == "" {
			_, _, err := b.Telegram.SendContactRequest(ctx, msg.From, reply, tgContactButtonText)
			return err
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
