// Package packs loads industry workflow packs (SPEC-CRM §C) from a mounted
// directory of YAML files (default /industries), validates them on load and
// caches them in memory. The pack summary exposed via GET /v1/tenants/{slug}
// lets the voice runtime and web console consume pack data without parsing
// YAML themselves.
package packs

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultDir is used when INDUSTRIES_DIR is not set.
const DefaultDir = "/industries"

// DefaultIndustry is applied to tenants created without an explicit industry.
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

// Reminders mirrors the pack reminders block.
type Reminders struct {
	Offsets  []string `yaml:"offsets" json:"offsets"`
	Channels []string `yaml:"channels" json:"channels"`
}

// KnowledgeDoc is a knowledge-base seed document of a pack.
type KnowledgeDoc struct {
	Title string `yaml:"title" json:"title"`
	Body  string `yaml:"body" json:"body"`
}

// Agent is one specialist of a multi-agent crew (SPEC-W3 §4, innovation 6):
// the voice runtime routes turns to it by matching Intents keywords and swaps
// its Persona into the system prompt while active.
type Agent struct {
	ID      string   `yaml:"id" json:"id"`
	Name    string   `yaml:"name" json:"name"`
	Persona string   `yaml:"persona" json:"persona"`
	Intents []string `yaml:"intents" json:"intents"`
}

// CustomTool is a declarative HTTP plugin tool (SPEC-W3 §4, innovation 15
// MVP): the voice runtime registers it as a function tool and executes it via
// httpx with {{var}} template substitution from the tool-call arguments.
// WASM-sandboxed plugins are the documented phase-2 (docs/plugins.md).
type CustomTool struct {
	Name         string `yaml:"name" json:"name"`
	Description  string `yaml:"description" json:"description"`
	Method       string `yaml:"method" json:"method"`
	URL          string `yaml:"url" json:"url"`
	BodyTemplate string `yaml:"bodyTemplate" json:"bodyTemplate,omitempty"`
}

// MCPServer is one external MCP (Model Context Protocol) server a pack
// connects to the voice tool layer (SPEC-W9 Part C2): the voice runtime
// handshakes it (initialize -> tools/list) and exposes its tools namespaced
// mcp__{server}__{tool}. Https-only; packs carry no credentials — auth
// headers belong in the runtime's MCP_SERVERS env var.
type MCPServer struct {
	Name string `yaml:"name" json:"name"`
	URL  string `yaml:"url" json:"url"`
}

// VoiceDefaults is the optional pack-level voice/TTS defaults block
// (SPEC-W10 Part D): the default TTS provider chain entry plus per-language
// provider-qualified overrides ("provider:voiceId") the voice runtime merges
// into its TTS voice map.
type VoiceDefaults struct {
	Provider  string            `yaml:"provider" json:"provider"`
	VoiceID   string            `yaml:"voiceId" json:"voiceId,omitempty"`
	Languages map[string]string `yaml:"languages" json:"languages,omitempty"`
}

// Pack is one industry pack definition (industries/<id>.yaml).
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
	// SPEC-W3 §4: optional, validated when present.
	Agents      []Agent      `yaml:"agents" json:"agents,omitempty"`
	CustomTools []CustomTool `yaml:"customTools" json:"customTools,omitempty"`
	// SPEC-W9 Part C2: optional MCP servers, validated when present.
	MCPServers []MCPServer `yaml:"mcpServers" json:"mcpServers,omitempty"`
	// SPEC-W10 Part D: optional pack-level voice/TTS defaults, validated
	// when present and passed through to the runtime Summary.
	Voice *VoiceDefaults `yaml:"voice" json:"voice,omitempty"`
	// Optional compliance/localisation fields (not validated; see
	// industries/nigeria-sme.yaml and docs/compliance/ndpa.md). ConsentText
	// is the data-processing/call-recording notice read to callers;
	// Languages lists the deployment's supported ISO-639 language codes.
	ConsentText string   `yaml:"consentText" json:"consentText,omitempty"`
	Languages   []string `yaml:"languages" json:"languages,omitempty"`
}

// Summary is the pack projection served by GET /v1/tenants/{slug}: the fields
// other services need at runtime, with tenant terminology overrides merged in.
type Summary struct {
	ID               string            `json:"id"`
	DisplayName      string            `json:"displayName"`
	Terminology      map[string]string `json:"terminology"`
	BookingPolicy    BookingPolicy     `json:"bookingPolicy"`
	DashboardLabels  map[string]string `json:"dashboardLabels"`
	AgentPersona     string            `json:"agentPersona"`
	TemporalWorkflow string            `json:"temporalWorkflow"`
	Agents           []Agent           `json:"agents,omitempty"`
	CustomTools      []CustomTool      `json:"customTools,omitempty"`
	MCPServers       []MCPServer       `json:"mcpServers,omitempty"`
	Voice            *VoiceDefaults    `json:"voice,omitempty"`
	ConsentText      string            `json:"consentText,omitempty"`
	Languages        []string          `json:"languages,omitempty"`
}

// Summary builds the runtime projection of the pack. terminologyOverrides
// (tenant.terminology) win over the pack defaults key by key.
func (p Pack) Summary(terminologyOverrides map[string]string) Summary {
	merged := make(map[string]string, len(p.Terminology)+len(terminologyOverrides))
	for k, v := range p.Terminology {
		merged[k] = v
	}
	for k, v := range terminologyOverrides {
		if v != "" {
			merged[k] = v
		}
	}
	labels := make(map[string]string, len(p.DashboardLabels))
	for k, v := range p.DashboardLabels {
		labels[k] = v
	}
	return Summary{
		ID:               p.ID,
		DisplayName:      p.DisplayName,
		Terminology:      merged,
		BookingPolicy:    p.BookingPolicy,
		DashboardLabels:  labels,
		AgentPersona:     p.AgentPersona,
		TemporalWorkflow: p.TemporalWorkflow,
		Agents:           p.Agents,
		CustomTools:      p.CustomTools,
		MCPServers:       p.MCPServers,
		Voice:            p.Voice,
		ConsentText:      p.ConsentText,
		Languages:        p.Languages,
	}
}

// Registry is an in-memory cache of packs keyed by pack id.
type Registry struct {
	packs map[string]Pack
}

var terminologyKeys = []string{"offering", "team_member", "booking", "contact"}

var dashboardLabelKeys = []string{"bookingSingular", "bookingPlural", "customerTerm"}

// Load reads every *.yaml / *.yml file in dir, validates it and returns the
// cached registry. A missing directory is not an error: it yields an empty
// registry (industry validation is then skipped — see Registry.Has) so the
// service can boot outside compose. Any invalid pack file is fatal.
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
	return r, nil
}

// Validate enforces the SPEC-CRM §C pack schema.
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
	if err := validateAgents(p.Agents); err != nil {
		return err
	}
	if err := validateCustomTools(p.CustomTools); err != nil {
		return err
	}
	if err := validateMCPServers(p.MCPServers); err != nil {
		return err
	}
	if err := validateVoice(p.Voice); err != nil {
		return err
	}
	return nil
}

var agentIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

var toolNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// validateAgents enforces the SPEC-W3 §4 innovation 6 crew schema: optional
// list, ids unique and slug-like, persona + at least one intent required.
func validateAgents(agents []Agent) error {
	seen := make(map[string]bool, len(agents))
	for i, a := range agents {
		if !agentIDRe.MatchString(a.ID) {
			return fmt.Errorf("agents[%d]: id %q must match %s", i, a.ID, agentIDRe)
		}
		if seen[a.ID] {
			return fmt.Errorf("agents[%d]: duplicate agent id %q", i, a.ID)
		}
		seen[a.ID] = true
		if strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("agents[%d] (%s): name is required", i, a.ID)
		}
		if strings.TrimSpace(a.Persona) == "" {
			return fmt.Errorf("agents[%d] (%s): persona is required", i, a.ID)
		}
		if len(a.Intents) == 0 {
			return fmt.Errorf("agents[%d] (%s): at least one intent is required", i, a.ID)
		}
		for j, intent := range a.Intents {
			if strings.TrimSpace(intent) == "" {
				return fmt.Errorf("agents[%d] (%s): intents[%d] must not be empty", i, a.ID, j)
			}
		}
	}
	return nil
}

// validateCustomTools enforces the SPEC-W3 §4 innovation 15 plugin-tool
// schema: function-tool-safe unique names, an HTTP method allowlist and an
// absolute http(s) URL (the runtime SSRF guard applies on top).
func validateCustomTools(tools []CustomTool) error {
	seen := make(map[string]bool, len(tools))
	for i, t := range tools {
		if !toolNameRe.MatchString(t.Name) {
			return fmt.Errorf("customTools[%d]: name %q must match %s", i, t.Name, toolNameRe)
		}
		if seen[t.Name] {
			return fmt.Errorf("customTools[%d]: duplicate tool name %q", i, t.Name)
		}
		seen[t.Name] = true
		switch strings.ToUpper(t.Method) {
		case "GET", "POST", "PUT", "PATCH", "DELETE":
		default:
			return fmt.Errorf("customTools[%d] (%s): method %q not allowed", i, t.Name, t.Method)
		}
		u, err := url.Parse(t.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("customTools[%d] (%s): url must be absolute http(s)", i, t.Name)
		}
	}
	return nil
}

// validateMCPServers enforces the SPEC-W9 Part C2 mcpServers schema:
// optional list, slug-rule unique names (same regex as agent ids) and
// absolute https URLs only — MCP servers sit outside the platform SSRF
// guard, so plaintext http is rejected outright. Kept in sync with
// scripts/validate_pack.py.
func validateMCPServers(servers []MCPServer) error {
	seen := make(map[string]bool, len(servers))
	for i, s := range servers {
		if !agentIDRe.MatchString(s.Name) {
			return fmt.Errorf("mcpServers[%d]: name %q must match %s", i, s.Name, agentIDRe)
		}
		if seen[s.Name] {
			return fmt.Errorf("mcpServers[%d]: duplicate server name %q", i, s.Name)
		}
		seen[s.Name] = true
		u, err := url.Parse(s.URL)
		if err != nil || u.Host == "" || u.Scheme != "https" {
			return fmt.Errorf("mcpServers[%d] (%s): url must be absolute https", i, s.Name)
		}
	}
	return nil
}

var voiceLangValueRe = regexp.MustCompile(`^(piper|mms|xtts|azure|spitch):[A-Za-z0-9_\-\.]+$`)

// validateVoice enforces the SPEC-W10 Part D voice block: optional mapping,
// provider required and one of piper/mms/xtts/azure/spitch, optional voiceId
// string, and optional languages map of lang -> "provider:voiceId". Kept in
// sync with scripts/validate_pack.py.
func validateVoice(voice *VoiceDefaults) error {
	if voice == nil {
		return nil
	}
	switch voice.Provider {
	case "piper", "mms", "xtts", "azure", "spitch":
	default:
		return fmt.Errorf("voice.provider %q must be one of piper, mms, xtts, azure, spitch", voice.Provider)
	}
	if strings.TrimSpace(voice.VoiceID) == "" && voice.VoiceID != "" {
		return fmt.Errorf("voice.voiceId must be a non-empty string")
	}
	for lang, value := range voice.Languages {
		if !voiceLangValueRe.MatchString(value) {
			return fmt.Errorf("voice.languages[%q]: value %q must match %s", lang, value, voiceLangValueRe)
		}
	}
	return nil
}

// Get returns the pack for an id.
func (r *Registry) Get(id string) (Pack, bool) {
	p, ok := r.packs[id]
	return p, ok
}

// Has reports whether id is a usable industry. When no packs are loaded
// (empty registry — e.g. the industries dir is not mounted) every id is
// accepted so provisioning keeps working; callers receive no pack summary.
func (r *Registry) Has(id string) bool {
	if len(r.packs) == 0 {
		return true
	}
	_, ok := r.packs[id]
	return ok
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
