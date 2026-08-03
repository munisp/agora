package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// SPEC-W12 Agent B: /v1/dnd REST tests (in-memory fake store; the Postgres
// semantics themselves are covered by internal/store/dnd_test.go).

type fakeDNDStore struct {
	global map[string]string // phone → source
	tenant map[string]map[string]bool
}

func newFakeDNDStore() *fakeDNDStore {
	return &fakeDNDStore{global: map[string]string{}, tenant: map[string]map[string]bool{}}
}

func (f *fakeDNDStore) ImportGlobalDND(_ context.Context, phones []string, source string) (int, error) {
	if source == "" {
		source = "ncc2442"
	}
	inserted := 0
	for _, p := range phones {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := f.global[p]; !ok {
			inserted++
		}
		f.global[p] = source
	}
	return inserted, nil
}

func (f *fakeDNDStore) RemoveDND(_ context.Context, phone, tenantSlug string) (int64, error) {
	var removed int64
	if tenantSlug == "" {
		if _, ok := f.global[phone]; ok {
			delete(f.global, phone)
			removed++
		}
		for slug := range f.tenant {
			if f.tenant[slug][phone] {
				delete(f.tenant[slug], phone)
				removed++
			}
		}
		return removed, nil
	}
	if f.tenant[tenantSlug][phone] {
		delete(f.tenant[tenantSlug], phone)
		removed++
	}
	return removed, nil
}

func (f *fakeDNDStore) IsSuppressed(_ context.Context, tenantSlug, phone string) (bool, string, error) {
	if tenantSlug != "" && f.tenant[tenantSlug][phone] {
		return true, "tenant_optout", nil
	}
	if _, ok := f.global[phone]; ok {
		return true, "global_dnd", nil
	}
	return false, "", nil
}

func dndTestServer(t *testing.T, st DNDStore) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(&Server{Log: zap.NewNop(), DND: st}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDNDImportAndCheck(t *testing.T) {
	st := newFakeDNDStore()
	srv := dndTestServer(t, st)

	resp, err := http.Post(srv.URL+"/v1/dnd/import", "application/json",
		strings.NewReader(`{"phones":["+2348011111111","+2348022222222"]}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	resp.Body.Close()
	require.EqualValues(t, 2, body["received"])
	require.EqualValues(t, 2, body["imported"])

	// Re-import is idempotent.
	resp, err = http.Post(srv.URL+"/v1/dnd/import", "application/json",
		strings.NewReader(`{"phones":["+2348011111111"]}`))
	require.NoError(t, err)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	resp.Body.Close()
	require.EqualValues(t, 0, body["imported"])

	// Check: suppressed with the global reason.
	resp, err = http.Get(srv.URL + "/v1/dnd/check?phone=%2B2348011111111&tenant=acme")
	require.NoError(t, err)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	resp.Body.Close()
	require.Equal(t, true, body["suppressed"])
	require.Equal(t, "global_dnd", body["reason"])

	// Check: unknown number passes.
	resp, err = http.Get(srv.URL + "/v1/dnd/check?phone=%2B2348099999999")
	require.NoError(t, err)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	resp.Body.Close()
	require.Equal(t, false, body["suppressed"])
	require.Equal(t, "", body["reason"])
}

func TestDNDImportValidation(t *testing.T) {
	srv := dndTestServer(t, newFakeDNDStore())

	resp, err := http.Post(srv.URL+"/v1/dnd/import", "application/json", strings.NewReader(`not json`))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	resp, err = http.Post(srv.URL+"/v1/dnd/import", "application/json", strings.NewReader(`{"phones":[]}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

func TestDNDDeleteOptOutHonor(t *testing.T) {
	st := newFakeDNDStore()
	srv := dndTestServer(t, st)

	// Not on the registry → 404.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/dnd/+2348011111111", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()

	// Import, then honor the opt-out (removal).
	_, err = http.Post(srv.URL+"/v1/dnd/import", "application/json",
		strings.NewReader(`{"phones":["+2348011111111"]}`))
	require.NoError(t, err)

	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/v1/dnd/+2348011111111", nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	resp.Body.Close()
	require.EqualValues(t, 1, body["removed"])

	resp, err = http.Get(srv.URL + "/v1/dnd/check?phone=%2B2348011111111")
	require.NoError(t, err)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	resp.Body.Close()
	require.Equal(t, false, body["suppressed"], "removed number must pass the guard")
}

func TestDNDRoutes503WithoutStore(t *testing.T) {
	srv := dndTestServer(t, nil)

	resp, err := http.Get(srv.URL + "/v1/dnd/check?phone=%2B1")
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	resp.Body.Close()

	resp, err = http.Post(srv.URL+"/v1/dnd/import", "application/json", strings.NewReader(`{"phones":["+1"]}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/dnd/+1", nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	resp.Body.Close()
}
