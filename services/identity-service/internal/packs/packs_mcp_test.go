package packs

import (
	"encoding/json"
	"strings"
	"testing"
)

// SPEC-W9 Part C2: optional mcpServers pack field — validated like
// customTools and passed through to the runtime Summary (tenant context
// JSON) so the voice runtime can handshake the servers.

const packWithMCPServers = validPack + `
mcpServers:
- name: n8n
  url: https://n8n.example.com/mcp/front-desk/sse
- name: crm
  url: https://mcp.crm.example.com/
`

func TestLoadPackWithMCPServers(t *testing.T) {
	reg, err := Load(writePack(t, "mcp.yaml", packWithMCPServers))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := reg.Get("test-pack")
	if !ok {
		t.Fatal("pack not found")
	}
	if len(p.MCPServers) != 2 || p.MCPServers[0].Name != "n8n" {
		t.Fatalf("mcpServers parsed incorrectly: %+v", p.MCPServers)
	}
	// Passthrough into the runtime Summary (camelCase JSON).
	s := p.Summary(nil)
	if len(s.MCPServers) != 2 || s.MCPServers[1].URL != "https://mcp.crm.example.com/" {
		t.Fatalf("summary must pass mcpServers through: %+v", s)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if !strings.Contains(string(raw), `"mcpServers":[{"name":"n8n"`) {
		t.Fatalf("summary JSON must expose mcpServers camelCase: %s", raw)
	}
}

func TestValidateMCPServers(t *testing.T) {
	bad := []string{
		`mcpServers: [{name: "Bad Name", url: "https://mcp.example.com/sse"}]`,
		`mcpServers: [{name: n8n, url: "http://mcp.example.com/sse"}]`,
		`mcpServers: [{name: n8n, url: "not-a-url"}]`,
		`mcpServers: [{name: n8n, url: ""}]`,
		`mcpServers: [{name: n8n, url: "https://a.example.com/sse"}, {name: n8n, url: "https://b.example.com/sse"}]`,
	}
	for i, body := range bad {
		if err := mustValidate(t, "\n"+body+"\n"); err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
	}
	if err := mustValidate(t, "\n"+`mcpServers: [{name: n8n, url: "https://mcp.example.com/sse"}]`+"\n"); err != nil {
		t.Fatalf("valid mcpServers rejected: %v", err)
	}
}

func TestPacksWithoutMCPServersStillValidate(t *testing.T) {
	if err := mustValidate(t, ""); err != nil {
		t.Fatalf("pack without mcpServers must validate: %v", err)
	}
}

