// SPEC-W61 (Scope A): the WhatsApp media -> booking ANCHOR client. After a
// successful OCR extract the gateway posts one idempotent internauth call
// to booking-service's /v1/media/inbox/ingest so the media row can be
// resolved-or-parked against the sender's phone (binding ruling: unknown
// senders are PARKED; the gateway never creates contacts or leads — that
// decision lives entirely booking-side).
//
// Contract (mirrors the httpIncidentIngester / USSD action-dispatcher
// idiom):
//
//	POST {bookingBase}/v1/media/inbox/ingest
//	X-Internal-Token: <MESSAGING_BOOKING_INTERNAL_TOKEN>  (K2 machine auth;
//	                                booking fail-closes 503 when unset,
//	                                401 constant-time on mismatch)
//	{"tenant_id","channel","message_id","document_id","sender_phone",
//	 "sender_phone_digits","mime","bytes","sha256_prefix"}
//
//	200 {"id","state",...} — parked|anchored; idempotent on
//	(tenant_id, channel, message_id) so an N-05 redelivery is safe.
//
// Posture: ONE attempt, NO retries (the webhook redelivery IS the retry —
// ocr.go's double-store/double-billing argument), a 30s bound, and
// FAIL-CLOSED when MESSAGING_BOOKING_INTERNAL_TOKEN is unset (never an
// unauthenticated machine call — the kyc/ocr internauth mirror). P8: the
// payload carries ids, byte counts and a 16-hex-char sha256 PREFIX only —
// never the caption, never the bytes. tenant_id is sent EXPLICITLY (the
// gateway knows it from the CHANNEL_SITE_MAP resolution) so booking-side
// resolves the tenant from the body, the ingestIncident idiom.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AnchorTimeout bounds the single anchor attempt. It is far shorter than
// the OCR extract bound (OCRTimeout, 150s): the ingest is a handful of
// indexed SQL statements, and the whole webhook is bounded by
// webhookTimeout (25s) anyway — 30s only guards a direct (non-webhook)
// caller from hanging.
const AnchorTimeout = 30 * time.Second

// ErrAnchorNotConfigured marks the fail-closed posture: the anchor client
// has no base URL or no MESSAGING_BOOKING_INTERNAL_TOKEN and must never
// place an unauthenticated call.
var ErrAnchorNotConfigured = errors.New("media anchor client not configured (booking base / MESSAGING_BOOKING_INTERNAL_TOKEN unset)")

// AnchorPayload is the body of POST /v1/media/inbox/ingest. TenantID is
// explicit (site-map resolution, ingestIncident idiom); SenderPhone is the
// raw Meta `from` (digits, no '+') and SenderPhoneDigits its pinned
// phone.Digits canonical form (G3) — booking re-derives the digits SQL-side
// as trust-but-verify. SHA256Prefix is the FIRST 16 hex chars of the
// content sha256 (PII-light; the W60 log form uses 12 — the anchor row is a
// durable record so it pins 16). The caption NEVER rides this payload.
type AnchorPayload struct {
	TenantID          string `json:"tenant_id"`
	Channel           string `json:"channel"` // "whatsapp"
	MessageID         string `json:"message_id"`
	DocumentID        string `json:"document_id"`
	SenderPhone       string `json:"sender_phone"`
	SenderPhoneDigits string `json:"sender_phone_digits"`
	Mime              string `json:"mime"`
	Bytes             int    `json:"bytes"`
	SHA256Prefix      string `json:"sha256_prefix"`
}

// AnchorClient is the bounded, single-attempt internauth client for the
// booking-service media inbox ingest endpoint. Construct with
// NewAnchorClient; base resolution follows the ResolveIncidentBase idiom
// (BOOKING_URL override, else Dapr invoke, else the direct compose base) —
// the caller passes the already-resolved base.
type AnchorClient struct {
	base  string
	token string // MESSAGING_BOOKING_INTERNAL_TOKEN
	hc    *http.Client
}

// NewAnchorClient builds the client for a resolved booking-service base
// (see ResolveIncidentBase) and the K2 machine token. Empty base or token
// yields a client whose Configured() is false — every Anchor call then
// fails closed with ErrAnchorNotConfigured.
func NewAnchorClient(base, token string) *AnchorClient {
	return &AnchorClient{
		base:  strings.TrimRight(strings.TrimSpace(base), "/"),
		token: token,
		hc:    &http.Client{Timeout: AnchorTimeout},
	}
}

// Configured reports whether the client may place calls: base URL AND
// internal token both set (fail-closed mirror of the OCR client posture).
func (c *AnchorClient) Configured() bool {
	return c != nil && c.base != "" && c.token != ""
}

// Anchor posts ONE ingest call. Exactly one attempt — no retries (N-05: a
// failure answers 500 at the webhook and Meta's redelivery is the retry,
// converging through the ingest ledger without re-fetch/re-OCR). Any
// transport error or non-2xx status is returned as an error; booking-side
// idempotency on (tenant_id, channel, message_id) makes replays safe.
func (c *AnchorClient) Anchor(ctx context.Context, p AnchorPayload) error {
	if !c.Configured() {
		return ErrAnchorNotConfigured
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("build anchor request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/media/inbox/ingest", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build anchor request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Internal-Token", c.token)
	resp, err := c.hc.Do(req) // single attempt — NO retry loop by contract
	if err != nil {
		return fmt.Errorf("anchor ingest call: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("anchor ingest: status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// String describes the client for boot logs (never logs the token).
func (c *AnchorClient) String() string {
	if c == nil || c.base == "" {
		return "media-anchor disabled (booking base unresolved)"
	}
	if c.token == "" {
		return "media-anchor fail-closed (MESSAGING_BOOKING_INTERNAL_TOKEN unset)"
	}
	return fmt.Sprintf("media-anchor via %s", c.base)
}
