package kycclient

// SPEC-W58 Scope C item C4: kycclient unit tests — contract shape of the
// booking -> kyc-service passthrough (headers, multipart fields, review
// body), fail-closed configuration posture, mime/doc-type pre-checks and
// the ErrUnavailable mapping. All network is httptest-local.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x01}

func TestValidateMime(t *testing.T) {
	cases := []struct {
		name    string
		mime    string
		data    []byte
		wantErr bool
	}{
		{"png ok", "image/png", pngBytes, false},
		{"jpeg ok", "image/jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0}, false},
		{"pdf ok", "application/pdf", []byte("%PDF-1.7 fake"), false},
		{"webp ok", "image/webp", []byte("RIFFxxxxWEBP"), false},
		{"text rejected", "text/plain", []byte("hello"), true},
		{"empty rejected", "image/png", nil, true},
		{"magic mismatch", "image/png", []byte("%PDF-1.7"), true},
		{"mime with params", "image/png; charset=binary", pngBytes, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMime(tc.mime, tc.data)
			if tc.wantErr && !errors.Is(err, ErrMedia) {
				t.Fatalf("want ErrMedia, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}

func TestIsDocType(t *testing.T) {
	for _, d := range []string{"kyc_nin_slip", "kyc_drivers_license", "kyc_passport", "kyc_voters_card", "kyc_cac_document"} {
		if !IsDocType(d) {
			t.Fatalf("pinned P9 kind %q must be valid", d)
		}
	}
	for _, d := range []string{"", "receipt", "kyc_unknown", "nin"} {
		if IsDocType(d) {
			t.Fatalf("%q must be rejected", d)
		}
	}
}

// capturedUpload records what the fake kyc-service received.
type capturedUpload struct {
	token, tenantHdr string
	fields           map[string]string
	fileBytes        []byte
	fileMime         string
}

func fakeKYCUpload(t *testing.T, status int, respBody string, cap *capturedUpload) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/kyc/documents" || r.Method != http.MethodPost {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		cap.token = r.Header.Get("X-Internal-Token")
		cap.tenantHdr = r.Header.Get("X-Tenant-Slug")
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("multipart reader: %v", err)
			return
		}
		cap.fields = map[string]string{}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("multipart part: %v", err)
				return
			}
			b, _ := io.ReadAll(part)
			if part.FormName() == "file" {
				cap.fileBytes = b
				cap.fileMime = part.Header.Get("Content-Type")
			} else {
				cap.fields[part.FormName()] = string(b)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUploadDocumentContract(t *testing.T) {
	var cap capturedUpload
	srv := fakeKYCUpload(t, http.StatusOK, `{"document_id":"doc-1","reference":"ref-1","status":"extracted","review_state":"pending_review"}`, &cap)
	tenantID := uuid.New()
	c := New(srv.URL, "sekrit", zap.NewNop())

	res, err := c.UploadDocument(context.Background(), tenantID, "image/png", pngBytes,
		"+2348012345678", "kyc_nin_slip", "consent-9")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("status passthrough: got %d", res.Status)
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body, &body); err != nil || body["document_id"] != "doc-1" {
		t.Fatalf("body passthrough: %v %s", err, res.Body)
	}
	// Contract headers + form fields (cross-scope pinned).
	if cap.token != "sekrit" {
		t.Fatalf("X-Internal-Token = %q", cap.token)
	}
	if cap.tenantHdr != tenantID.String() {
		t.Fatalf("X-Tenant-Slug = %q (want tenant uuid)", cap.tenantHdr)
	}
	wantFields := map[string]string{
		"subject_phone": "+2348012345678",
		"doc_type":      "kyc_nin_slip",
		"consent_id":    "consent-9",
		"tenant_id":     tenantID.String(),
	}
	for k, v := range wantFields {
		if cap.fields[k] != v {
			t.Fatalf("form field %q = %q (want %q)", k, cap.fields[k], v)
		}
	}
	if string(cap.fileBytes) != string(pngBytes) || cap.fileMime != "image/png" {
		t.Fatalf("file passthrough broken (mime %q, %d bytes)", cap.fileMime, len(cap.fileBytes))
	}
}

func TestUploadDocumentOmitsEmptyConsentID(t *testing.T) {
	var cap capturedUpload
	srv := fakeKYCUpload(t, http.StatusOK, `{}`, &cap)
	c := New(srv.URL, "sekrit", zap.NewNop())
	if _, err := c.UploadDocument(context.Background(), uuid.New(), "image/png", pngBytes, "+2348012345678", "kyc_passport", ""); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if _, present := cap.fields["consent_id"]; present {
		t.Fatalf("empty consent_id must be omitted, got fields %v", cap.fields)
	}
}

// Downstream domain statuses (403 consent denied, 422, ...) pass through
// verbatim — they are NOT ErrUnavailable.
func TestUploadDocumentPassthroughStatus(t *testing.T) {
	var cap capturedUpload
	srv := fakeKYCUpload(t, http.StatusForbidden, `{"error":"consent denied"}`, &cap)
	c := New(srv.URL, "sekrit", zap.NewNop())
	res, err := c.UploadDocument(context.Background(), uuid.New(), "image/png", pngBytes, "+2348012345678", "kyc_nin_slip", "")
	if err != nil {
		t.Fatalf("downstream 403 must pass through, got err %v", err)
	}
	if res.Status != http.StatusForbidden || !strings.Contains(string(res.Body), "consent denied") {
		t.Fatalf("passthrough broken: %d %s", res.Status, res.Body)
	}
}

func TestUploadDocumentPreChecks(t *testing.T) {
	c := New("http://kyc.invalid", "sekrit", zap.NewNop())
	if _, err := c.UploadDocument(context.Background(), uuid.New(), "text/plain", []byte("hi"), "+2348012345678", "kyc_nin_slip", ""); !errors.Is(err, ErrMedia) {
		t.Fatalf("mime pre-check: %v", err)
	}
	if _, err := c.UploadDocument(context.Background(), uuid.New(), "image/png", pngBytes, "+2348012345678", "receipt", ""); !errors.Is(err, ErrDocType) {
		t.Fatalf("doc_type pre-check: %v", err)
	}
	over := make([]byte, MaxDocumentBytes+1)
	if _, err := c.UploadDocument(context.Background(), uuid.New(), "image/png", over, "+2348012345678", "kyc_nin_slip", ""); !errors.Is(err, ErrMedia) {
		t.Fatalf("cap pre-check: %v", err)
	}
}

func TestGetDocumentContract(t *testing.T) {
	var gotToken, gotTenant, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Internal-Token")
		gotTenant = r.Header.Get("X-Tenant-Slug")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"document_id":"doc-1","fields_available":true,"fields":{"full_name":"Ada"}}`))
	}))
	t.Cleanup(srv.Close)
	tenantID := uuid.New()
	c := New(srv.URL, "sekrit", zap.NewNop())
	res, err := c.GetDocument(context.Background(), tenantID, "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if res.Status != http.StatusOK || !strings.Contains(string(res.Body), "fields_available") {
		t.Fatalf("get passthrough: %d %s", res.Status, res.Body)
	}
	if gotPath != "/v1/kyc/documents/11111111-1111-1111-1111-111111111111" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotToken != "sekrit" || gotTenant != tenantID.String() {
		t.Fatalf("headers token=%q tenant=%q", gotToken, gotTenant)
	}
}

func TestReviewDocumentContract(t *testing.T) {
	var gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"document_id":"doc-1","review_state":"approved"}`))
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "sekrit", zap.NewNop())
	res, err := c.ReviewDocument(context.Background(), uuid.New(), "doc-1", "approved", "fields match the document")
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("review passthrough: %d", res.Status)
	}
	if gotPath != "/v1/kyc/documents/doc-1/review" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["decision"] != "approved" || gotBody["reason"] != "fields match the document" {
		t.Fatalf("review body = %v", gotBody)
	}
}

func TestNotConfiguredFailsClosed(t *testing.T) {
	for _, c := range []*Client{
		New("", "sekrit", nil),   // URL unset
		New("http://kyc", "", nil), // token unset
	} {
		if c.Configured() {
			t.Fatalf("client %s must be unconfigured", c.String())
		}
		if _, err := c.GetDocument(context.Background(), uuid.New(), "x"); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("get: %v", err)
		}
		if _, err := c.ReviewDocument(context.Background(), uuid.New(), "x", "approved", ""); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("review: %v", err)
		}
		if _, err := c.UploadDocument(context.Background(), uuid.New(), "image/png", pngBytes, "+1", "kyc_nin_slip", ""); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("upload: %v", err)
		}
	}
}

func TestTransportErrorIsUnavailable(t *testing.T) {
	// A closed httptest server guarantees connection-refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	c := New(srv.URL, "sekrit", zap.NewNop())
	if _, err := c.GetDocument(context.Background(), uuid.New(), "x"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("transport error must map to ErrUnavailable, got %v", err)
	}
}

// multipart writer sanity: the request we build must parse as
// multipart/form-data (guards a regression to a bare octet-stream).
func TestUploadDocumentSendsMultipart(t *testing.T) {
	var cap capturedUpload
	srv := fakeKYCUpload(t, http.StatusOK, `{}`, &cap)
	c := New(srv.URL, "sekrit", zap.NewNop())
	if _, err := c.UploadDocument(context.Background(), uuid.New(), "image/png", pngBytes, "+2348012345678", "kyc_voters_card", ""); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if len(cap.fileBytes) == 0 {
		t.Fatalf("no multipart file part received")
	}
}
