package consent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// ---------------------------------------------------------------------------
// SPEC-W44 F4 / V2-D3: auth matrix for the gated consent surfaces
// (POST /v1/consents/erasure — destructive K4 fanout — and
// GET /v1/consents?subject= — cross-tenant PII read).
//
// Contract: 401 without credentials (or malformed presented token), 403 for
// an authenticated subject NOT bound to the request tenant, 202/200 for the
// K2 internal-token service caller and the K1 tenant-bound subject.
// ---------------------------------------------------------------------------

// bearerJWT builds an unsigned JWT carrying the given claims payload (the
// signature is gateway-verified upstream; the service only decodes).
func bearerJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) +
		"." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

// captureAs seeds one consent record for subject as the service caller.
func (h *harness) captureAs(t *testing.T, subject string) {
	t.Helper()
	rec := h.do(http.MethodPost, "/v1/consents",
		fmt.Sprintf(`{"tenant":"acme","subject":%q,"purpose":"kyc"}`, subject), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed capture: %d %s", rec.Code, rec.Body)
	}
}

func erasureBody(subject string) string {
	return fmt.Sprintf(`{"tenant":"acme","subject":%q,"purpose":"kyc"}`, subject)
}

func TestErasureAuthMatrix(t *testing.T) {
	const path = "/v1/consents/erasure"

	t.Run("no credentials -> 401 and nothing tombstoned", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, "sub-none")
		rec := h.doRaw(http.MethodPost, path, erasureBody("sub-none"), nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 (%s)", rec.Code, rec.Body)
		}
		// No tombstone: record still active via the service-caller read.
		got := h.do(http.MethodGet, "/v1/consents?subject=sub-none&tenant=acme", "", nil)
		var out struct {
			Consents []Record `json:"consents"`
		}
		if err := json.Unmarshal(got.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Consents) != 1 || out.Consents[0].ErasureTS != nil {
			t.Errorf("unauthorized erasure mutated state: %+v", out.Consents)
		}
	})

	t.Run("malformed bearer -> 401 (error-closed)", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, "sub-mal")
		rec := h.doRaw(http.MethodPost, path, erasureBody("sub-mal"),
			map[string]string{"Authorization": "Bearer not-a-jwt"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("wrong internal token only -> 401", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, "sub-badtok")
		rec := h.doRaw(http.MethodPost, path, erasureBody("sub-badtok"),
			map[string]string{"X-Internal-Token": "wrong"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("internal token unset on server -> 401 fail-closed", func(t *testing.T) {
		h := newHarness()
		h.handlerInternalToken("") // simulate IDENTITY_INTERNAL_TOKEN unset
		h.captureAs(t, "sub-unset")
		rec := h.doRaw(http.MethodPost, path, erasureBody("sub-unset"),
			map[string]string{"X-Internal-Token": "anything"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("internal-token service caller -> 202", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, "sub-svc")
		rec := h.doRaw(http.MethodPost, path, erasureBody("sub-svc"),
			map[string]string{"X-Internal-Token": testInternalToken})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("authenticated subject without tenant binding -> 403", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, "sub-nobind")
		rec := h.doRaw(http.MethodPost, path, erasureBody("sub-nobind"),
			map[string]string{"X-User-Id": "u-1"})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("cross-tenant subject -> 403 (V2-D3 attack)", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, "sub-cross")
		rec := h.doRaw(http.MethodPost, path, erasureBody("sub-cross"),
			map[string]string{"X-User-Id": "u-2", "X-Tenant-Slugs": "other,evil"})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("tenant-bound member (header) -> 202", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, "sub-member")
		rec := h.doRaw(http.MethodPost, path, erasureBody("sub-member"),
			map[string]string{"X-User-Id": "u-3", "X-Tenant-Slugs": "acme"})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("tenant-bound member (JWT tenant_slugs) -> 202", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, "sub-jwt")
		rec := h.doRaw(http.MethodPost, path, erasureBody("sub-jwt"),
			map[string]string{"Authorization": bearerJWT(t, map[string]any{
				"sub": "u-4", "tenant_slugs": []string{"acme"},
			})})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("uuid tenant ref + bound member -> 202 (GetTenantByID path)", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, "sub-uuid")
		body := fmt.Sprintf(`{"tenant_id":%q,"subject":"sub-uuid","purpose":"kyc"}`, h.tid.String())
		rec := h.doRaw(http.MethodPost, path, body,
			map[string]string{"X-User-Id": "u-5", "X-Tenant-Slugs": "acme"})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("uuid tenant ref + cross-tenant member -> 403", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, "sub-uuid-x")
		body := fmt.Sprintf(`{"tenant_id":%q,"subject":"sub-uuid-x","purpose":"kyc"}`, h.tid.String())
		rec := h.doRaw(http.MethodPost, path, body,
			map[string]string{"X-User-Id": "u-6", "X-Tenant-Slugs": "other"})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("dev escape: subject without slugs + TRUST_DIRECT_TENANT -> 202", func(t *testing.T) {
		h := newHarness()
		h.handlerTrustDirect(true)
		h.captureAs(t, "sub-dev")
		rec := h.doRaw(http.MethodPost, path, erasureBody("sub-dev"),
			map[string]string{"X-User-Id": "u-dev"})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("dev escape does not help anonymous callers -> 401", func(t *testing.T) {
		h := newHarness()
		h.handlerTrustDirect(true)
		h.captureAs(t, "sub-dev-anon")
		rec := h.doRaw(http.MethodPost, path, erasureBody("sub-dev-anon"), nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}

func TestListAuthMatrix(t *testing.T) {
	path := func(subject string) string {
		return "/v1/consents?subject=" + subject + "&tenant=acme"
	}

	t.Run("no credentials -> 401", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, "lst-none")
		rec := h.doRaw(http.MethodGet, path("lst-none"), "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("malformed bearer -> 401", func(t *testing.T) {
		h := newHarness()
		rec := h.doRaw(http.MethodGet, path("lst-mal"), "",
			map[string]string{"Authorization": "Bearer .."})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("internal-token service caller -> 200", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, "lst-svc")
		rec := h.doRaw(http.MethodGet, path("lst-svc"), "",
			map[string]string{"X-Internal-Token": testInternalToken})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("cross-tenant subject -> 403 (V2-D3 leak)", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, "lst-cross")
		rec := h.doRaw(http.MethodGet, path("lst-cross"), "",
			map[string]string{"X-User-Id": "u-7", "X-Tenant-Slugs": "other"})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("tenant-bound member -> 200", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, "lst-member")
		rec := h.doRaw(http.MethodGet, path("lst-member"), "",
			map[string]string{"X-User-Id": "u-8", "X-Tenant-Slugs": "acme"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("tenant-bound member via JWT -> 200", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, "lst-jwt")
		rec := h.doRaw(http.MethodGet, path("lst-jwt"), "",
			map[string]string{"Authorization": bearerJWT(t, map[string]any{
				"sub": "u-9", "tenant_slugs": []string{"acme"},
			})})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("dev escape -> 200", func(t *testing.T) {
		h := newHarness()
		h.handlerTrustDirect(true)
		h.captureAs(t, "lst-dev")
		rec := h.doRaw(http.MethodGet, path("lst-dev"), "",
			map[string]string{"X-User-Id": "u-dev"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
		}
	})
}

// TestCheckStaysServiceToService: /internal/consents/check keeps relying on
// the router-level internauth (K2) only — no V2-D3 subject gate was added.
func TestCheckStaysServiceToService(t *testing.T) {
	h := newHarness()
	h.captureAs(t, "chk-sub")
	rec := h.doRaw(http.MethodGet, "/internal/consents/check?subject=chk-sub&purpose=kyc", "",
		map[string]string{"X-Tenant-Slug": "acme"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
}

// helpers to mutate the harness handler for the fail-closed/dev-escape cases.

func (h *harness) handlerInternalToken(tok string) { h.h.InternalToken = tok }
func (h *harness) handlerTrustDirect(on bool)      { h.h.TrustDirectTenancy = on }
