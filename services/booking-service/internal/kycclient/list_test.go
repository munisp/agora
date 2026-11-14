package kycclient

// SPEC-W65 Scope 2 (D): ListDocuments unit tests — query passthrough
// contract (review_state/limit/offset), auth/tenant headers, fail-closed
// configuration posture and the ErrUnavailable mapping. All network is
// httptest-local.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// capturedList records what the fake kyc-service list endpoint received.
type capturedList struct {
	token, tenantHdr string
	method, path     string
	rawQuery         string
}

func fakeKYCList(t *testing.T, status int, respBody string, cap *capturedList) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.method = r.Method
		cap.path = r.URL.Path
		cap.rawQuery = r.URL.RawQuery
		cap.token = r.Header.Get("X-Internal-Token")
		cap.tenantHdr = r.Header.Get("X-Tenant-Slug")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The list call hits GET /v1/kyc/documents with the internauth + tenant
// headers and forwards the query verbatim (defaults omitted so kyc-service
// applies its own).
func TestListDocumentsContract(t *testing.T) {
	var cap capturedList
	srv := fakeKYCList(t, http.StatusOK,
		`{"items":[{"document_id":"d1","subject_phone":"+2348012345678","doc_type":"kyc_passport","status":"extracted","review_state":"pending_review"}],"limit":25,"offset":50}`, &cap)
	tenantID := uuid.New()
	c := New(srv.URL, "sekrit", zap.NewNop())

	res, err := c.ListDocuments(context.Background(), tenantID, "pending_review", 25, 50)
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/v1/kyc/documents" {
		t.Fatalf("upstream call = %s %s", cap.method, cap.path)
	}
	if cap.rawQuery != "limit=25&offset=50&review_state=pending_review" {
		t.Fatalf("raw query = %q", cap.rawQuery)
	}
	if cap.token != "sekrit" {
		t.Fatalf("X-Internal-Token = %q", cap.token)
	}
	if cap.tenantHdr != tenantID.String() {
		t.Fatalf("X-Tenant-Slug = %q", cap.tenantHdr)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d", res.Status)
	}
	var env struct {
		Items []map[string]any `json:"items"`
		Limit int              `json:"limit"`
	}
	if err := json.Unmarshal(res.Body, &env); err != nil || len(env.Items) != 1 || env.Limit != 25 {
		t.Fatalf("envelope decode: %v %s", err, res.Body)
	}
	if env.Items[0]["subject_phone"] != "+2348012345678" {
		t.Fatalf("subject_phone projection missing: %v", env.Items[0])
	}
}

// Empty filter + zero limit/offset send NO query params (kyc-service
// defaults apply: limit=50, newest-first).
func TestListDocumentsDefaultsOmitted(t *testing.T) {
	var cap capturedList
	srv := fakeKYCList(t, http.StatusOK, `{"items":[],"limit":50,"offset":0}`, &cap)
	c := New(srv.URL, "sekrit", zap.NewNop())
	if _, err := c.ListDocuments(context.Background(), uuid.New(), "", 0, 0); err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if cap.rawQuery != "" {
		t.Fatalf("defaults must be omitted, got query %q", cap.rawQuery)
	}
	// Whitespace-only filter is treated as empty.
	if _, err := c.ListDocuments(context.Background(), uuid.New(), "  ", 0, 0); err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if cap.rawQuery != "" {
		t.Fatalf("blank filter must be omitted, got query %q", cap.rawQuery)
	}
}

// Fail-closed: unconfigured client => ErrNotConfigured; dead upstream =>
// ErrUnavailable; downstream non-2xx passes through verbatim.
func TestListDocumentsErrors(t *testing.T) {
	if _, err := New("", "sekrit", nil).ListDocuments(context.Background(), uuid.New(), "", 0, 0); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("url unset: want ErrNotConfigured, got %v", err)
	}
	if _, err := New("http://kyc.invalid", "", nil).ListDocuments(context.Background(), uuid.New(), "", 0, 0); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("token unset: want ErrNotConfigured, got %v", err)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close() // connection-refused
	if _, err := New(dead.URL, "sekrit", nil).ListDocuments(context.Background(), uuid.New(), "", 0, 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("dead upstream: want ErrUnavailable, got %v", err)
	}

	var cap capturedList
	srv := fakeKYCList(t, http.StatusBadRequest, `{"error":"limit must be an integer in 1..200"}`, &cap)
	res, err := New(srv.URL, "sekrit", zap.NewNop()).ListDocuments(context.Background(), uuid.New(), "", 999, 0)
	if err != nil {
		t.Fatalf("downstream 400 must NOT be an error: %v", err)
	}
	if res.Status != http.StatusBadRequest || string(res.Body) != `{"error":"limit must be an integer in 1..200"}` {
		t.Fatalf("downstream status/body must pass through verbatim: %d %s", res.Status, res.Body)
	}
}
