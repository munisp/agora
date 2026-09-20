package consent

// SPEC-W45 STK O13: data subjects self-serve consent data-access/erasure
// with a booking portal JWT (HS256, PORTAL_SECRET, contact+tenant claims
// bound to the request), and capture carries captured_by provenance.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

const testPortalSecret = "portal-test-secret"

// portalJWT signs a booking-portal-shaped HS256 JWT (same construction as
// booking-service signPortalJWT).
func portalJWT(t *testing.T, secret string, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	body := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "Bearer " + body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h *harness) enablePortal() { h.h.PortalSecret = testPortalSecret }

func portalClaimsFor(contactID, tenantSlug string) map[string]any {
	return map[string]any{
		"sub": contactID, "tid": "ignored-for-binding", "tsl": tenantSlug,
		"scope": "portal", "exp": time.Now().Add(10 * time.Minute).Unix(),
	}
}

func TestPortalSelfServeDataAccess(t *testing.T) {
	contactID := "4f3c2b10-1111-4222-8333-abcdef012345"

	t.Run("portal JWT bound to subject+tenant -> 200", func(t *testing.T) {
		h := newHarness()
		h.enablePortal()
		h.captureAs(t, contactID)
		rec := h.doRaw(http.MethodGet, "/v1/consents?subject="+contactID+"&tenant=acme", "",
			map[string]string{"Authorization": portalJWT(t, testPortalSecret, portalClaimsFor(contactID, "acme"))})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("portal JWT for a DIFFERENT subject -> 403", func(t *testing.T) {
		h := newHarness()
		h.enablePortal()
		h.captureAs(t, contactID)
		rec := h.doRaw(http.MethodGet, "/v1/consents?subject="+contactID+"&tenant=acme", "",
			map[string]string{"Authorization": portalJWT(t, testPortalSecret, portalClaimsFor("someone-else", "acme"))})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("portal JWT for a DIFFERENT tenant -> 403", func(t *testing.T) {
		h := newHarness()
		h.enablePortal()
		h.captureAs(t, contactID)
		rec := h.doRaw(http.MethodGet, "/v1/consents?subject="+contactID+"&tenant=acme", "",
			map[string]string{"Authorization": portalJWT(t, testPortalSecret, portalClaimsFor(contactID, "other-co"))})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("wrong secret -> 403 (signature rejected, falls through)", func(t *testing.T) {
		h := newHarness()
		h.enablePortal()
		h.captureAs(t, contactID)
		rec := h.doRaw(http.MethodGet, "/v1/consents?subject="+contactID+"&tenant=acme", "",
			map[string]string{"Authorization": portalJWT(t, "wrong-secret", portalClaimsFor(contactID, "acme"))})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("expired portal JWT -> 403", func(t *testing.T) {
		h := newHarness()
		h.enablePortal()
		h.captureAs(t, contactID)
		claims := portalClaimsFor(contactID, "acme")
		claims["exp"] = time.Now().Add(-time.Minute).Unix()
		rec := h.doRaw(http.MethodGet, "/v1/consents?subject="+contactID+"&tenant=acme", "",
			map[string]string{"Authorization": portalJWT(t, testPortalSecret, claims)})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("PORTAL_SECRET unset -> portal path unavailable (fail-closed)", func(t *testing.T) {
		h := newHarness()
		h.captureAs(t, contactID)
		rec := h.doRaw(http.MethodGet, "/v1/consents?subject="+contactID+"&tenant=acme", "",
			map[string]string{"Authorization": portalJWT(t, testPortalSecret, portalClaimsFor(contactID, "acme"))})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (portal path disabled)", rec.Code)
		}
	})
}

func TestPortalSelfServeErasure(t *testing.T) {
	contactID := "4f3c2b10-9999-4222-8333-abcdef012345"
	h := newHarness()
	h.enablePortal()
	h.captureAs(t, contactID)

	// Own records: tombstoned (202).
	rec := h.doRaw(http.MethodPost, "/v1/consents/erasure", erasureBody(contactID),
		map[string]string{"Authorization": portalJWT(t, testPortalSecret, portalClaimsFor(contactID, "acme"))})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("self-serve erasure: status = %d, want 202 (%s)", rec.Code, rec.Body)
	}

	// Another subject's records: denied, nothing moved.
	h.captureAs(t, "victim")
	rec = h.doRaw(http.MethodPost, "/v1/consents/erasure", erasureBody("victim"),
		map[string]string{"Authorization": portalJWT(t, testPortalSecret, portalClaimsFor(contactID, "acme"))})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-subject erasure: status = %d, want 403", rec.Code)
	}
	got := h.do(http.MethodGet, "/v1/consents?subject=victim&tenant=acme", "", nil)
	if strings.Contains(got.Body.String(), "erasure_ts\":\"") {
		t.Errorf("cross-subject erasure mutated state: %s", got.Body)
	}
}

func TestCaptureProvenance(t *testing.T) {
	contactID := "4f3c2b10-7777-4222-8333-abcdef012345"

	// Anonymous public capture → "public".
	h := newHarness()
	rec := h.doRaw(http.MethodPost, "/v1/consents",
		`{"tenant":"acme","subject":"anon-1","purpose":"kyc"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("public capture: %d %s", rec.Code, rec.Body)
	}
	var out Record
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.CapturedBy != "public" {
		t.Errorf("captured_by = %q, want public", out.CapturedBy)
	}

	// Portal-session capture → "portal:<contact_id>".
	h.enablePortal()
	rec = h.doRaw(http.MethodPost, "/v1/consents",
		`{"tenant":"acme","subject":"`+contactID+`","purpose":"marketing"}`,
		map[string]string{"Authorization": portalJWT(t, testPortalSecret, portalClaimsFor(contactID, "acme"))})
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.CapturedBy != "portal:"+contactID {
		t.Errorf("captured_by = %q, want portal:%s", out.CapturedBy, contactID)
	}

	// Explicit channel descriptor wins.
	rec = h.doRaw(http.MethodPost, "/v1/consents",
		`{"tenant":"acme","subject":"anon-2","purpose":"kyc","captured_by":"ussd"}`, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.CapturedBy != "ussd" {
		t.Errorf("captured_by = %q, want ussd", out.CapturedBy)
	}

	// Service caller (K2 token) → "service:internal".
	rec = h.do(http.MethodPost, "/v1/consents",
		`{"tenant":"acme","subject":"anon-3","purpose":"kyc"}`, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.CapturedBy != "service:internal" {
		t.Errorf("captured_by = %q, want service:internal", out.CapturedBy)
	}
}
