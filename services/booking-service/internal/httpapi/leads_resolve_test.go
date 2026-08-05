package httpapi

// SPEC-W28 fix: /v1/leads/resolve tests — tenant scoping (other tenant's
// ids omitted), unknown/malformed ids omitted, the 500-id cap, and the
// handler's 503 posture without a store.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/store"
	"go.uber.org/zap"
)

// fakeLeadGetter returns leads only for its known (tenantID, id) pairs —
// anything else is store.ErrNotFound, exactly like the real store under RLS.
type fakeLeadGetter struct {
	leads map[string]store.Lead // "tenant/id" → lead
}

func (f *fakeLeadGetter) key(tenantID, id uuid.UUID) string {
	return tenantID.String() + "/" + id.String()
}

func (f *fakeLeadGetter) GetLead(_ context.Context, tenantID, id uuid.UUID) (store.Lead, error) {
	if l, ok := f.leads[f.key(tenantID, id)]; ok {
		return l, nil
	}
	return store.Lead{}, store.ErrNotFound
}

func leadFor(tenantID, id uuid.UUID, phone string) store.Lead {
	return store.Lead{ID: id, TenantID: tenantID, PhoneE164: phone}
}

// Tenant scoping (SPEC-W28 §5 gate 1, RLS belt and braces): only leads that
// exist AND belong to the requesting tenant are resolved; another tenant's
// ids, unknown ids and malformed ids are all omitted, indistinguishably.
func TestResolveLeadPhoneMapTenantScoping(t *testing.T) {
	tenantA := uuid.New()
	tenantB := uuid.New()
	leadA := uuid.New()
	leadNoPhone := uuid.New()
	leadB := uuid.New() // belongs to tenant B
	unknown := uuid.New()

	fake := &fakeLeadGetter{leads: map[string]store.Lead{
		tenantA.String() + "/" + leadA.String():       leadFor(tenantA, leadA, "+234801111111"),
		tenantA.String() + "/" + leadNoPhone.String(): {ID: leadNoPhone, TenantID: tenantA},
		tenantB.String() + "/" + leadB.String():       leadFor(tenantB, leadB, "+234802222222"),
	}}

	phones, err := resolveLeadPhoneMap(context.Background(), fake, tenantA, []string{
		leadA.String(),
		leadNoPhone.String(),
		leadB.String(),   // cross-tenant: must be omitted
		unknown.String(), // unknown: omitted
		"not-a-uuid",     // malformed: omitted
		leadA.String(),   // duplicate: resolved once
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := map[string]string{leadA.String(): "+234801111111"}
	if len(phones) != len(want) {
		t.Fatalf("phones = %v, want exactly %v", phones, want)
	}
	for id, phone := range want {
		if phones[id] != phone {
			t.Fatalf("phones[%s] = %q, want %q (all: %v)", id, phones[id], phone, phones)
		}
	}

	// Tenant B resolving its own lead works (scoping is per-request).
	phonesB, err := resolveLeadPhoneMap(context.Background(), fake, tenantB, []string{leadB.String(), leadA.String()})
	if err != nil {
		t.Fatalf("resolve tenant B: %v", err)
	}
	if len(phonesB) != 1 || phonesB[leadB.String()] != "+234802222222" {
		t.Fatalf("tenant B phones = %v, want only its own lead", phonesB)
	}
}

// A store error other than not-found aborts the batch (the caller degrades
// the whole intake, never sends with partial data silently).
func TestResolveLeadPhoneMapStoreError(t *testing.T) {
	errGetter := &errLeadGetter{err: errors.New("db down")}
	if _, err := resolveLeadPhoneMap(context.Background(), errGetter, uuid.New(), []string{uuid.NewString()}); err == nil {
		t.Fatal("want store error to propagate")
	}
}

type errLeadGetter struct{ err error }

func (g *errLeadGetter) GetLead(context.Context, uuid.UUID, uuid.UUID) (store.Lead, error) {
	return store.Lead{}, g.err
}

// Handler: the 500-id cap is enforced with 400 BEFORE any lookup.
func TestResolveLeadPhonesCapEnforced(t *testing.T) {
	s := &server{d: Deps{Logger: zap.NewNop()}}
	ids := make([]string, 0, leadsResolveMaxIDs+1)
	for i := 0; i <= leadsResolveMaxIDs; i++ {
		ids = append(ids, uuid.NewString())
	}
	body := fmt.Sprintf(`{"lead_ids":["%s"]}`, strings.Join(ids, `","`))
	req := httptest.NewRequest(http.MethodPost, "/v1/leads/resolve", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxTenant,
		bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}))
	rec := httptest.NewRecorder()
	s.resolveLeadPhones(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cap: status = %d, want 400", rec.Code)
	}
}

// Handler without a store degrades to 503 (partial deployments keep the
// rest of the API intact — the /v1/leads posture).
func TestResolveLeadPhonesStoreUnavailable(t *testing.T) {
	s := &server{d: Deps{Logger: zap.NewNop()}}
	req := httptest.NewRequest(http.MethodPost, "/v1/leads/resolve", strings.NewReader(`{"lead_ids":[]}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxTenant,
		bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}))
	rec := httptest.NewRecorder()
	s.resolveLeadPhones(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store: status = %d, want 503", rec.Code)
	}
}

// Handler: malformed JSON is a 400.
func TestResolveLeadPhonesBadJSON(t *testing.T) {
	s := &server{d: Deps{Logger: zap.NewNop()}}
	req := httptest.NewRequest(http.MethodPost, "/v1/leads/resolve", strings.NewReader(`{`))
	req = req.WithContext(context.WithValue(req.Context(), ctxTenant,
		bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}))
	rec := httptest.NewRecorder()
	s.resolveLeadPhones(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON: status = %d, want 400", rec.Code)
	}
}

// Empty input resolves to an empty (non-nil) phones map — the response is
// always {"phones":{...}}, never {"phones":null}.
func TestResolveLeadPhoneMapEmpty(t *testing.T) {
	phones, err := resolveLeadPhoneMap(context.Background(), &fakeLeadGetter{leads: map[string]store.Lead{}}, uuid.New(), nil)
	if err != nil {
		t.Fatalf("resolve empty: %v", err)
	}
	if phones == nil || len(phones) != 0 {
		t.Fatalf("phones = %v, want empty non-nil map", phones)
	}
	raw, err := json.Marshal(map[string]any{"phones": phones})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"phones":{}}` {
		t.Fatalf("empty response = %s, want {\"phones\":{}}", raw)
	}
}
