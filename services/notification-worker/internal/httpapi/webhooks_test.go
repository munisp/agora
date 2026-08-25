package httpapi

import (
	"net"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/notification-worker/internal/store"
	"go.uber.org/zap"
)

// In-memory WebhookStore for REST tests.
type fakeWebhookStore struct {
	subs       map[uuid.UUID]store.WebhookSubscription
	deliveries map[uuid.UUID][]store.WebhookDelivery
}

func newFakeWebhookStore() *fakeWebhookStore {
	return &fakeWebhookStore{subs: map[uuid.UUID]store.WebhookSubscription{}, deliveries: map[uuid.UUID][]store.WebhookDelivery{}}
}

func (f *fakeWebhookStore) CreateSubscription(_ context.Context, sub *store.WebhookSubscription) error {
	if sub.ID == uuid.Nil {
		sub.ID = uuid.New()
	}
	sub.Active = true
	sub.CreatedAt = time.Now()
	f.subs[sub.ID] = *sub
	return nil
}

func (f *fakeWebhookStore) ListSubscriptions(_ context.Context, tenantID uuid.UUID) ([]store.WebhookSubscription, error) {
	var out []store.WebhookSubscription
	for _, s := range f.subs {
		if s.TenantID == tenantID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (f *fakeWebhookStore) GetSubscription(_ context.Context, tenantID, id uuid.UUID) (store.WebhookSubscription, error) {
	s, ok := f.subs[id]
	if !ok || s.TenantID != tenantID {
		return store.WebhookSubscription{}, store.ErrNotFound
	}
	return s, nil
}

func (f *fakeWebhookStore) DeleteSubscription(_ context.Context, tenantID, id uuid.UUID) error {
	s, ok := f.subs[id]
	if !ok || s.TenantID != tenantID {
		return store.ErrNotFound
	}
	delete(f.subs, id)
	return nil
}

func (f *fakeWebhookStore) ListDeliveries(_ context.Context, tenantID, subID uuid.UUID, _ int) ([]store.WebhookDelivery, error) {
	var out []store.WebhookDelivery
	for _, d := range f.deliveries[subID] {
		if d.TenantID == tenantID {
			out = append(out, d)
		}
	}
	return out, nil
}

func newWebhookTestServer(t *testing.T, signingRequired bool) (http.Handler, *fakeWebhookStore, uuid.UUID) {
	t.Helper()
	st := newFakeWebhookStore()
	tenantID := uuid.New()
	srv := &Server{
		Log:      zap.NewNop(),
		Webhooks: st,
		// SSRF guard with a stubbed resolver (all hosts → public IP); the
		// DNS/parse paths are covered by urlcheck_test.go.
		WebhookURLValidator: &URLValidator{LookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}},
		ResolveTenant: func(_ context.Context, slug string) (TenantRef, error) {
			if slug != "acme" {
				return TenantRef{}, errors.New("unknown tenant")
			}
			return TenantRef{ID: tenantID, Slug: slug}, nil
		},
		WebhookSigningRequired: signingRequired,
	}
	return NewRouter(srv), st, tenantID
}

func doReq(t *testing.T, h http.Handler, method, path, tenant string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if tenant != "" {
		req.Header.Set("X-Tenant-Slug", tenant)
		// K1: the gateway injects the verified tenant_slugs claim; the
		// requested slug must be a member (C1 binding).
		req.Header.Set("X-Tenant-Slugs", "acme,globex,"+tenant)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestWebhookSubscriptionLifecycle(t *testing.T) {
	h, st, tenantID := newWebhookTestServer(t, false)

	// Create without a secret → one is generated and shown once.
	code, resp := doReq(t, h, "POST", "/v1/webhooks", "acme", map[string]any{
		"url":    "https://receiver.example/hook",
		"events": []string{"com.opendesk.booking.*"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, resp)
	}
	secret, _ := resp["secret"].(string)
	if len(secret) != 48 {
		t.Fatalf("generated secret = %q, want 48 hex chars", secret)
	}
	id, _ := resp["id"].(string)
	if id == "" {
		t.Fatal("no id in create response")
	}

	// List masks the secret.
	code, resp = doReq(t, h, "GET", "/v1/webhooks", "acme", nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	list, _ := resp["webhooks"].([]any)
	if len(list) != 1 {
		t.Fatalf("webhooks = %d, want 1", len(list))
	}
	item, _ := list[0].(map[string]any)
	if _, leaked := item["secret"]; leaked {
		t.Fatal("list response leaks the secret")
	}
	if item["secret_set"] != true {
		t.Fatalf("secret_set = %v", item["secret_set"])
	}

	// Deliveries endpoint (empty history).
	code, resp = doReq(t, h, "GET", "/v1/webhooks/"+id+"/deliveries", "acme", nil)
	if code != http.StatusOK {
		t.Fatalf("deliveries = %d", code)
	}

	// Seed one delivery and read it back.
	subID := uuid.MustParse(id)
	st.deliveries[subID] = []store.WebhookDelivery{{
		ID: uuid.New(), SubID: subID, TenantID: tenantID, EventType: "com.opendesk.booking.BookingCreated",
		Status: "delivered", Attempts: 1,
	}}
	code, resp = doReq(t, h, "GET", "/v1/webhooks/"+id+"/deliveries", "acme", nil)
	if code != http.StatusOK {
		t.Fatalf("deliveries = %d", code)
	}
	dels, _ := resp["deliveries"].([]any)
	if len(dels) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(dels))
	}

	// Delete.
	code, _ = doReq(t, h, "DELETE", "/v1/webhooks/"+id, "acme", nil)
	if code != http.StatusOK {
		t.Fatalf("delete = %d", code)
	}
	code, _ = doReq(t, h, "DELETE", "/v1/webhooks/"+id, "acme", nil)
	if code != http.StatusNotFound {
		t.Fatalf("re-delete = %d, want 404", code)
	}
}

func TestWebhookTenantScoping(t *testing.T) {
	h, _, _ := newWebhookTestServer(t, false)
	code, resp := doReq(t, h, "POST", "/v1/webhooks", "acme", map[string]any{
		"url": "https://receiver.example/hook", "events": []string{"*"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, resp)
	}
	id, _ := resp["id"].(string)

	// Missing tenant header → 400; unknown tenant → 404.
	code, _ = doReq(t, h, "GET", "/v1/webhooks", "", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("no tenant = %d, want 400", code)
	}
	code, _ = doReq(t, h, "GET", "/v1/webhooks", "globex", nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown tenant = %d, want 404", code)
	}
	// K1 binding: a slug outside the verified X-Tenant-Slugs claim → 403.
	code, _ = doReq(t, h, "GET", "/v1/webhooks", "acme", nil)
	if code != http.StatusOK {
		t.Fatalf("bound tenant = %d, want 200", code)
	}
	// Another tenant's subscription id is invisible.
	code, _ = doReq(t, h, "GET", "/v1/webhooks/"+id+"/deliveries", "globex", nil)
	if code != http.StatusNotFound {
		t.Fatalf("cross-tenant deliveries = %d, want 404", code)
	}
}

func TestWebhookTenantBindingForbidden(t *testing.T) {
	h, _, _ := newWebhookTestServer(t, false)
	req := httptest.NewRequest("GET", "/v1/webhooks", nil)
	req.Header.Set("X-Tenant-Slug", "acme")
	req.Header.Set("X-Tenant-Slugs", "other-tenant") // acme NOT in the claim
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unbound tenant = %d, want 403", rec.Code)
	}
	// No X-Tenant-Slugs at all (no gateway claim) fails closed.
	req = httptest.NewRequest("GET", "/v1/webhooks", nil)
	req.Header.Set("X-Tenant-Slug", "acme")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing tenant claim = %d, want 403 (fail-closed)", rec.Code)
	}
}

func TestWebhookCreateValidation(t *testing.T) {
	h, _, _ := newWebhookTestServer(t, false)
	for _, body := range []map[string]any{
		{"url": "", "events": []string{"*"}},
		{"url": "ftp://x", "events": []string{"*"}},
		{"url": "https://x.example", "events": []string{}},
		{"url": "https://x.example"},
	} {
		if code, _ := doReq(t, h, "POST", "/v1/webhooks", "acme", body); code != http.StatusBadRequest {
			t.Fatalf("create %v = %d, want 400", body, code)
		}
	}
}

func TestWebhookSigningRequired(t *testing.T) {
	h, _, _ := newWebhookTestServer(t, true)
	// Without a secret → 400 when signing is required.
	code, _ := doReq(t, h, "POST", "/v1/webhooks", "acme", map[string]any{
		"url": "https://x.example", "events": []string{"*"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("no-secret create = %d, want 400 (signing required)", code)
	}
	// With an explicit secret → 201 and the secret is echoed once.
	code, resp := doReq(t, h, "POST", "/v1/webhooks", "acme", map[string]any{
		"url": "https://x.example", "events": []string{"*"}, "secret": "my-secret",
	})
	if code != http.StatusCreated || resp["secret"] != "my-secret" {
		t.Fatalf("create = %d %v", code, resp)
	}
}
