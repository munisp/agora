package packs

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validPackAlt is a second minimal valid pack (distinct id) so multi-pack
// fixture dirs do not trip the duplicate-id check.
const validPackAlt = `
id: test-pack-alt
displayName: Test Pack Alt
terminology: {offering: session, team_member: coach, booking: session, contact: client}
agentPersona: |
  You are an alt test persona.
bookingPolicy: {depositPercent: 10, noShowFeeCents: 500, phoneConfirmation: false, intakeRequired: false, cancellationWindowHours: 12}
temporalWorkflow: ClinicIntakeWorkflow
offerings:
- {name: Consult, duration_min: 45, buffer_min: 5, price_cents: 8000, capacity: 1}
reminders: {offsets: ["24h"], channels: ["sms"]}
knowledgeSeed:
- {title: Location, body: "Suite 2."}
dashboardLabels: {bookingSingular: Session, bookingPlural: Sessions, customerTerm: Client}
`

// sha256Hex returns the lowercase hex sha256 of content (real digest, matching
// what Load computes).
func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", sum)
}

// writeIntegrityFixture writes the given files (name -> content) into a fresh
// t.TempDir() and, unless manifest is nil, writes a SHA256SUMS manifest whose
// lines are exactly the strings in manifest (callers pass real digests via
// sha256Hex so the fixture mirrors coreutils output).
func writeIntegrityFixture(t *testing.T, files map[string]string, manifest []string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if manifest != nil {
		body := strings.Join(manifest, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// (a) A manifest with correct digests for every pack lets Load succeed.
func TestLoadIntegrityManifestValid(t *testing.T) {
	dir := writeIntegrityFixture(t,
		map[string]string{"one.yaml": validPack, "two.yaml": validPackAlt},
		[]string{
			sha256Hex(validPack) + "  one.yaml",
			sha256Hex(validPackAlt) + "  two.yaml",
		})
	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load with valid manifest: %v", err)
	}
	if !reg.Has("test-pack") || !reg.Has("test-pack-alt") {
		t.Fatalf("expected both packs loaded, got %v", reg.IDs())
	}
}

// (b) Tampered pack bytes must make Load fail, naming the offending file.
func TestLoadIntegrityManifestTamperedPack(t *testing.T) {
	tampered := validPack + "\n# tampered after signing\n"
	dir := writeIntegrityFixture(t,
		map[string]string{"one.yaml": tampered},
		[]string{sha256Hex(validPack) + "  one.yaml"})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected hash-mismatch error for tampered pack")
	}
	if !strings.Contains(err.Error(), "one.yaml") {
		t.Fatalf("error must name the offending file, got: %v", err)
	}
}

// (c) A *.yaml file that is not listed in the manifest must make Load fail.
func TestLoadIntegrityManifestExtraPack(t *testing.T) {
	dir := writeIntegrityFixture(t,
		map[string]string{"one.yaml": validPack, "extra.yaml": validPackAlt},
		[]string{sha256Hex(validPack) + "  one.yaml"})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for pack missing from manifest")
	}
	if !strings.Contains(err.Error(), "extra.yaml") {
		t.Fatalf("error must name the unlisted file, got: %v", err)
	}
}

// (d) A manifest entry whose file is absent from the dir must make Load fail.
func TestLoadIntegrityManifestMissingFile(t *testing.T) {
	dir := writeIntegrityFixture(t,
		map[string]string{"one.yaml": validPack},
		[]string{
			sha256Hex(validPack) + "  one.yaml",
			sha256Hex("ghost") + "  ghost.yaml",
		})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for manifest entry without file")
	}
	if !strings.Contains(err.Error(), "ghost.yaml") {
		t.Fatalf("error must name the absent file, got: %v", err)
	}
}

// (e) Without a manifest, Load must warn-and-continue (never fail solely
// because the manifest is absent).
func TestLoadIntegrityManifestAbsent(t *testing.T) {
	dir := writeIntegrityFixture(t, map[string]string{"one.yaml": validPack}, nil)
	reg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load without manifest must not fail: %v", err)
	}
	if !reg.Has("test-pack") {
		t.Fatalf("pack test-pack not loaded (loaded: %v)", reg.IDs())
	}
}

// index.json is covered by the manifest contract when listed and present:
// a tampered index.json must make Load fail.
func TestLoadIntegrityManifestTamperedIndexJSON(t *testing.T) {
	index := `{"packs":["test-pack"]}`
	tamperedIndex := index + " "
	dir := writeIntegrityFixture(t,
		map[string]string{"one.yaml": validPack, "index.json": tamperedIndex},
		[]string{
			sha256Hex(validPack) + "  one.yaml",
			sha256Hex(index) + "  index.json",
		})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected hash-mismatch error for tampered index.json")
	}
	if !strings.Contains(err.Error(), "index.json") {
		t.Fatalf("error must name the offending file, got: %v", err)
	}
}

// A coreutils binary-mode marker (" *") in the manifest must be tolerated.
func TestLoadIntegrityManifestBinaryMarker(t *testing.T) {
	dir := writeIntegrityFixture(t,
		map[string]string{"one.yaml": validPack},
		[]string{sha256Hex(validPack) + " *one.yaml"})
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load with binary-mode manifest marker: %v", err)
	}
}
