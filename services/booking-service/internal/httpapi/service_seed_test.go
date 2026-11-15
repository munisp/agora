package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
	"go.uber.org/zap"
)

// requireOrService: onboarding's ApplyIndustryPack seeds offerings as a service (no user). The internal token admits
// it; anything else still needs a user with the Permify permission.
func TestRequireOrServiceAdmitsOnlyTheInternalToken(t *testing.T) {
	s := &server{d: Deps{InternalToken: "svc-tok", Authz: failingAuthz{err: errors.New("deny")}, Logger: zap.NewNop()}}
	run := func(token string) (int, bool) {
		reached := false
		h := s.requireOrService("manage_catalog")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
		r := httptest.NewRequest(http.MethodPost, "/v1/offerings", nil)
		r = r.WithContext(context.WithValue(r.Context(), ctxTenant, bookingops.TenantInfo{ID: uuid.New(), Slug: "harbour"}))
		if token != "" {
			r.Header.Set("X-Internal-Token", token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code, reached
	}
	if _, reached := run("svc-tok"); !reached {
		t.Fatal("the booking internal token must admit the onboarding seed")
	}
	if code, reached := run("wrong"); reached || code != http.StatusUnauthorized {
		t.Fatalf("a wrong token must fall through to the user gate (got %d, reached %v)", code, reached)
	}
	if code, reached := run(""); reached || code != http.StatusUnauthorized {
		t.Fatalf("no token and no user must be refused (got %d, reached %v)", code, reached)
	}
	// Token unset on the server: never admits by token (fail closed).
	s.d.InternalToken = ""
	if _, reached := run("svc-tok"); reached {
		t.Fatal("an unset server token must not admit anyone by token")
	}
}
