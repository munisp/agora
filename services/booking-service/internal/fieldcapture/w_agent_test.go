package fieldcapture

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"testing"
)

// 2026-11-21: the capturing agent rides the context to ContactCaptured.
func TestAgentContext(t *testing.T) {
	ctx := WithAgent(context.Background(), " user-42 ")
	if got := agentFrom(ctx); got != "user-42" {
		t.Fatalf("agentFrom = %q", got)
	}
	if got := agentFrom(WithAgent(context.Background(), "")); got != "" {
		t.Fatalf("empty agent must not be set, got %q", got)
	}
}

func TestCallerSub(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/field/capture", nil)
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"kc-sub-1","email":"a@b.c"}`))
	r.Header.Set("Authorization", "Bearer x."+payload+".y")
	if got := callerSub(r); got != "kc-sub-1" {
		t.Fatalf("callerSub from JWT = %q", got)
	}
	r2 := httptest.NewRequest("POST", "/v1/field/capture", nil)
	r2.Header.Set("X-User-Id", "op-7")
	if got := callerSub(r2); got != "op-7" {
		t.Fatalf("callerSub from header = %q", got)
	}
}
