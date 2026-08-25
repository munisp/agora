package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func makeToken(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(claims)
	body := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + body + ".sig"
}

func TestParseBearerClaims(t *testing.T) {
	tok := makeToken(map[string]any{
		"sub":          "user-1",
		"tenant_slugs": []string{"acme", "globex"},
	})
	claims, err := parseBearerClaims("Bearer " + tok)
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if claims.Sub != "user-1" {
		t.Fatalf("sub = %q", claims.Sub)
	}
	if !claims.hasTenant("acme") || !claims.hasTenant("globex") {
		t.Fatal("expected both tenants")
	}
	if claims.hasTenant("other") {
		t.Fatal("unexpected tenant")
	}
	if claims.firstTenant() != "acme" {
		t.Fatalf("firstTenant = %q", claims.firstTenant())
	}
}

// SPEC-W43 K-07: presented-but-malformed Bearer tokens are an ERROR
// (error-closed 401 in the middleware); absent/non-Bearer headers stay
// anonymous (zero claims, nil error).
func TestParseBearerClaimsInvalid(t *testing.T) {
	for _, h := range []string{"", "Basic abc"} {
		claims, err := parseBearerClaims(h)
		if err != nil {
			t.Fatalf("anonymous header %q must not error: %v", h, err)
		}
		if claims.Sub != "" || len(claims.TenantSlugs) != 0 {
			t.Fatalf("expected empty claims for %q, got %+v", h, claims)
		}
	}
	for _, h := range []string{"Bearer x", "Bearer a.b", "Bearer a.!!!.c"} {
		claims, err := parseBearerClaims(h)
		if err == nil {
			t.Fatalf("malformed token %q must error (error-closed)", h)
		}
		if claims.Sub != "" || len(claims.TenantSlugs) != 0 {
			t.Fatalf("expected empty claims for %q, got %+v", h, claims)
		}
	}
	// A token whose payload is valid JSON of the wrong shape must not leak
	// partial claims (unmarshal type error => error, zero claims).
	bad := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"u1","tenant_slugs":"acme"}`)) + ".sig"
	claims, err := parseBearerClaims("Bearer " + bad)
	if err == nil {
		t.Fatal("type-mismatched claims payload must error")
	}
	if claims.Sub != "" {
		t.Fatalf("partial claims leaked: %+v", claims)
	}
}

func TestParseBearerClaimsEmail(t *testing.T) {
	tok := makeToken(map[string]any{"sub": "user-2", "email": "ana@acme.test"})
	claims, err := parseBearerClaims("Bearer " + tok)
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if claims.Email != "ana@acme.test" {
		t.Fatalf("email = %q", claims.Email)
	}
	// tokens without an email claim decode to empty (mine=true then 403s
	// or falls back to X-User-Email)
	tok2 := makeToken(map[string]any{"sub": "user-3"})
	c, err := parseBearerClaims("Bearer " + tok2)
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if c.Email != "" {
		t.Fatalf("email = %q, want empty", c.Email)
	}
}
