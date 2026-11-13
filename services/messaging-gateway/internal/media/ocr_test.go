package media

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

var testTenant = uuid.MustParse("9f1c2a3b-0000-4000-8000-0000000000aa")

var jpegData = []byte{0xFF, 0xD8, 0xFF, 0xE0, 'x'}

// P2: the kind mapping is hard-coded — images => receipt, pdf => document,
// EVERYTHING else (incl. any kyc_*-shaped input) is unmappable.
func TestKindForOCRHardCoded(t *testing.T) {
	for mime, want := range map[string]string{
		"image/jpeg": "receipt", "image/png": "receipt", "image/webp": "receipt",
		"application/pdf": "document",
	} {
		if kind, ok := KindForOCR(mime); !ok || kind != want {
			t.Fatalf("%s: got (%q,%v), want (%q,true)", mime, kind, ok, want)
		}
	}
	for _, mime := range []string{"image/gif", "video/mp4", "audio/ogg", "kyc_nin_slip", "", "text/plain"} {
		if kind, ok := KindForOCR(mime); ok {
			t.Fatalf("%s must be unmappable (kyc_* structurally unemittable), got %q", mime, kind)
		}
	}
}

// Fail-closed: unset URL disables; unset token keeps Configured() false and
// Extract refuses before any network call.
func TestOCRClientFailClosedUnset(t *testing.T) {
	if NewOCRClient("", "tok", nil).Enabled() {
		t.Fatal("empty URL must disable the client")
	}
	c := NewOCRClient("http://ocr:7020", "", nil)
	if !c.Enabled() || c.Configured() {
		t.Fatal("unset token must keep Configured() false (fail-closed)")
	}
	if _, err := c.Extract(context.Background(), testTenant, "image/jpeg", "a.jpg", jpegData); err == nil {
		t.Fatal("unconfigured client must refuse to call out")
	}
	if NewOCRClient("", "", nil).String() == "" || c.String() == "" {
		t.Fatal("String() must describe the posture for boot logs")
	}
}

// Client-side validation: magic-byte mismatch and oversize are
// ErrRejectedMedia (deterministic) without any network call.
func TestOCRClientValidatesBeforeNetwork(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	c := NewOCRClient(srv.URL, "tok", zap.NewNop())
	if _, err := c.Extract(context.Background(), testTenant, "image/gif", "a.gif", jpegData); !errors.Is(err, ErrRejectedMedia) {
		t.Fatalf("gif must be ErrRejectedMedia, got %v", err)
	}
	if _, err := c.Extract(context.Background(), testTenant, "image/png", "a.png", jpegData); !errors.Is(err, ErrRejectedMedia) {
		t.Fatalf("magic-byte mismatch must be ErrRejectedMedia, got %v", err)
	}
	if _, err := c.Extract(context.Background(), testTenant, "image/jpeg", "a.jpg", make([]byte, MaxMediaBytes+1)); !errors.Is(err, ErrRejectedMedia) {
		t.Fatalf("oversize must be ErrRejectedMedia, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("client-side rejects must not hit the network, got %d", hits.Load())
	}
}

// NO RETRIES by contract (double-bill/double-store guard): a 500 is one
// attempt, ErrUnavailable; 415 is one attempt, ErrRejectedMedia; 2xx
// without document_id is a contract violation, ErrUnavailable.
func TestOCRClientNoRetriesAndStatusMapping(t *testing.T) {
	var hits atomic.Int32
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		if status == http.StatusOK {
			w.Write([]byte(`{"status":"uploaded"}`)) //nolint:errcheck // no document_id
		}
	}))
	defer srv.Close()
	c := NewOCRClient(srv.URL, "tok", zap.NewNop())
	status = http.StatusInternalServerError
	if _, err := c.Extract(context.Background(), testTenant, "image/jpeg", "a.jpg", jpegData); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("500 must be ErrUnavailable, got %v", err)
	}
	status = http.StatusUnsupportedMediaType
	if _, err := c.Extract(context.Background(), testTenant, "image/jpeg", "a.jpg", jpegData); !errors.Is(err, ErrRejectedMedia) {
		t.Fatalf("415 must be ErrRejectedMedia, got %v", err)
	}
	status = http.StatusOK
	if _, err := c.Extract(context.Background(), testTenant, "image/jpeg", "a.jpg", jpegData); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("2xx without document_id must be a contract violation, got %v", err)
	}
	if hits.Load() != 3 {
		t.Fatalf("each call must be a single attempt, got %d hits", hits.Load())
	}
}

// Happy path: kind/tenant_id form fields, X-Internal-Token, X-Tenant-Slug,
// filename passthrough with extension fallback for unsafe names.
func TestOCRClientExtractHappyPath(t *testing.T) {
	var gotKind, gotTenant, gotToken, gotSlug, gotFile string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Internal-Token")
		gotSlug = r.Header.Get("X-Tenant-Slug")
		_ = r.ParseMultipartForm(MaxMediaBytes)
		if r.MultipartForm != nil {
			gotKind = strings.Join(r.MultipartForm.Value["kind"], "")
			gotTenant = strings.Join(r.MultipartForm.Value["tenant_id"], "")
			if f := r.MultipartForm.File["file"]; len(f) == 1 {
				gotFile = f[0].Filename
			}
		}
		if gotKind == "" || gotTenant == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"document_id":"11111111-2222-3333-4444-555555555555","status":"uploaded"}`)) //nolint:errcheck
	}))
	defer srv.Close()
	c := NewOCRClient(srv.URL, "ocr-token", zap.NewNop())
	docID, err := c.Extract(context.Background(), testTenant, "application/pdf", "../../etc/evil", []byte("%PDF-1.7 x"))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if gotKind != "document" || gotTenant != testTenant.String() {
		t.Fatalf("form fields wrong: kind=%q tenant_id=%q", gotKind, gotTenant)
	}
	if gotToken != "ocr-token" || gotSlug != testTenant.String() {
		t.Fatalf("auth headers wrong: token=%q slug=%q", gotToken, gotSlug)
	}
	if gotFile != "whatsapp-media.pdf" {
		t.Fatalf("unsafe/wrong-extension filename must fall back, got %q", gotFile)
	}
	if docID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("document_id not returned, got %q", docID)
	}
}
