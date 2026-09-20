package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// SPEC-W45 CODER-H exception: POST /v1/helpdesk/tickets accepts
// service-to-service callers via X-Internal-Token == BOOKING_INTERNAL_TOKEN
// (K2 pattern) so conversation-service escalation automation can open
// tickets; humans keep the Permify manage_bookings path. This unit-tests
// the alternative-auth middleware itself (no DB needed).
func TestRequireOrInternalToken(t *testing.T) {
	var gotActor string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotActor = userFrom(r.Context())
		w.WriteHeader(http.StatusCreated)
	})
	newReq := func(headers map[string]string) (*httptest.ResponseRecorder, *http.Request) {
		req := httptest.NewRequest(http.MethodPost, "/v1/helpdesk/tickets", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return httptest.NewRecorder(), req
	}

	t.Run("valid internal token stamps service actor and skips permify", func(t *testing.T) {
		s := &server{d: Deps{InternalToken: "secret-tok", Logger: zap.NewNop()}}
		rec, req := newReq(map[string]string{
			"X-Internal-Token": "secret-tok",
			"X-Service-Name":   "conversation-service",
		})
		s.requireOrInternalToken("manage_bookings")(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("valid token = %d (%s), want 201", rec.Code, rec.Body.String())
		}
		if gotActor != "service:conversation-service" {
			t.Fatalf("service actor = %q, want service:conversation-service", gotActor)
		}
	})

	t.Run("valid token without service name defaults to service:internal", func(t *testing.T) {
		s := &server{d: Deps{InternalToken: "secret-tok", Logger: zap.NewNop()}}
		rec, req := newReq(map[string]string{"X-Internal-Token": "secret-tok"})
		s.requireOrInternalToken("manage_bookings")(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated || gotActor != "service:internal" {
			t.Fatalf("code=%d actor=%q, want 201 service:internal", rec.Code, gotActor)
		}
	})

	t.Run("wrong token 401", func(t *testing.T) {
		s := &server{d: Deps{InternalToken: "secret-tok", Logger: zap.NewNop()}}
		rec, req := newReq(map[string]string{"X-Internal-Token": "nope"})
		s.requireOrInternalToken("manage_bookings")(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d, want 401", rec.Code)
		}
	})

	t.Run("fail closed 503 when server token unset", func(t *testing.T) {
		s := &server{d: Deps{InternalToken: "", Logger: zap.NewNop()}}
		rec, req := newReq(map[string]string{"X-Internal-Token": "anything"})
		s.requireOrInternalToken("manage_bookings")(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("unset server token = %d, want 503 (fail closed)", rec.Code)
		}
	})

	t.Run("no token header keeps the human Permify path (401 without subject)", func(t *testing.T) {
		s := &server{d: Deps{InternalToken: "secret-tok", Logger: zap.NewNop()}}
		rec, req := newReq(nil) // no headers, no ctxUser → require() 401s
		s.requireOrInternalToken("manage_bookings")(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("no token = %d, want 401 (human path untouched)", rec.Code)
		}
	})
}

// The helpdesk perms wrapper routes ONLY POST /v1/helpdesk/tickets through
// the internal-token alternative; every other method/path keeps the plain
// read/write Permify posture.
func TestHelpdeskPermsScopesAlternativeToTicketCreate(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	s := &server{d: Deps{InternalToken: "secret-tok", Logger: zap.NewNop()}}
	h := s.helpdeskPerms()(next)

	// POST create + valid token → allowed (204 from next).
	req := httptest.NewRequest(http.MethodPost, "/v1/helpdesk/tickets", nil)
	req.Header.Set("X-Internal-Token", "secret-tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST create with token = %d, want 204", rec.Code)
	}

	// Same token on another helpdesk route → NOT honored (human perms path:
	// no subject → 401).
	req = httptest.NewRequest(http.MethodPost, "/v1/helpdesk/sla-policies", nil)
	req.Header.Set("X-Internal-Token", "secret-tok")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("token on non-create route = %d, want 401 (perms path)", rec.Code)
	}
}
