package packs

import (
	"encoding/json"
	"strings"
	"testing"
)

// SPEC-W15 §2/§3: optional growth + i18n blocks — the pack-level CAC
// playbook defaults ({referral_bounty_ngn, primary_channels,
// cac_target_ngn}) and localised user-facing strings ({locale:
// {key: text}}, locales en|pcm|ha|yo|ig) passed through to the runtime
// Summary as pack.growth / pack.i18n so the growth dashboard, referral
// engine, voice runtime and messaging-gateway can consume them from
// GET /v1/tenants/{slug} without parsing YAML.

const packWithGrowthI18n = validPack + `
growth:
  referral_bounty_ngn: 5000
  primary_channels: [rider-referrals, ussd, whatsapp]
  cac_target_ngn: 7000
i18n:
  pcm:
    greeting: "Welcome! How we fit help you today?"
    referral: "Refer rider, collect bounty."
  en:
    greeting: "Welcome! How can we help you today?"
`

func TestLoadPackWithGrowthAndI18n(t *testing.T) {
	reg, err := Load(writePack(t, "growth.yaml", packWithGrowthI18n))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := reg.Get("test-pack")
	if !ok {
		t.Fatal("pack not found")
	}
	if p.Growth == nil || p.Growth.ReferralBountyNGN != 5000 ||
		len(p.Growth.PrimaryChannels) != 3 ||
		p.Growth.PrimaryChannels[0] != "rider-referrals" ||
		p.Growth.CACTargetNGN != 7000 {
		t.Fatalf("growth parsed incorrectly: %+v", p.Growth)
	}
	if p.I18n["pcm"]["greeting"] == "" || p.I18n["en"]["greeting"] == "" {
		t.Fatalf("i18n parsed incorrectly: %+v", p.I18n)
	}
	// Passthrough into the runtime Summary.
	s := p.Summary(nil)
	if s.Growth == nil || s.Growth.CACTargetNGN != 7000 ||
		s.I18n["pcm"]["referral"] == "" {
		t.Fatalf("summary must pass growth+i18n through: %+v", s)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if !strings.Contains(string(raw), `"growth":{"referral_bounty_ngn":5000,"primary_channels":["rider-referrals","ussd","whatsapp"],"cac_target_ngn":7000}`) {
		t.Fatalf("summary JSON must expose growth as snake_case fields: %s", raw)
	}
	// encoding/json sorts map keys, so "en" precedes "pcm".
	if !strings.Contains(string(raw), `"i18n":{"en":{"greeting":"Welcome! How can we help you today?"},"pcm":{"greeting":"Welcome! How we fit help you today?","referral":"Refer rider, collect bounty."}}`) {
		t.Fatalf("summary JSON must expose i18n by locale: %s", raw)
	}
}

func TestValidateGrowth(t *testing.T) {
	bad := []string{
		`growth: {referral_bounty_ngn: -1, primary_channels: [ussd], cac_target_ngn: 100}`,            // negative bounty
		`growth: {referral_bounty_ngn: 0, cac_target_ngn: 100}`,                                       // missing channels
		`growth: {referral_bounty_ngn: 0, primary_channels: [], cac_target_ngn: 100}`,                 // empty channels
		`growth: {referral_bounty_ngn: 0, primary_channels: ["whatsapp", ""], cac_target_ngn: 100}`,   // blank channel
		`growth: {referral_bounty_ngn: 0, primary_channels: ["whatsapp", "  "], cac_target_ngn: 100}`, // whitespace channel
		`growth: {referral_bounty_ngn: 0, primary_channels: [ussd]}`,                                  // missing cac target
		`growth: {referral_bounty_ngn: 0, primary_channels: [ussd], cac_target_ngn: 0}`,               // zero cac target
		`growth: {referral_bounty_ngn: 0, primary_channels: [ussd], cac_target_ngn: -500}`,            // negative cac target
	}
	for i, body := range bad {
		if err := mustValidate(t, "\n"+body+"\n"); err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
	}
	good := []string{
		`growth: {referral_bounty_ngn: 0, primary_channels: [ussd], cac_target_ngn: 1}`,
		`growth: {referral_bounty_ngn: 1500, primary_channels: [whatsapp, referral], cac_target_ngn: 8000}`,
	}
	for i, body := range good {
		if err := mustValidate(t, "\n"+body+"\n"); err != nil {
			t.Fatalf("case %d: valid growth block rejected: %v", i, err)
		}
	}
}

func TestValidateI18n(t *testing.T) {
	bad := []string{
		`i18n: {fr: {greeting: "Bonjour"}}`,                     // locale not in allowlist
		`i18n: {pcm: {greeting: ""}}`,                           // empty value
		`i18n: {pcm: {greeting: "   "}}`,                        // whitespace-only value
		`i18n: {en: {greeting: "Hi"}, de: {greeting: "Hallo"}}`, // one bad locale among good
	}
	for i, body := range bad {
		if err := mustValidate(t, "\n"+body+"\n"); err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
	}
	good := []string{
		`i18n: {pcm: {greeting: "Welcome o!"}}`,
		`i18n: {en: {greeting: "Welcome"}, pcm: {greeting: "Welcome o"}, ha: {greeting: "Sannu"}, yo: {greeting: "Kaabo"}, ig: {greeting: "Nnoo"}}`,
	}
	for i, body := range good {
		if err := mustValidate(t, "\n"+body+"\n"); err != nil {
			t.Fatalf("case %d: valid i18n block rejected: %v", i, err)
		}
	}
}

func TestPacksWithoutGrowthI18nStillValidate(t *testing.T) {
	if err := mustValidate(t, ""); err != nil {
		t.Fatalf("pack without growth/i18n must validate: %v", err)
	}
	reg, err := Load(writePack(t, "nogrowth.yaml", validPack))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, _ := reg.Get("test-pack")
	raw, err := json.Marshal(p.Summary(nil))
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if strings.Contains(string(raw), `"growth"`) || strings.Contains(string(raw), `"i18n"`) {
		t.Fatalf("growth and i18n must be omitted when unset: %s", raw)
	}
}
