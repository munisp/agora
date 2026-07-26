# MCP tools (SPEC-W9 Part C)

OpenDesk's voice/chat tool layer can consume external **MCP (Model Context
Protocol)** servers — the open protocol (Anthropic, now broadly adopted) for
exposing tools to LLM agents over JSON-RPC 2.0. An MCP server advertises a
tool catalog (`tools/list`); the agent calls them (`tools/call`). This lets a
tenant's receptionist use workflows built in n8n, a CRM's MCP endpoint, or any
other MCP-compatible server **without writing a custom integration** — the
remote tool schemas are passed straight through to the model.

The client is hand-rolled in `services/voice-agent-runtime/app/mcp_client.py`
(httpx only — no new dependencies) and supports both HTTP transports:

- **Streamable HTTP** (protocol `2025-03-26`) — one POST per JSON-RPC message,
  JSON or SSE response, `mcp-session-id` header echoed after `initialize`.
- **Legacy HTTP+SSE** (`2024-11-05`, what n8n's MCP Server Trigger exposes at
  `.../sse`) — a hanging GET streams an `endpoint` event then `message`
  responses; messages are POSTed to that endpoint.

Handshake: `initialize` (protocolVersion `2025-03-26`, clientInfo
`opendesk-voice/1.0`) → `notifications/initialized` → `tools/list`. Tool
execution is `tools/call`.

## How tools appear to the agent

Every remote tool is namespaced **`mcp__{server}__{tool}`** (truncated to 64
chars), so an MCP tool can never shadow a built-in receptionist tool or a
pack `customTools` entry. The remote `inputSchema` is passed through verbatim
as the function parameters. MCP tools are appended **after** customTools in
`build_plugin_tools`, so both the chat (`/voice/chat`) and voice paths get
them through the existing tool layer with zero changes to `chat.py`/`tools.py`.

Tool results are normalised: MCP `content[].text` is JSON-parsed when possible
and returned as `{status, body}`; `isError: true` maps to `status: "error"`.
Calls run inside the existing `AsyncToolRunner` hard timeout — a timeout or
failure resolves to the spoken-apology payload, never dead air, never an
exception into the pipeline.

## Configuration

### Environment (operator-level, may carry credentials)

`MCP_SERVERS` — JSON list, read directly from the environment by
`mcp_client.py` (no `config.py` change):

```bash
MCP_SERVERS='[
  {
    "name": "n8n",
    "url": "https://n8n.example.com/mcp/front-desk/sse",
    "headers": {"authorization": "Bearer <token>"},
    "transport": "sse"
  }
]'
```

| field | required | notes |
| --- | --- | --- |
| `name` | yes | slug `^[a-z][a-z0-9-]*$`; becomes the `mcp__{name}__` namespace |
| `url` | yes | **https only** |
| `headers` | no | auth headers — **env only, never in packs** |
| `transport` | no | `auto` (default; URLs ending in `/sse` use the legacy SSE transport), `http`, `sse` |

Tunables (all optional):

| env | default | meaning |
| --- | --- | --- |
| `MCP_CONNECT_TIMEOUT_SECONDS` | `5` | connect/handshake/tools-list timeout per server |
| `MCP_TOOL_TIMEOUT_SECONDS` | `TOOL_TIMEOUT_SECONDS` (`4`) | hard per-call timeout via AsyncToolRunner |
| `MCP_TOOLS_CACHE_SECONDS` | `300` | TTL of the per-server tool-catalog cache |
| `MCP_FAILURE_CACHE_SECONDS` | `30` | negative-cache window for unreachable servers |

### Pack level (per-tenant)

Industry packs may declare `mcpServers` — validated by
`scripts/validate_pack.py` and identity-service's Go pack loader, and passed
through into the tenant context JSON exactly like `customTools`:

```yaml
mcpServers:
- name: n8n
  url: https://n8n.example.com/mcp/acme-front-desk/sse
```

Pack entries are `{name, url}` **only** — no `headers` (the validator rejects
them): credentials live in the operator-controlled env var, never in a pack
file that is shipped, hashed and registry-listed. Env and pack lists are
merged at session setup; on a name clash the env entry wins.

> Wiring note: the Go loader and validator passthrough landed in Wave 9. The
> chat path currently calls `build_plugin_tools(ctx.custom_tools, …)` without
> the tenant context, so per-tenant pack servers activate there as soon as a
> one-line follow-up passes `tenant_ctx=ctx` through (the API and the
> defensive `mcp_servers`/`pack.mcpServers` extraction are already in place
> and covered by tests).

## n8n walkthrough

Expose an n8n workflow as MCP tools for the receptionist:

1. In n8n, create a workflow starting with an **MCP Server Trigger** node.
   n8n gives it a production URL like
   `https://n8n.example.com/mcp/front-desk/sse`.
2. Attach tool nodes under the trigger (e.g. *Google Sheets: add row*, *HTTP
   Request: check order status*, *Postgres: lookup customer*). Each becomes
   one MCP tool with its own input schema.
3. Protect the endpoint (n8n MCP trigger auth → Bearer token or header auth).
4. Point OpenDesk at it:

   ```bash
   MCP_SERVERS='[{"name":"n8n","url":"https://n8n.example.com/mcp/front-desk/sse","headers":{"authorization":"Bearer <token>"}}]'
   ```

   …or, per tenant, add the `mcpServers` block to the tenant's industry pack
   (no token in the pack — put it in env with a matching server `name`, or use
   n8n's path-level secret).
5. Restart the voice runtime. At session setup you should see
   `mcp tool registered … tool=mcp__n8n__add_row` log lines; the agent can
   then call those tools mid-conversation ("add this caller to the callback
   list", "check the status of order 1042").

If the server is down, the log shows `mcp server skipped …` and the session
continues without those tools — nothing else is affected.

## Security model

- **https only.** MCP servers live outside the `PLUGIN_ALLOWED_HOSTS` SSRF
  guard (they are arbitrary external hosts by design), so plaintext http is
  rejected outright in all three validators (env parser, pack YAML validator,
  Go pack loader).
- **Per-tenant scoping.** Pack `mcpServers` ride the same tenant-context path
  as `customTools` — a tenant only ever gets its own pack's servers plus the
  operator's env list.
- **No credentials in packs or prompts.** Auth headers come from the env
  config only. Tool *names/descriptions/schemas* reach the model; server URLs
  and headers never appear in the system prompt.
- **Timeouts everywhere.** 5s connect/handshake budget; every `tools/call`
  inside the AsyncToolRunner hard timeout (default 4s) with the spoken-apology
  fallback.
- **Failure isolation.** Any server failure (connect, handshake, list, call)
  logs a warning and skips that server; a misconfigured entry in `MCP_SERVERS`
  or a pack is dropped with a warning. The session never breaks.
- **Namespaces.** `mcp__{server}__{tool}` prevents collisions with built-in
  tools, pack customTools, or another MCP server.

## Tests

`services/voice-agent-runtime/tests/test_mcp_client.py` (fake MCP servers on
`httpx.MockTransport`): initialize handshake (both transports, session-id
echo), tools/list → ToolDef shape + JSON-schema passthrough, tools/call
round-trip incl. `isError` mapping, server-down graceful skip, namespacing
(+64-char truncation), env/pack parsing rules, sync-bridge caching, and the
`build_plugin_tools` no-MCP byte-compatibility. Validator coverage for the
`mcpServers` block is in `tests/packs/test_validate_pack.py`; Go loader
coverage in `internal/packs/packs_mcp_test.go`.
