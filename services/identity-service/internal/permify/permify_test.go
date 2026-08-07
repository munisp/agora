package permify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// SPEC-W34 GF15: per-tenant Permify schema provisioning.

// permifyMock records the requests identity-service makes against the
// Permify HTTP API.
type permifyMock struct {
	mu        sync.Mutex
	schemas   []string // raw schema payloads posted to schemas/write
	created   []string // tenant ids posted to tenants/create
	failWrite bool     // simulate a Permify schema-write outage
	existing  bool     // simulate tenants/create -> 409 (already exists)
}

func (m *permifyMock) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tenants/create", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.created = append(m.created, body.ID)
		existing := m.existing
		m.mu.Unlock()
		if existing {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/schemas/write") {
			var body struct {
				Schema string `json:"schema"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.schemas = append(m.schemas, body.Schema)
			fail := m.failWrite
			m.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("schema store unavailable"))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"schema_version":"v1"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	return mux
}

func TestCreateTenantWritesSchema(t *testing.T) {
	mock := &permifyMock{}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	c := NewHTTPClient(srv.URL)
	tenantID := "3f6b1f9e-7c2a-4d3e-9a11-abcdef012345"
	if err := c.CreateTenant(context.Background(), tenantID, "acme"); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.created) != 1 || mock.created[0] != tenantID {
		t.Errorf("tenants/create calls = %v, want [%s]", mock.created, tenantID)
	}
	if len(mock.schemas) != 1 {
		t.Fatalf("schemas/write calls = %d, want exactly 1", len(mock.schemas))
	}
	if mock.schemas[0] != Schema {
		t.Errorf("schemas/write payload mismatch:\n got %q\nwant %q", mock.schemas[0], Schema)
	}
	if !strings.Contains(mock.schemas[0], "entity organization") {
		t.Errorf("written schema missing the organization entity")
	}
}

func TestCreateTenantExistingStillWritesSchema(t *testing.T) {
	// 409 (already provisioned) must NOT skip the schema write — pre-GF15
	// tenants self-heal on the next ensure-permify call.
	mock := &permifyMock{existing: true}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	c := NewHTTPClient(srv.URL)
	if err := c.CreateTenant(context.Background(), "t1", "bootstrap"); err != nil {
		t.Fatalf("CreateTenant (existing): %v", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.schemas) != 1 || mock.schemas[0] != Schema {
		t.Errorf("existing tenant must still get the schema, calls=%d", len(mock.schemas))
	}
}

func TestCreateTenantSchemaWriteFailureFailsClosed(t *testing.T) {
	mock := &permifyMock{failWrite: true}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	c := NewHTTPClient(srv.URL)
	err := c.CreateTenant(context.Background(), "t-x", "broken")
	if err == nil {
		t.Fatalf("schema write failure must propagate (fail-closed)")
	}
	if !strings.Contains(err.Error(), "write schema") {
		t.Errorf("error should identify the schema write, got: %v", err)
	}
}

// TestEmbeddedSchemaMatchesCanonical guards against drift between the
// embedded copy (this package) and the canonical infra/permify/schema.perm
// (also used by infra/permify/load-schema.sh for the bootstrap tenant).
// The canonical file is read at TEST time via the repo-relative path; the
// embedded copy's 3-line GF15 provenance header is excluded from the
// comparison.
func TestEmbeddedSchemaMatchesCanonical(t *testing.T) {
	canonicalPath := filepath.Join("..", "..", "..", "..", "infra", "permify", "schema.perm")
	canonical, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Skipf("canonical schema not reachable from test working dir: %v", err)
	}
	embedded := Schema
	const header = "// schema.perm — EMBEDDED COPY for identity-service (SPEC-W34 GF15)."
	if !strings.HasPrefix(embedded, header) {
		t.Fatalf("embedded schema lost its GF15 provenance header")
	}
	embedded = strings.TrimPrefix(embedded, header)
	// Drop the remaining 2 header lines (canonical-source note + assertion
	// note), keep the canonical body byte-for-byte.
	parts := strings.SplitN(embedded, "\n", 4)
	if len(parts) != 4 {
		t.Fatalf("embedded schema header malformed")
	}
	embedded = parts[3]
	if embedded != string(canonical) {
		t.Errorf("embedded schema.perm drifted from canonical infra/permify/schema.perm\n" +
			"— update services/identity-service/internal/permify/schema.perm")
	}
}
