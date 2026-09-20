package httpapi

// SPEC-W45 tests: K16 member lifecycle + STK O3/O4 invite semantics,
// K17 tenant API keys, K18 plan gate + plan change, K9 delete cascade.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/opendesk/identity-service/internal/store"
)

// ---------------------------------------------------------------------------
// STK O3: re-invite of an existing e-mail → 409 already_invited_or_member
// ---------------------------------------------------------------------------

func TestInviteMemberReInviteConflict(t *testing.T) {
	h := newHarness("secret-token")
	h.kc.userExists = true
	h.allowPermify("manage_catalog", "u-admin")
	rec := h.do(http.MethodPost, "/v1/tenants/acme/members",
		`{"email":"x@example.com","role":"staff"}`,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)})
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-invite: status = %d (want 409, not 502), body %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["error"] != "already_invited_or_member" || out["resend"] != true {
		t.Errorf("body = %v, want {error: already_invited_or_member, resend: true}", out)
	}
}

// ---------------------------------------------------------------------------
// STK O4: realm-role assignment at invite time
// ---------------------------------------------------------------------------

func TestInviteMemberRealmRoles(t *testing.T) {
	h := newHarness("secret-token")
	h.allowPermify("manage_catalog", "u-admin")

	// Base role mirror: admin → same-named realm role.
	rec := h.do(http.MethodPost, "/v1/tenants/acme/members",
		`{"email":"a@example.com","role":"admin"}`,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("invite: status = %d, body %s", rec.Code, rec.Body)
	}
	if len(h.kc.assigned) != 1 || !strings.HasSuffix(h.kc.assigned[0], ":admin") {
		t.Errorf("realm role assignments = %v, want one :admin", h.kc.assigned)
	}
	// execute-actions-email fired after create (K8).
	if len(h.kc.actionsEmail) != 1 || !strings.Contains(h.kc.actionsEmail[0], "UPDATE_PASSWORD") {
		t.Errorf("execute-actions-email calls = %v", h.kc.actionsEmail)
	}

	// Functional realm roles require platform-admin.
	rec = h.do(http.MethodPost, "/v1/tenants/acme/members",
		`{"email":"b@example.com","role":"staff","realm_roles":["billing"]}`,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)})
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-platform-admin billing grant: status = %d, want 403", rec.Code)
	}
	// Platform-admin may grant analyst/billing.
	h.allowPermify("manage_catalog", "boss")
	rec = h.do(http.MethodPost, "/v1/tenants/acme/members",
		`{"email":"c@example.com","role":"staff","realm_roles":["analyst","billing"]}`,
		map[string]string{"Authorization": jwt("boss", []string{"platform-admin"}, nil)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("platform-admin invite: status = %d, body %s", rec.Code, rec.Body)
	}
	joined := strings.Join(h.kc.assigned, ",")
	if !strings.Contains(joined, ":staff") || !strings.Contains(joined, ":analyst") || !strings.Contains(joined, ":billing") {
		t.Errorf("assignments = %v, want staff+analyst+billing", h.kc.assigned)
	}
	// Unknown functional role: 400.
	rec = h.do(http.MethodPost, "/v1/tenants/acme/members",
		`{"email":"d@example.com","realm_roles":["superuser"]}`,
		map[string]string{"Authorization": jwt("boss", []string{"platform-admin"}, nil)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown realm role: status = %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// K18: plan member-limit gate in inviteMember
// ---------------------------------------------------------------------------

func TestInviteMemberPlanGate(t *testing.T) {
	h := newHarness("secret-token")
	h.allowPermify("manage_catalog", "u-admin")
	// free plan (the acme fixture default) allows 3 members; seed 3.
	for _, u := range []string{"m1", "m2", "m3"} {
		_ = h.st.AddMember(context.Background(), store.Membership{TenantID: h.tenID, UserID: u, Role: "staff"})
	}
	rec := h.do(http.MethodPost, "/v1/tenants/acme/members",
		`{"email":"x@example.com","role":"staff"}`,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("4th member on free: status = %d, want 403, body %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["upgrade"] != true || out["limit"] != float64(3) {
		t.Errorf("upgrade body = %v", out)
	}
	if !strings.Contains(out["error"].(string), "upgrade") {
		t.Errorf("error message must carry the upgrade hint: %v", out["error"])
	}

	// Pro plan allows 20 → the same invite passes after upgrade.
	_ = h.st.SetTenantPlan(context.Background(), "acme", "pro")
	rec = h.do(http.MethodPost, "/v1/tenants/acme/members",
		`{"email":"x@example.com","role":"staff"}`,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)})
	if rec.Code != http.StatusCreated {
		t.Errorf("pro invite: status = %d, body %s", rec.Code, rec.Body)
	}
}

func TestMemberLimitMap(t *testing.T) {
	for plan, want := range map[string]int{"free": 3, "pro": 20} {
		if got := planMemberLimits[plan]; got != want {
			t.Errorf("planMemberLimits[%q] = %d, want %d", plan, got, want)
		}
	}
	// scale/enterprise/twin unlimited.
	for _, plan := range []string{"scale", "enterprise", "twin"} {
		if exceeded, _ := memberLimitExceeded(plan, 100000); exceeded {
			t.Errorf("plan %q must be unlimited", plan)
		}
	}
	if exceeded, _ := memberLimitExceeded("free", 2); exceeded {
		t.Errorf("free with 2 members must allow the 3rd")
	}
}

// ---------------------------------------------------------------------------
// K16: DELETE + PATCH /v1/tenants/{slug}/members/{user_id}
// ---------------------------------------------------------------------------

func TestRemoveMemberLifecycle(t *testing.T) {
	h := newHarness("secret-token")
	_ = h.st.AddMember(context.Background(), store.Membership{TenantID: h.tenID, UserID: "u-staff", Role: "staff"})

	// 401 anonymous / 403 non-admin / 404 unknown member.
	if rec := h.do(http.MethodDelete, "/v1/tenants/acme/members/u-staff", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d, want 401", rec.Code)
	}
	if rec := h.do(http.MethodDelete, "/v1/tenants/acme/members/u-staff", "",
		map[string]string{"Authorization": jwt("u-member", nil, []string{"acme"})}); rec.Code != http.StatusForbidden {
		t.Errorf("plain member: %d, want 403", rec.Code)
	}
	h.allowPermify("manage_catalog", "u-admin")
	if rec := h.do(http.MethodDelete, "/v1/tenants/acme/members/ghost", "",
		map[string]string{"Authorization": jwt("u-admin", nil, nil)}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown member: %d, want 404", rec.Code)
	}

	// 200: disable + logout + permify unlink + row removal.
	rec := h.do(http.MethodDelete, "/v1/tenants/acme/members/u-staff", "",
		map[string]string{"Authorization": jwt("u-admin", nil, nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("remove: status = %d, body %s", rec.Code, rec.Body)
	}
	if len(h.kc.disabled) != 1 || h.kc.disabled[0] != "u-staff" {
		t.Errorf("keycloak disables = %v", h.kc.disabled)
	}
	if len(h.kc.loggedOut) != 1 || h.kc.loggedOut[0] != "u-staff" {
		t.Errorf("keycloak logouts = %v", h.kc.loggedOut)
	}
	if len(h.perm.relDeleted) != 1 || !strings.Contains(h.perm.relDeleted[0], "#member@user:u-staff") {
		t.Errorf("permify unlinks = %v (staff must map to member)", h.perm.relDeleted)
	}
	if _, err := h.st.GetMember(context.Background(), h.tenID, "u-staff"); err != store.ErrNotFound {
		t.Errorf("membership row must be gone: %v", err)
	}
}

func TestRemoveMemberOwnerGuard(t *testing.T) {
	h := newHarness("secret-token")
	_ = h.st.AddMember(context.Background(), store.Membership{TenantID: h.tenID, UserID: "u-owner2", Role: "owner"})
	h.allowPermify("manage_catalog", "u-admin")

	// Admin (manage_catalog but no owner relation) cannot remove an owner.
	if rec := h.do(http.MethodDelete, "/v1/tenants/acme/members/u-owner2", "",
		map[string]string{"Authorization": jwt("u-admin", nil, nil)}); rec.Code != http.StatusForbidden {
		t.Fatalf("admin removing owner: %d, want 403", rec.Code)
	}
	// Owner caller may.
	h.allowPermify("manage_catalog", "u-owner")
	h.allowPermify("owner", "u-owner")
	if rec := h.do(http.MethodDelete, "/v1/tenants/acme/members/u-owner2", "",
		map[string]string{"Authorization": jwt("u-owner", nil, nil)}); rec.Code != http.StatusOK {
		t.Errorf("owner removing owner: %d, want 200", rec.Code)
	}
}

func TestUpdateMemberRoleLifecycle(t *testing.T) {
	h := newHarness("secret-token")
	_ = h.st.AddMember(context.Background(), store.Membership{TenantID: h.tenID, UserID: "u-staff", Role: "staff"})
	h.allowPermify("manage_catalog", "u-admin")

	// staff → admin: DB role, Permify rel swap, realm-role mirror swap.
	rec := h.do(http.MethodPatch, "/v1/tenants/acme/members/u-staff", `{"role":"admin"}`,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: status = %d, body %s", rec.Code, rec.Body)
	}
	m, err := h.st.GetMember(context.Background(), h.tenID, "u-staff")
	if err != nil || m.Role != "admin" {
		t.Fatalf("role readback: %v, %+v", err, m)
	}
	if len(h.perm.relDeleted) != 1 || !strings.Contains(h.perm.relDeleted[0], "#member@") {
		t.Errorf("old relation unlink = %v", h.perm.relDeleted)
	}
	if len(h.kc.removedRoles) != 1 || !strings.HasSuffix(h.kc.removedRoles[0], ":staff") ||
		len(h.kc.assigned) != 1 || !strings.HasSuffix(h.kc.assigned[0], ":admin") {
		t.Errorf("realm role swap: removed=%v assigned=%v", h.kc.removedRoles, h.kc.assigned)
	}

	// Idempotent same-role patch: changed=false, no side effects.
	rec = h.do(http.MethodPatch, "/v1/tenants/acme/members/u-staff", `{"role":"admin"}`,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)})
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out["changed"] != false {
		t.Errorf("same-role patch: %d %v", rec.Code, out)
	}

	// Invalid role / unknown member.
	if rec := h.do(http.MethodPatch, "/v1/tenants/acme/members/u-staff", `{"role":"superuser"}`,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)}); rec.Code != http.StatusBadRequest {
		t.Errorf("bad role: %d, want 400", rec.Code)
	}
	if rec := h.do(http.MethodPatch, "/v1/tenants/acme/members/ghost", `{"role":"viewer"}`,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)}); rec.Code != http.StatusNotFound {
		t.Errorf("ghost member: %d, want 404", rec.Code)
	}

	// Admin cannot promote TO owner; owner can.
	if rec := h.do(http.MethodPatch, "/v1/tenants/acme/members/u-staff", `{"role":"owner"}`,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)}); rec.Code != http.StatusForbidden {
		t.Errorf("admin promoting to owner: %d, want 403", rec.Code)
	}
	h.allowPermify("owner", "u-admin")
	if rec := h.do(http.MethodPatch, "/v1/tenants/acme/members/u-staff", `{"role":"owner"}`,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)}); rec.Code != http.StatusOK {
		t.Errorf("owner promoting to owner: %d, want 200", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// K17: tenant API keys
// ---------------------------------------------------------------------------

func TestAPIKeyLifecycle(t *testing.T) {
	h := newHarness("secret-token")
	h.allowPermify("manage_catalog", "u-admin")
	authz := map[string]string{"Authorization": jwt("u-admin", nil, nil)}

	// 401/403 matrix.
	if rec := h.do(http.MethodPost, "/v1/tenants/acme/api-keys", `{"name":"x"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous create: %d, want 401", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/v1/tenants/acme/api-keys", `{"name":"x"}`,
		map[string]string{"Authorization": jwt("u-member", nil, []string{"acme"})}); rec.Code != http.StatusForbidden {
		t.Errorf("plain member create: %d, want 403", rec.Code)
	}

	// Create: the full key is returned ONCE as prefix.secret.
	rec := h.do(http.MethodPost, "/v1/tenants/acme/api-keys", `{"name":"ext"}`, authz)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d, body %s", rec.Code, rec.Body)
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	fullKey, _ := created["key"].(string)
	prefix, _ := created["prefix"].(string)
	if !strings.HasPrefix(fullKey, prefix+".") || !strings.HasPrefix(prefix, "odk_") {
		t.Fatalf("key shape = %q (prefix %q), want prefix.secret", fullKey, prefix)
	}
	if scopes, _ := created["scopes"].([]any); len(scopes) != 1 || scopes[0] != "bookings:read" {
		t.Errorf("default scope = %v, want [bookings:read]", created["scopes"])
	}
	keyID, _ := created["id"].(string)

	// Unknown scope rejected.
	if rec := h.do(http.MethodPost, "/v1/tenants/acme/api-keys", `{"name":"bad","scopes":["admin:all"]}`, authz); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown scope: %d, want 400", rec.Code)
	}

	// List: key material absent (no key, no key_hash).
	rec = h.do(http.MethodGet, "/v1/tenants/acme/api-keys", "", authz)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), fullKey) || strings.Contains(rec.Body.String(), "key_hash") {
		t.Errorf("list must not leak key material: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), prefix) {
		t.Errorf("list must carry the prefix: %s", rec.Body)
	}

	// Validate (internal): wrong token → 401; valid key → tenant + scopes.
	if rec := h.do(http.MethodPost, "/internal/api-keys/validate", `{"key":"`+fullKey+`"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("validate without internal token: %d, want 401", rec.Code)
	}
	rec = h.do(http.MethodPost, "/internal/api-keys/validate", `{"key":"`+fullKey+`"}`,
		map[string]string{"X-Internal-Token": "secret-token"})
	if rec.Code != http.StatusOK {
		t.Fatalf("validate: %d, body %s", rec.Code, rec.Body)
	}
	var val map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &val)
	if val["tenant_slug"] != "acme" {
		t.Errorf("validate body = %v", val)
	}
	// Unknown key → 401.
	if rec := h.do(http.MethodPost, "/internal/api-keys/validate", `{"key":"odk_nope.nope"}`,
		map[string]string{"X-Internal-Token": "secret-token"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown key: %d, want 401", rec.Code)
	}

	// Revoke: stops validating, second revoke 404s.
	if rec := h.do(http.MethodDelete, "/v1/tenants/acme/api-keys/"+keyID, "", authz); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := h.do(http.MethodPost, "/internal/api-keys/validate", `{"key":"`+fullKey+`"}`,
		map[string]string{"X-Internal-Token": "secret-token"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked key: %d, want 401", rec.Code)
	}
	if rec := h.do(http.MethodDelete, "/v1/tenants/acme/api-keys/"+keyID, "", authz); rec.Code != http.StatusNotFound {
		t.Errorf("re-revoke: %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// K18: PATCH /v1/tenants/{slug}/plan
// ---------------------------------------------------------------------------

func TestUpdatePlan(t *testing.T) {
	h := newHarness("secret-token")

	// 401 anonymous; 403 plain member and non-owner admin; 400 bogus plan.
	if rec := h.do(http.MethodPatch, "/v1/tenants/acme/plan", `{"plan":"pro"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d, want 401", rec.Code)
	}
	if rec := h.do(http.MethodPatch, "/v1/tenants/acme/plan", `{"plan":"pro"}`,
		map[string]string{"Authorization": jwt("u-member", nil, []string{"acme"})}); rec.Code != http.StatusForbidden {
		t.Errorf("member: %d, want 403", rec.Code)
	}
	h.allowPermify("manage_catalog", "u-admin") // admin but NOT owner
	if rec := h.do(http.MethodPatch, "/v1/tenants/acme/plan", `{"plan":"pro"}`,
		map[string]string{"Authorization": jwt("u-admin", nil, nil)}); rec.Code != http.StatusForbidden {
		t.Errorf("non-owner admin: %d, want 403", rec.Code)
	}

	// Owner relation may change the plan.
	h.allowPermify("owner", "u-owner")
	rec := h.do(http.MethodPatch, "/v1/tenants/acme/plan", `{"plan":"pro"}`,
		map[string]string{"Authorization": jwt("u-owner", nil, nil)})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner plan change: %d, body %s", rec.Code, rec.Body)
	}
	tn, _ := h.st.GetTenantBySlug(context.Background(), "acme")
	if tn.Plan != "pro" {
		t.Errorf("plan readback = %q, want pro", tn.Plan)
	}

	// Platform admin may change the plan; bogus plan → 400; twin excluded.
	rec = h.do(http.MethodPatch, "/v1/tenants/acme/plan", `{"plan":"enterprise"}`,
		map[string]string{"Authorization": jwt("boss", []string{"platform-admin"}, nil)})
	if rec.Code != http.StatusOK {
		t.Errorf("platform-admin plan change: %d", rec.Code)
	}
	if rec := h.do(http.MethodPatch, "/v1/tenants/acme/plan", `{"plan":"twin"}`,
		map[string]string{"Authorization": jwt("boss", []string{"platform-admin"}, nil)}); rec.Code != http.StatusBadRequest {
		t.Errorf("twin plan via public patch: %d, want 400", rec.Code)
	}
	if rec := h.do(http.MethodPatch, "/v1/tenants/acme/plan", `{"plan":"gold"}`,
		map[string]string{"Authorization": jwt("boss", []string{"platform-admin"}, nil)}); rec.Code != http.StatusBadRequest {
		t.Errorf("bogus plan: %d, want 400", rec.Code)
	}
}
