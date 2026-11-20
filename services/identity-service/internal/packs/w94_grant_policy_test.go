package packs

// SPEC-W94 tests: the verificationGrants pack policy block — nil when
// absent (inherit global env), validated when present (ttlHours 0..720),
// projected (deep-copied) into the tenant-facing Summary.
// Redirection: dropping the validation lets the out-of-range case load;
// dropping the Summary projection fails the projection assertion.

import (
	"strings"
	"testing"
)

func TestGrantPolicyValidation_W94(t *testing.T) {
	// Absent block = nil policy (inherit the global env behavior).
	reg, err := Load(writePack(t, "test.yaml", validPack))
	if err != nil {
		t.Fatalf("absent verificationGrants must load: %v", err)
	}
	p, ok := reg.Get("test-pack")
	if !ok {
		t.Fatal("pack missing after load")
	}
	if p.VerificationGrants != nil {
		t.Fatalf("VerificationGrants = %+v, want nil", p.VerificationGrants)
	}
	if sum := p.Summary(nil); sum.VerificationGrants != nil {
		t.Fatalf("Summary.VerificationGrants = %+v, want nil", sum.VerificationGrants)
	}

	// Present block loads, fields land, Summary projects a DEEP COPY.
	content := validPack + `verificationGrants:
  enabled: true
  ttlHours: 8
`
	reg, err = Load(writePack(t, "test.yaml", content))
	if err != nil {
		t.Fatalf("verificationGrants block must load: %v", err)
	}
	p, _ = reg.Get("test-pack")
	if p.VerificationGrants == nil || !p.VerificationGrants.Enabled || p.VerificationGrants.TTLHours != 8 {
		t.Fatalf("VerificationGrants = %+v", p.VerificationGrants)
	}
	sum := p.Summary(nil)
	if sum.VerificationGrants == nil || !sum.VerificationGrants.Enabled || sum.VerificationGrants.TTLHours != 8 {
		t.Fatalf("Summary.VerificationGrants = %+v", sum.VerificationGrants)
	}
	if sum.VerificationGrants == p.VerificationGrants {
		t.Fatal("Summary must deep-copy the policy (shared pointer)")
	}

	// ttlHours 0 = no override — valid.
	content = validPack + `verificationGrants:
  enabled: false
`
	if _, err := Load(writePack(t, "test.yaml", content)); err != nil {
		t.Fatalf("ttlHours 0 (no override) must load: %v", err)
	}

	// Out-of-range ttlHours rejected verbatim.
	for _, bad := range []string{"-1", "9999"} {
		content = validPack + `verificationGrants:
  enabled: true
  ttlHours: ` + bad + "\n"
		_, err := Load(writePack(t, "test.yaml", content))
		if err == nil {
			t.Fatalf("ttlHours %s must be rejected", bad)
		}
		want := "verificationGrants.ttlHours " + bad + " must be between 0 and 720"
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want substring %q", err, want)
		}
	}
}
