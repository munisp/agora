package packs

import (
	"encoding/json"
	"strings"
	"testing"
)

// SPEC-W10 Part D: optional voice block — pack-level TTS defaults, validated
// like mcpServers and passed through to the runtime Summary (tenant context
// JSON, camelCase "voice") so the voice runtime can merge per-language
// provider-qualified voice overrides into its TTS voice map.

const packWithVoice = validPack + `
voice:
  provider: mms
  languages:
    en: "azure:en-NG-EzinneNeural"
    pcm: "mms:pcm"
`

func TestLoadPackWithVoice(t *testing.T) {
	reg, err := Load(writePack(t, "voice.yaml", packWithVoice))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := reg.Get("test-pack")
	if !ok {
		t.Fatal("pack not found")
	}
	if p.Voice == nil || p.Voice.Provider != "mms" ||
		p.Voice.Languages["en"] != "azure:en-NG-EzinneNeural" ||
		p.Voice.Languages["pcm"] != "mms:pcm" {
		t.Fatalf("voice parsed incorrectly: %+v", p.Voice)
	}
	// Passthrough into the runtime Summary (camelCase JSON).
	s := p.Summary(nil)
	if s.Voice == nil || s.Voice.Provider != "mms" ||
		s.Voice.Languages["pcm"] != "mms:pcm" {
		t.Fatalf("summary must pass voice through: %+v", s)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if !strings.Contains(string(raw), `"voice":{"provider":"mms"`) {
		t.Fatalf("summary JSON must expose voice camelCase: %s", raw)
	}
}

func TestValidateVoice(t *testing.T) {
	bad := []string{
		`voice: {}`,
		`voice: {provider: elevenlabs}`,
		`voice: {provider: MMS}`,
		`voice: {provider: mms, voiceId: "  "}`,
		`voice: {provider: mms, languages: {en: "azure"}}`,
		`voice: {provider: mms, languages: {en: "azure:bad voice!"}}`,
		`voice: {provider: mms, languages: {en: "unknown:en-X-Neural"}}`,
		`voice: {provider: mms, languages: {pcm: "mms:"}}`,
	}
	for i, body := range bad {
		if err := mustValidate(t, "\n"+body+"\n"); err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
	}
	good := []string{
		`voice: {provider: piper}`,
		`voice: {provider: azure, voiceId: en-NG-AbeoNeural}`,
		`voice: {provider: mms, languages: {en: "azure:en-NG-EzinneNeural", pcm: "mms:pcm"}}`,
	}
	for i, body := range good {
		if err := mustValidate(t, "\n"+body+"\n"); err != nil {
			t.Fatalf("case %d: valid voice rejected: %v", i, err)
		}
	}
}

func TestPacksWithoutVoiceStillValidate(t *testing.T) {
	if err := mustValidate(t, ""); err != nil {
		t.Fatalf("pack without voice must validate: %v", err)
	}
	reg, err := Load(writePack(t, "novoice.yaml", validPack))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, _ := reg.Get("test-pack")
	raw, err := json.Marshal(p.Summary(nil))
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if strings.Contains(string(raw), `"voice"`) {
		t.Fatalf("voice must be omitted when unset: %s", raw)
	}
}
