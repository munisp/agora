// SPEC-W61 tests for the booking media-inbox anchor client: payload shape
// (tenant_id explicit — contract amendment), X-Internal-Token internauth,
// fail-closed posture, single-attempt/no-retry, error mapping.
package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// anchorTestPayload is a fully-populated payload the tests assert
// byte-for-byte on the wire.
func anchorTestPayload() AnchorPayload {
	return AnchorPayload{
		TenantID:          "9f1c2a3b-0000-4000-8000-0000000000aa",
		Channel:           "whatsapp",
		MessageID:         "wamid.Anchor1",
		DocumentID:        "11111111-2222-3333-4444-555555555555",
		SenderPhone:       "2348012345678",
		SenderPhoneDigits: "2348012345678",
		Mime:              "image/jpeg",
		Bytes:             12345,
		SHA256Prefix:      "0123456789abcdef",
	}
}

// TestAnchorClientContract pins the wire contract: POST path,
// X-Internal-Token, and the exact JSON body keys (incl. tenant_id).
func TestAnchorClientContract(t *testing.T) {
	var hits atomic.Int32
	var got struct {
		path  string
		token string
		ctype string
		body  map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		got.path = r.URL.Path
		got.token = r.Header.Get("X-Internal-Token")
		got.ctype = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got.body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"aaaaaaaa-0000-4000-8000-000000000001","state":"parked"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewAnchorClient(srv.URL, "booking-token")
	if !c.Configured() {
		t.Fatal("client with base+token must be Configured")
	}
	if err := c.Anchor(context.Background(), anchorTestPayload()); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("exactly one attempt, got %d", n)
	}
	if got.path != "/v1/media/inbox/ingest" {
		t.Fatalf("wrong path %q", got.path)
	}
	if got.token != "booking-token" {
		t.Fatalf("X-Internal-Token missing/wrong: %q", got.token)
	}
	if got.ctype != "application/json" {
		t.Fatalf("content-type %q", got.ctype)
	}
	// Every contract field must be present with the exact value.
	want := map[string]any{
		"tenant_id":           "9f1c2a3b-0000-4000-8000-0000000000aa",
		"channel":             "whatsapp",
		"message_id":          "wamid.Anchor1",
		"document_id":         "11111111-2222-3333-4444-555555555555",
		"sender_phone":        "2348012345678",
		"sender_phone_digits": "2348012345678",
		"mime":                "image/jpeg",
		"bytes":               float64(12345),
		"sha256_prefix":       "0123456789abcdef",
	}
	if len(got.body) != len(want) {
		t.Fatalf("body keys = %v, want exactly %v", got.body, want)
	}
	for k, v := range want {
		if got.body[k] != v {
			t.Errorf("body[%q] = %v, want %v", k, got.body[k], v)
		}
	}
	// P8: the caption must never appear — there is no field for it.
	if _, ok := got.body["caption"]; ok {
		t.Fatal("caption must never ride the anchor payload")
	}
	// SPEC-W66 WS2: no hint on the fixture payload => the key is omitted
	// entirely (byte-identical W61 wire form). The hint-bearing wire form
	// is pinned by TestAnchorClientContractWithHint.
	if _, ok := got.body["booking_ref_hint"]; ok {
		t.Fatal("no-hint payload must omit booking_ref_hint (byte-identical W61)")
	}
}

// TestAnchorClientContractWithHint (SPEC-W66 WS2): a payload with a hint
// rides the wire with booking_ref_hint = the 11-char token ONLY — the
// caption string never appears anywhere in the request body.
func TestAnchorClientContractWithHint(t *testing.T) {
	var rawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rawBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"aaaaaaaa-0000-4000-8000-000000000001","state":"anchored"}`)) //nolint:errcheck
	}))
	defer srv.Close()

	p := anchorTestPayload()
	p.BookingRefHint = "BK-7C3X9Q2M"
	c := NewAnchorClient(srv.URL, "booking-token")
	if err := c.Anchor(context.Background(), p); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(rawBody), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["booking_ref_hint"] != "BK-7C3X9Q2M" {
		t.Fatalf("booking_ref_hint = %v, want BK-7C3X9Q2M", body["booking_ref_hint"])
	}
	// P8: neither a caption key nor any caption text may ride the wire.
	if _, ok := body["caption"]; ok {
		t.Fatal("caption must never ride the anchor payload")
	}
	if strings.Contains(rawBody, "do not log me") {
		t.Fatal("caption text must never ride the anchor payload (P8)")
	}
}

// TestAnchorClientFailClosed: an unset token (or unset base) never places a
// call — ErrAnchorNotConfigured, zero HTTP hits.
func TestAnchorClientFailClosed(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	for name, c := range map[string]*AnchorClient{
		"token unset": NewAnchorClient(srv.URL, ""),
		"base unset":  NewAnchorClient("", "tok"),
		"both unset":  NewAnchorClient("", ""),
	} {
		if c.Configured() {
			t.Fatalf("%s: must NOT be Configured", name)
		}
		if err := c.Anchor(context.Background(), anchorTestPayload()); err != ErrAnchorNotConfigured {
			t.Fatalf("%s: want ErrAnchorNotConfigured, got %v", name, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("fail-closed client must never call out, got %d hits", n)
	}
}

// TestAnchorClientErrorMapping: non-2xx maps to an error and exactly ONE
// attempt is made (no retries — the webhook redelivery is the retry).
func TestAnchorClientErrorMapping(t *testing.T) {
	for _, status := range []int{400, 401, 500, 503} {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(status)
			w.Write([]byte(`{"error":"boom"}`)) //nolint:errcheck
		}))
		c := NewAnchorClient(srv.URL, "tok")
		if err := c.Anchor(context.Background(), anchorTestPayload()); err == nil {
			t.Fatalf("status %d: want error", status)
		}
		if n := hits.Load(); n != 1 {
			t.Fatalf("status %d: exactly one attempt, got %d", status, n)
		}
		srv.Close()
	}
}

// TestAnchorClientTransportError: an unreachable base errors (no panic).
func TestAnchorClientTransportError(t *testing.T) {
	c := NewAnchorClient("http://127.0.0.1:1", "tok")
	if err := c.Anchor(context.Background(), anchorTestPayload()); err == nil {
		t.Fatal("unreachable base must error")
	}
}

// TestAnchorClientString never leaks the token.
func TestAnchorClientString(t *testing.T) {
	if s := NewAnchorClient("http://booking:7002", "supersecret").String(); s == "" ||
		strings.Contains(s, "supersecret") {
		t.Fatalf("String() must describe without leaking the token, got %q", s)
	}
	if NewAnchorClient("", "t").String() == "" || NewAnchorClient("http://b", "").String() == "" {
		t.Fatal("String() must describe disabled/fail-closed postures")
	}
}
