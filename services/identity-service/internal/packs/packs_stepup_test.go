package packs

import (
	"encoding/json"
	"strings"
	"testing"
)

// SPEC-W84: optional stepUpPolicy block — per-tenant step-up 2FA tool-tier
// overrides ({tools: {name: tier}}) validated at the identity boundary and
// passed through to the runtime Summary (camelCase "stepUpPolicy") so the
// voice runtime can refine per-tool verification tiers per tenant. Tiers
// are shape-validated here (0|1|2, plugin-conformant names); the
// never-weakening invariant vs built-in floors is enforced at runtime
// (voice-agent-runtime parse_pack_tool_tiers), not at this shape boundary.
//
// This file also carries the SPEC-W84 voice.lexicon regression: before W84
// the yaml boundary silently dropped VoiceDefaults.Lexicon (the field did
// not exist on the struct), so pack voice.lexicon never reached the runtime.

const packWithStepUp = validPack + `
stepUpPolicy:
  tools:
    book_appointment: 2
    cancel_booking: 1
`

func TestLoadPackWithStepUpPolicy(t *testing.T) {
	reg, err := Load(writePack(t, "stepup.yaml", packWithStepUp))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := reg.Get("test-pack")
	if !ok {
		t.Fatal("pack not found")
	}
	if p.StepUpPolicy == nil || p.StepUpPolicy.Tools["book_appointment"] != 2 ||
		p.StepUpPolicy.Tools["cancel_booking"] != 1 {
		t.Fatalf("stepUpPolicy parsed incorrectly: %+v", p.StepUpPolicy)
	}
	// Passthrough into the runtime Summary.
	s := p.Summary(nil)
	if s.StepUpPolicy == nil || s.StepUpPolicy.Tools["book_appointment"] != 2 {
		t.Fatalf("summary must pass stepUpPolicy through: %+v", s)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if !strings.Contains(string(raw), `"stepUpPolicy":{"tools":{"book_appointment":2,"cancel_booking":1}}`) {
		t.Fatalf("summary JSON must expose stepUpPolicy.tools: %s", raw)
	}
}

func TestValidateStepUpPolicy(t *testing.T) {
	bad := []string{
		`stepUpPolicy: {tools: {}}`,                             // empty tools map
		`stepUpPolicy: {tools: {"bad-name": 1}}`,                // hyphen not toolNameRe-conformant
		`stepUpPolicy: {tools: {"9tool": 1}}`,                   // leading digit
		`stepUpPolicy: {tools: {book_appointment: -1}}`,         // negative tier
		`stepUpPolicy: {tools: {book_appointment: 3}}`,          // tier above 2
		`stepUpPolicy: {tools: {good_tool: 1, "bad name": 2}}`,  // one bad name among good
		`stepUpPolicy: {tools: {good_tool: 1, other_tool: 99}}`, // one bad tier among good
	}
	for i, body := range bad {
		if err := mustValidate(t, "\n"+body+"\n"); err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
	}
	good := []string{
		`stepUpPolicy: {tools: {book_appointment: 2}}`,
		`stepUpPolicy: {tools: {plugin_tool: 0}}`,   // plugin tools may lower to 0
		`stepUpPolicy: {tools: {a: 0, b: 1, c: 2}}`, // full tier range
	}
	for i, body := range good {
		if err := mustValidate(t, "\n"+body+"\n"); err != nil {
			t.Fatalf("case %d: valid stepUpPolicy block rejected: %v", i, err)
		}
	}
}

const packWithVoiceLexicon = validPack + `
voice:
  provider: mms
  languages:
    pcm: "mms:pcm"
  lexicon:
    Adaeze: "ah-DAH-zeh"
    Chukwudi: "choo-KWOO-dee"
`

func TestLoadPackWithVoiceLexicon(t *testing.T) {
	reg, err := Load(writePack(t, "lexicon.yaml", packWithVoiceLexicon))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := reg.Get("test-pack")
	if !ok {
		t.Fatal("pack not found")
	}
	// W82 seam regression: lexicon must survive the yaml boundary.
	if p.Voice == nil || p.Voice.Lexicon["Adaeze"] != "ah-DAH-zeh" ||
		p.Voice.Lexicon["Chukwudi"] != "choo-KWOO-dee" {
		t.Fatalf("voice.lexicon parsed incorrectly: %+v", p.Voice)
	}
	s := p.Summary(nil)
	if s.Voice == nil || s.Voice.Lexicon["Adaeze"] != "ah-DAH-zeh" {
		t.Fatalf("summary must pass voice.lexicon through: %+v", s)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if !strings.Contains(string(raw), `"lexicon":{"Adaeze":"ah-DAH-zeh","Chukwudi":"choo-KWOO-dee"}`) {
		t.Fatalf("summary JSON must expose voice.lexicon: %s", raw)
	}
}

func TestValidateVoiceLexicon(t *testing.T) {
	longKey := strings.Repeat("k", 65)
	longVal := strings.Repeat("v", 129)
	bad := []string{
		`voice: {provider: mms, lexicon: {"": "hint"}}`,                // empty key
		`voice: {provider: mms, lexicon: {Term: ""}}`,                  // empty value
		`voice: {provider: mms, lexicon: {Term: "   "}}`,               // whitespace-only value
		`voice: {provider: mms, lexicon: {"` + longKey + `": "hint"}}`, // key > 64 chars
		`voice: {provider: mms, lexicon: {Term: "` + longVal + `"}}`,   // value > 128 chars
	}
	for i, body := range bad {
		if err := mustValidate(t, "\n"+body+"\n"); err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
	}
	good := []string{
		`voice: {provider: mms}`,                                  // no lexicon
		`voice: {provider: mms, lexicon: {Adaeze: "ah-DAH-zeh"}}`, // single entry
		`voice: {provider: mms, lexicon: {"` + strings.Repeat("k", 64) + `": "` + strings.Repeat("v", 128) + `"}}`, // boundary
	}
	for i, body := range good {
		if err := mustValidate(t, "\n"+body+"\n"); err != nil {
			t.Fatalf("case %d: valid voice block rejected: %v", i, err)
		}
	}
}

func TestPacksWithoutStepUpPolicyStillValidate(t *testing.T) {
	if err := mustValidate(t, ""); err != nil {
		t.Fatalf("pack without stepUpPolicy must validate: %v", err)
	}
	reg, err := Load(writePack(t, "nostepup.yaml", validPack))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, _ := reg.Get("test-pack")
	raw, err := json.Marshal(p.Summary(nil))
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if strings.Contains(string(raw), `"stepUpPolicy"`) {
		t.Fatalf("stepUpPolicy must be omitted when unset: %s", raw)
	}
}
