package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// WhatsApp sends messages via the WhatsApp Cloud API
// (https://developers.facebook.com/docs/whatsapp/cloud-api —
// POST {base}/{phoneNumberID}/messages, bearer token).
type WhatsApp struct {
	Client        *Client
	BaseURL       string // https://graph.facebook.com/v21.0
	Token         string
	PhoneNumberID string

	// MediaHC fetches inbound media BYTES (SPEC-W60 P5): a separate 30s
	// client — the 10s send client is too tight for up-to-25MB downloads.
	// Nil falls back to a default 30s client. The metadata GET stays on the
	// shared Client machinery (idempotent, retriable); the byte GET is a
	// single attempt (N-05: a failure answers 500 so Meta redelivers with a
	// freshly minted media URL).
	MediaHC *http.Client
}

// Configured reports whether the provider has the credentials it needs.
func (w *WhatsApp) Configured() bool { return w.Token != "" && w.PhoneNumberID != "" }

// SendMessage delivers a free-form text message, or a template message when
// template is non-empty (required outside the 24h customer service window).
func (w *WhatsApp) SendMessage(ctx context.Context, to, message, template string) (int, []byte, error) {
	var payload map[string]any
	if template != "" {
		payload = map[string]any{
			"messaging_product": "whatsapp",
			"to":                to,
			"type":              "template",
			"template": map[string]any{
				"name":     template,
				"language": map[string]string{"code": "en_US"},
			},
		}
	} else {
		payload = map[string]any{
			"messaging_product": "whatsapp",
			"to":                to,
			"type":              "text",
			"text":              map[string]string{"body": message},
		}
	}
	return w.Client.send(ctx, func(ctx context.Context) (*http.Request, error) {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal whatsapp payload: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			w.BaseURL+"/"+w.PhoneNumberID+"/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+w.Token)
		return req, nil
	})
}

// ---------------------------------------------------------------------------
// Inbound media fetch (SPEC-W60 P5) — additive; the send path above is
// untouched.
// ---------------------------------------------------------------------------

// MediaFetchTimeout bounds ONE inbound-media byte GET (30s — media runs up
// to 25MB; the 10s send client is too tight). The metadata GET uses the
// shared 10s Client machinery instead (small JSON, idempotent, retriable).
const MediaFetchTimeout = 30 * time.Second

// MaxMediaBytes is the hard byte cap for one inbound media download
// (SPEC-W60 P5: 25MB, the ocr-service sacred cap). FetchMedia streams with
// io.LimitReader(MaxMediaBytes+1) and rejects past the cap.
const MaxMediaBytes = 25 << 20

// Deterministic fetch failures (SPEC-W60 P6): these can NEVER succeed on
// redelivery (the bytes/mime are fixed for the media id), so the webhook
// answers 200 + drop-log instead of 500 (no redelivery storm).
var (
	// ErrMediaType — mime outside the allow-list {jpeg,png,webp,pdf}.
	ErrMediaType = errors.New("whatsapp media type not allowed")
	// ErrMediaTooLarge — byte stream exceeds MaxMediaBytes.
	ErrMediaTooLarge = errors.New("whatsapp media exceeds the 25MB cap")
	// ErrMediaChecksum — bytes do not match Meta's declared sha256.
	ErrMediaChecksum = errors.New("whatsapp media sha256 mismatch")
)

// DeterministicMediaError reports whether a FetchMedia failure is a P6
// deterministic reject (200 + drop) rather than a transient failure (500 +
// Meta redelivery).
func DeterministicMediaError(err error) bool {
	return errors.Is(err, ErrMediaType) || errors.Is(err, ErrMediaTooLarge) ||
		errors.Is(err, ErrMediaChecksum)
}

// allowedMediaMimes is the SPEC-W60 P5 allow-list — exactly the ocr-service
// ALLOWED_MIMES; audio/video/stickers/gif are structurally excluded. The
// value is the fallback file extension for the OCR upload filename.
var allowedMediaMimes = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"application/pdf": ".pdf",
}

// AllowedWhatsAppMime reports whether a (declared or metadata) mime is on
// the P5 allow-list.
func AllowedWhatsAppMime(mime string) bool {
	mime = strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
	_, ok := allowedMediaMimes[mime]
	return ok
}

// MediaExtension is the fallback extension for an allowed mime (used when
// the webhook block carries no filename).
func MediaExtension(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
	return allowedMediaMimes[mime]
}

// Media is one fetched inbound media blob. Data is PII: never log it (the
// package header idiom) — ids, byte counts and sha256 prefixes only.
type Media struct {
	MimeType string // metadata mime (allow-listed)
	SHA256   string // Meta's declared sha256 ("" when absent)
	Data     []byte
}

// mediaMetadata mirrors the Meta Cloud API media metadata response:
// GET {BaseURL}/{media-id} -> {url, mime_type, sha256, file_size}. The url
// expires in ~5 minutes; the media id does not (a webhook redelivery
// re-mints a fresh url — the N-05 500 contract stays safe).
type mediaMetadata struct {
	URL      string `json:"url"`
	MimeType string `json:"mime_type"`
	SHA256   string `json:"sha256"`
	FileSize int64  `json:"file_size"`
}

func (w *WhatsApp) mediaHTTP() *http.Client {
	if w.MediaHC != nil {
		return w.MediaHC
	}
	return &http.Client{Timeout: MediaFetchTimeout}
}

// FetchMedia downloads one inbound media blob by Meta media id (P5 2-GET
// flow): (1) GET {BaseURL}/{mediaID} via the shared Client machinery
// (retriable — the GET is idempotent); (2) GET the returned url ONCE on the
// 30s media client, stream-capped at MaxMediaBytes+1, verifying Meta's
// declared sha256 when present. Deterministic rejects wrap
// ErrMediaType/ErrMediaTooLarge/ErrMediaChecksum; everything else is
// transient (the caller answers 500 so Meta redelivers).
func (w *WhatsApp) FetchMedia(ctx context.Context, mediaID string) (*Media, error) {
	if mediaID == "" {
		return nil, fmt.Errorf("whatsapp media id is required")
	}
	if w.Client == nil {
		return nil, fmt.Errorf("whatsapp client not configured")
	}
	var meta mediaMetadata
	_, body, err := w.Client.send(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			w.BaseURL+"/"+mediaID, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+w.Token)
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("whatsapp media metadata: %w", err)
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, fmt.Errorf("whatsapp media metadata: undecodable body: %w", err)
	}
	if meta.URL == "" {
		return nil, fmt.Errorf("whatsapp media metadata: empty download url")
	}
	if !AllowedWhatsAppMime(meta.MimeType) {
		return nil, fmt.Errorf("%w: %q", ErrMediaType, meta.MimeType)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, meta.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("whatsapp media download: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+w.Token)
	resp, err := w.mediaHTTP().Do(req) // single attempt — N-05 redelivery instead of mid-stream retries
	if err != nil {
		return nil, fmt.Errorf("whatsapp media download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("whatsapp media download: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxMediaBytes+1))
	if err != nil {
		return nil, fmt.Errorf("whatsapp media download: %w", err)
	}
	if len(data) > MaxMediaBytes {
		return nil, fmt.Errorf("%w: stream exceeds %d bytes", ErrMediaTooLarge, MaxMediaBytes)
	}
	if meta.SHA256 != "" && !mediaSHA256Matches(meta.SHA256, data) {
		return nil, ErrMediaChecksum
	}
	return &Media{MimeType: strings.ToLower(strings.TrimSpace(meta.MimeType)), SHA256: meta.SHA256, Data: data}, nil
}

// mediaSHA256Matches verifies Meta's declared sha256 against the downloaded
// bytes. Meta ships it hex-encoded (webhook + metadata); base64 is accepted
// too so an encoding flip never false-rejects.
func mediaSHA256Matches(declared string, data []byte) bool {
	sum := sha256.Sum256(data)
	if raw, err := hex.DecodeString(strings.TrimSpace(declared)); err == nil {
		return bytes.Equal(raw, sum[:])
	}
	if raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(declared)); err == nil {
		return bytes.Equal(raw, sum[:])
	}
	return false
}
