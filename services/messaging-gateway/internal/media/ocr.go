// Package media implements the SPEC-W60 WhatsApp inbound media -> OCR
// funnel client: bytes fetched from Meta are POSTed to the ocr-service
// internal extract endpoint, inheriting its magic-byte validation, W59
// ClamAV scan, MinIO PII bucket and tenant-delete erasure.
//
// Contract (MUST stay in exact sync with services/ocr-service
// POST /v1/ocr/extract — mirrors the booking fieldcapture OCR client):
//
//	POST {OCR_SERVICE_URL}/v1/ocr/extract
//	X-Internal-Token: <OCR_INTERNAL_TOKEN>   (service-to-service auth; A
//	                                constant-time-compares it and fails
//	                                closed when unset)
//	X-Tenant-Slug: <tenant uuid>             (also the tenant_id form field)
//	multipart form: kind=receipt|document, tenant_id=<uuid>, file=<bytes>,
//	expires_in_hours=<n>                     (OPTIONAL, SPEC-W61 — see
//	                                SetExpiresInHours; absent = byte-identical
//	                                pre-W61 behavior)
//
//	200 {"document_id": uuid, "status": ..., "extracted_fields": ..., "page_count": int}
//
// Pinned decisions honored here:
//
//	P2 — kind mapping is HARD-CODED image/*=>receipt, application/pdf=>
//	     document; kyc_* kinds are structurally unemittable from this
//	     package (the W58 consent gate cannot be bootstrapped by a
//	     WhatsApp message).
//	P6 — deterministic rejects (415 AV-infected / validation) are surfaced
//	     as ErrRejectedMedia so the webhook answers 200 (no redelivery
//	     storm); every other failure is ErrUnavailable => 500 (N-05).
//	P8 — bytes and captions are PII: never logged (ids/byte-counts only).
//	     The caption is NOT sent to OCR (no field for it); the filename is
//	     passed through for extension validation.
//
// Posture: BOUNDED (OCRTimeout), NO RETRIES (a retry would double-store PII
// bytes and double-bill ocr_page usage — a webhook redelivery is the retry),
// fail-closed when OCR_SERVICE_URL / OCR_INTERNAL_TOKEN are unset.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// OCRTimeout bounds the whole extract call (SPEC-W53: 150s — CPU OCR is
// multi-second per page and a multi-page PDF rasterizes into several).
const OCRTimeout = 150 * time.Second

// MaxMediaBytes is the hard byte cap for one upload (SPEC-W53/W60 sacred
// constraint: 25 MB; the fetch side already stream-caps at 25MB+1).
const MaxMediaBytes = 25 << 20

// ErrUnavailable marks any transient ocr-service failure (transport,
// timeout, non-2xx other than 415, contract-violating 2xx body). The
// webhook maps it to 500 so Meta redelivers (N-05).
var ErrUnavailable = errors.New("ocr service unavailable")

// ErrRejectedMedia marks DETERMINISTIC rejection: ocr answered 415
// (AV-infected or failed magic-byte/extension validation — the W59 scan
// contract). Redelivery can never succeed on the same bytes; the webhook
// maps it to 200 + drop-log (P6).
var ErrRejectedMedia = errors.New("ocr rejected the media (415)")

// errNotConfigured is the fail-closed posture when OCR_SERVICE_URL or
// OCR_INTERNAL_TOKEN is unset (kyc-service internauth mirror: unset token →
// never a silent open call). The webhook checks Configured() up front; this
// guards direct client use.
var errNotConfigured = errors.New("ocr client not configured (OCR_SERVICE_URL / OCR_INTERNAL_TOKEN unset)")

// allowedMimes is the ocr-service allow-list (extension + magic-byte
// validated server-side; mirrored client-side so a bad blob is rejected
// before spending the round-trip). It is EXACTLY the WhatsApp fetch
// allow-list (provider.allowedMediaMimes): jpeg/png/webp/pdf.
var allowedMimes = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"application/pdf": ".pdf",
}

// KindForOCR is the SPEC-W60 P2 HARD-CODED kind mapping: images become
// kind=receipt, PDFs kind=document, NOTHING else — kyc_* kinds are
// structurally unemittable from the gateway. The bool is false for any mime
// outside the allow-list.
func KindForOCR(mime string) (string, bool) {
	mime = strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
	switch mime {
	case "image/jpeg", "image/png", "image/webp":
		return "receipt", true
	case "application/pdf":
		return "document", true
	}
	return "", false
}

// OCRClient is the bounded, no-retry HTTP client for the ocr-service
// internal extract endpoint. Construct with NewOCRClient; an empty baseURL
// yields a disabled client (Enabled() == false).
type OCRClient struct {
	baseURL string
	token   string
	client  *http.Client
	log     *zap.Logger
	// expiresInHours (MEDIA_INBOX_TTL_HOURS, SPEC-W61 G2): when >0 the
	// extract call carries the OPTIONAL expires_in_hours form field so the
	// ocr row gets expires_at = now()+hours and the kind-generic sweeper
	// reaps bytes+row — TTL bounds parked media PII retention. Zero (the
	// default) omits the field: byte-identical pre-W61 behavior, and the
	// kyc path (its own TTL is ocr-side) is untouched.
	expiresInHours int
}

// NewOCRClient builds the client for an ocr-service base URL
// (OCR_SERVICE_URL, e.g. "http://ocr:7020") and the shared
// service-to-service token (OCR_INTERNAL_TOKEN — sent as X-Internal-Token;
// the SAME shared .env entry wires booking, kyc and this gateway).
// An empty baseURL yields a DISABLED client (Enabled() false); an empty
// token keeps Configured() false — the webhook then drops per-message
// (fail-closed, fail-visible), never calling out unauthenticated.
func NewOCRClient(baseURL, token string, log *zap.Logger) *OCRClient {
	if log == nil {
		log = zap.NewNop()
	}
	return &OCRClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		client:  &http.Client{Timeout: OCRTimeout},
		log:     log,
	}
}

// Enabled reports whether OCR_SERVICE_URL is set.
func (c *OCRClient) Enabled() bool { return c != nil && c.baseURL != "" }

// Configured reports whether the client may place calls: URL AND internal
// token both set (fail-closed mirror of the kyc-service internauth posture).
func (c *OCRClient) Configured() bool { return c.Enabled() && c.token != "" }

// SetExpiresInHours sets the optional expires_in_hours form field sent on
// extract calls (MEDIA_INBOX_TTL_HOURS; SPEC-W61: wired only when the
// media anchor flag is on). Values outside 1..8760 are ignored (config
// already clamps to the default; this is the belt to its braces). 0 resets
// to the byte-identical field-less form.
func (c *OCRClient) SetExpiresInHours(hours int) {
	if hours < 1 || hours > 8760 {
		hours = 0
	}
	c.expiresInHours = hours
}

// ValidateMime enforces the allow-list client-side: the declared mime must
// be one of image/jpeg|png|webp / application/pdf AND the payload's magic
// bytes must match it. Pure validation — no network.
func ValidateMime(declared string, data []byte) error {
	declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	if _, ok := allowedMimes[declared]; !ok {
		return fmt.Errorf("unsupported media type %q (want image/jpeg|image/png|image/webp|application/pdf)", declared)
	}
	if len(data) == 0 {
		return fmt.Errorf("empty media body")
	}
	var magicOK bool
	switch declared {
	case "image/jpeg":
		magicOK = len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF
	case "image/png":
		magicOK = len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
	case "image/webp":
		magicOK = len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP"))
	case "application/pdf":
		magicOK = bytes.HasPrefix(data, []byte("%PDF-"))
	}
	if !magicOK {
		return fmt.Errorf("magic bytes do not match declared %s", declared)
	}
	return nil
}

// Extract forwards one media blob to the ocr-service and returns the
// document_id (the audit anchor — the ocr_documents row IS the durability
// record, P9). Exactly ONE attempt (no retries — see the package comment).
// The caption is deliberately NOT a parameter: OCR has no field for it and
// it is PII (P8). filename is passed through (basename only) for the
// extension validation; empty falls back to "whatsapp-media"+ext.
//
// Errors: errNotConfigured (fail-closed), ErrRejectedMedia (deterministic
// 415 / client-side validation — webhook 200 + drop), ErrUnavailable
// (transient — webhook 500, N-05 redelivery).
func (c *OCRClient) Extract(ctx context.Context, tenantID uuid.UUID, declaredMime, filename string, data []byte) (string, error) {
	if !c.Configured() {
		return "", errNotConfigured
	}
	kind, ok := KindForOCR(declaredMime)
	if !ok {
		// Unreachable when the fetch allow-list gate ran first; kept as a
		// hard guard so the P2 mapping can never be bypassed.
		return "", fmt.Errorf("%w: no kind mapping for %q", ErrRejectedMedia, declaredMime)
	}
	if err := ValidateMime(declaredMime, data); err != nil {
		return "", fmt.Errorf("%w: %v", ErrRejectedMedia, err)
	}
	if len(data) > MaxMediaBytes {
		return "", fmt.Errorf("%w: %d bytes exceeds the %d-byte cap", ErrRejectedMedia, len(data), MaxMediaBytes)
	}
	declared := strings.ToLower(strings.TrimSpace(strings.Split(declaredMime, ";")[0]))

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("kind", kind); err != nil {
		return "", fmt.Errorf("build ocr request: %w", err)
	}
	// The ocr-service tenant-prefixes MinIO keys {tenant_id}/{uuid}.<ext>;
	// the uuid rides both the form field and the X-Tenant-Slug header
	// (geo-enrich precedent: the uuid form, never the slug).
	if err := mw.WriteField("tenant_id", tenantID.String()); err != nil {
		return "", fmt.Errorf("build ocr request: %w", err)
	}
	// SPEC-W61 G2: optional TTL for the ingested row. Sent only when the
	// media anchor rollout wired it (SetExpiresInHours > 0); absent =
	// byte-identical pre-W61 request (kyc/fieldcapture callers unaffected —
	// they use their own clients).
	if c.expiresInHours > 0 {
		if err := mw.WriteField("expires_in_hours", strconv.Itoa(c.expiresInHours)); err != nil {
			return "", fmt.Errorf("build ocr request: %w", err)
		}
	}
	fh := textproto.MIMEHeader{}
	fh.Set("Content-Disposition", `form-data; name="file"; filename="`+safeFilename(filename, declared)+`"`)
	fh.Set("Content-Type", declared)
	part, err := mw.CreatePart(fh)
	if err != nil {
		return "", fmt.Errorf("build ocr request: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("build ocr request: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("build ocr request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/ocr/extract", &body)
	if err != nil {
		return "", fmt.Errorf("%w: request build: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Internal-Token", c.token)
	req.Header.Set("X-Tenant-Slug", tenantID.String())

	resp, err := c.client.Do(req) // single attempt — NO retry loop by contract
	if err != nil {
		c.log.Warn("ocr extract call failed (media ingest retryable)",
			zap.String("tenant_id", tenantID.String()), zap.Int("bytes", len(data)), zap.Error(err))
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode == http.StatusUnsupportedMediaType {
		// Deterministic (P6): AV-infected or failed validation — the same
		// bytes can never pass, so no redelivery storm.
		c.log.Warn("ocr extract 415 (AV/validation reject — dropping media)",
			zap.String("tenant_id", tenantID.String()), zap.Int("bytes", len(data)))
		return "", fmt.Errorf("%w: status 415", ErrRejectedMedia)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.log.Warn("ocr extract non-2xx (media ingest retryable)",
			zap.String("tenant_id", tenantID.String()), zap.Int("status", resp.StatusCode))
		return "", fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	var res struct {
		DocumentID uuid.UUID `json:"document_id"`
		Status     string    `json:"status"`
	}
	// Bound the body so a misbehaving peer cannot stream forever.
	dec := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<20))
	if err := dec.Decode(&res); err != nil {
		c.log.Warn("ocr extract undecodable body (media ingest retryable)",
			zap.String("tenant_id", tenantID.String()), zap.Error(err))
		return "", fmt.Errorf("%w: undecodable response: %v", ErrUnavailable, err)
	}
	if res.DocumentID == uuid.Nil {
		// Contract violation: a 2xx without the document anchor is useless —
		// the media would land nowhere auditable. Retryable (N-05).
		c.log.Warn("ocr extract 2xx without document_id (contract violation)",
			zap.String("tenant_id", tenantID.String()))
		return "", fmt.Errorf("%w: response missing document_id", ErrUnavailable)
	}
	return res.DocumentID.String(), nil
}

// safeFilename reduces the webhook filename to a basename with an
// allow-listed extension; empty/unsafe input falls back to
// "whatsapp-media"+ext. It is used ONLY for the multipart filename
// (extension validation) — never logged with content.
func safeFilename(filename, declared string) string {
	name := path.Base(strings.TrimSpace(filename))
	ext := strings.ToLower(path.Ext(name))
	if name == "" || name == "." || name == "/" || ext != allowedMimes[declared] {
		return "whatsapp-media" + allowedMimes[declared]
	}
	return name
}

// String describes the client for boot logs (never logs the token).
func (c *OCRClient) String() string {
	if !c.Enabled() {
		return "whatsapp-media-ocr disabled (OCR_SERVICE_URL unset)"
	}
	if c.token == "" {
		return "whatsapp-media-ocr fail-closed (OCR_INTERNAL_TOKEN unset)"
	}
	return fmt.Sprintf("whatsapp-media-ocr via %s", c.baseURL)
}
