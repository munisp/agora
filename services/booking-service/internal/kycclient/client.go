// Package kycclient is the booking-service -> kyc-service passthrough
// client for the SPEC-W58 KYC document surface (Scope C, P1 Option A):
//
//	admin-web -> booking /v1/kyc/documents (owner/admin gate, P5)
//	  -> kyc-service /v1/kyc/documents (internauth + consent gate
//	     purpose=kyc_document, P6 + hash-only record, P8 — Scope B)
//	    -> ocr-service extraction (Scope A)
//
// Contract (cross-scope, pinned in SPEC-W58):
//
//	POST {BOOKING_KYC_SERVICE_URL}/v1/kyc/documents
//	X-Internal-Token: <KYC_INTERNAL_TOKEN>   (the shared service-to-service
//	                                token already read for lending —
//	                                main.go LENDING wiring precedent;
//	                                kyc-service constant-time-compares it and
//	                                fails closed when unset)
//	X-Tenant-Slug: <tenant uuid>             (ocr-client precedent: the uuid
//	                                form, also sent as the tenant_id form
//	                                field for explicitness)
//	multipart form: file=<bytes>, subject_phone=<E.164>, doc_type=<kyc_*>,
//	                consent_id=<optional>, tenant_id=<uuid>
//	  -> envelope {document_id, reference, status, review_state,
//	               fields?, fields_available}
//
//	GET  /v1/kyc/documents/{id}            (same auth headers; fields may be
//	                                       absent post-TTL — kyc-service
//	                                       returns fields_available:false)
//	POST /v1/kyc/documents/{id}/review     JSON {decision: approved|rejected,
//	                                       reason}
//
// Posture (mirrors internal/fieldcapture/ocr_client.go): BOUNDED (Timeout),
// NO RETRIES (a retry would double-store PII bytes and double-spend the
// consent-gated audit — the operator re-submits explicitly). Transport
// errors / timeouts are ErrUnavailable -> 502 at the handler; downstream
// non-2xx statuses PASS THROUGH verbatim (the kyc-service owns the domain
// statuses: 400 validation, 403 consent/K22, 404, 422...). Uploaded bytes
// are PII: they are never logged (only sizes/statuses).
package kycclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Timeout bounds the whole kyc-service call (SPEC-W58 C1: the 150s posture
// mirrors fieldcapture.OCRTimeout — the kyc-service forwards to ocr-service
// synchronously, and MiniCPM-OCR on CPU is multi-second per page). The
// httpapi router's chi middleware.Timeout (60s) would cancel the upload
// earlier — the owner/admin upload route is carved out in
// httpapi/server.go (receipt precedent, 150s+30s).
const Timeout = 150 * time.Second

// MaxDocumentBytes is the hard byte cap for one KYC document upload
// (SPEC-W58: 25 MB, same sacred constraint as receipts).
const MaxDocumentBytes = 25 << 20

// maxResponseBytes bounds a downstream response body; the KYC document
// envelope (ids + hashes + whitelisted fields) is kilobytes at most.
const maxResponseBytes = 4 << 20

// ErrUnavailable marks any kyc-service transport failure (dial, timeout,
// unreadable body). The handlers map it to 502 {code:"KYC_UNAVAILABLE"}.
var ErrUnavailable = errors.New("kyc service unavailable")

// ErrNotConfigured is the fail-closed posture when BOOKING_KYC_SERVICE_URL
// or KYC_INTERNAL_TOKEN is unset (kyc-service internauth mirror: unset
// token -> 503, never a silent open call). The handlers check Configured()
// up front; this guards direct client use.
var ErrNotConfigured = errors.New("kyc client not configured (BOOKING_KYC_SERVICE_URL / KYC_INTERNAL_TOKEN unset)")

// ErrMedia marks client-side rejection of the upload: unsupported or
// missing declared mime, or magic bytes that do not match it (415).
var ErrMedia = errors.New("unsupported document media type")

// ErrDocType marks a doc_type outside the pinned P9 kind set (400).
var ErrDocType = errors.New("unsupported kyc doc_type")

// allowedMimes is the upload allow-list (extension + magic-byte validated;
// no AV scanner exists in the platform — documented gap, W53 R9).
var allowedMimes = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"application/pdf": ".pdf",
}

// docTypes is the pinned SPEC-W58 P9 kind set. The kyc-service validates
// the same enum authoritatively (B2); this is the client-side fail-fast
// mirror (the ocr-service "kind validated BEFORE any work" posture).
var docTypes = map[string]struct{}{
	"kyc_nin_slip":        {},
	"kyc_drivers_license": {},
	"kyc_passport":        {},
	"kyc_voters_card":     {},
	"kyc_cac_document":    {},
}

// IsDocType reports whether doc_type is one of the pinned P9 kinds.
func IsDocType(docType string) bool {
	_, ok := docTypes[strings.ToLower(strings.TrimSpace(docType))]
	return ok
}

// ValidateMime enforces the allow-list client-side: the declared
// Content-Type must be one of image/jpeg|png|webp / application/pdf AND
// the payload's magic bytes must match it (mirror of
// fieldcapture.ValidateReceiptMime — same sacred constraint). Pure
// validation — no network, safe to call before any forward.
func ValidateMime(declared string, data []byte) error {
	declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	if _, ok := allowedMimes[declared]; !ok {
		return fmt.Errorf("%w: %q (want image/jpeg|image/png|image/webp|application/pdf)", ErrMedia, declared)
	}
	if len(data) == 0 {
		return fmt.Errorf("%w: empty body", ErrMedia)
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
		return fmt.Errorf("%w: magic bytes do not match declared %s", ErrMedia, declared)
	}
	return nil
}

// Response is the verbatim downstream response: HTTP status + raw body.
// The handlers pass it through untouched — the kyc-service owns the
// envelope shape and the domain status codes (B2/cross-scope contract).
type Response struct {
	Status int
	Body   []byte
}

// Client is the bounded, no-retry HTTP client for the kyc-service internal
// document endpoints. Construct with New; an empty baseURL yields a
// disabled client (Enabled() == false).
type Client struct {
	baseURL string
	token   string
	client  *http.Client
	log     *zap.Logger
}

// New builds the client for a kyc-service base URL
// (BOOKING_KYC_SERVICE_URL, e.g. "http://kyc:7013") and the shared
// service-to-service token (KYC_INTERNAL_TOKEN — sent as X-Internal-Token,
// the same env the lending integration already reads). An empty baseURL
// yields a DISABLED client (Enabled() false); an empty token keeps
// Configured() false — the endpoints then fail closed with 503, never
// calling out unauthenticated.
func New(baseURL, token string, log *zap.Logger) *Client {
	if log == nil {
		log = zap.NewNop()
	}
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		client:  &http.Client{Timeout: Timeout},
		log:     log,
	}
}

// Enabled reports whether BOOKING_KYC_SERVICE_URL is set.
func (c *Client) Enabled() bool { return c != nil && c.baseURL != "" }

// Configured reports whether the client may place calls: URL AND internal
// token both set (fail-closed mirror of the kyc-service internauth
// posture — a call without the token is guaranteed a 401/503 downstream,
// so booking refuses locally with 503 instead).
func (c *Client) Configured() bool { return c.Enabled() && c.token != "" }

// UploadDocument forwards one KYC document image/PDF plus its consent
// context to kyc-service POST /v1/kyc/documents. Exactly ONE attempt (no
// retries — see the package comment). Errors:
//   - ErrNotConfigured — URL/token unset (fail-closed; handler 503);
//   - ErrMedia — mime/magic rejection (handler 415; the kyc-service would
//     reject the same bytes, so booking rejects before spending the
//     round-trip);
//   - ErrDocType — doc_type outside the pinned P9 set (handler 400);
//   - ErrUnavailable — transport/timeout/unreadable response (handler
//     502). Downstream non-2xx statuses are NOT errors here: they pass
//     through as Response{Status, Body}.
func (c *Client) UploadDocument(ctx context.Context, tenantID uuid.UUID, declaredMime string, data []byte, subjectPhone, docType, consentID string) (*Response, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	docType = strings.ToLower(strings.TrimSpace(docType))
	if !IsDocType(docType) {
		return nil, fmt.Errorf("%w: %q", ErrDocType, docType)
	}
	if err := ValidateMime(declaredMime, data); err != nil {
		return nil, err
	}
	if len(data) > MaxDocumentBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds the %d-byte cap", ErrMedia, len(data), MaxDocumentBytes)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fields := map[string]string{
		"subject_phone": strings.TrimSpace(subjectPhone),
		"doc_type":      docType,
		// The tenant uuid rides both the form field and the X-Tenant-Slug
		// header (ocr-client precedent: the uuid form, never the slug).
		"tenant_id": tenantID.String(),
	}
	if cid := strings.TrimSpace(consentID); cid != "" {
		fields["consent_id"] = cid
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return nil, fmt.Errorf("build kyc request: %w", err)
		}
	}
	declared := strings.ToLower(strings.TrimSpace(strings.Split(declaredMime, ";")[0]))
	fh := textproto.MIMEHeader{}
	fh.Set("Content-Disposition", `form-data; name="file"; filename="document`+allowedMimes[declared]+`"`)
	fh.Set("Content-Type", declared)
	part, err := mw.CreatePart(fh)
	if err != nil {
		return nil, fmt.Errorf("build kyc request: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, fmt.Errorf("build kyc request: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("build kyc request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/kyc/documents", &body)
	if err != nil {
		return nil, fmt.Errorf("%w: request build: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	return c.do(req, tenantID, "upload")
}

// GetDocument fetches the review surface for one KYC document
// (GET /v1/kyc/documents/{id}) — hash-only record + TTL-bound fields
// proxied from ocr-service by kyc-service (fields_available:false after
// TTL expiry). Single bounded attempt; errors are ErrNotConfigured /
// ErrUnavailable only — downstream statuses pass through.
func (c *Client) GetDocument(ctx context.Context, tenantID uuid.UUID, documentID string) (*Response, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/kyc/documents/"+url.PathEscape(documentID), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: request build: %v", ErrUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	return c.do(req, tenantID, "get")
}

// ReviewDocument submits the human-review decision for one KYC document
// (POST /v1/kyc/documents/{id}/review, JSON {decision, reason}). The
// decision enum (approved|rejected) is validated by the caller AND
// authoritatively by kyc-service (B2). Single bounded attempt; errors are
// ErrNotConfigured / ErrUnavailable only — downstream statuses pass
// through.
func (c *Client) ReviewDocument(ctx context.Context, tenantID uuid.UUID, documentID, decision, reason string) (*Response, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	payload, err := json.Marshal(map[string]string{"decision": decision, "reason": reason})
	if err != nil {
		return nil, fmt.Errorf("build kyc review request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/kyc/documents/"+url.PathEscape(documentID)+"/review",
		bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: request build: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return c.do(req, tenantID, "review")
}

// do performs the single bounded attempt with the shared auth/tenant
// headers and returns the downstream response verbatim. PII bytes are
// never logged — sizes and statuses only.
func (c *Client) do(req *http.Request, tenantID uuid.UUID, op string) (*Response, error) {
	req.Header.Set("X-Internal-Token", c.token)
	req.Header.Set("X-Tenant-Slug", tenantID.String())

	resp, err := c.client.Do(req) // single attempt — NO retry loop by contract
	if err != nil {
		c.log.Warn("kyc-service call failed",
			zap.String("op", op), zap.String("tenant_id", tenantID.String()), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	// Bound the body so a misbehaving peer cannot stream forever.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		c.log.Warn("kyc-service response unreadable",
			zap.String("op", op), zap.String("tenant_id", tenantID.String()), zap.Error(err))
		return nil, fmt.Errorf("%w: response unreadable: %v", ErrUnavailable, err)
	}
	if len(body) > maxResponseBytes {
		c.log.Warn("kyc-service response over the size bound",
			zap.String("op", op), zap.String("tenant_id", tenantID.String()))
		return nil, fmt.Errorf("%w: response exceeds the %d-byte bound", ErrUnavailable, maxResponseBytes)
	}
	return &Response{Status: resp.StatusCode, Body: body}, nil
}

// String describes the client for boot logs (never logs the token).
func (c *Client) String() string {
	if !c.Enabled() {
		return "kyc-documents disabled (BOOKING_KYC_SERVICE_URL unset)"
	}
	if c.token == "" {
		return "kyc-documents fail-closed (KYC_INTERNAL_TOKEN unset)"
	}
	return fmt.Sprintf("kyc-documents via %s", c.baseURL)
}
