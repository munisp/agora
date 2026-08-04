package consent

// Tests for the SPEC-W17 §8.8 is_synthetic erasure fast-path (Agent D).
// Fakes/harness are shared with consent_test.go (same package).

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestIsSyntheticSubject(t *testing.T) {
	cases := []struct {
		subject string
		want    bool
	}{
		// seed-tagged id patterns (SPEC-W17 §8.8)
		{"9f2b4c0a1d3e5f60718293a4b5c6d7e8f90123456789abcdef0123456789abcd", true}, // deterministic_id sha256 hex
		{"seed:customer:00001234", true},
		{"seed-agent:000123", true},
		// real subjects — must NOT fast-path
		{"+2348031234567", false}, // real-band phone: no phone-band auto-classification
		{"+2348012345678", false}, // W17 synthetic phone band, still not auto-classified (overlaps real allocations)
		{"c7b9f2a0-3c1e-4f5a-9b8d-1e2f3a4b5c6d", false},
		{"user@example.com", false},
		{"9F2B4C0A1D3E5F60718293A4B5C6D7E8F90123456789ABCDEF0123456789ABCD", false}, // uppercase hex is not the _lib output shape
		{"9f2b4c0a1d3e5f60", false}, // too short for a sha256 hex
		{"", false},
		{"   ", false},
	}
	for _, c := range cases {
		if got := IsSyntheticSubject(c.subject); got != c.want {
			t.Errorf("IsSyntheticSubject(%q) = %v, want %v", c.subject, got, c.want)
		}
	}
}

func TestEvaluateErasureEligibility(t *testing.T) {
	// Synthetic short-circuit: immediate regardless of the waiting period.
	syn := EvaluateErasureEligibility("seed:customer:00001234")
	if !syn.Immediate || !syn.Synthetic || syn.WaitingPeriod != 0 {
		t.Errorf("synthetic eligibility: %+v, want immediate+synthetic+no wait", syn)
	}

	// Real subject: waiting period applies — zero today (behaviour unchanged
	// from W12), so also immediate, but NOT flagged synthetic.
	real := EvaluateErasureEligibility("+2348031234567")
	if real.Synthetic {
		t.Errorf("real subject flagged synthetic: %+v", real)
	}
	if ErasureWaitingPeriod <= 0 && !real.Immediate {
		t.Errorf("real subject must be immediate while ErasureWaitingPeriod=0: %+v", real)
	}
}

// Handler-level: erasure of a seed-tagged subject takes the fast-path —
// immediate tombstone, response + CloudEvent carry synthetic=true.
func TestErasureSyntheticFastPath(t *testing.T) {
	h := newHarness()
	subject := "9f2b4c0a1d3e5f60718293a4b5c6d7e8f90123456789abcdef0123456789abcd"

	// Capture a consent for the synthetic subject first (64-hex deterministic
	// seed id — the shape seeded customers carry downstream).
	rec := h.do(http.MethodPost, "/v1/consents",
		`{"tenant":"acme","subject":"`+subject+`","purpose":"kyc"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("capture: status = %d, body %s", rec.Code, rec.Body)
	}

	rec = h.do(http.MethodPost, "/v1/consents/erasure",
		`{"tenant":"acme","subject":"`+subject+`","purpose":"kyc"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("erasure: status = %d, body %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["status"] != "erasure_recorded" || out["synthetic"] != true || out["eligibility"] != "immediate" {
		t.Errorf("fast-path response: %v", out)
	}
	if out["erased_records"] != float64(1) {
		t.Errorf("erased_records: %v", out)
	}

	if len(h.pub.events) != 1 {
		t.Fatalf("published events = %d, want 1", len(h.pub.events))
	}
	payload, _ := json.Marshal(h.pub.events[0].data)
	var ce map[string]any
	_ = json.Unmarshal(payload, &ce)
	data, _ := ce["data"].(map[string]any)
	if data["synthetic"] != true {
		t.Errorf("CloudEvent must carry synthetic=true for downstream fast-path: %v", data)
	}
}

// Handler-level: a real subject's erasure is unchanged — immediate (waiting
// period is zero) but flagged synthetic=false in response + event.
func TestErasureRealSubjectNotSynthetic(t *testing.T) {
	h := newHarness()
	h.captureOnce(t, "kyc") // subject +2348012345678 (harness fixture)

	rec := h.do(http.MethodPost, "/v1/consents/erasure",
		`{"tenant":"acme","subject":"+2348012345678","purpose":"kyc"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("erasure: status = %d, body %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["synthetic"] != false || out["status"] != "erasure_recorded" {
		t.Errorf("real-subject response: %v", out)
	}
	payload, _ := json.Marshal(h.pub.events[0].data)
	var ce map[string]any
	_ = json.Unmarshal(payload, &ce)
	data, _ := ce["data"].(map[string]any)
	if data["synthetic"] != false {
		t.Errorf("CloudEvent synthetic flag: %v", data)
	}
}
