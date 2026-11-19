package packs

// SPEC-W92 C6 tests: the slaAnchor pack field — validation accepts the
// two anchors (and empty = legacy), rejects junk, and the field projects
// into the tenant-facing Summary. Redirection: dropping the validation
// lets the junk-anchor case load; dropping the Summary projection fails
// the projection assertion.

import (
	"strings"
	"testing"
)

func TestSLAAnchorValidation_W92(t *testing.T) {
	// Empty (absent) = legacy booking_start — loads fine.
	if _, err := Load(writePack(t, "test.yaml", validPack)); err != nil {
		t.Fatalf("absent slaAnchor must load: %v", err)
	}

	for _, anchor := range []string{"ticket_created", "booking_start"} {
		content := validPack + "slaAnchor: " + anchor + "\n"
		reg, err := Load(writePack(t, "test.yaml", content))
		if err != nil {
			t.Fatalf("slaAnchor %s must load: %v", anchor, err)
		}
		p, ok := reg.Get("test-pack")
		if !ok {
			t.Fatalf("pack missing after load (anchor %s)", anchor)
		}
		if p.SLAAnchor != anchor {
			t.Fatalf("SLAAnchor = %q, want %q", p.SLAAnchor, anchor)
		}
		// Projection into the tenant-facing Summary.
		sum := p.Summary(nil)
		if sum.SLAAnchor != anchor {
			t.Fatalf("Summary.SLAAnchor = %q, want %q", sum.SLAAnchor, anchor)
		}
	}

	// Junk anchor is a load-time validation error (fail fast at pack
	// authoring, not at escalation time).
	content := validPack + "slaAnchor: yesterday\n"
	if _, err := Load(writePack(t, "test.yaml", content)); err == nil ||
		!strings.Contains(err.Error(), "slaAnchor") {
		t.Fatalf("junk slaAnchor: err = %v, want slaAnchor validation error", err)
	}
}
