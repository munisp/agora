package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/opendesk/messaging-gateway/internal/metrics"
	"go.uber.org/zap"
)

// waMediaTestClient builds a WhatsApp provider against base with no retry
// backoff sleep.
func waMediaTestClient(base string) *WhatsApp {
	w := &WhatsApp{Client: NewClient("whatsapp", metrics.New(), zap.NewNop()), BaseURL: base, Token: "t"}
	w.Client.sleep = func(context.Context, int) {}
	return w
}

// waMediaMetaBody is the metadata JSON FetchMedia expects.
func waMediaMetaBody(url, mime, sha string) string {
	return fmt.Sprintf(`{"url":%q,"mime_type":%q,"sha256":%q,"file_size":5}`, url, mime, sha)
}

// FetchMedia (SPEC-W60 P5): metadata GET is retriable via the shared Client
// machinery; the byte GET is a single attempt; mime/sha256/size rejects are
// deterministic (P6).
func TestWhatsAppFetchMedia(t *testing.T) {
	payload := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'x'}
	sum := sha256.Sum256(payload)
	sha := hex.EncodeToString(sum[:])

	t.Run("happy path returns allow-listed media", func(t *testing.T) {
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/bytes/m1" {
				w.Write(payload) //nolint:errcheck
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(waMediaMetaBody(srv.URL+"/bytes/m1", "image/jpeg", sha))) //nolint:errcheck
		}))
		defer srv.Close()
		m, err := waMediaTestClient(srv.URL).FetchMedia(context.Background(), "m1")
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if m.MimeType != "image/jpeg" || m.SHA256 != sha || len(m.Data) != len(payload) {
			t.Fatalf("media mismatch: %+v", m)
		}
	})

	t.Run("metadata 500 is retriable and fails transient", func(t *testing.T) {
		var metaHits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			metaHits.Add(1)
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		if _, err := waMediaTestClient(srv.URL).FetchMedia(context.Background(), "m1"); err == nil || DeterministicMediaError(err) {
			t.Fatalf("metadata 500 must be a transient error, got %v", err)
		}
		if metaHits.Load() < 2 {
			t.Fatalf("metadata GET must be retried via the Client machinery, got %d hit(s)", metaHits.Load())
		}
	})

	t.Run("metadata mime outside allow-list is deterministic", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(waMediaMetaBody("http://lookaside/x", "image/gif", ""))) //nolint:errcheck
		}))
		defer srv.Close()
		_, err := waMediaTestClient(srv.URL).FetchMedia(context.Background(), "m1")
		if !errors.Is(err, ErrMediaType) || !DeterministicMediaError(err) {
			t.Fatalf("gif must be ErrMediaType/deterministic, got %v", err)
		}
	})

	t.Run("sha256 mismatch is deterministic", func(t *testing.T) {
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/bytes/m1" {
				w.Write(payload) //nolint:errcheck
				return
			}
			w.Write([]byte(waMediaMetaBody(srv.URL+"/bytes/m1", "image/jpeg", "00000000000000000000000000000000000000000000000000000000000000ff"))) //nolint:errcheck
		}))
		defer srv.Close()
		if _, err := waMediaTestClient(srv.URL).FetchMedia(context.Background(), "m1"); !errors.Is(err, ErrMediaChecksum) {
			t.Fatalf("checksum mismatch must be ErrMediaChecksum, got %v", err)
		}
	})

	t.Run("oversize stream is deterministic and single-attempt", func(t *testing.T) {
		big := make([]byte, MaxMediaBytes+64)
		bigSum := sha256.Sum256(big)
		var byteHits atomic.Int32
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/bytes/m1" {
				byteHits.Add(1)
				w.Write(big) //nolint:errcheck
				return
			}
			w.Write([]byte(waMediaMetaBody(srv.URL+"/bytes/m1", "image/jpeg", hex.EncodeToString(bigSum[:])))) //nolint:errcheck
		}))
		defer srv.Close()
		if _, err := waMediaTestClient(srv.URL).FetchMedia(context.Background(), "m1"); !errors.Is(err, ErrMediaTooLarge) {
			t.Fatalf("oversize must be ErrMediaTooLarge, got %v", err)
		}
		if byteHits.Load() != 1 {
			t.Fatalf("byte GET must be single-attempt, got %d", byteHits.Load())
		}
	})

	t.Run("sha256 match helper", func(t *testing.T) {
		s := sha256.Sum256([]byte("hello"))
		if !mediaSHA256Matches(hex.EncodeToString(s[:]), []byte("hello")) {
			t.Fatal("hex sha256 must match")
		}
		if mediaSHA256Matches(hex.EncodeToString(s[:]), []byte("bye")) {
			t.Fatal("wrong bytes must not match")
		}
		if mediaSHA256Matches("not-a-hash", []byte("hello")) {
			t.Fatal("undeclarable encoding must fail closed")
		}
	})
}

func TestAllowedWhatsAppMime(t *testing.T) {
	for mime, want := range map[string]bool{
		"image/jpeg": true, "image/png": true, "image/webp": true, "application/pdf": true,
		"image/gif": false, "video/mp4": false, "audio/ogg": false, "": false,
	} {
		if got := AllowedWhatsAppMime(mime); got != want {
			t.Fatalf("%s: got %v, want %v", mime, got, want)
		}
		if want && MediaExtension(mime) == "" {
			t.Fatalf("%s: allowed mime must have a fallback extension", mime)
		}
	}
}
