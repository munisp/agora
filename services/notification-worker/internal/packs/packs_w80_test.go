package packs

// SPEC-W80 C2 tests: the loader enforces identity-service's Validate()
// rigor plus fail-closed SHA256SUMS manifest verification (ported with
// attribution — see IntegrityManifestName in packs.go).

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validPackYAML = `id: testpack
displayName: Test Pack
terminology:
  offering: service
  team_member: stylist
  booking: appointment
  contact: client
agentPersona: "You are a test receptionist."
bookingPolicy:
  depositPercent: 25
  noShowFeeCents: 500
  phoneConfirmation: false
  intakeRequired: false
  cancellationWindowHours: 24
temporalWorkflow: SalonDepositWorkflow
offerings:
- name: Cut
  duration_min: 30
  buffer_min: 5
  price_cents: 2000
  capacity: 1
reminders:
  offsets: ["48h", "4h"]
  channels: ["email", "sms"]
knowledgeSeed:
- title: Hours
  body: "We are open 9-5."
dashboardLabels:
  bookingSingular: Appointment
  bookingPlural: Appointments
  customerTerm: Client
`

// writePack writes one pack file (and optionally a manifest covering the
// given entries) into a fresh temp dir.
func writePack(t *testing.T, yaml string, manifest map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	raw := []byte(yaml)
	if err := os.WriteFile(filepath.Join(dir, "testpack.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if manifest != nil {
		var lines string
		for name, hash := range manifest {
			lines += hash + "  " + name + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, IntegrityManifestName), []byte(lines), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func sha256hex(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

func TestLoad_ValidPackWithManifest(t *testing.T) {
	dir := writePack(t, validPackYAML, map[string]string{"testpack.yaml": sha256hex([]byte(validPackYAML))})
	r, err := Load(dir)
	if err != nil {
		t.Fatalf("valid pack + manifest must load: %v", err)
	}
	p, ok := r.Get("testpack")
	if !ok {
		t.Fatal("pack not registered")
	}
	if got := p.Reminders.Offsets; len(got) != 2 || got[0] != "48h" || got[1] != "4h" {
		t.Fatalf("reminders offsets not parsed: %v", got)
	}
	if got := p.Reminders.Channels; len(got) != 2 || got[0] != "email" {
		t.Fatalf("reminders channels not parsed: %v", got)
	}
}

func TestLoad_TamperedHashRejected(t *testing.T) {
	tampered := []byte(validPackYAML + "\n# tampered\n")
	dir := writePack(t, string(tampered), map[string]string{"testpack.yaml": sha256hex([]byte(validPackYAML))})
	if _, err := Load(dir); err == nil {
		t.Fatal("tampered pack content must be rejected by the manifest (fail-closed)")
	} else {
		t.Logf("tampered rejection: %v", err)
	}
}

func TestLoad_UnlistedPackRejected(t *testing.T) {
	// Manifest present but does not list the pack -> unverified file.
	dir := writePack(t, validPackYAML, map[string]string{"other.yaml": sha256hex([]byte("x"))})
	if _, err := Load(dir); err == nil {
		t.Fatal("pack absent from the manifest must be rejected (fail-closed)")
	} else {
		t.Logf("unlisted rejection: %v", err)
	}
}

func TestLoad_ManifestEntryAbsentFromDirRejected(t *testing.T) {
	dir := writePack(t, validPackYAML, map[string]string{
		"testpack.yaml": sha256hex([]byte(validPackYAML)),
		"ghost.yaml":    sha256hex([]byte("ghost")),
	})
	if _, err := Load(dir); err == nil {
		t.Fatal("manifest entry absent from dir must be rejected (incomplete install)")
	} else {
		t.Logf("absent-entry rejection: %v", err)
	}
}

func TestLoad_ManifestAbsentStillLoads(t *testing.T) {
	dir := writePack(t, validPackYAML, nil)
	if _, err := Load(dir); err != nil {
		t.Fatalf("absent manifest (dev posture) must not fail the load: %v", err)
	}
}

func TestLoad_UnknownWorkflowRejected(t *testing.T) {
	bad := replace(validPackYAML, "temporalWorkflow: SalonDepositWorkflow", "temporalWorkflow: TotallyMadeUpWorkflow")
	dir := writePack(t, bad, nil)
	if _, err := Load(dir); err == nil {
		t.Fatal("unknown temporalWorkflow must be rejected")
	} else {
		t.Logf("unknown workflow rejection: %v", err)
	}
}

func TestLoad_BadDepositPercentRejected(t *testing.T) {
	for _, pct := range []string{"depositPercent: 150", "depositPercent: -5"} {
		bad := replace(validPackYAML, "depositPercent: 25", pct)
		dir := writePack(t, bad, nil)
		if _, err := Load(dir); err == nil {
			t.Fatalf("%s must be rejected", pct)
		} else {
			t.Logf("depositPercent rejection: %v", err)
		}
	}
}

func TestLoad_MissingTerminologyKeyRejected(t *testing.T) {
	bad := replace(validPackYAML, "  contact: client\n", "")
	dir := writePack(t, bad, nil)
	if _, err := Load(dir); err == nil {
		t.Fatal("missing terminology.contact must be rejected")
	} else {
		t.Logf("terminology rejection: %v", err)
	}
}

func TestLoad_IndexJsonManifestEntryVerified(t *testing.T) {
	dir := writePack(t, validPackYAML, nil)
	indexRaw := []byte(`{"schemaVersion":1,"packs":[]}`)
	if err := os.WriteFile(filepath.Join(dir, "index.json"), indexRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]string{
		"testpack.yaml": sha256hex([]byte(validPackYAML)),
		"index.json":    sha256hex(indexRaw),
	}
	var lines string
	for name, hash := range manifest {
		lines += hash + "  " + name + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, IntegrityManifestName), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("non-yaml manifest entry (index.json) must be skip-but-verify, not rejected: %v", err)
	}
	// Tamper index.json -> fail-closed.
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(`{"tampered":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("tampered index.json must be rejected")
	} else {
		t.Logf("index.json tamper rejection: %v", err)
	}
}

// TestLoad_RealIndustriesDir loads the shipped repo packs (39 yamls +
// index.json + SHA256SUMS) through the hardened loader: every real pack
// must pass the ported Validate() and manifest verification.
func TestLoad_RealIndustriesDir(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "..", "industries")
	if _, err := os.Stat(filepath.Join(dir, IntegrityManifestName)); err != nil {
		t.Skipf("repo industries dir not available: %v", err)
	}
	r, err := Load(dir)
	if err != nil {
		t.Fatalf("real industries dir must load clean: %v", err)
	}
	if got := len(r.IDs()); got != 39 {
		t.Fatalf("expected 39 packs, got %d", got)
	}
}

func replace(s, old, new string) string { return strings.Replace(s, old, new, 1) }
