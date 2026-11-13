// KYC document extraction surface (SPEC-W58 Scope B): consent-gated
// document upload -> ocr-service extraction -> HASH-ONLY durable record
// (P8) + append-only audit -> human review (P7: review is terminal, OCR
// never auto-verifies).
//
// Routes (all under the existing /v1/kyc internauth, server.go):
//
//	POST /v1/kyc/documents            multipart: file + tenant_id +
//	                                  subject_phone + doc_type +
//	                                  consent_id? -> 200 {document_id,
//	                                  reference, status, review_state}
//	GET  /v1/kyc/documents/{id}       ?tenant_id=<uuid> (or X-Tenant-ID)
//	                                  -> row + fields proxied from ocr
//	POST /v1/kyc/documents/{id}/review {tenant_id, decision, reason?}
//
// Hash-only posture (P8): raw extracted field values are NEVER persisted
// by kyc-service and NEVER logged; the kyc_documents row stores per-field
// HMAC-SHA256 digests only. Cleartext fields live exclusively in the ocr
// DB under TTL and transit this service only in-memory on the review GET.
package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/kyc-service/internal/store"
	"go.uber.org/zap"
)

// PurposeKYCDocument is the consent-registry purpose for KYC document
// capture (SPEC-W58 P6 / recon R5): distinct from purpose=kyc (BVN/NIN
// resolution) so onboarding consent never silently authorizes document
// retention. Identity purposes are free-form text (consent/model.go).
const PurposeKYCDocument = "kyc_document"

// MaxDocumentBytes is the hard byte cap for one document upload (25 MB —
// the SPEC-W53 sacred constraint, mirrored from ocr validate.py).
const MaxDocumentBytes = 25 << 20

// OCRTimeout bounds the whole extract call (150s, SPEC-W58 B2): the OCR
// model is multi-second PER PAGE on CPU (booking fieldcapture ocr_client.go
// precedent). NO RETRIES — a retry would double-store PII bytes and
// double-bill ocr_page usage; the caller re-uploads explicitly.
const OCRTimeout = 150 * time.Second

// ocrGetTimeout bounds the cheap metadata/fields GET against ocr-service.
const ocrGetTimeout = 30 * time.Second

// docIDField maps each doc_type to the extracted-field key that carries
// the document's ID number — the value hashed into id_value_hash for the
// K22 document binding (SPEC-W58 P8 + P9 whitelists).
var docIDField = map[string]string{
	"kyc_nin_slip":        "nin",
	"kyc_drivers_license": "license_no",
	"kyc_passport":        "passport_no",
	"kyc_voters_card":     "vin",
	"kyc_cac_document":    "rc_number",
}

// allowedDocMimes is the mime allow-list (jpeg/png/webp/pdf, magic-byte
// checked) — identical to the ocr-service and booking fieldcapture lists.
var allowedDocMimes = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"application/pdf": ".pdf",
}

// e164 is the E.164 shape enforced on subject_phone (SPEC-W58 B2).
var e164 = regexp.MustCompile(`^\+[1-9]\d{6,14}$`)

// documentStatuses is the kyc_documents.status CHECK domain (the ocr
// extract endpoint answers "extracted" on success; the other two keep the
// row honest for future async flows).
var documentStatuses = map[string]bool{"uploaded": true, "extracted": true, "failed": true}

// Review decisions (SPEC-W58 cross-scope contract).
const (
	ReviewApproved = "approved"
	ReviewRejected = "rejected"
)

// ErrOCRUnavailable marks any ocr-service failure (transport, timeout,
// non-2xx, contract-violating 2xx body) — mapped to 502 by the handler
// (booking fieldcapture ErrOCRUnavailable precedent).
var ErrOCRUnavailable = errors.New("ocr service unavailable")

// errOCRNotConfigured is the fail-closed posture when KYC_OCR_BASE_URL or
// OCR_INTERNAL_TOKEN is unset (internauth mirror: never a silent open
// call). The handler answers 503.
var errOCRNotConfigured = errors.New("ocr client not configured (KYC_OCR_BASE_URL / OCR_INTERNAL_TOKEN unset)")

// ---------------------------------------------------------------------------
// OCR backend (client contract — mirrors ocr-service app/main.py exactly)
// ---------------------------------------------------------------------------

// OCRExtractResult mirrors the ocr-service POST /v1/ocr/extract 200 body.
// extracted_text is DELIBERATELY not mapped (P8: the raw transcript never
// enters a kyc-service struct).
type OCRExtractResult struct {
	DocumentID uuid.UUID      `json:"document_id"`
	Status     string         `json:"status"` // extracted on the 200 path
	Fields     map[string]any `json:"extracted_fields"`
	PageCount  int            `json:"page_count"`
}

// OCRDocumentInfo mirrors the ocr-service GET /v1/ocr/documents/{id} 200
// body (subset — the keys kyc-service consumes). extracted_text,
// storage_bucket and storage_key are DELIBERATELY not mapped.
type OCRDocumentInfo struct {
	DocumentID uuid.UUID      `json:"document_id"`
	Kind       string         `json:"kind"`
	Status     string         `json:"status"` // uploaded|extracted|failed|erased (P3 erasure)
	SHA256     string         `json:"sha256"`
	PHash      string         `json:"phash"`
	Fields     map[string]any `json:"extracted_fields"`
}

// OCRBackend is the ocr-service dependency the document handlers use.
// *OCRClient is the production implementation; tests substitute a fake.
type OCRBackend interface {
	// Configured reports whether calls may be placed (base URL AND internal
	// token both set — fail-closed mirror of the internauth posture).
	Configured() bool
	// Extract forwards document bytes to POST /v1/ocr/extract with
	// kind = doc_type verbatim (cross-scope contract). Bounded, no retries.
	Extract(ctx context.Context, tenantID uuid.UUID, kind, mime string, data []byte) (*OCRExtractResult, error)
	// GetDocument proxies GET /v1/ocr/documents/{id}. (nil, nil) when the
	// ocr row is gone (404 — TTL sweep or TenantDeleted cascade).
	GetDocument(ctx context.Context, tenantID, documentID uuid.UUID) (*OCRDocumentInfo, error)
}

// OCRClient is the bounded, no-retry HTTP client for the ocr-service
// internal endpoints (booking fieldcapture OCRClient precedent).
type OCRClient struct {
	baseURL string
	token   string
	hc      *http.Client
	log     *zap.Logger
}

// NewOCRClient builds the client for an ocr-service base URL
// (KYC_OCR_BASE_URL, e.g. "http://ocr:7020") and the shared
// service-to-service token (OCR_INTERNAL_TOKEN — the same shared secret
// var the booking block already sends; SPEC-W58 P12 "reuses existing
// internal-token"). Empty base URL or token => Configured() false and the
// document endpoints fail closed with 503.
func NewOCRClient(baseURL, token string, log *zap.Logger) *OCRClient {
	if log == nil {
		log = zap.NewNop()
	}
	return &OCRClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		hc:      &http.Client{Timeout: OCRTimeout},
		log:     log,
	}
}

// Enabled reports whether KYC_OCR_BASE_URL is set.
func (c *OCRClient) Enabled() bool { return c != nil && c.baseURL != "" }

// Configured implements OCRBackend.
func (c *OCRClient) Configured() bool { return c.Enabled() && c.token != "" }

// Extract implements OCRBackend. Uploaded bytes are PII: they are never
// logged (only sizes/statuses). Exactly ONE attempt by contract.
func (c *OCRClient) Extract(ctx context.Context, tenantID uuid.UUID, kind, mime string, data []byte) (*OCRExtractResult, error) {
	if !c.Configured() {
		return nil, errOCRNotConfigured
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	// Cross-scope contract: ocr kind = doc_type verbatim (kyc_* values);
	// tenant uuid rides the form field AND the X-Tenant-Slug header (they
	// must agree — ocr _authenticate 400s on conflicting selectors).
	if err := mw.WriteField("kind", kind); err != nil {
		return nil, fmt.Errorf("build ocr request: %w", err)
	}
	if err := mw.WriteField("tenant_id", tenantID.String()); err != nil {
		return nil, fmt.Errorf("build ocr request: %w", err)
	}
	fh := textproto.MIMEHeader{}
	fh.Set("Content-Disposition", `form-data; name="file"; filename="document`+allowedDocMimes[mime]+`"`)
	fh.Set("Content-Type", mime)
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

	resp, err := c.hc.Do(req) // single attempt — NO retry loop by contract
	if err != nil {
		c.log.Warn("ocr extract call failed",
			zap.String("tenant_id", tenantID.String()), zap.Int("bytes", len(data)), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", ErrOCRUnavailable, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.log.Warn("ocr extract non-2xx",
			zap.String("tenant_id", tenantID.String()), zap.Int("status", resp.StatusCode))
		return nil, fmt.Errorf("%w: status %d", ErrOCRUnavailable, resp.StatusCode)
	}
	var res OCRExtractResult
	// Bound the body so a misbehaving peer cannot stream forever.
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<20)).Decode(&res); err != nil {
		return nil, fmt.Errorf("%w: undecodable response: %v", ErrOCRUnavailable, err)
	}
	if res.DocumentID == uuid.Nil {
		return nil, fmt.Errorf("%w: response missing document_id", ErrOCRUnavailable)
	}
	if !documentStatuses[res.Status] {
		return nil, fmt.Errorf("%w: response status %q outside the contract", ErrOCRUnavailable, res.Status)
	}
	return &res, nil
}

// GetDocument implements OCRBackend. (nil, nil) on a 404 — the ocr row is
// gone (TTL sweep / erasure), which the handler reports honestly as
// fields_available:false.
func (c *OCRClient) GetDocument(ctx context.Context, tenantID, documentID uuid.UUID) (*OCRDocumentInfo, error) {
	if !c.Configured() {
		return nil, errOCRNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, ocrGetTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/ocr/documents/"+documentID.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: request build: %v", ErrOCRUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Internal-Token", c.token)
	req.Header.Set("X-Tenant-Slug", tenantID.String())

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOCRUnavailable, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: status %d", ErrOCRUnavailable, resp.StatusCode)
	}
	var info OCRDocumentInfo
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<20)).Decode(&info); err != nil {
		return nil, fmt.Errorf("%w: undecodable response: %v", ErrOCRUnavailable, err)
	}
	return &info, nil
}

// String describes the client for boot logs (never logs the token).
func (c *OCRClient) String() string {
	if !c.Enabled() {
		return "kyc-document-ocr disabled (KYC_OCR_BASE_URL unset)"
	}
	if c.token == "" {
		return "kyc-document-ocr fail-closed (OCR_INTERNAL_TOKEN unset)"
	}
	return fmt.Sprintf("kyc-document-ocr via %s", c.baseURL)
}

// ---------------------------------------------------------------------------
// Hashing (P8) — HMAC-SHA256 keyed with KYC_HASH_SECRET, never raw values
// ---------------------------------------------------------------------------

// hashFields computes the per-field HMAC-SHA256 map for the extracted
// fields plus the id_value_hash of the document number. Returns
// (fieldHashes, idValueHash). idValueHash is "" when the document-number
// field for this doc_type is absent/null/empty in the OCR output — the K22
// binding is then skipped (documented in docs/kyc.md).
//
// Canonicalization: string values are trimmed; document numbers are
// additionally upper-cased so "A123" and "a123" bind together. Non-string
// scalars (e.g. mrz_valid booleans) hash their JSON encoding. Nested
// values (should not occur per the P9 whitelists) hash their JSON
// encoding. Field digests are domain-separated from the bare-value digest
// (field name mixed in) so a field hash never collides with id_value_hash
// semantics.
func hashFields(secret, docType string, fields map[string]any) (map[string]string, string) {
	hashes := map[string]string{}
	idHash := ""
	idKey := docIDField[docType]
	for k, v := range fields {
		if v == nil {
			continue
		}
		var canonical string
		switch t := v.(type) {
		case string:
			canonical = strings.TrimSpace(t)
		default:
			raw, err := json.Marshal(v)
			if err != nil {
				continue
			}
			canonical = string(raw)
		}
		if canonical == "" {
			continue
		}
		hashes[k] = hashIDValue(secret, "kycfield\x00"+k+"\x00"+canonical)
		if k == idKey {
			if s, ok := v.(string); ok {
				norm := strings.ToUpper(strings.TrimSpace(s))
				if norm != "" {
					idHash = hashIDValue(secret, norm)
				}
			}
		}
	}
	return hashes, idHash
}

// docReferenceFor derives the deterministic document reference (uuid5 of
// tenant+subject+doc_type+id hash) — the resolve referenceFor precedent,
// with a distinct prefix so document and resolution references never mix.
func docReferenceFor(tenantID uuid.UUID, subject, docType, idValueHash string) string {
	return "kyc_doc_" + uuid.NewSHA1(uuid.NameSpaceURL,
		[]byte(tenantID.String()+"|"+subject+"|"+docType+"|"+idValueHash)).String()
}

// documentSHA256 is the sha256 of the uploaded bytes — identical to the
// digest ocr-service computes at upload (validate.py sha256_hex), so the
// kyc_documents row can corroborate the ocr row without a second hop.
func documentSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// validateDocumentMime enforces the allow-list client-side: declared
// Content-Type in {jpeg,png,webp,pdf} AND magic bytes matching it
// (ocr validate.py contract — A would reject the same bytes, so B rejects
// before spending the round-trip).
func validateDocumentMime(declared string, data []byte) error {
	if _, ok := allowedDocMimes[declared]; !ok {
		return fmt.Errorf("unsupported mime %q (want image/jpeg|image/png|image/webp|application/pdf)", declared)
	}
	if len(data) == 0 {
		return fmt.Errorf("empty file")
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

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// documentResponse is the cross-scope response envelope (SPEC-W58
// cross-scope contract: B2/C2/D1 share it).
type documentResponse struct {
	DocumentID      uuid.UUID      `json:"document_id"`
	Reference       string         `json:"reference"`
	Status          string         `json:"status"`
	ReviewState     string         `json:"review_state"`
	FieldsAvailable bool           `json:"fields_available"`
	Fields          map[string]any `json:"fields,omitempty"`
}

// documentDetailResponse is the GET row projection (hash-only — no raw
// extracted values from kyc storage, ever) plus the TTL-bound fields
// proxied from ocr-service when still available.
type documentDetailResponse struct {
	documentResponse
	TenantID      uuid.UUID         `json:"tenant_id"`
	SubjectPhone  string            `json:"subject_phone"`
	DocType       string            `json:"doc_type"`
	OCRDocumentID uuid.UUID         `json:"ocr_document_id"`
	SHA256        string            `json:"sha256"`
	PHash         string            `json:"phash"`
	IDValueHash   string            `json:"id_value_hash"`
	FieldHashes   map[string]string `json:"field_hashes"`
	ReviewerActor string            `json:"reviewer_actor,omitempty"`
	ReviewedAt    *time.Time        `json:"reviewed_at,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
}

// createDocument (POST /v1/kyc/documents) — SPEC-W58 B2. Sequencing
// (pinned in NOTES + docs/kyc.md):
//  1. validate: multipart shape, doc_type enum, subject_phone E.164,
//     bytes <= 25MB, mime allow-list + magic bytes;
//  2. consent gate purpose=kyc_document (P6): denied => 403, gate-down =>
//  502. The id_hash binding context is EMPTY here by construction — the
//     document number only exists after OCR extraction;
//  3. forward bytes to ocr-service (150s bounded, no retry, kind =
//     doc_type verbatim). Failure => audit row (status=failed) + 502;
//  4. compute per-field HMAC hashes + id_value_hash of the extracted
//     document number (P8) — raw values stay in memory only;
//  5. K22 binding on (doc_type, id_value_hash), POST-extraction
//     PRE-commit: same hash under a different subject_phone => audit row
//     (status=failed, detail=id_binding_violation) + 403. The binding is
//     established by the first committed kyc_documents row;
//  6. insert the hash-only row + audit row (every action audits);
//  7. 200 {document_id, reference, status, review_state:"pending_review"}.
func (s *server) createDocument(w http.ResponseWriter, r *http.Request) {
	// 25MB cap on the file + 1MB slack for the surrounding form fields.
	r.Body = http.MaxBytesReader(w, r.Body, MaxDocumentBytes+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "document exceeds the 25MB cap")
			return
		}
		writeError(w, http.StatusBadRequest, "expected multipart/form-data (file + tenant_id + subject_phone + doc_type)")
		return
	}
	tenantRef := strings.TrimSpace(r.FormValue("tenant_id"))
	subject := strings.TrimSpace(r.FormValue("subject_phone"))
	docType := strings.ToLower(strings.TrimSpace(r.FormValue("doc_type")))
	consentID := strings.TrimSpace(r.FormValue("consent_id"))
	if tenantRef == "" || subject == "" {
		writeError(w, http.StatusBadRequest, "tenant_id and subject_phone are required")
		return
	}
	if !e164.MatchString(subject) {
		writeError(w, http.StatusBadRequest, "subject_phone must be E.164 (+<country><number>)")
		return
	}
	if _, ok := docIDField[docType]; !ok {
		writeError(w, http.StatusBadRequest, "doc_type must be one of kyc_nin_slip|kyc_drivers_license|kyc_passport|kyc_voters_card|kyc_cac_document")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "multipart field 'file' is required")
		return
	}
	defer file.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(file, MaxDocumentBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading upload failed")
		return
	}
	if len(data) > MaxDocumentBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "document exceeds the 25MB cap")
		return
	}
	mime := strings.ToLower(strings.TrimSpace(strings.Split(header.Header.Get("Content-Type"), ";")[0]))
	if err := validateDocumentMime(mime, data); err != nil {
		writeError(w, http.StatusUnsupportedMediaType, err.Error())
		return
	}

	// Consent gate (P6): purpose=kyc_document on the consented subject.
	// id_hash is "" — the document number is unknown until OCR extraction;
	// the ID-level binding is enforced at step 5 instead.
	tenantID, err := s.d.Consent.CheckConsent(r.Context(), tenantRef, subject, PurposeKYCDocument, docType, "")
	if errors.Is(err, ErrConsentDenied) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":  "consent_required",
			"detail": "no active consent for subject_phone with purpose kyc_document",
		})
		return
	}
	if err != nil {
		s.d.Logger.Error("consent gate failure", zap.Error(err))
		writeError(w, http.StatusBadGateway, "consent gate unavailable")
		return
	}

	// Fail-closed when the OCR upstream is not wired (never a silent open
	// call, internauth mirror posture).
	if s.d.OCR == nil || !s.d.OCR.Configured() {
		s.d.Logger.Error("KYC_OCR_BASE_URL/OCR_INTERNAL_TOKEN unset — refusing document upload (fail-closed)")
		writeError(w, http.StatusServiceUnavailable, "ocr upstream not configured")
		return
	}

	actor := strings.TrimSpace(r.Header.Get("X-Actor"))
	// reference/idHash are filled in once extraction yields the document
	// number; the audit closure reads them at call time (locals — never
	// package state, concurrent uploads must not share them).
	reference := ""
	idHash := ""
	audit := func(status, detail string) error {
		return s.d.Store.InsertAudit(r.Context(), &store.Audit{
			TenantID:     tenantID,
			Actor:        actor,
			SubjectPhone: subject,
			IDType:       docType,
			IDValueHash:  idHash,
			Status:       status,
			Reference:    reference,
			ConsentID:    consentID,
			Detail:       detail,
		})
	}

	// Forward to ocr-service (bounded, no retry, kind = doc_type verbatim).
	res, err := s.d.OCR.Extract(r.Context(), tenantID, docType, mime, data)
	if err != nil {
		s.d.Logger.Warn("ocr extract failed for kyc document",
			zap.String("tenant_id", tenantID.String()), zap.String("doc_type", docType), zap.Error(err))
		reference = docReferenceFor(tenantID, subject, docType, "")
		if aerr := audit("failed", "ocr_unavailable"); aerr != nil {
			s.internal(w, aerr)
			return
		}
		writeError(w, http.StatusBadGateway, "ocr extraction unavailable")
		return
	}

	// P8: hash-only from here on. Raw field values are used in memory to
	// compute digests and are never stored or logged.
	fieldHashes, hash := hashFields(s.d.HashSecret, docType, res.Fields)
	idHash = hash
	reference = docReferenceFor(tenantID, subject, docType, idHash)

	// K22 document binding (P8), POST-extraction PRE-commit: the first
	// committed (doc_type, id_value_hash) row binds the document number to
	// that subject_phone; a match under a DIFFERENT phone => 403. Skipped
	// when extraction found no document number (nothing to bind).
	if idHash != "" {
		owner, err := s.d.Store.DocumentIDHashOwner(r.Context(), tenantID, docType, idHash)
		if err != nil {
			s.internal(w, err)
			return
		}
		if owner != "" && owner != subject {
			s.d.Logger.Warn("kyc document id binding violation",
				zap.String("tenant_id", tenantID.String()), zap.String("doc_type", docType))
			if aerr := audit("failed", "id_binding_violation"); aerr != nil {
				s.internal(w, aerr)
				return
			}
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error":  "id_binding_violation",
				"detail": "this document is not associated with the consented subject_phone",
			})
			return
		}
	}

	// Best-effort phash corroboration: ocr computed it at upload but does
	// not return it from /v1/ocr/extract, so fetch the metadata row once.
	// Failure degrades to "" (D9 fraud signals run on the ocr row
	// regardless) — never fail the upload over a corroboration read.
	phash := ""
	if info, gerr := s.d.OCR.GetDocument(r.Context(), tenantID, res.DocumentID); gerr != nil {
		s.d.Logger.Warn("ocr metadata fetch failed; phash left empty",
			zap.String("tenant_id", tenantID.String()), zap.Error(gerr))
	} else if info != nil {
		phash = info.PHash
		if info.SHA256 != "" && info.SHA256 != documentSHA256(data) {
			// Defense in depth: the ocr row must describe the same bytes.
			s.d.Logger.Error("ocr sha256 mismatch with uploaded bytes",
				zap.String("tenant_id", tenantID.String()))
			if aerr := audit("failed", "ocr_sha256_mismatch"); aerr != nil {
				s.internal(w, aerr)
				return
			}
			writeError(w, http.StatusBadGateway, "ocr extraction corroboration failed")
			return
		}
	}

	doc := &store.Document{
		TenantID:      tenantID,
		SubjectPhone:  subject,
		DocType:       docType,
		OCRDocumentID: res.DocumentID,
		SHA256:        documentSHA256(data),
		PHash:         phash,
		IDValueHash:   idHash,
		FieldHashes:   fieldHashes,
		Status:        res.Status,
	}
	if err := s.d.Store.InsertDocument(r.Context(), doc); err != nil {
		s.internal(w, err)
		return
	}
	if err := audit(res.Status, ""); err != nil {
		s.internal(w, err)
		return
	}

	writeJSON(w, http.StatusOK, documentResponse{
		DocumentID:      doc.DocumentID,
		Reference:       reference,
		Status:          doc.Status,
		ReviewState:     doc.ReviewState,
		FieldsAvailable: len(res.Fields) > 0,
	})
}

// getDocument (GET /v1/kyc/documents/{id}?tenant_id=<uuid>) — SPEC-W58
// B2: the hash-only row plus fields proxied from ocr-service. Fields are
// TTL-bound upstream: when the ocr row is gone (sweeper / erasure) or
// blanked, the response says fields_available:false honestly. Every view
// emits a view-audit log line (recon R8: actor + document_id + tenant).
// tenant_id must be the canonical uuid on this hop (no identity lookup in
// kyc-service; the booking proxy knows the uuid).
func (s *server) getDocument(w http.ResponseWriter, r *http.Request) {
	documentID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid document id")
		return
	}
	tenantRef := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	if tenantRef == "" {
		tenantRef = strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
	}
	tenantID, err := uuid.Parse(tenantRef)
	if err != nil {
		writeError(w, http.StatusBadRequest, "tenant_id (uuid) is required as query param or X-Tenant-ID header")
		return
	}

	doc, err := s.d.Store.GetDocument(r.Context(), tenantID, documentID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if doc == nil {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}

	actor := strings.TrimSpace(r.Header.Get("X-Actor"))
	// View audit (R8) — structlog line minimum; hashes/ids only.
	s.d.Logger.Info("kyc document viewed",
		zap.String("actor", actor),
		zap.String("tenant_id", tenantID.String()),
		zap.String("document_id", documentID.String()),
		zap.String("doc_type", doc.DocType))

	out := documentDetailResponse{
		documentResponse: documentResponse{
			DocumentID:  doc.DocumentID,
			Reference:   docReferenceFor(doc.TenantID, doc.SubjectPhone, doc.DocType, doc.IDValueHash),
			Status:      doc.Status,
			ReviewState: doc.ReviewState,
		},
		TenantID:      doc.TenantID,
		SubjectPhone:  doc.SubjectPhone,
		DocType:       doc.DocType,
		OCRDocumentID: doc.OCRDocumentID,
		SHA256:        doc.SHA256,
		PHash:         doc.PHash,
		IDValueHash:   doc.IDValueHash,
		FieldHashes:   doc.FieldHashes,
		ReviewerActor: doc.ReviewerActor,
		ReviewedAt:    doc.ReviewedAt,
		CreatedAt:     doc.CreatedAt,
	}

	// Proxy the TTL-bound cleartext fields from ocr-service (the review
	// surface; P5 access scoping happens at the booking owner/admin gate).
	switch {
	case s.d.OCR == nil || !s.d.OCR.Configured():
		s.d.Logger.Error("KYC_OCR_BASE_URL/OCR_INTERNAL_TOKEN unset — cannot proxy document fields")
		writeError(w, http.StatusServiceUnavailable, "ocr upstream not configured")
		return
	default:
		info, err := s.d.OCR.GetDocument(r.Context(), doc.TenantID, doc.OCRDocumentID)
		if err != nil {
			// Honest failure: upstream DOWN is distinct from fields EXPIRED.
			s.d.Logger.Warn("ocr fields proxy failed",
				zap.String("tenant_id", tenantID.String()), zap.String("document_id", documentID.String()), zap.Error(err))
			writeError(w, http.StatusBadGateway, "ocr fields unavailable")
			return
		}
		if info != nil && info.Status != "erased" && len(info.Fields) > 0 {
			out.Fields = info.Fields
			out.FieldsAvailable = true
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// reviewRequest is the POST /v1/kyc/documents/{id}/review body.
type reviewRequest struct {
	TenantID string `json:"tenant_id"` // canonical uuid
	Decision string `json:"decision"`  // approved|rejected
	Reason   string `json:"reason"`
}

// reviewDocument (POST /v1/kyc/documents/{id}/review) — SPEC-W58 B2/B1:
// the ONLY mutation kyc_documents allows (review_state + reviewer_actor +
// reviewed_at; DB-trigger-enforced), plus exactly one audit row carrying
// the decision and reason. Human review is the terminal state (P7) — OCR
// never auto-verifies. Re-review (correction) is allowed and audited.
func (s *server) reviewDocument(w http.ResponseWriter, r *http.Request) {
	documentID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid document id")
		return
	}
	var req reviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Decision = strings.ToLower(strings.TrimSpace(req.Decision))
	tenantID, err := uuid.Parse(strings.TrimSpace(req.TenantID))
	if err != nil {
		writeError(w, http.StatusBadRequest, "tenant_id (uuid) is required")
		return
	}
	if req.Decision != ReviewApproved && req.Decision != ReviewRejected {
		writeError(w, http.StatusBadRequest, "decision must be approved|rejected")
		return
	}
	actor := strings.TrimSpace(r.Header.Get("X-Actor"))

	doc, err := s.d.Store.ReviewDocument(r.Context(), tenantID, documentID, req.Decision, actor)
	if err != nil {
		s.internal(w, err)
		return
	}
	if doc == nil {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}

	reference := docReferenceFor(doc.TenantID, doc.SubjectPhone, doc.DocType, doc.IDValueHash)
	if err := s.d.Store.InsertAudit(r.Context(), &store.Audit{
		TenantID:     doc.TenantID,
		Actor:        actor,
		SubjectPhone: doc.SubjectPhone,
		IDType:       doc.DocType,
		IDValueHash:  doc.IDValueHash,
		Status:       req.Decision,
		Reference:    reference,
		Detail:       strings.TrimSpace(req.Reason),
	}); err != nil {
		s.internal(w, err)
		return
	}

	writeJSON(w, http.StatusOK, documentResponse{
		DocumentID:  doc.DocumentID,
		Reference:   reference,
		Status:      doc.Status,
		ReviewState: doc.ReviewState,
	})
}
