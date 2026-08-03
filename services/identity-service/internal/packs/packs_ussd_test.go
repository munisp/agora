package packs

import (
	"encoding/json"
	"strings"
	"testing"
)

// SPEC-W12 §1: optional ussd block — the pack-level low-literacy numeric
// menu (list of {key,label,action}) passed through to the runtime Summary
// as pack.ussd.menu so messaging-gateway's ParseUSSDMenu can resolve it
// from GET /v1/tenants/{slug} and conversation-service can drive the
// 1/2/… select, 0 = back, 00 = main menu navigation.

const packWithUSSD = validPack + `
ussd:
  menu:
  - {key: "1", label: "Book appointment", action: book}
  - {key: "2", label: "Talk to an agent", action: handoff}
`

func TestLoadPackWithUSSD(t *testing.T) {
	reg, err := Load(writePack(t, "ussd.yaml", packWithUSSD))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := reg.Get("test-pack")
	if !ok {
		t.Fatal("pack not found")
	}
	if p.USSD == nil || len(p.USSD.Menu) != 2 ||
		p.USSD.Menu[0].Key != "1" || p.USSD.Menu[0].Label != "Book appointment" ||
		p.USSD.Menu[0].Action != "book" ||
		p.USSD.Menu[1].Key != "2" || p.USSD.Menu[1].Action != "handoff" {
		t.Fatalf("ussd parsed incorrectly: %+v", p.USSD)
	}
	// Passthrough into the runtime Summary — the exact pack.ussd.menu shape
	// messaging-gateway's ParseUSSDMenu reads (lowercase key/label/action).
	s := p.Summary(nil)
	if s.USSD == nil || len(s.USSD.Menu) != 2 || s.USSD.Menu[1].Action != "handoff" {
		t.Fatalf("summary must pass ussd through: %+v", s)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if !strings.Contains(string(raw), `"ussd":{"menu":[{"key":"1","label":"Book appointment","action":"book"}`) {
		t.Fatalf("summary JSON must expose ussd.menu as {key,label,action}: %s", raw)
	}
}

func TestValidateUSSD(t *testing.T) {
	bad := []string{
		`ussd: {}`,
		`ussd: {menu: []}`,
		`ussd: {menu: [{label: "Book appointment", action: book}]}`,                          // missing key
		`ussd: {menu: [{key: "1", action: book}]}`,                                           // missing label
		`ussd: {menu: [{key: "123456789", label: "Too long key", action: book}]}`,            // key > 8 chars
		`ussd: {menu: [{key: "1", label: "A", action: book}, {key: "1", label: "B"}]}`,       // duplicate key
		`ussd: {menu: [{key: "1", label: "` + strings.Repeat("x", 81) + `", action: book}]}`, // label > 80
		`ussd: {menu: [{key: "1", label: "Book appointment", action: teleport}]}`,            // unknown action
	}
	for i, body := range bad {
		if err := mustValidate(t, "\n"+body+"\n"); err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
	}
	good := []string{
		`ussd: {menu: [{key: "1", label: "Book appointment", action: book}]}`,
		`ussd: {menu: [{key: "1", label: "Opening hours", action: info}, {key: "2", label: "Report emergency", action: sos}, {key: "3", label: "Check booking status", action: status}]}`,
		`ussd: {menu: [{key: "1", label: "Opening hours"}]}`, // action optional (informational item)
		`ussd: {menu: [{key: "1", label: "` + strings.Repeat("x", 80) + `", action: handoff}]}`,
	}
	for i, body := range good {
		if err := mustValidate(t, "\n"+body+"\n"); err != nil {
			t.Fatalf("case %d: valid ussd block rejected: %v", i, err)
		}
	}
}

func TestPacksWithoutUSSDStillValidate(t *testing.T) {
	if err := mustValidate(t, ""); err != nil {
		t.Fatalf("pack without ussd must validate: %v", err)
	}
	reg, err := Load(writePack(t, "noussd.yaml", validPack))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, _ := reg.Get("test-pack")
	raw, err := json.Marshal(p.Summary(nil))
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if strings.Contains(string(raw), `"ussd"`) {
		t.Fatalf("ussd must be omitted when unset: %s", raw)
	}
}
