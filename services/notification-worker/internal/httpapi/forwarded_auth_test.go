package httpapi

import (
	"context"
	"net/http"
	"testing"
)

func TestForwardedAuthKeepsOnlyIdentityHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer abc")
	h.Set("X-User-Id", "u-1")
	h.Set("Cookie", "session=secret")
	got := ForwardedAuth(WithForwardedAuth(context.Background(), h))
	if got["Authorization"] != "Bearer abc" || got["X-User-Id"] != "u-1" {
		t.Fatalf("identity headers forwarded: %v", got)
	}
	if _, leaked := got["Cookie"]; leaked {
		t.Fatal("cookies must never be forwarded")
	}
	if ForwardedAuth(context.Background()) != nil {
		t.Fatal("nothing forwarded without a caller")
	}
}
