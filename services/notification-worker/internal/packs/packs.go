// Package packs loads industry workflow packs (SPEC-CRM §C) from a mounted
// directory of YAML files (default /industries) so the ApplyIndustryPack
// activity can seed offerings, knowledge documents and terminology. The
// schema mirrors identity-service's internal/packs (duplicated per
// service-boundary rules — no shared top-level package).
package packs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultDir is used when INDUSTRIES_DIR is not set.
const DefaultDir = "/industries"

// DefaultIndustry is applied when no industry is given.
const DefaultIndustry = "salon"

// BookingPolicy mirrors the pack bookingPolicy block.
type BookingPolicy struct {
	DepositPercent          int   `yaml:"depositPercent" json:"depositPercent"`
	NoShowFeeCents          int64 `yaml:"noShowFeeCents" json:"noShowFeeCents"`
	PhoneConfirmation       bool  `yaml:"phoneConfirmation" json:"phoneConfirmation"`
	IntakeRequired          bool  `yaml:"intakeRequired" json:"intakeRequired"`
	CancellationWindowHours int   `yaml:"cancellationWindowHours" json:"cancellationWindowHours"`
}

// Offering is a catalog seed entry of a pack.
type Offering struct {
	Name        string `yaml:"name" json:"name"`
	DurationMin int    `yaml:"duration_min" json:"duration_min"`
	BufferMin   int    `yaml:"buffer_min" json:"buffer_min"`
	PriceCents  int64  `yaml:"price_cents" json:"price_cents"`
	Capacity    int    `yaml:"capacity" json:"capacity"`
}

// Reminders mirrors the pack reminders block (SPEC-W80 C3).
type Reminders struct {
	Offsets  []string `yaml:"offsets" json:"offsets"`
	Channels []string `yaml:"channels" json:"channels"`
}

// KnowledgeDoc is a knowledge-base seed document of a pack.
type KnowledgeDoc struct {
	Title string `yaml:"title" json:"title"`
	Body  string `yaml:"body" json:"body"`
}

// Pack is one industry pack definition (industries/<id>.yaml). Fields not
// needed by the worker (agentPersona, dashboardLabels, ...) are still parsed
// so a malformed pack fails validation here exactly as in identity-service.
type Pack struct {
	ID               string            `yaml:"id" json:"id"`
	DisplayName      string            `yaml:"displayName" json:"displayName"`
	Terminology      map[string]string `yaml:"terminology" json:"terminology"`
	AgentPersona     string            `yaml:"agentPersona" json:"agentPersona"`
	BookingPolicy    BookingPolicy     `yaml:"bookingPolicy" json:"bookingPolicy"`
	TemporalWorkflow string            `yaml:"temporalWorkflow" json:"temporalWorkflow"`
	Offerings        []Offering        `yaml:"offerings" json:"offerings"`
	Reminders        Reminders         `yaml:"reminders" json:"reminders"`
	KnowledgeSeed    []KnowledgeDoc    `yaml:"knowledgeSeed" json:"knowledgeSeed"`
	DashboardLabels  map[string]string `yaml:"dashboardLabels" json:"dashboardLabels"`
}

// Registry is an in-memory cache of packs keyed by pack id.
type Registry struct {
	packs map[string]Pack
}

// IntegrityManifestName is the coreutils-format sha256 manifest shipped with
// the packs (W39 SIM-027). When present in the pack dir, Load verifies every
// pack against it before accepting any pack (fail-closed).
//
// SPEC-W80 C2: the manifest verification below is ported from
// services/identity-service/internal/packs/packs.go (W39 SIM-027 /
// SPEC-CRM §C) and adapted to this loader's API, so both services enforce
// the identical fail-closed integrity contract on the same manifest.
const IntegrityManifestName = "SHA256SUMS"

// Load reads every *.yaml / *.yml file in dir, validates it and returns the
// cached registry. A missing directory yields an empty registry (the worker
// still boots; ApplyIndustryPack then no-ops). Any invalid pack file is
// fatal.
//
// When dir contains a SHA256SUMS manifest (coreutils format, one
// "<64-hex>  <name>" line per file), Load enforces it fail-closed: a hash
// mismatch, a *.yaml file absent from the manifest, or a manifest entry
// whose file is absent from dir are all hard errors and NOTHING is loaded.
// When the manifest itself is absent (dev bind-mount posture), Load only
// logs a warning and continues — the manifest simply not being shipped is
// never an error.
func Load(dir string) (*Registry, error) {
	if dir == "" {
		dir = DefaultDir
	}
	r := &Registry{packs: map[string]Pack{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return nil, fmt.Errorf("read industries dir %s: %w", dir, err)
	}
	manifest, err := loadIntegrityManifest(dir)
	if err != nil {
		return nil, err
	}
	onDisk := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		onDisk[e.Name()] = true
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read pack %s: %w", path, err)
		}
		if manifest != nil {
			if err := manifest.verify(name, raw); err != nil {
				return nil, err
			}
		}
		var p Pack
		if err := yaml.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("parse pack %s: %w", path, err)
		}
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("invalid pack %s: %w", path, err)
		}
		if _, dup := r.packs[p.ID]; dup {
			return nil, fmt.Errorf("duplicate pack id %q (%s)", p.ID, path)
		}
		r.packs[p.ID] = p
	}
	if manifest != nil {
		// Non-yaml manifest entries (index.json etc.) are covered by the
		// manifest contract too: verify them when present in both, and treat
		// every listed-but-absent file as an incomplete install.
		for _, name := range manifest.names() {
			if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
				// yaml entries were already verified above when present;
				// here they only need the absent-from-dir check.
				if !onDisk[name] {
					return nil, fmt.Errorf("pack integrity manifest %s lists %s but the file is absent from %s (incomplete install)", filepath.Join(dir, IntegrityManifestName), name, dir)
				}
				continue
			}
			path := filepath.Join(dir, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, fmt.Errorf("pack integrity manifest %s lists %s but the file is absent from %s (incomplete install)", filepath.Join(dir, IntegrityManifestName), name, dir)
				}
				return nil, fmt.Errorf("read manifest-listed file %s: %w", path, err)
			}
			if err := manifest.verify(name, raw); err != nil {
				return nil, err
			}
		}
	}
	return r, nil
}

// terminologyKeys and dashboardLabelKeys are the required map keys of every
// pack (mirrors identity-service's pack schema, SPEC-CRM §C).
var terminologyKeys = []string{"offering", "team_member", "booking", "contact"}

var dashboardLabelKeys = []string{"bookingSingular", "bookingPlural", "customerTerm"}

// Validate enforces the SPEC-CRM §C pack schema. Ported from
// services/identity-service/internal/packs (W80 C2) and adapted to this
// loader's Pack fields (the worker does not parse the agent/voice/ussd
// blocks, so only the blocks this struct models are validated here).
func (p Pack) Validate() error {
	if p.ID == "" {
		return fmt.Errorf("id is required")
	}
	if p.DisplayName == "" {
		return fmt.Errorf("displayName is required")
	}
	for _, k := range terminologyKeys {
		if p.Terminology[k] == "" {
			return fmt.Errorf("terminology.%s is required", k)
		}
	}
	if strings.TrimSpace(p.AgentPersona) == "" {
		return fmt.Errorf("agentPersona is required")
	}
	if p.BookingPolicy.DepositPercent < 0 || p.BookingPolicy.DepositPercent > 100 {
		return fmt.Errorf("bookingPolicy.depositPercent must be 0-100, got %d", p.BookingPolicy.DepositPercent)
	}
	if p.BookingPolicy.NoShowFeeCents < 0 {
		return fmt.Errorf("bookingPolicy.noShowFeeCents must be >= 0")
	}
	if p.BookingPolicy.CancellationWindowHours < 0 {
		return fmt.Errorf("bookingPolicy.cancellationWindowHours must be >= 0")
	}
	switch p.TemporalWorkflow {
	case "SalonDepositWorkflow", "ClinicIntakeWorkflow", "ConsultancyFollowupWorkflow", "SupportEscalationWorkflow":
	default:
		return fmt.Errorf("temporalWorkflow %q is not a known pack workflow", p.TemporalWorkflow)
	}
	for i, o := range p.Offerings {
		if o.Name == "" {
			return fmt.Errorf("offerings[%d].name is required", i)
		}
		if o.DurationMin <= 0 {
			return fmt.Errorf("offerings[%d] (%s): duration_min must be > 0", i, o.Name)
		}
		if o.BufferMin < 0 || o.PriceCents < 0 || o.Capacity < 0 {
			return fmt.Errorf("offerings[%d] (%s): buffer_min/price_cents/capacity must be >= 0", i, o.Name)
		}
	}
	for i, d := range p.KnowledgeSeed {
		if d.Title == "" || strings.TrimSpace(d.Body) == "" {
			return fmt.Errorf("knowledgeSeed[%d]: title and body are required", i)
		}
	}
	for _, k := range dashboardLabelKeys {
		if p.DashboardLabels[k] == "" {
			return fmt.Errorf("dashboardLabels.%s is required", k)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// SHA256SUMS integrity manifest (W39 SIM-027; ported from identity-service
// for SPEC-W80 C2 — see the attribution note on IntegrityManifestName).
// ---------------------------------------------------------------------------

// integrityManifest is the parsed <dir>/SHA256SUMS.
type integrityManifest struct {
	dir   string
	sums  map[string]string
	order []string
}

// loadIntegrityManifest reads and parses <dir>/SHA256SUMS. It returns
// (nil, nil) — after logging a warning — when the manifest simply does not
// exist; an unreadable or malformed manifest is a hard error (fail-closed).
func loadIntegrityManifest(dir string) (*integrityManifest, error) {
	path := filepath.Join(dir, IntegrityManifestName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("pack integrity manifest not found; packs unverified (dir %s)", dir)
			return nil, nil
		}
		return nil, fmt.Errorf("read pack integrity manifest %s: %w", path, err)
	}
	m := &integrityManifest{dir: dir, sums: map[string]string{}}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Coreutils format: "<64-hex>  <name>" with a two-space separator;
		// binary mode replaces the second space with '*' ("<hex> *<name>").
		if len(line) < 67 || line[64] != ' ' || (line[65] != ' ' && line[65] != '*') {
			return nil, fmt.Errorf("pack integrity manifest %s: malformed line %d: %q", path, i+1, line)
		}
		hash, name := line[:64], line[66:]
		if _, err := hex.DecodeString(hash); err != nil {
			return nil, fmt.Errorf("pack integrity manifest %s: malformed hash on line %d: %q", path, i+1, line)
		}
		if _, dup := m.sums[name]; dup {
			return nil, fmt.Errorf("pack integrity manifest %s: duplicate entry for %s", path, name)
		}
		m.sums[name] = hash
		m.order = append(m.order, name)
	}
	return m, nil
}

// names returns the manifest entries in sorted order for deterministic
// verification errors.
func (m *integrityManifest) names() []string {
	names := make([]string, len(m.order))
	copy(names, m.order)
	sort.Strings(names)
	return names
}

// verify checks that content's sha256 matches the manifest entry for name.
// A file absent from the manifest is a hard error (unlisted packs are not
// trusted when a manifest is shipped).
func (m *integrityManifest) verify(name string, content []byte) error {
	want, ok := m.sums[name]
	if !ok {
		return fmt.Errorf("pack integrity manifest %s does not list %s; refusing to load unverified file", filepath.Join(m.dir, IntegrityManifestName), name)
	}
	sum := sha256.Sum256(content)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("pack integrity check failed for %s: sha256 %s does not match manifest %s (%s)", name, got, want, filepath.Join(m.dir, IntegrityManifestName))
	}
	return nil
}

// Get returns the pack for an id.
func (r *Registry) Get(id string) (Pack, bool) {
	p, ok := r.packs[id]
	return p, ok
}

// IDs returns the sorted list of loaded pack ids.
func (r *Registry) IDs() []string {
	ids := make([]string, 0, len(r.packs))
	for id := range r.packs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
