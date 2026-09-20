package campaignstudio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// SPEC-W45 U6: assignment FAILS OPEN to the control arm (champion) on
// 404/unset/down — the send rail must never be gated on the registry;
// outcome reporting FAILS CLOSED (any failure is an error).

func TestAssignVariantFailOpen(t *testing.T) {
	experimentID, tenantID := uuid.New(), uuid.New()
	cases := []struct {
		name   string
		server *httptest.Server
	}{
		{"404 (registry may not serve the POST assignment form)", httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))},
		{"500 registry error", httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))},
		{"200 with unknown arm", httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{"arm": "variant-z"})
		}))},
		{"200 with malformed body", httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer tc.server.Close()
			c := &RegistryClient{BaseURL: tc.server.URL}
			if arm := c.AssignVariant(context.Background(), experimentID, tenantID, "person-1"); arm != ControlArm {
				t.Fatalf("arm = %q, want fail-open %q", arm, ControlArm)
			}
		})
	}
	t.Run("unset URL", func(t *testing.T) {
		c := &RegistryClient{}
		if arm := c.AssignVariant(context.Background(), experimentID, tenantID, "person-1"); arm != ControlArm {
			t.Fatalf("arm = %q, want %q", arm, ControlArm)
		}
	})
	t.Run("nil client", func(t *testing.T) {
		var c *RegistryClient
		if arm := c.AssignVariant(context.Background(), experimentID, tenantID, "person-1"); arm != ControlArm {
			t.Fatalf("arm = %q, want %q", arm, ControlArm)
		}
	})
	t.Run("registry down (connection refused)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.Close() // immediately unreachable
		c := &RegistryClient{BaseURL: srv.URL}
		if arm := c.AssignVariant(context.Background(), experimentID, tenantID, "person-1"); arm != ControlArm {
			t.Fatalf("arm = %q, want %q", arm, ControlArm)
		}
	})
}

func TestAssignVariantHappyPath(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody assignmentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Internal-Token")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]string{"arm": ChallengerArm})
	}))
	defer srv.Close()
	experimentID, tenantID := uuid.New(), uuid.New()
	c := &RegistryClient{BaseURL: srv.URL, InternalToken: "sekret"}
	if arm := c.AssignVariant(context.Background(), experimentID, tenantID, "enroll-1"); arm != ChallengerArm {
		t.Fatalf("arm = %q, want challenger from the registry", arm)
	}
	if gotAuth != "sekret" {
		t.Fatalf("X-Internal-Token = %q, want the internal token", gotAuth)
	}
	if !strings.HasSuffix(gotPath, "/v1/registry/experiments/"+experimentID.String()+"/assignment") {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody.TenantID != tenantID || gotBody.PersonID != "enroll-1" {
		t.Fatalf("body = %+v", gotBody)
	}
}

func TestReportOutcomeFailClosed(t *testing.T) {
	experimentID, tenantID := uuid.New(), uuid.New()

	t.Run("unset URL errors", func(t *testing.T) {
		c := &RegistryClient{}
		if err := c.ReportOutcome(context.Background(), experimentID, tenantID, "p", ControlArm, true); err == nil {
			t.Fatal("want error for unset registry (fail closed)")
		}
	})
	t.Run("invalid arm errors client-side", func(t *testing.T) {
		c := &RegistryClient{BaseURL: "http://127.0.0.1:1"}
		if err := c.ReportOutcome(context.Background(), experimentID, tenantID, "p", "variant-z", true); err == nil {
			t.Fatal("want error for arm outside champion|challenger")
		}
	})
	t.Run("non-2xx errors", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
		}))
		defer srv.Close()
		c := &RegistryClient{BaseURL: srv.URL}
		if err := c.ReportOutcome(context.Background(), experimentID, tenantID, "p", ChallengerArm, false); err == nil {
			t.Fatal("want error for registry 422 (fail closed)")
		}
	})
	t.Run("transport error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.Close()
		c := &RegistryClient{BaseURL: srv.URL}
		if err := c.ReportOutcome(context.Background(), experimentID, tenantID, "p", ControlArm, true); err == nil {
			t.Fatal("want transport error (fail closed)")
		}
	})
}

// The neutral report: prediction fields (1, 0.5) so a marketing conversion
// never biases the arm curves; the signal travels in true_label.
func TestReportOutcomeNeutralShape(t *testing.T) {
	var got outcomeRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Token") != "sekret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	experimentID, tenantID := uuid.New(), uuid.New()
	c := &RegistryClient{BaseURL: srv.URL, InternalToken: "sekret"}
	if err := c.ReportOutcome(context.Background(), experimentID, tenantID, "enroll-9", ChallengerArm, true); err != nil {
		t.Fatalf("report: %v", err)
	}
	if got.AssignedArm != ChallengerArm || got.PersonID != "enroll-9" || got.TenantID != tenantID {
		t.Fatalf("outcome = %+v", got)
	}
	if got.PredictedLabel != 1 || got.PredictedScore != 0.5 || got.TrueLabel != 1 {
		t.Fatalf("neutral report = %+v, want predicted (1, 0.5) with the signal in true_label=1", got)
	}
}
