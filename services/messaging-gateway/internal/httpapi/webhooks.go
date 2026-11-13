// Omnichannel inbound webhooks (SPEC-W6 Part A): Meta WhatsApp Cloud API
// verification + message ingestion and Telegram Bot API updates.
//
// Reliability contract (SPEC-W44 N-05, updated from the W6 "always 200"
// posture): authentication failures answer 403/401; a BRIDGE failure
// answers 500 (fail loud) so Meta/Telegram REDELIVER — the bridge dedupes
// on MessageID so the redelivery skips the already-completed work instead
// of double-replying. Malformed payloads still answer 200 (a poison
// payload must not loop forever). Processing is synchronous but bounded by
// a 25s context. The Telegram route does not exist (404) until
// TELEGRAM_WEBHOOK_SECRET is configured — an unconfigured webhook is never
// an open ingest surface. All secret comparisons are constant-time.
package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/messaging-gateway/internal/channel"
	"github.com/opendesk/messaging-gateway/internal/media"
	"github.com/opendesk/messaging-gateway/internal/phone"
	"github.com/opendesk/messaging-gateway/internal/provider"
	"go.uber.org/zap"
)

// webhookTimeout bounds the synchronous bridge processing per message
// (SPEC-W6 §A1: process synchronously but bounded).
const webhookTimeout = 25 * time.Second

// Bridger is the inbound bridge contract used by the webhook handlers
// (implemented by *channel.Bridge; faked in tests).
type Bridger interface {
	Handle(ctx context.Context, msg channel.InboundMessage, routeID string) error
}

// ---------------------------------------------------------------------------
// WhatsApp (Meta Cloud API)
// ---------------------------------------------------------------------------

// handleWhatsAppVerify implements the Meta webhook verification handshake:
// hub.mode=subscribe + matching hub.verify_token → 200 with the raw
// challenge body; anything else → 403 (SPEC-W6 §A1).
func (s *Server) handleWhatsAppVerify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// Constant-time token compare (SIM-007 posture); an unset token fails
	// closed (403) even against an empty probe token.
	if q.Get("hub.mode") == "subscribe" &&
		s.WhatsAppVerifyToken != "" &&
		subtle.ConstantTimeCompare([]byte(q.Get("hub.verify_token")), []byte(s.WhatsAppVerifyToken)) == 1 {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(q.Get("hub.challenge"))) //nolint:errcheck
		return
	}
	writeError(w, http.StatusForbidden, "webhook verification failed")
}

// waWebhook is the Meta Cloud API webhook payload shape (only the fields
// the bridge and the SPEC-W60 media funnel need).
type waWebhook struct {
	Entry []struct {
		Changes []struct {
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				Messages []waMessage       `json:"messages"`
				Statuses []json.RawMessage `json:"statuses"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// waMessage is one inbound WhatsApp message. Text carries the text body;
// image/document (SPEC-W60 A1) carry the media block Meta fetches by id.
// The caption is PII — it is parsed (so the payload is well-formed) but
// NEVER logged and NEVER sent to OCR (P8).
type waMessage struct {
	From      string `json:"from"` // E.164 without '+'
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"` // unix seconds, string-typed by Meta
	Type      string `json:"type"`
	Text      struct {
		Body string `json:"body"`
	} `json:"text"`
	Image    *waMediaBlock `json:"image"`
	Document *waMediaBlock `json:"document"`
}

// waMediaBlock is the image/document block of a waMessage (snake_case per
// the Meta wire format).
type waMediaBlock struct {
	ID       string `json:"id"`
	MimeType string `json:"mime_type"`
	SHA256   string `json:"sha256"`
	Caption  string `json:"caption"`  // PII — never logged, never forwarded (P8)
	Filename string `json:"filename"` // passed to OCR for extension validation only
}

// verifyWhatsAppSignature enforces the Meta WhatsApp webhook signature
// (SIM-007/SIM-008, fail-closed): X-Hub-Signature-256 must be
// "sha256=" + hex HMAC-SHA256(WHATSAPP_APP_SECRET, raw body). A missing or
// invalid signature — or a missing app-secret configuration — rejects the
// post with 401 and NOTHING is processed. The ONLY bypass is the explicit
// dev opt-in WHATSAPP_MOCK=1 (Server.WhatsAppMock, default false), which
// exists so local/dev journeys can post unsigned payloads.
func (s *Server) verifyWhatsAppSignature(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		s.Log.Warn("whatsapp webhook: unreadable body, dropping", zap.Error(err))
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		return nil, false
	}
	if s.WhatsAppMock {
		// Dev/test opt-in (WHATSAPP_MOCK=1): unsigned payloads accepted.
		return body, true
	}
	if s.WhatsAppAppSecret == "" {
		s.Log.Error("whatsapp webhook: WHATSAPP_APP_SECRET not configured, rejecting (fail-closed)")
		writeError(w, http.StatusUnauthorized, "webhook signature verification not configured")
		return nil, false
	}
	sig := r.Header.Get("X-Hub-Signature-256")
	mac := hmac.New(sha256.New, []byte(s.WhatsAppAppSecret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if sig == "" || !hmac.Equal([]byte(sig), []byte(want)) {
		s.Log.Warn("whatsapp webhook: missing or invalid X-Hub-Signature-256, rejecting")
		writeError(w, http.StatusUnauthorized, "invalid webhook signature")
		return nil, false
	}
	return body, true
}

// handleWhatsAppWebhook ingests inbound WhatsApp messages. Text messages
// are normalized and bridged; image/document messages go through the
// SPEC-W60 media->OCR funnel (behind WHATSAPP_MEDIA_ENABLED); statuses[]
// delivery receipts and every other message type are ignored. Always 200
// (SPEC-W6 §A1) except 401 for a missing/invalid X-Hub-Signature-256
// (authentication failure — Meta does not retry-storm on those) and 500 for
// a transient bridge/fetch/OCR failure (N-05: Meta redelivers).
func (s *Server) handleWhatsAppWebhook(w http.ResponseWriter, r *http.Request) {
	body, ok := s.verifyWhatsAppSignature(w, r)
	if !ok {
		return
	}
	var payload waWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		// Malformed payload: still 200 — Meta retries non-200 forever and a
		// poison payload would loop. Log and drop.
		s.Log.Warn("whatsapp webhook: invalid JSON body, dropping", zap.Error(err))
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		return
	}
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			v := change.Value
			// v.Statuses (delivery receipts) are ignored silently — only
			// messages[] below produce bridge/media work.
			for i := range v.Messages {
				m := &v.Messages[i]
				switch m.Type {
				case "text":
					ts, _ := strconv.ParseInt(m.Timestamp, 10, 64)
					err := s.bridge(r.Context(), channel.InboundMessage{
						Channel:   "whatsapp",
						From:      m.From,
						MessageID: m.ID,
						Text:      m.Text.Body,
						Timestamp: ts,
					}, v.Metadata.PhoneNumberID)
					if err != nil {
						// N-05 fail-loud: 500 → Meta redelivers; the bridge
						// dedupes on MessageID so the redelivery skips the
						// already-completed steps.
						writeError(w, http.StatusInternalServerError, "bridge processing failed")
						return
					}
				case "image", "document":
					// SPEC-W60: media -> OCR funnel. Transient failures
					// answer 500 (N-05 redelivery; the media dedupe claims
					// only on success); deterministic rejects (mime /
					// oversize / checksum / OCR 415) answer 200 (P6).
					if err := s.ingestWhatsAppMedia(r.Context(), v.Metadata.PhoneNumberID, m); err != nil {
						writeError(w, http.StatusInternalServerError, "media processing failed")
						return
					}
				default:
					s.Log.Info("whatsapp webhook: ignoring unsupported message type",
						zap.String("type", m.Type), zap.String("message_id", m.ID))
					continue
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// ---------------------------------------------------------------------------
// Telegram (Bot API)
// ---------------------------------------------------------------------------

// tgUpdate is the Telegram Bot API Update shape (message-only subset).
type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64  `json:"message_id"`
		Date      int64  `json:"date"` // unix seconds
		Text      string `json:"text"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From *struct {
			ID int64 `json:"id"`
		} `json:"from"`
	} `json:"message"`
}

// handleTelegramWebhook ingests Telegram Bot API updates. The route does
// not exist until TELEGRAM_WEBHOOK_SECRET is configured (404 — N-05); with
// it set, X-Telegram-Bot-Api-Secret-Token must match (constant-time, else
// 403). Updates without message.text are ignored. A bridge failure answers
// 500 so Telegram redelivers (bridge dedupes on MessageID).
func (s *Server) handleTelegramWebhook(w http.ResponseWriter, r *http.Request) {
	if s.TelegramWebhookSecret == "" {
		// Unconfigured webhook = no surface (404), never an open ingest.
		http.NotFound(w, r)
		return
	}
	if subtle.ConstantTimeCompare(
		[]byte(r.Header.Get("X-Telegram-Bot-Api-Secret-Token")),
		[]byte(s.TelegramWebhookSecret)) != 1 {
		writeError(w, http.StatusForbidden, "bad webhook secret")
		return
	}
	var upd tgUpdate
	if err := json.NewDecoder(r.Body).Decode(&upd); err != nil {
		s.Log.Warn("telegram webhook: invalid JSON body, dropping", zap.Error(err))
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		return
	}
	if upd.Message == nil || upd.Message.Text == "" {
		// Edits, stickers, join events, …: nothing to bridge.
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		return
	}
	m := upd.Message
	if err := s.bridge(r.Context(), channel.InboundMessage{
		Channel:   "telegram",
		From:      strconv.FormatInt(m.Chat.ID, 10), // chat_id as string
		MessageID: strconv.FormatInt(m.MessageID, 10),
		Text:      m.Text,
		Timestamp: m.Date,
	}, s.TelegramBotUsername); err != nil {
		writeError(w, http.StatusInternalServerError, "bridge processing failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// bridge runs the inbound bridge with the bounded 25s context; a failure is
// returned to the caller (N-05 fail-loud → provider redelivery; the bridge
// dedupes on MessageID).
func (s *Server) bridge(parent context.Context, msg channel.InboundMessage, routeID string) error {
	if s.Bridge == nil {
		s.Log.Warn("inbound bridge not configured, dropping message",
			zap.String("channel", msg.Channel))
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, webhookTimeout)
	defer cancel()
	if err := s.Bridge.Handle(ctx, msg, routeID); err != nil {
		s.Log.Warn("inbound bridge failed",
			zap.String("channel", msg.Channel), zap.Error(err))
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// WhatsApp inbound media -> OCR funnel (SPEC-W60, Option A)
// ---------------------------------------------------------------------------

// mediaDedupeTTL bounds how long a completed media message id is remembered
// (mirrors bridge.go: provider redelivery storms are minutes, not days).
const mediaDedupeTTL = 24 * time.Hour

// mediaDedupeMaxEntries bounds the dedupe map (every cache needs a bound;
// mirrors bridge.go bridgeDedupeMaxEntries).
const mediaDedupeMaxEntries = 100_000

// MediaDeduper dedupes completed media ingestions on the provider message
// id (N-05 idiom, claim-on-success-only: only mark()ed — i.e. fully
// ingested — ids are remembered, so a redelivery after a FAILED attempt
// re-runs the whole fetch->OCR pipeline). In-memory and per-replica, like
// the bridge's dedupe heap.
//
// SPEC-W61: the deduper doubles as the INGEST LEDGER (message_id → anchor
// payload incl. document_id) for the booking-anchor rollout. ledgerMark()
// records the claim BETWEEN OCR success and the anchor POST, so an
// anchor-failure redelivery (500, N-05) finds the ledger hit and retries
// ONLY the anchor POST — no re-fetch, no re-OCR (a re-OCR would double-
// store PII bytes and double-bill ocr_page). The ledger is in-memory and
// per-replica (the accepted W60 posture, ratelimit.go precedent); a
// cross-replica/restart redelivery after an anchor failure re-runs OCR
// once — the same residual window W60 already accepts for post-commit
// transport failures. With WHATSAPP_MEDIA_ANCHOR_ENABLED=false (default)
// the ledger is never written and behavior is byte-identical to W60.
type MediaDeduper struct {
	mu     sync.Mutex
	done   map[string]time.Time
	ledger map[string]anchorEntry // SPEC-W61 ingest ledger (flag-gated)
	now    func() time.Time       // injectable for tests
}

// anchorEntry is one ledger row: the full anchor payload (so the retry
// sends byte-identical data — incl. the VERIFIED sha256/byte count from
// the fetch, not a redelivery's declared values) plus whether the anchor
// POST already succeeded (a post-success redelivery is then a pure no-op,
// exactly the W60 semantics).
type anchorEntry struct {
	payload  AnchorPayload
	at       time.Time
	anchored bool
}

// NewMediaDeduper builds an empty deduper.
func NewMediaDeduper() *MediaDeduper {
	return &MediaDeduper{done: map[string]time.Time{}, ledger: map[string]anchorEntry{}, now: time.Now}
}

// SetClock injects the clock used by the dedupe window (tests).
func (d *MediaDeduper) SetClock(now func() time.Time) { d.now = now }

// seen reports whether the message id was fully ingested (W60 claim) OR
// ledger-claimed (W61: OCR done, anchor pending/anchored) within the
// dedupe window. Empty ids cannot dedupe (processed every time).
func (d *MediaDeduper) seen(id string) bool {
	if id == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepLocked(d.now())
	if _, ok := d.done[id]; ok {
		return true
	}
	_, ok := d.ledger[id]
	return ok
}

// mark records a completed ingestion (call on SUCCESS only — a failure must
// release the claim so the redelivery retries).
func (d *MediaDeduper) mark(id string) {
	if id == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	d.sweepLocked(now)
	d.done[id] = now
	// Capacity bound: at the cap, drop expired entries first, then the
	// oldest survivors (a capacity eviction at 100k ids ≈ a full day of
	// traffic cannot open a realistic replay window).
	if len(d.done) > mediaDedupeMaxEntries {
		var oldest string
		var oldestAt time.Time
		first := true
		for k, at := range d.done {
			if first || at.Before(oldestAt) {
				oldest, oldestAt, first = k, at, false
			}
		}
		if oldest != "" {
			delete(d.done, oldest)
		}
	}
}

// ledgerMark records the W61 ingest-ledger claim BETWEEN OCR success and
// the anchor POST (SPEC-W61 §Gateway sequencing). The payload is the exact
// anchor body (tenant/message/document ids, digits-normalized sender,
// VERIFIED mime/bytes/sha256-prefix) so an anchor-failure redelivery
// retries byte-identical data. Capacity/TTL bounds mirror mark().
func (d *MediaDeduper) ledgerMark(id string, payload AnchorPayload) {
	if id == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	d.sweepLocked(now)
	d.ledger[id] = anchorEntry{payload: payload, at: now}
	if len(d.ledger) > mediaDedupeMaxEntries {
		var oldest string
		var oldestAt time.Time
		first := true
		for k, e := range d.ledger {
			if first || e.at.Before(oldestAt) {
				oldest, oldestAt, first = k, e.at, false
			}
		}
		if oldest != "" {
			delete(d.ledger, oldest)
		}
	}
}

// anchorPending reports the stored anchor payload for a ledger-claimed
// message whose anchor POST has NOT yet succeeded — the redelivery retries
// ONLY that POST (no re-fetch, no re-OCR).
func (d *MediaDeduper) anchorPending(id string) (AnchorPayload, bool) {
	if id == "" {
		return AnchorPayload{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepLocked(d.now())
	e, ok := d.ledger[id]
	if !ok || e.anchored {
		return AnchorPayload{}, false
	}
	return e.payload, true
}

// markAnchored flips a ledger entry to anchored after the anchor POST
// succeeded; a further redelivery is then the W60 pure no-op.
func (d *MediaDeduper) markAnchored(id string) {
	if id == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if e, ok := d.ledger[id]; ok {
		e.anchored = true
		d.ledger[id] = e
	}
}

// sweepLocked expires completions past the TTL. Caller holds d.mu.
func (d *MediaDeduper) sweepLocked(now time.Time) {
	if len(d.done)+len(d.ledger) < 1024 {
		return // tiny maps: TTL is enforced on read anyway via seen(); sweep lazily
	}
	for k, at := range d.done {
		if now.Sub(at) > mediaDedupeTTL {
			delete(d.done, k)
		}
	}
	for k, e := range d.ledger {
		if now.Sub(e.at) > mediaDedupeTTL {
			delete(d.ledger, k)
		}
	}
}

// sha256Prefix is the P8 log form of a hash: prefix only, never the value
// (and never the bytes/caption it summarizes).
func sha256Prefix(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

// sha256Prefix16 is the W61 anchor-payload form: the FIRST 16 hex chars of
// the content sha256 (PII-light — enough to spot duplicate bytes at review
// time, never the full value).
func sha256Prefix16(sum string) string {
	if len(sum) > 16 {
		return sum[:16]
	}
	return sum
}

// ingestWhatsAppMedia runs the SPEC-W60 media funnel for one image/document
// message. P3 cost-gate order: site-map resolve FIRST (unmapped
// phone_number_id drops BEFORE any fetch) → dedupe BEFORE fetch
// (claim-on-success) → per-sender throttle (P4) → Meta 2-GET fetch (P5) →
// OCR POST. A returned error answers 500 (N-05 redelivery); deterministic
// rejects (P6: mime / oversize / checksum / OCR 415) return nil after a
// drop-log so Meta never redelivers them. P8: logs carry ids, byte counts
// and sha256 prefixes ONLY — never the caption, never the bytes.
func (s *Server) ingestWhatsAppMedia(ctx context.Context, phoneNumberID string, m *waMessage) error {
	block := m.Image
	if block == nil {
		block = m.Document
	}
	// P7 rollout flag: off => drop + log exactly like the pre-W60 posture
	// (zero behavior change on upgrade).
	if !s.WhatsAppMediaEnabled {
		s.Log.Info("whatsapp webhook: ignoring non-text message",
			zap.String("type", m.Type), zap.String("message_id", m.ID))
		return nil
	}
	if block == nil || block.ID == "" {
		s.Log.Info("whatsapp media: empty media block, dropping",
			zap.String("type", m.Type), zap.String("message_id", m.ID))
		return nil
	}
	// P6/P5 mime allow-list on the DECLARED mime BEFORE any fetch: audio /
	// video / stickers / gif are structurally excluded (and cost nothing).
	if !provider.AllowedWhatsAppMime(block.MimeType) {
		s.Log.Info("whatsapp media: mime not allowed, dropping (no fetch)",
			zap.String("mime_type", block.MimeType), zap.String("message_id", m.ID))
		return nil
	}
	// P3 cost gate 1: tenant resolve via the EXISTING CHANNEL_SITE_MAP
	// before any byte is fetched (unmapped route = unroutable, drop).
	site, ok := s.Sites["whatsapp:"+phoneNumberID]
	if !ok {
		s.Log.Info("whatsapp media dropped: no CHANNEL_SITE_MAP entry",
			zap.String("route_id", phoneNumberID), zap.String("message_id", m.ID))
		return nil
	}
	tenantID, err := uuid.Parse(site.TenantID)
	if err != nil {
		// Misconfigured site map (R9): deterministic, redelivery cannot fix.
		s.Log.Error("whatsapp media dropped: CHANNEL_SITE_MAP tenant_id is not a uuid",
			zap.String("route_id", phoneNumberID), zap.String("message_id", m.ID))
		return nil
	}
	// P3 cost gate 2: dedupe BEFORE fetch (redelivery after success = no-op;
	// claim recorded on success only, bridge.go:200-204 idiom).
	deduper := s.MediaDedupe
	if deduper == nil {
		deduper = NewMediaDeduper() // defensive; Router() always initializes
	}
	if deduper.seen(m.ID) {
		// SPEC-W61: an anchor-failure redelivery finds the ingest-ledger hit
		// and retries ONLY the anchor POST — no re-fetch, no re-OCR (R4).
		if s.WhatsAppMediaAnchorEnabled {
			if payload, pending := deduper.anchorPending(m.ID); pending {
				return s.retryMediaAnchor(ctx, deduper, tenantID, payload)
			}
		}
		s.Log.Info("whatsapp media redelivery skipped (already ingested)",
			zap.String("message_id", m.ID))
		return nil
	}
	// P4: per-sender sliding-window throttle (R3 — APISIX limit-count keys
	// Meta's egress IPs, NOT senders, so this service-side per-phone limit
	// is REQUIRED). A tripped throttle is a 200 drop: redelivery cannot
	// help and must not storm.
	if s.MediaRate != nil && !s.MediaRate.Allow(m.From) {
		s.Log.Warn("whatsapp media dropped: per-sender throttle tripped",
			zap.String("message_id", m.ID),
			zap.String("tenant_id", tenantID.String()))
		return nil
	}
	// P7 fail-closed: flag on but OCR unconfigured => per-message drop
	// (fail-visible error log; boot already logged CRITICAL).
	if s.Media == nil || !s.Media.Configured() {
		s.Log.Error("whatsapp media dropped: OCR not configured (OCR_SERVICE_URL / OCR_INTERNAL_TOKEN unset) — fail-closed",
			zap.String("message_id", m.ID), zap.String("tenant_id", tenantID.String()))
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()
	mediaBlob, err := s.WhatsApp.FetchMedia(ctx, block.ID)
	if err != nil {
		if provider.DeterministicMediaError(err) {
			// P6: mime/oversize/checksum rejects never redelivery-storm.
			s.Log.Warn("whatsapp media dropped: deterministic fetch reject",
				zap.String("message_id", m.ID), zap.String("tenant_id", tenantID.String()), zap.Error(err))
			return nil
		}
		s.Log.Warn("whatsapp media fetch failed (redelivery will retry)",
			zap.String("message_id", m.ID), zap.String("tenant_id", tenantID.String()), zap.Error(err))
		return err
	}
	docID, err := s.Media.Extract(ctx, tenantID, mediaBlob.MimeType, block.Filename, mediaBlob.Data)
	if err != nil {
		if errors.Is(err, media.ErrRejectedMedia) {
			// P6: OCR 415 (AV-infected / failed validation) => 200 + log.
			s.Log.Warn("whatsapp media dropped: ocr rejected (415)",
				zap.String("message_id", m.ID), zap.String("tenant_id", tenantID.String()))
			return nil
		}
		s.Log.Warn("whatsapp media ocr failed (redelivery will retry)",
			zap.String("message_id", m.ID), zap.String("tenant_id", tenantID.String()), zap.Error(err))
		return err
	}
	// P9 audit record (structured log; the ocr_documents row is the durable
	// record). P8: ids/byte-counts/sha256-prefix only.
	s.Log.Info("whatsapp.media.ingested",
		zap.String("tenant_id", tenantID.String()),
		zap.String("message_id", m.ID),
		zap.String("document_id", docID),
		zap.Int("bytes", len(mediaBlob.Data)),
		zap.String("sha256_prefix", sha256Prefix(mediaBlob.SHA256)))
	if !s.WhatsAppMediaAnchorEnabled {
		deduper.mark(m.ID) // claim recorded on SUCCESS only (byte-identical W60 path)
		return nil
	}
	// SPEC-W61: ledger claim BEFORE the anchor POST so an anchor failure
	// (500, N-05) redelivers into the anchorPending branch above and retries
	// ONLY the anchor. The payload carries the VERIFIED fetch facts
	// (mime/bytes/sha256) and the pinned digits-normalized sender (G3) —
	// never the caption (P8).
	payload := AnchorPayload{
		TenantID:          tenantID.String(),
		Channel:           "whatsapp",
		MessageID:         m.ID,
		DocumentID:        docID,
		SenderPhone:       m.From,
		SenderPhoneDigits: phone.Digits(m.From),
		Mime:              mediaBlob.MimeType,
		Bytes:             len(mediaBlob.Data),
		SHA256Prefix:      sha256Prefix16(mediaBlob.SHA256),
	}
	deduper.ledgerMark(m.ID, payload)
	return s.retryMediaAnchor(ctx, deduper, tenantID, payload)
}

// retryMediaAnchor places the single-attempt anchor POST for a
// ledger-claimed message (fresh path after OCR, or an anchor-failure
// redelivery) and flips the ledger entry on success. A failed POST returns
// the error => webhook 500 (N-05); a fail-closed client (token unset —
// deterministic misconfiguration, P6 philosophy) drops 200 and completes
// the ledger so Meta never redelivery-storms it.
func (s *Server) retryMediaAnchor(ctx context.Context, deduper *MediaDeduper, tenantID uuid.UUID, payload AnchorPayload) error {
	if s.MediaAnchor == nil || !s.MediaAnchor.Configured() {
		s.Log.Error("whatsapp media anchor dropped: MESSAGING_BOOKING_INTERNAL_TOKEN unset — fail-closed",
			zap.String("message_id", payload.MessageID),
			zap.String("tenant_id", tenantID.String()))
		deduper.markAnchored(payload.MessageID) // deterministic: no storm, no re-OCR
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()
	if err := s.MediaAnchor.Anchor(ctx, payload); err != nil {
		s.Log.Warn("whatsapp media anchor failed (redelivery retries anchor only)",
			zap.String("message_id", payload.MessageID),
			zap.String("tenant_id", tenantID.String()),
			zap.String("document_id", payload.DocumentID),
			zap.Error(err))
		return err
	}
	deduper.markAnchored(payload.MessageID)
	s.Log.Info("whatsapp.media.anchored",
		zap.String("tenant_id", tenantID.String()),
		zap.String("message_id", payload.MessageID),
		zap.String("document_id", payload.DocumentID))
	return nil
}

// ---------------------------------------------------------------------------
// IoT incident ingest (SPEC-W11 Part B §6)
// ---------------------------------------------------------------------------
//
// POST /webhooks/incidents is the public IoT trigger: an alarm panel /
// sensor gateway posts a PARTIAL Incident Data Packet; the gateway
// authenticates the tenant via the per-tenant shared secret
// (INCIDENT_WEBHOOK_SECRETS env JSON) and forwards to booking-service's
// internal POST /v1/incidents/ingest via Dapr service invocation, which
// completes the IDP (channel=webhook), persists it and triggers
// auto-dispatch + critical/high outreach.
//
// Response contract mirrors the provider webhooks: 400 only for garbage
// JSON, 403 for an unknown tenant / bad secret (authentication failure),
// 200 otherwise — internal forward failures are logged, not propagated.

// incidentWebhookBody is the accepted payload shape. Exactly one of
// tenant_slug / tenant_id addresses the tenant.
type incidentWebhookBody struct {
	TenantSlug string          `json:"tenant_slug,omitempty"`
	TenantID   string          `json:"tenant_id,omitempty"`
	Secret     string          `json:"secret,omitempty"`
	Incident   json.RawMessage `json:"incident"`
}

// ParseIncidentSecrets decodes the INCIDENT_WEBHOOK_SECRETS env JSON:
//
//	{"acme-ng": "s3cret", "9f1c…-uuid": "other-secret"}
//
// Keys may be tenant slugs or tenant ids (either addresses the tenant in the
// webhook body). An empty string yields an empty map (ingest disabled —
// every post is 403).
func ParseIncidentSecrets(raw string) (map[string]string, error) {
	m := map[string]string{}
	if raw == "" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("parse INCIDENT_WEBHOOK_SECRETS: %w", err)
	}
	return m, nil
}

// IncidentIngester forwards a validated incident to booking-service
// (httpIncidentIngester in prod; faked in tests).
type IncidentIngester interface {
	Ingest(ctx context.Context, body []byte) error
}

// httpIncidentIngester posts the ingest envelope to booking-service:
// {base}/v1/incidents/ingest, where base is the BOOKING_URL override or the
// Dapr sidecar invoke base (…/v1.0/invoke/booking/method).
type httpIncidentIngester struct {
	base string
	hc   *http.Client
}

// NewIncidentIngester builds the ingester; base must already be resolved
// (direct base or Dapr invoke base) — see ResolveIncidentBase.
func NewIncidentIngester(base string) IncidentIngester {
	return &httpIncidentIngester{base: base, hc: &http.Client{Timeout: 10 * time.Second}}
}

// DefaultBookingURL is the direct booking-service base for sidecar-less
// deployments (SPEC-W45 ORPH O2, compose service name/port).
const DefaultBookingURL = "http://booking:7002"

// ResolveIncidentBase maps the BOOKING_URL override onto the
// booking-service base URL (same convention as channel.ResolveBases):
// override wins; with a Dapr sidecar the invoke API (registered app-id
// `booking`); without a sidecar the direct-base default.
func ResolveIncidentBase(bookingOverride string, daprHTTPPort int, sidecar bool) string {
	if bookingOverride != "" {
		return bookingOverride
	}
	if sidecar {
		return fmt.Sprintf("http://127.0.0.1:%d/v1.0/invoke/booking/method", daprHTTPPort)
	}
	return DefaultBookingURL
}

func (c *httpIncidentIngester) Ingest(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/incidents/ingest", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("invoke booking ingest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("booking ingest: status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// incidentSignal is the minimal validation subset of the partial IDP: at
// least one signal field must be present, otherwise the post is junk
// telemetry and ignored (200, not forwarded).
type incidentSignal struct {
	IncidentType     string          `json:"incident_type"`
	NarrativeSummary string          `json:"narrative_summary"`
	CallbackNumber   *string         `json:"callback_number"`
	Location         json.RawMessage `json:"location"`
}

func (s *Server) handleIncidentWebhook(w http.ResponseWriter, r *http.Request) {
	var body incidentWebhookBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// Garbage JSON is the ONE client error we report (400): unlike the
		// provider webhooks there is no retry-storm risk from IoT gateways.
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	tenantKey := body.TenantSlug
	if tenantKey == "" {
		tenantKey = body.TenantID
	}
	if tenantKey == "" {
		writeError(w, http.StatusForbidden, "tenant_slug or tenant_id is required")
		return
	}
	want, known := s.IncidentSecrets[tenantKey]
	if !known || want == "" ||
		subtle.ConstantTimeCompare([]byte(body.Secret), []byte(want)) != 1 {
		writeError(w, http.StatusForbidden, "bad incident webhook secret")
		return
	}
	if len(body.Incident) == 0 || string(body.Incident) == "null" {
		// Valid JSON but nothing to ingest.
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		return
	}
	var sig incidentSignal
	if err := json.Unmarshal(body.Incident, &sig); err != nil {
		writeError(w, http.StatusBadRequest, "incident must be a JSON object")
		return
	}
	if sig.IncidentType == "" && sig.NarrativeSummary == "" &&
		(sig.CallbackNumber == nil || *sig.CallbackNumber == "") && len(sig.Location) == 0 {
		s.Log.Info("incident webhook: no signal fields, ignoring",
			zap.String("tenant", tenantKey))
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		return
	}
	if s.IncidentIngest == nil {
		s.Log.Warn("incident ingest not configured, dropping", zap.String("tenant", tenantKey))
		writeJSON(w, http.StatusOK, map[string]any{"status": "dropped"})
		return
	}
	forward, err := json.Marshal(map[string]any{
		"tenant_slug": body.TenantSlug,
		"tenant_id":   body.TenantID,
		"incident":    body.Incident,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid incident payload")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), webhookTimeout)
	defer cancel()
	if err := s.IncidentIngest.Ingest(ctx, forward); err != nil {
		// Internal failure: logged, still 200 (the caller must not retry-storm;
		// booking-side ingest is idempotent on incident_id for safe manual replays).
		s.Log.Warn("incident ingest forward failed", zap.String("tenant", tenantKey), zap.Error(err))
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// ---------------------------------------------------------------------------
// Telegram send endpoint (outbound parity with the other providers, §A4)
// ---------------------------------------------------------------------------

type telegramRequest struct {
	To      string `json:"to"` // chat_id as string
	Message string `json:"message"`
}

func (s *Server) handleTelegramSend(w http.ResponseWriter, r *http.Request) {
	var req telegramRequest
	if !decodeJSON(w, r, &req) || !requireToMessage(w, req.To, req.Message) {
		return
	}
	if !s.Telegram.Configured() {
		writeError(w, http.StatusServiceUnavailable, "telegram provider not configured (TELEGRAM_BOT_TOKEN)")
		return
	}
	status, body, err := s.Telegram.SendMessage(r.Context(), req.To, req.Message)
	s.respond(w, r, status, body, err)
}
