package httpapi

// SPEC-W47 UIWEB-01: PATCH /v1/tenants/{slug} — settings save, pack
// activation, terminology replace.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/opendesk/identity-service/internal/packs"
	"github.com/opendesk/identity-service/internal/store"
)

func TestUpdateTenantAuthzMatrix(t *testing.T) {
	h := newHarness("secret-token")
	body := `{"name":"Acme Two"}`

	// 401: anonymous.
	if rec := h.do(http.MethodPatch, "/v1/tenants/acme", body, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: status = %d, want 401", rec.Code)
	}
	// 403: authenticated member without manage_catalog (viewer posture).
	if rec := h.do(http.MethodPatch, "/v1/tenants/acme", body,
		map[string]string{"Authorization": jwt("u-member", nil, []string{"acme"})}); rec.Code != http.StatusForbidden {
		t.Errorf("plain member: status = %d, want 403", rec.Code)
	}
	// 404: unknown tenant.
	h.allowPermify("manage_catalog", "u-admin")
	if rec := h.do(http.MethodPatch, "/v1/tenants/ghost", body,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown tenant: status = %d, want 404", rec.Code)
	}
	// 200: manage_catalog grant (owner|admin).
	rec := h.do(http.MethodPatch, "/v1/tenants/acme", body,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin patch: status = %d, body %s", rec.Code, rec.Body)
	}
	tn, _ := h.st.GetTenantBySlug(context.Background(), "acme")
	if tn.Name != "Acme Two" {
		t.Errorf("name readback = %q, want Acme Two", tn.Name)
	}
}

func TestUpdateTenantSettingsSave(t *testing.T) {
	h := newHarness("secret-token")
	h.allowPermify("manage_catalog", "u-admin")
	// Exactly the settings-client.tsx:65 shape.
	rec := h.do(http.MethodPatch, "/v1/tenants/acme",
		`{"name":"Acme Clinic","timezone":"Africa/Lagos","currency":"ngn","locale":"en-NG"}`,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	// Response mirrors GET /v1/tenants/{slug}; currency is normalised.
	if out["name"] != "Acme Clinic" {
		t.Errorf("name = %v, want Acme Clinic", out["name"])
	}
	if out["timezone"] != "Africa/Lagos" || out["currency"] != "NGN" || out["locale"] != "en-NG" {
		t.Errorf("payload = %v", out)
	}
	if _, ok := out["created_at"]; !ok {
		t.Errorf("payload must carry created_at (settings page member-since): %v", out)
	}
	tn, _ := h.st.GetTenantBySlug(context.Background(), "acme")
	if tn.Currency != "NGN" || tn.Locale != "en-NG" || tn.Timezone != "Africa/Lagos" {
		t.Errorf("readback = %+v", tn)
	}
}

func TestUpdateTenantValidation(t *testing.T) {
	h := newHarness("secret-token")
	h.allowPermify("manage_catalog", "u-admin")
	authz := map[string]string{"Authorization": jwt("u-admin", nil, nil)}

	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty patch", `{}`, http.StatusBadRequest},
		{"plan not patchable here", `{"plan":"enterprise"}`, http.StatusBadRequest},
		{"blank name", `{"name":"  "}`, http.StatusBadRequest},
		{"bad currency", `{"currency":"naira"}`, http.StatusBadRequest},
		{"bad locale", `{"locale":"!!"}`, http.StatusBadRequest},
		{"bad timezone", `{"timezone":"not a tz"}`, http.StatusBadRequest},
		{"non-string terminology value", `{"terminology":{"booking":3}}`, http.StatusBadRequest},
		{"array terminology", `{"terminology":["a"]}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		if rec := h.do(http.MethodPatch, "/v1/tenants/acme", tc.body, authz); rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (body %s)", tc.name, rec.Code, tc.want, rec.Body)
		}
	}
	// Invalid JSON → 400, not 500.
	if rec := h.do(http.MethodPatch, "/v1/tenants/acme", `{`, authz); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON: status = %d, want 400", rec.Code)
	}
}

func TestUpdateTenantTerminologyReplace(t *testing.T) {
	h := newHarness("secret-token")
	h.allowPermify("manage_catalog", "u-admin")
	authz := map[string]string{"Authorization": jwt("u-admin", nil, nil)}
	h.st.addTenant(store.Tenant{Slug: "acme2", Name: "Acme2",
		Terminology: json.RawMessage(`{"booking":"reservation","contact":"guest"}`)})
	h.perm.allowed["manage_catalog|user:u-admin|organization:"+mustTenantID(t, h, "acme2")] = true

	// voice-agent-client.tsx:79 shape — the full edited map; REPLACE semantics
	// must drop the removed "contact" key.
	rec := h.do(http.MethodPatch, "/v1/tenants/acme2", `{"terminology":{"booking":"session"}}`, authz)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	tn, _ := h.st.GetTenantBySlug(context.Background(), "acme2")
	var m map[string]string
	_ = json.Unmarshal(tn.Terminology, &m)
	if len(m) != 1 || m["booking"] != "session" {
		t.Errorf("terminology = %s, want replaced map with only booking=session", tn.Terminology)
	}
	// Empty object clears the document.
	rec = h.do(http.MethodPatch, "/v1/tenants/acme2", `{"terminology":{}}`, authz)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: status = %d, body %s", rec.Code, rec.Body)
	}
	tn, _ = h.st.GetTenantBySlug(context.Background(), "acme2")
	if string(tn.Terminology) != `{}` {
		t.Errorf("cleared terminology = %s, want {}", tn.Terminology)
	}
}

func TestUpdateTenantPackActivation(t *testing.T) {
	const repoPacks = "../../../../industries"
	if _, err := os.Stat(repoPacks); err != nil {
		t.Skip("industries pack dir not available")
	}
	reg, err := packs.Load(repoPacks)
	if err != nil {
		t.Fatalf("packs.Load: %v", err)
	}
	h := newHarness("secret-token")
	d := h.deps()
	d.Packs = reg
	h.http = NewRouter(d)
	h.allowPermify("manage_catalog", "u-admin")
	authz := map[string]string{"Authorization": jwt("u-admin", nil, nil)}

	// packs-client.tsx:187 shape: {industry: entry.id}.
	rec := h.do(http.MethodPatch, "/v1/tenants/acme", `{"industry":"clinic"}`, authz)
	if rec.Code != http.StatusOK {
		t.Fatalf("pack activation: status = %d, body %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["industry"] != "clinic" {
		t.Errorf("industry = %v, want clinic", out["industry"])
	}
	if out["pack"] == nil {
		t.Errorf("response must carry the resolved pack summary: %v", out)
	}
	// Unknown pack id → 400 naming the loaded catalog.
	rec = h.do(http.MethodPatch, "/v1/tenants/acme", `{"industry":"space-travel"}`, authz)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown pack: status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "loaded packs") {
		t.Errorf("error must list loaded packs: %s", rec.Body)
	}
}

// mustTenantID resolves the fake-store tenant id for permify allow keys.
func mustTenantID(t *testing.T, h *harness, slug string) string {
	t.Helper()
	tn, err := h.st.GetTenantBySlug(context.Background(), slug)
	if err != nil {
		t.Fatalf("tenant %s: %v", slug, err)
	}
	return tn.ID.String()
}
