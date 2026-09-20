package httpapi

// SPEC-W45 INT (K contract note): after a successful plan change, identity
// best-effort pushes the plan to billing-engine
// (PUT {BILLING_URL}/v1/tenants/{uuid}/plan, X-Internal-Token). Failures log
// ERROR + land in the response warnings[]; the identity change is NEVER
// rolled back.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opendesk/identity-service/internal/billing"
)

func TestUpdatePlanBillingPush(t *testing.T) {
	type call struct {
		method, path, token, body string
	}
	var got []call
	billingUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, call{r.Method, r.URL.Path, r.Header.Get("X-Internal-Token"), string(b)})
		w.WriteHeader(http.StatusOK)
	}))
	defer billingUp.Close()

	h := newHarness("secret-token")
	deps := h.deps()
	deps.Billing = billing.New(billingUp.URL, "bill-tok")
	h.http = NewRouter(deps)
	h.allowPermify("owner", "u-owner")

	rec := h.do(http.MethodPatch, "/v1/tenants/acme/plan", `{"plan":"pro"}`,
		map[string]string{"Authorization": jwt("u-owner", nil, nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("plan change: %d, body %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := out["warnings"]; ok {
		t.Errorf("warnings present on successful push: %v", out["warnings"])
	}
	if len(got) != 1 {
		t.Fatalf("billing calls = %d, want 1", len(got))
	}
	c := got[0]
	if c.method != http.MethodPut || c.path != "/v1/tenants/"+h.tenID.String()+"/plan" {
		t.Errorf("billing call = %s %s, want PUT /v1/tenants/%s/plan", c.method, c.path, h.tenID)
	}
	if c.token != "bill-tok" {
		t.Errorf("X-Internal-Token = %q, want bill-tok", c.token)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(c.body), &payload); err != nil || payload["plan"] != "pro" {
		t.Errorf("billing body = %q, want {\"plan\":\"pro\"}", c.body)
	}
}

func TestUpdatePlanBillingPushFailureIsBestEffort(t *testing.T) {
	billingDown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer billingDown.Close()

	h := newHarness("secret-token")
	deps := h.deps()
	deps.Billing = billing.New(billingDown.URL, "bill-tok")
	h.http = NewRouter(deps)
	h.allowPermify("owner", "u-owner")

	rec := h.do(http.MethodPatch, "/v1/tenants/acme/plan", `{"plan":"pro"}`,
		map[string]string{"Authorization": jwt("u-owner", nil, nil)})
	// NEVER rolled back: still 200, plan still changed.
	if rec.Code != http.StatusOK {
		t.Fatalf("billing failure must not fail the plan change: %d, body %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out["plan"] != "pro" || out["changed"] != true {
		t.Errorf("response = %v, want plan=pro changed=true", out)
	}
	warns, ok := out["warnings"].([]any)
	if !ok || len(warns) != 1 {
		t.Fatalf("warnings = %v, want exactly one entry", out["warnings"])
	}
	if w, _ := warns[0].(string); w == "" {
		t.Errorf("warning entry must be a non-empty string: %v", warns[0])
	}
	tn, _ := h.st.GetTenantBySlug(context.Background(), "acme")
	if tn.Plan != "pro" {
		t.Errorf("plan readback = %q, want pro (no rollback)", tn.Plan)
	}

	// Nil Billing (BILLING_URL unset) → no push, no warnings.
	h2 := newHarness("secret-token")
	h2.allowPermify("owner", "u-owner")
	rec = h2.do(http.MethodPatch, "/v1/tenants/acme/plan", `{"plan":"pro"}`,
		map[string]string{"Authorization": jwt("u-owner", nil, nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("no-billing plan change: %d", rec.Code)
	}
	out = map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if _, ok := out["warnings"]; ok {
		t.Errorf("warnings present without billing client: %v", out["warnings"])
	}
}
