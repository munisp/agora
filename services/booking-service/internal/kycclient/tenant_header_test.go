package kycclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestListSendsTheTenantTheWayKYCReadsIt(t *testing.T) {
	tenant := uuid.New()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Tenant-ID")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"limit":50,"offset":0}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "token", zap.NewNop())
	res, err := c.ListDocuments(context.Background(), tenant, "pending_review", 50, 0)
	if err != nil || res.Status != 200 {
		t.Fatalf("list: %v %+v", err, res)
	}
	if got != tenant.String() {
		t.Fatalf("X-Tenant-ID = %q, want the tenant id", got)
	}
}
