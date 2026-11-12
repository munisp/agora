package geo

// SPEC-W52 Scope B: enrich client tests — no database. The A↔B contract
// fixture below is the exact response shape services/geo-service documents
// for GET /v1/geo/enrich (200 with nulls on a no-hit).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// w52TenantUUID is a canonical tenant uuid: the geo-service parses the
// X-Tenant-Slug header as a uuid and feeds it to the RLS GUC app.tenant_id,
// so the client must forward the uuid verbatim — a slug silently degrades
// the answer to reference boundaries only.
const w52TenantUUID = "3f6b2a90-7d6e-4f6a-9c1e-2f4a5b6c7d8e"

// w52EnrichFixture is the canonical A↔B contract body: state/lga/ward plus
// the boundary list. If this stops decoding, the contract broke.
const w52EnrichFixture = `{
  "state": "Lagos",
  "lga": "Eti-Osa",
  "ward": "Victoria Island I",
  "boundaries": [
    {"kind": "state", "name": "Lagos", "code": "NG-LA"},
    {"kind": "lga", "name": "Eti-Osa", "code": "NG-LA-EO"},
    {"kind": "ward", "name": "Victoria Island I", "code": "NG-LA-EO-VI1"}
  ]
}`

func TestW52EnrichDisabledWhenURLEmpty(t *testing.T) {
	c := NewEnrichClient("", nil)
	if c.Enabled() {
		t.Fatal("empty GEO_SERVICE_URL must disable the client")
	}
	if res := c.Enrich(context.Background(), w52TenantUUID, 6.5244, 3.3792); res != nil {
		t.Fatalf("disabled client returned %+v, want nil", res)
	}
	var nilClient *EnrichClient
	if nilClient.Enabled() {
		t.Fatal("nil client must report disabled")
	}
	if res := nilClient.Enrich(context.Background(), w52TenantUUID, 6.5, 3.3); res != nil {
		t.Fatalf("nil client returned %+v, want nil", res)
	}
}

func TestW52EnrichContractDecodeAndTenantHeader(t *testing.T) {
	var gotPath, gotQuery, gotTenantHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotTenantHeader = r.Header.Get("X-Tenant-Slug")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(w52EnrichFixture))
	}))
	defer srv.Close()

	c := NewEnrichClient(srv.URL+"/", nil) // trailing slash must be trimmed
	res := c.Enrich(context.Background(), w52TenantUUID, 6.5244, 3.3792)
	if res == nil {
		t.Fatal("enrich returned nil for a healthy geo-service")
	}
	if gotPath != "/v1/geo/enrich" {
		t.Fatalf("path = %q, want /v1/geo/enrich", gotPath)
	}
	if gotQuery != "lat=6.5244&lng=3.3792" {
		t.Fatalf("query = %q, want lat=6.5244&lng=3.3792", gotQuery)
	}
	// The header must carry the tenant uuid VERBATIM: the geo-service parses
	// it as a uuid for the RLS GUC app.tenant_id; anything else (a slug)
	// silently degrades the answer to reference boundaries only.
	if gotTenantHeader != w52TenantUUID {
		t.Fatalf("X-Tenant-Slug = %q, want the tenant uuid %q verbatim", gotTenantHeader, w52TenantUUID)
	}
	if _, err := uuid.Parse(gotTenantHeader); err != nil {
		t.Fatalf("X-Tenant-Slug = %q is not a uuid: %v", gotTenantHeader, err)
	}
	if res.State != "Lagos" || res.LGA != "Eti-Osa" || res.Ward != "Victoria Island I" {
		t.Fatalf("scalars: %+v", res)
	}
	if len(res.Boundaries) != 3 || res.Boundaries[1].Kind != "lga" ||
		res.Boundaries[1].Name != "Eti-Osa" || res.Boundaries[1].Code != "NG-LA-EO" {
		t.Fatalf("boundaries: %+v", res.Boundaries)
	}
	// Round-trip: the struct must marshal back to the same contract keys.
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"state", "lga", "ward", "boundaries"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("marshalled contract missing key %q: %s", k, raw)
		}
	}
}

func TestW52EnrichNoHitNulls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("X-Tenant-Slug"); h != "" {
			t.Errorf("empty/uuid.Nil tenant must omit X-Tenant-Slug, got %q", h)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":null,"lga":null,"ward":null,"boundaries":null}`))
	}))
	defer srv.Close()
	res := NewEnrichClient(srv.URL, nil).Enrich(context.Background(), "", 0, 0)
	if res == nil {
		t.Fatal("no-hit 200-with-nulls must still decode (nil is reserved for failures)")
	}
	if res.State != "" || res.LGA != "" || res.Ward != "" || len(res.Boundaries) != 0 {
		t.Fatalf("no-hit decode: %+v", res)
	}
	// uuid.Nil must omit the header too (reference-boundaries-only posture).
	res = NewEnrichClient(srv.URL, nil).Enrich(context.Background(), uuid.Nil.String(), 0, 0)
	if res == nil {
		t.Fatal("uuid.Nil tenant: no-hit 200-with-nulls must still decode")
	}
	if res.State != "" || res.LGA != "" || res.Ward != "" || len(res.Boundaries) != 0 {
		t.Fatalf("uuid.Nil tenant no-hit decode: %+v", res)
	}
}

func TestW52EnrichFailOpen(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"500 from geo-service", http.StatusInternalServerError, `{"error":"boom"}`},
		{"404-style miss", http.StatusNotFound, `{"error":"no hit"}`},
		{"undecodable body", http.StatusOK, `not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			if res := NewEnrichClient(srv.URL, nil).Enrich(context.Background(), w52TenantUUID, 6.5, 3.3); res != nil {
				t.Fatalf("got %+v, want nil (fail-open)", res)
			}
		})
	}
	// Connection refused (nothing listening).
	c := NewEnrichClient("http://127.0.0.1:1", nil)
	if res := c.Enrich(context.Background(), w52TenantUUID, 6.5, 3.3); res != nil {
		t.Fatalf("unreachable geo-service: got %+v, want nil", res)
	}
}

func TestW52EnrichTimeoutFailOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond) // longer than the shrunk test timeout
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(w52EnrichFixture))
	}))
	defer srv.Close()
	c := NewEnrichClient(srv.URL, nil)
	c.client.Timeout = 100 * time.Millisecond // shrink; production uses EnrichTimeout (3s)
	start := time.Now()
	if res := c.Enrich(context.Background(), w52TenantUUID, 6.5, 3.3); res != nil {
		t.Fatalf("slow geo-service: got %+v, want nil (timeout fail-open)", res)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout fail-open took %v — must stay bounded", d)
	}
}
