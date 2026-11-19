package packs

// SPEC-W95 C5-B tests: bookingPolicy.intakeCutoffHours — absent = 0
// (worker default), validated 0..720 when present, projected into the
// tenant-facing Summary (booking-service's resolver mirror reads it there).
// Redirection: dropping the validation lets the out-of-range cases load;
// dropping the field fails the projection assertion.

import (
	"strings"
	"testing"
)

// w95PackWithCutoff splices intakeCutoffHours into validPack's flow-style
// bookingPolicy line (the only anchor is the closing `24}` pair).
func w95PackWithCutoff(cutoff string) string {
	return strings.Replace(validPack,
		"cancellationWindowHours: 24}",
		"cancellationWindowHours: 24, intakeCutoffHours: "+cutoff+"}", 1)
}

func TestIntakeCutoffHoursValidation_W95(t *testing.T) {
	// Absent field: loads, zero value, Summary projects zero.
	reg, err := Load(writePack(t, "test.yaml", validPack))
	if err != nil {
		t.Fatalf("absent intakeCutoffHours must load: %v", err)
	}
	p, ok := reg.Get("test-pack")
	if !ok {
		t.Fatal("pack missing after load")
	}
	if p.BookingPolicy.IntakeCutoffHours != 0 {
		t.Fatalf("IntakeCutoffHours = %d, want 0", p.BookingPolicy.IntakeCutoffHours)
	}
	if sum := p.Summary(nil); sum.BookingPolicy.IntakeCutoffHours != 0 {
		t.Fatalf("Summary.IntakeCutoffHours = %d, want 0", sum.BookingPolicy.IntakeCutoffHours)
	}

	// Present field loads and projects into the Summary.
	reg, err = Load(writePack(t, "test.yaml", w95PackWithCutoff("6")))
	if err != nil {
		t.Fatalf("intakeCutoffHours 6 must load: %v", err)
	}
	p, _ = reg.Get("test-pack")
	if p.BookingPolicy.IntakeCutoffHours != 6 {
		t.Fatalf("IntakeCutoffHours = %d, want 6", p.BookingPolicy.IntakeCutoffHours)
	}
	if sum := p.Summary(nil); sum.BookingPolicy.IntakeCutoffHours != 6 {
		t.Fatalf("Summary.IntakeCutoffHours = %d, want 6", sum.BookingPolicy.IntakeCutoffHours)
	}

	// Explicit 0 = worker default — valid.
	if _, err := Load(writePack(t, "test.yaml", w95PackWithCutoff("0"))); err != nil {
		t.Fatalf("intakeCutoffHours 0 must load: %v", err)
	}

	// Out-of-range rejected verbatim.
	for _, bad := range []string{"-1", "721"} {
		_, err := Load(writePack(t, "test.yaml", w95PackWithCutoff(bad)))
		if err == nil {
			t.Fatalf("intakeCutoffHours %s must be rejected", bad)
		}
		want := "bookingPolicy.intakeCutoffHours " + bad + " must be between 0 and 720"
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want substring %q", err, want)
		}
	}
}
