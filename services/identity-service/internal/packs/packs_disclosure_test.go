package packs

import (
	"encoding/json"
	"strings"
	"testing"
)

// SPEC-W11 Part D: optional disclosure block — pack-level spoken AI /
// recording disclosure defaults, validated like voice and passed through to
// the runtime Summary (tenant context JSON, camelCase "disclosure") so the
// voice runtime can prepend the automated-assistant line and append the
// recording notice to the greeting.

const packWithDisclosure = validPack + `
disclosure:
  spokenAiDisclosure: true
  recordingConsent: true
  text: "You are speaking with an automated assistant. This call may be recorded."
`

func TestLoadPackWithDisclosure(t *testing.T) {
	reg, err := Load(writePack(t, "disclosure.yaml", packWithDisclosure))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := reg.Get("test-pack")
	if !ok {
		t.Fatal("pack not found")
	}
	if p.Disclosure == nil || p.Disclosure.SpokenAIDisclosure == nil ||
		!*p.Disclosure.SpokenAIDisclosure || p.Disclosure.RecordingConsent == nil ||
		!*p.Disclosure.RecordingConsent ||
		p.Disclosure.Text != "You are speaking with an automated assistant. This call may be recorded." {
		t.Fatalf("disclosure parsed incorrectly: %+v", p.Disclosure)
	}
	// Passthrough into the runtime Summary (camelCase JSON).
	s := p.Summary(nil)
	if s.Disclosure == nil || s.Disclosure.SpokenAIDisclosure == nil ||
		!*s.Disclosure.SpokenAIDisclosure {
		t.Fatalf("summary must pass disclosure through: %+v", s)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if !strings.Contains(string(raw), `"disclosure":{"spokenAiDisclosure":true,"recordingConsent":true`) {
		t.Fatalf("summary JSON must expose disclosure camelCase: %s", raw)
	}
}

func TestValidateDisclosure(t *testing.T) {
	bad := []string{
		`disclosure: {}`,
		`disclosure: {spokenAiDisclosure: true}`,
		`disclosure: {recordingConsent: true}`,
		`disclosure: {spokenAiDisclosure: true, recordingConsent: true, text: "` + strings.Repeat("x", 201) + `"}`,
	}
	for i, body := range bad {
		if err := mustValidate(t, "\n"+body+"\n"); err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
	}
	good := []string{
		`disclosure: {spokenAiDisclosure: true, recordingConsent: true}`,
		`disclosure: {spokenAiDisclosure: false, recordingConsent: false, text: "hi"}`,
		`disclosure: {spokenAiDisclosure: true, recordingConsent: false, text: "` + strings.Repeat("x", 200) + `"}`,
	}
	for i, body := range good {
		if err := mustValidate(t, "\n"+body+"\n"); err != nil {
			t.Fatalf("case %d: valid disclosure rejected: %v", i, err)
		}
	}
}

func TestPacksWithoutDisclosureStillValidate(t *testing.T) {
	if err := mustValidate(t, ""); err != nil {
		t.Fatalf("pack without disclosure must validate: %v", err)
	}
	reg, err := Load(writePack(t, "nodisclosure.yaml", validPack))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, _ := reg.Get("test-pack")
	raw, err := json.Marshal(p.Summary(nil))
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if strings.Contains(string(raw), `"disclosure"`) {
		t.Fatalf("disclosure must be omitted when unset: %s", raw)
	}
}
