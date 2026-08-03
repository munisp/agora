package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMockResolverDeterministic(t *testing.T) {
	m := MockResolver{}
	// Contract: all digits & len>=10 -> verified.
	for _, v := range []string{"1234567890", "22223333444", "00000000000", strings.Repeat("9", 20)} {
		st, err := m.Resolve(context.Background(), "bvn", v)
		if err != nil || st != StatusVerified {
			t.Errorf("%q: %q, %v — want verified", v, st, err)
		}
	}
	for _, v := range []string{"123456789", "123456789a", "", "12 34567890", "+2348012345678"} {
		st, err := m.Resolve(context.Background(), "nin", v)
		if err != nil || st != StatusMismatch {
			t.Errorf("%q: %q, %v — want mismatch", v, st, err)
		}
	}
	// Determinism: same input twice, same verdict.
	a, _ := m.Resolve(context.Background(), "bvn", "12345678901")
	b, _ := m.Resolve(context.Background(), "bvn", "12345678901")
	if a != b {
		t.Errorf("mock not deterministic: %q vs %q", a, b)
	}
}

func TestLiveResolverContractShapes(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "verified"})
	}))
	defer srv.Close()
	weird := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "banana"})
	}))
	defer weird.Close()

	l := NewLiveResolver(srv.URL, "k", 5*time.Second)
	st, err := l.Resolve(context.Background(), "bvn", "12345678901")
	if err != nil || st != StatusVerified {
		t.Errorf("happy: %q %v", st, err)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"id_type":"bvn"`) || !strings.Contains(gotBody, `"id_value":"12345678901"`) {
		t.Errorf("request body = %s", gotBody)
	}

	// Unknown provider status degrades to pending with an error (never a
	// fabricated hard verdict).
	l2 := NewLiveResolver(weird.URL, "", 5*time.Second)
	st, err = l2.Resolve(context.Background(), "bvn", "12345678901")
	if st != StatusPending || err == nil {
		t.Errorf("unknown status: %q %v, want pending+err", st, err)
	}
	// Unreachable host: pending + error.
	l3 := NewLiveResolver("http://127.0.0.1:1", "", 200*time.Millisecond)
	st, err = l3.Resolve(context.Background(), "bvn", "12345678901")
	if st != StatusPending || err == nil {
		t.Errorf("unreachable: %q %v, want pending+err", st, err)
	}
}

func TestHashAndReferenceStable(t *testing.T) {
	h1 := hashIDValue("22223333444")
	if len(h1) != 64 || h1 != hashIDValue("22223333444") {
		t.Errorf("sha256 hex expected, got %q", h1)
	}
	if h1 == hashIDValue("22223333445") {
		t.Errorf("hash collision on adjacent values")
	}
	tid := uuid.New()
	r1 := referenceFor(tid, "+2348", "bvn", h1)
	if r1 != referenceFor(tid, "+2348", "bvn", h1) || !strings.HasPrefix(r1, "kyc_") {
		t.Errorf("reference not deterministic: %q", r1)
	}
	if r1 == referenceFor(tid, "+2348", "nin", h1) {
		t.Errorf("reference must differ per id_type")
	}
}
