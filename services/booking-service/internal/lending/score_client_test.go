package lending

// Tests for the credit-bureau sidecar client (SPEC-W33 §3 B2, invariant
// I1): every failure mode fails CLOSED to the local rule Score() with
// heuristic provenance; the happy path passes the bureau blend through.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var sidecarTestSignals = ScoreSignals{TenureDays: 120, CompletedBookings: 5, RepaidLoans: 1}

// local want: (120/30)*3=12 + 5*4=20 + 1*10=10 = 42 (see TestScore).
const sidecarWantLocal = 42

func TestScoreWithSidecarUnsetEnvFallsBack(t *testing.T) {
	t.Setenv("CREDIT_BUREAU_URL", "")
	score, reasons, version := ScoreWithSidecar(context.Background(), sidecarTestSignals)
	if score != sidecarWantLocal || version != SidecarModelVersionHeuristic {
		t.Fatalf("unset env = (%d, %q), want (%d, %q)", score, version, sidecarWantLocal, SidecarModelVersionHeuristic)
	}
	if len(reasons) != 0 {
		t.Fatalf("reasons = %v, want empty", reasons)
	}
}

func TestScoreWithSidecarHappyPathPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/credit/score" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var req struct {
			Signals ScoreSignals `json:"signals"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Signals != sidecarTestSignals {
			t.Errorf("bad request body: %v %+v", err, req.Signals)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"score":640,"reasons":[],"model_version":"credit-ml-v1","ml_contribution":88,"feature_schema":"fv1"}`))
	}))
	defer srv.Close()
	t.Setenv("CREDIT_BUREAU_URL", srv.URL)

	d := ScoreDecisionWithSidecar(context.Background(), sidecarTestSignals)
	if d.Score != 640 || d.ModelVersion != "credit-ml-v1" || d.Source != SidecarSourceSidecar {
		t.Fatalf("happy path = %+v, want score 640 credit-ml-v1 sidecar", d)
	}
}

func TestScoreWithSidecarTenantHeaderForwarded(t *testing.T) {
	var gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = r.Header.Get("X-Tenant-Id")
		_, _ = w.Write([]byte(`{"score":640,"reasons":[],"model_version":"credit-ml-v1"}`))
	}))
	defer srv.Close()
	t.Setenv("CREDIT_BUREAU_URL", srv.URL)
	t.Setenv("CREDIT_BUREAU_TENANT_ID", "tenant-a")

	if _, _, version := ScoreWithSidecar(context.Background(), sidecarTestSignals); version != "credit-ml-v1" {
		t.Fatalf("version = %q, want credit-ml-v1", version)
	}
	if gotTenant != "tenant-a" {
		t.Fatalf("X-Tenant-Id = %q, want tenant-a", gotTenant)
	}
}

func TestScoreWithSidecarTimeoutFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(750 * time.Millisecond) // beyond the 500ms budget
		_, _ = w.Write([]byte(`{"score":640,"reasons":[],"model_version":"credit-ml-v1"}`))
	}))
	defer srv.Close()
	t.Setenv("CREDIT_BUREAU_URL", srv.URL)

	start := time.Now()
	score, _, version := ScoreWithSidecar(context.Background(), sidecarTestSignals)
	elapsed := time.Since(start)
	if score != sidecarWantLocal || version != SidecarModelVersionHeuristic {
		t.Fatalf("timeout = (%d, %q), want local fallback", score, version)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout fallback took %v, want bounded by the 500ms budget", elapsed)
	}
}

func TestScoreWithSidecar500FallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("CREDIT_BUREAU_URL", srv.URL)
	if score, _, version := ScoreWithSidecar(context.Background(), sidecarTestSignals); score != sidecarWantLocal || version != SidecarModelVersionHeuristic {
		t.Fatalf("500 = (%d, %q), want local fallback", score, version)
	}
}

func TestScoreWithSidecarMalformedJSONFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"score": not-json`))
	}))
	defer srv.Close()
	t.Setenv("CREDIT_BUREAU_URL", srv.URL)
	if score, _, version := ScoreWithSidecar(context.Background(), sidecarTestSignals); score != sidecarWantLocal || version != SidecarModelVersionHeuristic {
		t.Fatalf("malformed = (%d, %q), want local fallback", score, version)
	}
}

func TestScoreWithSidecarInvalidPayloadFallsBack(t *testing.T) {
	cases := map[string]string{
		"score above band":   `{"score":950,"reasons":[],"model_version":"credit-ml-v1"}`,
		"score below band":   `{"score":120,"reasons":[],"model_version":"credit-ml-v1"}`,
		"missing version":    `{"score":640,"reasons":[]}`,
		"wrong types":        `{"score":"640","model_version":1}`,
		"empty object":       `{}`,
		"valid-but-zero ver": `{"score":640,"reasons":[],"model_version":"  "}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			t.Setenv("CREDIT_BUREAU_URL", srv.URL)
			if score, _, version := ScoreWithSidecar(context.Background(), sidecarTestSignals); score != sidecarWantLocal || version != SidecarModelVersionHeuristic {
				t.Fatalf("%s = (%d, %q), want local fallback", name, score, version)
			}
		})
	}
}

func TestScoreWithSidecarUnreachableFallsBack(t *testing.T) {
	// Closed port: guaranteed connection refused.
	t.Setenv("CREDIT_BUREAU_URL", "http://127.0.0.1:1")
	if score, _, version := ScoreWithSidecar(context.Background(), sidecarTestSignals); score != sidecarWantLocal || version != SidecarModelVersionHeuristic {
		t.Fatalf("unreachable = (%d, %q), want local fallback", score, version)
	}
}
