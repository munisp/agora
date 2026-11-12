package fieldcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// OCR receipt-extraction client (SPEC-W53 Scope B): forwards receipt/invoice
// bytes captured by field agents to the ocr-service (Scope A) internal
// extract endpoint.
//
// Contract (A↔B — MUST stay in exact sync with services/ocr-service
// POST /v1/ocr/extract):
//
//	POST {OCR_SERVICE_URL}/v1/ocr/extract
//	X-Internal-Token: <OCR_INTERNAL_TOKEN>   (service-to-service auth; A
//	                                constant-time-compares it and fails
//	                                closed when unset — kyc-service
//	                                server.go internauth posture)
//	X-Tenant-Slug: <tenant uuid>             (geo-enrich precedent: A parses
//	                                the tenant uuid for the RLS GUC +
//	                                tenant-prefixed MinIO keys; also sent as
//	                                the tenant_id form field for explicitness)
//	multipart form: kind=receipt, tenant_id=<uuid>, file=<bytes>
//
//	200 {"document_id": uuid,
//	     "status": "uploaded"|"extracted"|"failed",
//	     "extracted_fields": {"vendor","amount","date","items"} (nulls allowed),
//	     "page_count": int}
//
// Posture: BOUNDED (OCRTimeout), NO RETRIES (a retry would double-store PII
// bytes and double-bill ocr_page usage — the caller re-uploads explicitly).
// The ocr-service is fail-open for the capture (the capture row is never
// modified/lost by this path) but fail-closed for the receipt attach: any
// transport error / timeout / non-2xx / contract-violating body is
// ErrOCRUnavailable → 502 {code:"OCR_UNAVAILABLE"} at the handler.

// OCRTimeout bounds the whole extract call (SPEC-W53: 150s). MiniCPM-OCR on
// llama.cpp CPU is multi-second PER PAGE at ctx 40960 (RECON §5 CPU-latency
// risk) and a multi-page PDF rasterizes into several pages, so the usual
// 3s sidecar bound (geo.EnrichTimeout) would false-fail real receipts.
// NOTE: the httpapi router's chi middleware.Timeout (60s, server.go) can
// cancel the request context earlier — raising that bound for this route is
// a server.go-owner decision flagged in the W53 wiring note.
const OCRTimeout = 150 * time.Second

// MaxReceiptBytes is the hard byte cap for one receipt upload (SPEC-W53
// sacred constraint: 25 MB).
const MaxReceiptBytes = 25 << 20

// ErrOCRUnavailable marks any ocr-service failure (transport, timeout,
// non-2xx, contract-violating 2xx body). The handler maps it to
// 502 {code:"OCR_UNAVAILABLE"}; the capture row is never touched.
var ErrOCRUnavailable = errors.New("ocr service unavailable")

// errOCRNotConfigured is the fail-closed posture when OCR_SERVICE_URL or
// OCR_INTERNAL_TOKEN is unset (kyc-service internauth mirror: unset token →
// 503, never a silent open call). The handler checks Configured() up front;
// this guards direct client use.
var errOCRNotConfigured = errors.New("ocr client not configured (OCR_SERVICE_URL / OCR_INTERNAL_TOKEN unset)")

// ErrReceiptMedia marks client-side rejection of the upload: unsupported or
// missing declared mime, or magic bytes that do not match it (415).
var ErrReceiptMedia = errors.New("unsupported receipt media type")

// allowedReceiptMimes is the SPEC-W53 allow-list (extension + magic-byte
// validated; no AV scanner exists in the platform — documented gap).
var allowedReceiptMimes = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"application/pdf": ".pdf",
}

// ReceiptItem is one line item of the receipt-kind extracted_fields shape
// (A's parse is tolerant — every scalar may be null).
type ReceiptItem struct {
	Name     string   `json:"name"`
	Quantity *float64 `json:"quantity"`
	Price    *float64 `json:"price"`
}

// ReceiptFields is the receipt-kind extracted_fields shape (A↔B contract;
// nulls allowed). The capture row persists the RAW jsonb verbatim — this
// struct documents/validates the shape without constraining storage.
type ReceiptFields struct {
	Vendor *string       `json:"vendor"`
	Amount *float64      `json:"amount"`
	Date   *string       `json:"date"`
	Items  []ReceiptItem `json:"items"`
}

// OCRExtractResponse mirrors the ocr-service POST /v1/ocr/extract response
// body exactly (A↔B contract). ExtractedFields stays raw: it is persisted
// verbatim into field_captures.receipt_fields (jsonb, nulls allowed).
type OCRExtractResponse struct {
	DocumentID      uuid.UUID       `json:"document_id"`
	Status          string          `json:"status"` // uploaded | extracted | failed
	ExtractedFields json.RawMessage `json:"extracted_fields"`
	PageCount       int             `json:"page_count"`
}

// OCRClient is the bounded, no-retry HTTP client for the ocr-service
// internal extract endpoint. Construct with NewOCRClient; an empty baseURL
// yields a disabled client (Enabled() == false).
type OCRClient struct {
	baseURL string
	token   string
	client  *http.Client
	log     *zap.Logger
}

// NewOCRClient builds the client for an ocr-service base URL
// (OCR_SERVICE_URL, e.g. "http://ocr:7020") and the shared
// service-to-service token (OCR_INTERNAL_TOKEN — sent as X-Internal-Token).
// An empty baseURL yields a DISABLED client (Enabled() false); an empty
// token keeps Configured() false — the receipt endpoint then fails closed
// with 503, never calling out unauthenticated.
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
// token both set (fail-closed mirror of the kyc-service internauth posture —
// a call without the token is guaranteed a 401/503 at A, so B refuses
// locally with 503 instead).
func (c *OCRClient) Configured() bool { return c.Enabled() && c.token != "" }

// ValidateReceiptMime enforces the allow-list client-side (SPEC-W53 sacred
// constraint): the declared Content-Type must be one of
// image/jpeg|png|webp / application/pdf AND the payload's magic bytes must
// match it. Pure validation — no network, safe to call before any forward.
func ValidateReceiptMime(declared string, data []byte) error {
	declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	if _, ok := allowedReceiptMimes[declared]; !ok {
		return fmt.Errorf("%w: %q (want image/jpeg|image/png|image/webp|application/pdf)", ErrReceiptMedia, declared)
	}
	if len(data) == 0 {
		return fmt.Errorf("%w: empty body", ErrReceiptMedia)
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
		return fmt.Errorf("%w: magic bytes do not match declared %s", ErrReceiptMedia, declared)
	}
	return nil
}

// ExtractReceipt forwards one receipt image/PDF to the ocr-service and
// returns its extraction result. Exactly ONE attempt (no retries — see
// package-section comment). Errors:
//   - errOCRNotConfigured — URL/token unset (fail-closed; handler 503);
//   - ErrReceiptMedia — mime/magic rejection (handler 415; A would reject
//     the same bytes, so B rejects before spending the round-trip);
//   - ErrOCRUnavailable — transport/timeout/non-2xx/contract violation
//     (handler 502; the capture row is never touched on this path).
//
// Uploaded bytes are PII: they are never logged (only sizes/statuses).
func (c *OCRClient) ExtractReceipt(ctx context.Context, tenantID uuid.UUID, declaredMime string, data []byte) (*OCRExtractResponse, error) {
	if !c.Configured() {
		return nil, errOCRNotConfigured
	}
	if err := ValidateReceiptMime(declaredMime, data); err != nil {
		return nil, err
	}
	if len(data) > MaxReceiptBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds the %d-byte cap", ErrReceiptMedia, len(data), MaxReceiptBytes)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("kind", "receipt"); err != nil {
		return nil, fmt.Errorf("build ocr request: %w", err)
	}
	// A tenant-prefixes MinIO keys {tenant_id}/{uuid}.<ext>; the uuid rides
	// both the form field and the X-Tenant-Slug header (geo-enrich
	// precedent: the uuid form, never the slug).
	if err := mw.WriteField("tenant_id", tenantID.String()); err != nil {
		return nil, fmt.Errorf("build ocr request: %w", err)
	}
	declared := strings.ToLower(strings.TrimSpace(strings.Split(declaredMime, ";")[0]))
	fh := textproto.MIMEHeader{}
	fh.Set("Content-Disposition", `form-data; name="file"; filename="receipt`+allowedReceiptMimes[declared]+`"`)
	fh.Set("Content-Type", declared)
	part, err := mw.CreatePart(fh)
	if err != nil {
		return nil, fmt.Errorf("build ocr request: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, fmt.Errorf("build ocr request: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("build ocr request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/ocr/extract", &body)
	if err != nil {
		return nil, fmt.Errorf("%w: request build: %v", ErrOCRUnavailable, err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Internal-Token", c.token)
	req.Header.Set("X-Tenant-Slug", tenantID.String())

	resp, err := c.client.Do(req) // single attempt — NO retry loop by contract
	if err != nil {
		c.log.Warn("ocr extract call failed (receipt attach fail-closed)",
			zap.String("tenant_id", tenantID.String()), zap.Int("bytes", len(data)), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", ErrOCRUnavailable, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.log.Warn("ocr extract non-2xx (receipt attach fail-closed)",
			zap.String("tenant_id", tenantID.String()), zap.Int("status", resp.StatusCode))
		return nil, fmt.Errorf("%w: status %d", ErrOCRUnavailable, resp.StatusCode)
	}
	var res OCRExtractResponse
	// Bound the body so a misbehaving peer cannot stream forever; extraction
	// fields for a receipt are kilobytes at most.
	dec := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<20))
	if err := dec.Decode(&res); err != nil {
		c.log.Warn("ocr extract undecodable body (receipt attach fail-closed)",
			zap.String("tenant_id", tenantID.String()), zap.Error(err))
		return nil, fmt.Errorf("%w: undecodable response: %v", ErrOCRUnavailable, err)
	}
	if res.DocumentID == uuid.Nil {
		// Contract violation: a 2xx without the document anchor is useless —
		// the capture row would point at nothing. Fail-closed for the attach.
		c.log.Warn("ocr extract 2xx without document_id (contract violation)",
			zap.String("tenant_id", tenantID.String()))
		return nil, fmt.Errorf("%w: response missing document_id", ErrOCRUnavailable)
	}
	return &res, nil
}

// String describes the client for boot logs (never logs the token).
func (c *OCRClient) String() string {
	if !c.Enabled() {
		return "receipt-ocr disabled (OCR_SERVICE_URL unset)"
	}
	if c.token == "" {
		return "receipt-ocr fail-closed (OCR_INTERNAL_TOKEN unset)"
	}
	return fmt.Sprintf("receipt-ocr via %s", c.baseURL)
}
