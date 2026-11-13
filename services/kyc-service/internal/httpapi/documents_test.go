package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opendesk/kyc-service/internal/store"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// Fakes (SPEC-W58 B5)
// ---------------------------------------------------------------------------

// fakeDocStore is the full in-memory Repository for the document tests:
// kyc_documents rows plus the append-only audit trail.
type fakeDocStore struct {
	mu     sync.Mutex
	docs   []*store.Document
	audits []store.Audit

	docErr   error
	auditErr error
}

func (f *fakeDocStore) InsertAudit(_ context.Context, a *store.Audit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.auditErr != nil {
		return f.auditErr
	}
	if a.AuditID == uuid.Nil {
		a.AuditID = uuid.New()
	}
	f.audits = append(f.audits, *a)
	return nil
}

func (f *fakeDocStore) IDHashOwner(context.Context, uuid.UUID, string, string) (string, error) {
	return "", nil
}

func (f *fakeDocStore) InsertDocument(_ context.Context, d *store.Document) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.docErr != nil {
		return f.docErr
	}
	if d.DocumentID == uuid.Nil {
		d.DocumentID = uuid.New()
	}
	d.ReviewState = "pending_review"
	d.CreatedAt = time.Now()
	cp := *d
	f.docs = append(f.docs, &cp)
	*d = cp
	return nil
}

// DocumentIDHashOwner mirrors the store contract: the subject_phone of the
// FIRST committed (doc_type, id_value_hash) row for the tenant.
func (f *fakeDocStore) DocumentIDHashOwner(_ context.Context, tenantID uuid.UUID, docType, idValueHash string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.docs {
		if d.TenantID == tenantID && d.DocType == docType && d.IDValueHash == idValueHash {
			return d.SubjectPhone, nil
		}
	}
	return "", nil
}

func (f *fakeDocStore) GetDocument(_ context.Context, tenantID, documentID uuid.UUID) (*store.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.docs {
		if d.TenantID == tenantID && d.DocumentID == documentID {
			cp := *d
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeDocStore) ReviewDocument(_ context.Context, tenantID, documentID uuid.UUID, decision, actor string) (*store.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.docs {
		if d.TenantID == tenantID && d.DocumentID == documentID {
			now := time.Now()
			d.ReviewState = decision
			d.ReviewerActor = actor
			d.ReviewedAt = &now
			cp := *d
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeDocStore) Ping(context.Context) error { return nil }

// fakeOCR stubs the ocr-service extract + document GET.
type fakeOCR struct {
	mu          sync.Mutex
	fields      map[string]any
	status      string
	extractErr  error
	extractCall int
	gotKind     string
	gotMime     string
	gotTenant   uuid.UUID
	gotBytes    []byte

	getStatus   string // "" => "extracted"; "erased" simulates the erasure blank
	getNotFound bool   // simulates the TTL sweep / TenantDeleted delete (404)
	getErr      error
	getCalls    int
	corruptSHA  bool // return a sha256 that does NOT match the uploaded bytes
}

func (f *fakeOCR) Configured() bool { return true }

func (f *fakeOCR) Extract(_ context.Context, tenantID uuid.UUID, kind, mime string, data []byte) (*OCRExtractResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extractCall++
	f.gotKind, f.gotMime, f.gotTenant = kind, mime, tenantID
	f.gotBytes = append([]byte(nil), data...)
	if f.extractErr != nil {
		return nil, f.extractErr
	}
	status := f.status
	if status == "" {
		status = "extracted"
	}
	fields := map[string]any{}
	for k, v := range f.fields {
		fields[k] = v
	}
	return &OCRExtractResult{DocumentID: uuid.New(), Status: status, Fields: fields, PageCount: 1}, nil
}

func (f *fakeOCR) GetDocument(context.Context, uuid.UUID, uuid.UUID) (*OCRDocumentInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getNotFound {
		return nil, nil
	}
	status := f.getStatus
	if status == "" {
		status = "extracted"
	}
	fields := map[string]any{}
	for k, v := range f.fields {
		fields[k] = v
	}
	info := &OCRDocumentInfo{
		DocumentID: uuid.New(),
		Kind:       f.gotKind,
		Status:     status,
		PHash:      "0123456789abcdef",
		Fields:     fields,
	}
	if len(f.gotBytes) > 0 {
		info.SHA256 = documentSHA256(f.gotBytes) // corroborates the upload
	}
	if f.corruptSHA {
		info.SHA256 = strings.Repeat("00", 32)
	}
	return info, nil
}

// canaryNIN is the SPEC-W58 B5 canary: a raw document number that must
// NEVER appear in kyc_documents rows, kyc_audit rows, or POST/review
// responses (it may transit the review GET passthrough only).
const canaryNIN = "12345678901"
const canaryName = "Canary Adaeze Okafor"

var canaryFields = map[string]any{
	"full_name": canaryName,
	"dob":       "1990-01-01",
	"nin":       canaryNIN,
	"address":   "1 Canary Close, Lagos",
}

func pngBytes() []byte {
	return append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte{0x01}, 64)...)
}

type docHarness struct {
	store   *fakeDocStore
	ocr     *fakeOCR
	consent *fakeConsent
	tid     uuid.UUID
	http    http.Handler
}

func newDocHarness(consentErr error) *docHarness {
	ds := &fakeDocStore{}
	ocr := &fakeOCR{fields: canaryFields}
	consent := &fakeConsent{tenantID: uuid.New(), err: consentErr}
	d := Deps{
		Store:         ds,
		Consent:       consent,
		Resolver:      MockResolver{},
		Events:        &fakePublisher{},
		PubSub:        "pubsub-kafka",
		EventsTopic:   "opendesk.kyc.resolved.v1",
		InternalToken: testInternalToken,
		HashSecret:    testHashSecret,
		OCR:           ocr,
		Logger:        zap.NewNop(),
	}
	r := chi.NewRouter()
	r.Mount("/", NewRouter(d))
	return &docHarness{store: ds, ocr: ocr, consent: consent, tid: consent.tenantID, http: r}
}

// upload builds + sends one multipart POST /v1/kyc/documents.
func (h *docHarness) upload(t *testing.T, subject, docType, consentID, mime string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("tenant_id", h.tid.String())
	_ = mw.WriteField("subject_phone", subject)
	_ = mw.WriteField("doc_type", docType)
	if consentID != "" {
		_ = mw.WriteField("consent_id", consentID)
	}
	if data != nil {
		fw, err := mw.CreatePart(textProtoHeader(mime))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/kyc/documents", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Internal-Token", testInternalToken)
	req.Header.Set("X-Actor", "booking-service-test")
	rec := httptest.NewRecorder()
	h.http.ServeHTTP(rec, req)
	return rec
}

func textProtoHeader(mime string) textproto.MIMEHeader {
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="doc"`)
	h.Set("Content-Type", mime)
	return h
}

// serializedStore renders every stored row (documents + audits) as JSON so
// the canary test can grep the FULL durable record for raw PII.
func (h *docHarness) serializedStore(t *testing.T) string {
	t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	b, err := json.Marshal(map[string]any{"docs": h.store.docs, "audits": h.store.audits})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Happy path + hash-only canary (SPEC-W58 B5)
// ---------------------------------------------------------------------------

func TestDocumentUploadHappyPath(t *testing.T) {
	h := newDocHarness(nil)
	rec := h.upload(t, "+2348012345678", "kyc_nin_slip", "consent-123", "image/png", pngBytes())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var out documentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.DocumentID == uuid.Nil || out.Status != "extracted" || out.ReviewState != "pending_review" {
		t.Errorf("response: %+v", out)
	}
	if !strings.HasPrefix(out.Reference, "kyc_doc_") {
		t.Errorf("reference = %q", out.Reference)
	}

	// Consent gate ran with the pinned purpose (P6) and no id_hash
	// (unknown until extraction — the binding is post-extraction).
	if h.consent.gotPurpose != PurposeKYCDocument {
		t.Errorf("consent purpose = %q, want kyc_document", h.consent.gotPurpose)
	}
	if h.consent.gotIDHash != "" {
		t.Errorf("consent id_hash must be empty pre-extraction, got %q", h.consent.gotIDHash)
	}

	// ocr got the bytes unchanged, kind = doc_type verbatim (contract).
	if h.ocr.extractCall != 1 || h.ocr.gotKind != "kyc_nin_slip" || h.ocr.gotMime != "image/png" {
		t.Errorf("ocr extract: calls=%d kind=%q mime=%q", h.ocr.extractCall, h.ocr.gotKind, h.ocr.gotMime)
	}
	if h.ocr.gotTenant != h.tid || !bytes.Equal(h.ocr.gotBytes, pngBytes()) {
		t.Errorf("ocr extract tenant/bytes mismatch")
	}

	// Exactly one document row + exactly one audit row.
	if len(h.store.docs) != 1 {
		t.Fatalf("documents = %d, want 1", len(h.store.docs))
	}
	if len(h.store.audits) != 1 {
		t.Fatalf("audits = %d, want 1 (audit-once)", len(h.store.audits))
	}
	doc := h.store.docs[0]
	if doc.SubjectPhone != "+2348012345678" || doc.DocType != "kyc_nin_slip" || doc.ReviewState != "pending_review" {
		t.Errorf("document row: %+v", doc)
	}
	if doc.SHA256 != documentSHA256(pngBytes()) {
		t.Errorf("sha256 = %q, want digest of uploaded bytes", doc.SHA256)
	}
	if doc.PHash != "0123456789abcdef" {
		t.Errorf("phash = %q, want the ocr metadata corroboration", doc.PHash)
	}
	if doc.IDValueHash != hashIDValue(testHashSecret, canaryNIN) {
		t.Errorf("id_value_hash = %q, want HMAC-SHA256(secret, nin)", doc.IDValueHash)
	}
	if len(doc.FieldHashes) != len(canaryFields) {
		t.Errorf("field_hashes = %v, want one digest per extracted field", doc.FieldHashes)
	}
	a := h.store.audits[0]
	if a.IDType != "kyc_nin_slip" || a.Status != "extracted" || a.ConsentID != "consent-123" || a.Reference != out.Reference {
		t.Errorf("audit row: %+v", a)
	}

	// CANARY (B5): raw NIN / name / address NEVER appear in the durable
	// record or the POST response.
	dump := h.serializedStore(t)
	for _, canary := range []string{canaryNIN, canaryName, "Canary Close"} {
		if strings.Contains(dump, canary) {
			t.Errorf("canary %q leaked into the durable record: %s", canary, dump)
		}
		if strings.Contains(rec.Body.String(), canary) {
			t.Errorf("canary %q leaked into the POST response: %s", canary, rec.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// Consent gate (P6)
// ---------------------------------------------------------------------------

func TestDocumentUploadConsentDenied403(t *testing.T) {
	h := newDocHarness(ErrConsentDenied)
	rec := h.upload(t, "+2348012345678", "kyc_nin_slip", "", "image/png", pngBytes())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["error"] != "consent_required" {
		t.Errorf("body: %v", out)
	}
	if h.ocr.extractCall != 0 || len(h.store.docs) != 0 || len(h.store.audits) != 0 {
		t.Errorf("denied requests must not extract/store/audit: ocr=%d docs=%d audits=%d",
			h.ocr.extractCall, len(h.store.docs), len(h.store.audits))
	}
}

func TestDocumentUploadConsentGateDown502(t *testing.T) {
	h := newDocHarness(errors.New("identity unreachable"))
	rec := h.upload(t, "+2348012345678", "kyc_nin_slip", "", "image/png", pngBytes())
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if h.ocr.extractCall != 0 {
		t.Errorf("gate-down must not reach ocr: calls=%d", h.ocr.extractCall)
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestDocumentUploadValidation(t *testing.T) {
	h := newDocHarness(nil)
	cases := []struct {
		name                   string
		subject, docType, mime string
		data                   []byte
		want                   int
	}{
		{"bad doc_type", "+2348012345678", "passport", "image/png", pngBytes(), http.StatusBadRequest},
		{"phone not e164", "08012345678", "kyc_nin_slip", "image/png", pngBytes(), http.StatusBadRequest},
		{"phone missing plus", "2348012345678", "kyc_nin_slip", "image/png", pngBytes(), http.StatusBadRequest},
		{"missing file", "+2348012345678", "kyc_nin_slip", "image/png", nil, http.StatusBadRequest},
		{"mime not allowed", "+2348012345678", "kyc_nin_slip", "text/plain", []byte("hello"), http.StatusUnsupportedMediaType},
		{"magic mismatch", "+2348012345678", "kyc_nin_slip", "image/png", []byte("not a png at all..........."), http.StatusUnsupportedMediaType},
	}
	for _, tc := range cases {
		if rec := h.upload(t, tc.subject, tc.docType, "", tc.mime, tc.data); rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (body %s)", tc.name, rec.Code, tc.want, rec.Body)
		}
	}
	if h.ocr.extractCall != 0 || len(h.store.docs) != 0 || len(h.store.audits) != 0 {
		t.Errorf("invalid requests must not extract/store/audit")
	}
}

func TestDocumentUploadOverSize413(t *testing.T) {
	h := newDocHarness(nil)
	big := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, make([]byte, MaxDocumentBytes)...)
	rec := h.upload(t, "+2348012345678", "kyc_nin_slip", "", "image/png", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if h.ocr.extractCall != 0 {
		t.Errorf("oversize must not reach ocr")
	}
}

func TestDocumentUploadInternauth(t *testing.T) {
	h := newDocHarness(nil)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("tenant_id", h.tid.String())
	_ = mw.WriteField("subject_phone", "+2348012345678")
	_ = mw.WriteField("doc_type", "kyc_nin_slip")
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/kyc/documents", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// NO X-Internal-Token.
	rec := httptest.NewRecorder()
	h.http.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing token: status = %d, want 401", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// OCR failure + fail-closed config
// ---------------------------------------------------------------------------

func TestDocumentUploadOCRDown502(t *testing.T) {
	h := newDocHarness(nil)
	h.ocr.extractErr = errors.New("ocr sidecar down")
	rec := h.upload(t, "+2348012345678", "kyc_nin_slip", "", "image/png", pngBytes())
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if len(h.store.docs) != 0 {
		t.Errorf("failed extraction must not commit a row: docs=%d", len(h.store.docs))
	}
	// Every action audits — the failed attempt too (recon R8).
	if len(h.store.audits) != 1 || h.store.audits[0].Status != "failed" || h.store.audits[0].Detail != "ocr_unavailable" {
		t.Errorf("audits = %+v, want one failed/ocr_unavailable row", h.store.audits)
	}
}

func TestDocumentUploadOCRNotConfigured503(t *testing.T) {
	h := newDocHarness(nil)
	h.http = NewRouter(Deps{
		Store: h.store, Consent: h.consent, Resolver: MockResolver{},
		Events: &fakePublisher{}, PubSub: "p", EventsTopic: "t",
		InternalToken: testInternalToken, HashSecret: testHashSecret,
		OCR:    NewOCRClient("", "", nil), // KYC_OCR_BASE_URL unset
		Logger: zap.NewNop(),
	})
	rec := h.upload(t, "+2348012345678", "kyc_nin_slip", "", "image/png", pngBytes())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (fail-closed)", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// K22 document binding (P8): (doc_type, id_value_hash) -> first phone wins
// ---------------------------------------------------------------------------

func TestDocumentBindingRule(t *testing.T) {
	h := newDocHarness(nil)
	// First upload binds the canary NIN to phone A.
	if rec := h.upload(t, "+2348011111111", "kyc_nin_slip", "", "image/png", pngBytes()); rec.Code != http.StatusOK {
		t.Fatalf("first upload: status = %d, body %s", rec.Code, rec.Body)
	}
	// Same document under the SAME phone -> allowed (idempotent re-upload).
	if rec := h.upload(t, "+2348011111111", "kyc_nin_slip", "", "image/png", pngBytes()); rec.Code != http.StatusOK {
		t.Errorf("same-phone re-upload: status = %d, want 200", rec.Code)
	}
	// Same document under a DIFFERENT phone -> 403 id_binding_violation.
	rec := h.upload(t, "+2348099999999", "kyc_nin_slip", "", "image/png", pngBytes())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-phone upload: status = %d, want 403", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["error"] != "id_binding_violation" {
		t.Errorf("error = %v, want id_binding_violation", out)
	}
	if len(h.store.docs) != 2 {
		t.Errorf("docs = %d, want 2 (the violation must not commit)", len(h.store.docs))
	}
	// The violation IS audited (the extraction already happened — R8).
	last := h.store.audits[len(h.store.audits)-1]
	if last.Status != "failed" || last.Detail != "id_binding_violation" {
		t.Errorf("violation audit row: %+v", last)
	}
	// A different doc_type under the other phone is a different binding
	// domain -> allowed even with the same document number.
	h.ocr.fields = map[string]any{"full_name": canaryName, "license_no": canaryNIN}
	if rec := h.upload(t, "+2348099999999", "kyc_drivers_license", "", "image/png", pngBytes()); rec.Code != http.StatusOK {
		t.Errorf("different doc_type under other phone: status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
}

// OCR output without the document-number field: no id_value_hash, binding
// skipped, row still commits (honest pre-fill).
func TestDocumentUploadNoIDNumberExtracted(t *testing.T) {
	h := newDocHarness(nil)
	h.ocr.fields = map[string]any{"full_name": canaryName, "dob": nil}
	rec := h.upload(t, "+2348012345678", "kyc_nin_slip", "", "image/png", pngBytes())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	doc := h.store.docs[0]
	if doc.IDValueHash != "" {
		t.Errorf("id_value_hash = %q, want empty (no nin extracted)", doc.IDValueHash)
	}
	if _, ok := doc.FieldHashes["dob"]; ok {
		t.Errorf("null fields must not be hashed: %v", doc.FieldHashes)
	}
}

// Defense in depth: the ocr metadata sha256 must corroborate the uploaded
// bytes — a mismatch refuses the commit (audited).
func TestDocumentUploadSHA256Mismatch502(t *testing.T) {
	h := newDocHarness(nil)
	// Corrupt the fake's copy so its corroboration digest differs.
	rec := h.upload(t, "+2348012345678", "kyc_nin_slip", "", "image/png", pngBytes())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	// Now force a corroboration mismatch on a fresh doc_type (no binding).
	h.ocr.corruptSHA = true
	rec = h.upload(t, "+2348012345678", "kyc_passport", "", "image/png", pngBytes())
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("mismatch: status = %d, want 502", rec.Code)
	}
	last := h.store.audits[len(h.store.audits)-1]
	if last.Detail != "ocr_sha256_mismatch" {
		t.Errorf("audit detail = %q, want ocr_sha256_mismatch", last.Detail)
	}
}

// ---------------------------------------------------------------------------
// GET /v1/kyc/documents/{id} (fields proxied from ocr under TTL)
// ---------------------------------------------------------------------------

func (h *docHarness) mustUpload(t *testing.T) uuid.UUID {
	t.Helper()
	rec := h.upload(t, "+2348012345678", "kyc_nin_slip", "", "image/png", pngBytes())
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: status = %d, body %s", rec.Code, rec.Body)
	}
	var out documentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.DocumentID
}

func (h *docHarness) get(t *testing.T, id uuid.UUID, tenantQuery string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/kyc/documents/"+id.String()+"?tenant_id="+tenantQuery, nil)
	req.Header.Set("X-Internal-Token", testInternalToken)
	req.Header.Set("X-Actor", "reviewer-1")
	rec := httptest.NewRecorder()
	h.http.ServeHTTP(rec, req)
	return rec
}

func TestDocumentGetWithFields(t *testing.T) {
	h := newDocHarness(nil)
	id := h.mustUpload(t)
	rec := h.get(t, id, h.tid.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var out documentDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.FieldsAvailable || out.Fields["nin"] != canaryNIN {
		t.Errorf("fields passthrough (TTL window): %+v", out.documentResponse)
	}
	if out.ReviewState != "pending_review" || out.DocType != "kyc_nin_slip" || out.SubjectPhone != "+2348012345678" {
		t.Errorf("row projection: %+v", out)
	}
	if out.IDValueHash != hashIDValue(testHashSecret, canaryNIN) {
		t.Errorf("id_value_hash = %q", out.IDValueHash)
	}
}

func TestDocumentGetAfterTTLFieldsGone(t *testing.T) {
	h := newDocHarness(nil)
	id := h.mustUpload(t)
	h.ocr.getNotFound = true // the sweeper deleted the ocr row (P4)
	rec := h.get(t, id, h.tid.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["fields_available"] != false {
		t.Errorf("fields_available = %v, want false post-TTL", out["fields_available"])
	}
	if _, present := out["fields"]; present {
		t.Errorf("expired fields must not be served: %v", out["fields"])
	}
	// The hash-only durable record survives — by design (P8).
	if out["id_value_hash"] == "" || out["sha256"] == "" {
		t.Errorf("hash-only record must survive TTL: %v", out)
	}
	// Canary never in the response once fields are gone.
	if strings.Contains(rec.Body.String(), canaryNIN) {
		t.Errorf("canary leaked: %s", rec.Body.String())
	}
}

func TestDocumentGetErasedFieldsGone(t *testing.T) {
	h := newDocHarness(nil)
	id := h.mustUpload(t)
	h.ocr.getStatus = "erased" // consent erasure blanked the row (P3)
	h.ocr.fields = map[string]any{}
	rec := h.get(t, id, h.tid.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["fields_available"] != false {
		t.Errorf("fields_available = %v, want false post-erasure", out["fields_available"])
	}
}

func TestDocumentGetOCRDown502(t *testing.T) {
	h := newDocHarness(nil)
	id := h.mustUpload(t)
	h.ocr.getErr = errors.New("ocr down")
	rec := h.get(t, id, h.tid.String())
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (upstream DOWN is not fields EXPIRED)", rec.Code)
	}
}

func TestDocumentGetNotFoundAndValidation(t *testing.T) {
	h := newDocHarness(nil)
	if rec := h.get(t, uuid.New(), h.tid.String()); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id: status = %d, want 404", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/kyc/documents/not-a-uuid?tenant_id="+h.tid.String(), nil)
	req.Header.Set("X-Internal-Token", testInternalToken)
	rec := httptest.NewRecorder()
	h.http.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad id: status = %d, want 400", rec.Code)
	}
	if rec := h.get(t, uuid.New(), ""); rec.Code != http.StatusBadRequest {
		t.Errorf("missing tenant: status = %d, want 400", rec.Code)
	}
}

// Cross-tenant: a document is invisible outside its tenant (RLS enforced
// at the DB layer; the store contract is tenant-scoped by construction).
func TestDocumentGetCrossTenant404(t *testing.T) {
	h := newDocHarness(nil)
	id := h.mustUpload(t)
	if rec := h.get(t, id, uuid.New().String()); rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant get: status = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// POST /v1/kyc/documents/{id}/review (P7: human review terminal)
// ---------------------------------------------------------------------------

func (h *docHarness) review(t *testing.T, id uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/kyc/documents/"+id.String()+"/review", strings.NewReader(body))
	req.Header.Set("X-Internal-Token", testInternalToken)
	req.Header.Set("X-Actor", "reviewer-1")
	rec := httptest.NewRecorder()
	h.http.ServeHTTP(rec, req)
	return rec
}

func TestDocumentReviewTransition(t *testing.T) {
	h := newDocHarness(nil)
	id := h.mustUpload(t)
	auditsBefore := len(h.store.audits)

	rec := h.review(t, id, fmt.Sprintf(`{"tenant_id":%q,"decision":"approved","reason":"matches the physical card"}`, h.tid.String()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var out documentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.ReviewState != ReviewApproved || out.DocumentID != id {
		t.Errorf("response: %+v", out)
	}
	doc := h.store.docs[0]
	if doc.ReviewState != "approved" || doc.ReviewerActor != "reviewer-1" || doc.ReviewedAt == nil {
		t.Errorf("row after review: %+v", doc)
	}
	// Exactly one audit row for the review, carrying the reason (never
	// raw PII).
	if len(h.store.audits) != auditsBefore+1 {
		t.Fatalf("audits = %d, want %d (review must audit)", len(h.store.audits), auditsBefore+1)
	}
	a := h.store.audits[len(h.store.audits)-1]
	if a.Status != "approved" || a.Detail != "matches the physical card" || a.Actor != "reviewer-1" ||
		a.IDType != "kyc_nin_slip" || a.IDValueHash != doc.IDValueHash {
		t.Errorf("review audit row: %+v", a)
	}

	// Correction path: re-review to rejected is allowed and audited.
	rec = h.review(t, id, fmt.Sprintf(`{"tenant_id":%q,"decision":"rejected","reason":"blurry"}`, h.tid.String()))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-review: status = %d, body %s", rec.Code, rec.Body)
	}
	if h.store.docs[0].ReviewState != "rejected" {
		t.Errorf("re-review state = %q", h.store.docs[0].ReviewState)
	}

	// Canary: review responses never carry raw fields.
	if strings.Contains(rec.Body.String(), canaryNIN) || strings.Contains(h.serializedStore(t), canaryNIN) {
		t.Errorf("canary leaked through the review path")
	}
}

func TestDocumentReviewValidation(t *testing.T) {
	h := newDocHarness(nil)
	id := h.mustUpload(t)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"bad decision", fmt.Sprintf(`{"tenant_id":%q,"decision":"maybe"}`, h.tid.String()), http.StatusBadRequest},
		{"missing tenant", `{"decision":"approved"}`, http.StatusBadRequest},
		{"bad json", `{`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		if rec := h.review(t, id, tc.body); rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
	if rec := h.review(t, uuid.New(), fmt.Sprintf(`{"tenant_id":%q,"decision":"approved"}`, h.tid.String())); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id: status = %d, want 404", rec.Code)
	}
	// Nothing reviewed, nothing audited beyond the upload.
	if len(h.store.audits) != 1 {
		t.Errorf("audits = %d, want 1 (upload only)", len(h.store.audits))
	}
	if h.store.docs[0].ReviewState != "pending_review" {
		t.Errorf("failed reviews must not mutate: %q", h.store.docs[0].ReviewState)
	}
}

// ---------------------------------------------------------------------------
// OCR client contract test against a real HTTP stub (ocr-service shape)
// ---------------------------------------------------------------------------

func TestOCRClientAgainstStub(t *testing.T) {
	tid := uuid.New()
	docID := uuid.New()
	var gotToken, gotSlug, gotKind, gotTenantForm string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Internal-Token")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/ocr/extract":
			gotTenantForm = r.FormValue("tenant_id")
			gotKind = r.FormValue("kind")
			if r.Header.Get("X-Tenant-Slug") != tid.String() {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"document_id":%q,"status":"extracted","extracted_fields":{"nin":%q},"page_count":1,"extracted_text":"RAW TEXT"}`, docID, canaryNIN)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/ocr/documents/"):
			if strings.HasSuffix(r.URL.Path, uuid.Nil.String()) {
				// post-TTL sweep / erasure: the ocr row is gone.
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"document_id":%q,"kind":"kyc_nin_slip","status":"extracted","sha256":"ab","phash":"cd","extracted_fields":{"nin":%q}}`, docID, canaryNIN)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer stub.Close()

	c := NewOCRClient(stub.URL, "tok", nil)
	if !c.Configured() {
		t.Fatal("configured client expected")
	}
	res, err := c.Extract(context.Background(), tid, "kyc_nin_slip", "image/png", pngBytes())
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if res.DocumentID != docID || res.Status != "extracted" || res.Fields["nin"] != canaryNIN {
		t.Errorf("extract result: %+v", res)
	}
	if gotToken != "tok" || gotKind != "kyc_nin_slip" || gotTenantForm != tid.String() || gotSlug != "" {
		t.Errorf("contract: token=%q kind=%q tenant_form=%q", gotToken, gotKind, gotTenantForm)
	}

	info, err := c.GetDocument(context.Background(), tid, docID)
	if err != nil || info == nil || info.Fields["nin"] != canaryNIN {
		t.Errorf("get: %+v err %v", info, err)
	}
	// 404 (post-TTL) -> nil, nil.
	gone, err := c.GetDocument(context.Background(), tid, uuid.Nil)
	if err != nil || gone != nil {
		t.Errorf("post-TTL get: info=%+v err=%v, want nil/nil", gone, err)
	}
	// unconfigured client fails closed
	nc := NewOCRClient("", "", nil)
	if _, err := nc.Extract(context.Background(), tid, "kyc_nin_slip", "image/png", pngBytes()); !errors.Is(err, errOCRNotConfigured) {
		t.Errorf("unconfigured extract: %v, want errOCRNotConfigured", err)
	}
}
