# SPEC-W9 — Avatar Presence, Agent-Driven UI Actions, MCP Tool Protocol, Hospitality Pack

Wave 9 contract (gaps learned from the Tavus/n8n-MCP avatar demo). Repo: `/mnt/agents/output/opendesk`
(flaky FUSE — work in `/tmp`, rsync ADDITIVELY, md5-verify every written file).

OWNERSHIP (collision-critical):
- Agent A: `services/voice-agent-runtime/app/avatar/**`, `app/config.py`, `app/control_plane.py`,
  `services/avatar-renderer/**` (new), `apps/admin-web/components/voice-session-button.tsx`,
  `apps/admin-web/app/app/[orgSlug]/call/call-client.tsx`, `docs/avatar.md`
- Agent B: `services/voice-agent-runtime/app/tools.py`, `app/chat.py`, `app/ui_actions.py` (new),
  `apps/admin-web/public/embed.js`, `apps/admin-web/app/embed/**`, `docs/widget-actions.md`
- Agent C: `services/voice-agent-runtime/app/mcp_client.py` (new), `app/plugin_tools.py`,
  `scripts/validate_pack.py`, the Go pack loader (find it — the file that passes through
  consentText/languages/customTools), `docs/mcp.md`. Do NOT edit app/config.py — read env directly.
- Agent D: `industries/hospitality.yaml`, `industries/index.json`, `scripts/seed-industries.sh`,
  `docs/industries.md`, `apps/marketing/index.html`

## Part A — Avatar provider (Agent A)

### A1. Provider abstraction (`app/avatar/`)
- `base.py`: `AvatarProvider` protocol — `async def join_room(room: str, *, tenant_ctx) -> AvatarStatus`;
  `AvatarStatus{provider, status: "off"|"joining"|"unavailable", detail?}`. Registry by name.
- `tavus.py`: provider `tavus`. On session start, POST `https://api.tavus.io/v2/conversations`
  (`x-api-key: TAVUS_API_KEY`) with `{replica_id: TAVUS_REPLICA_ID, persona_id: TAVUS_PERSONA_ID,
  properties: {livekit_room_name: room}}` (verify the exact Tavus CVI LiveKit-join request shape against
  current Tavus docs via web search; isolate it behind one function so corrections are one-file).
  10s timeout, never blocks session creation — failure → status unavailable + warning log.
- `musetalk.py`: provider `musetalk` — open-source lip-sync path. Config: `MUSEtalk_ROOM_AGENT=true`
  publishes intent for the avatar-renderer sidecar to join the room (the sidecar does the actual
  inference; this provider returns joining when `AVATAR_RENDERER=enabled`).
- `AVATAR_PROVIDER=none|tavus|musetalk` (default none) in config.py; per-tenant override via tenant
  context `avatar_provider` if present (defensive getattr).

### A2. Session integration (`app/control_plane.py`)
- `/voice/session` LiveKit path: after minting the token, if provider != none, fire-and-forget
  `asyncio.create_task(provider.join_room(room, ...))`; extend `VoiceSessionResponse` additively:
  `avatar: dict | None = None` → `{provider, status}`.
- Frontend (voice-session-button.tsx, call-client.tsx): when a remote VIDEO track appears in the
  LiveKit room, render an avatar tile (rounded, warm-styled) above the audio visualizer; audio-only
  fallback unchanged. React = existing patterns; no new deps.

### A3. `services/avatar-renderer/` scaffold (open renderer sidecar)
- README + Dockerfile (python:3.11-slim base, pin `musetalk` per its repo instructions as comments with
  the exact pip/git lines ready to enable), `app/main.py`: LiveKit worker skeleton — subscribes to the
  agent audio track in `site-{slug}` rooms, runs frame generation behind a `Renderer` protocol with a
  `MockRenderer` (publishes a solid warm-color test pattern video track at 15fps so the pipeline is
  e2e-testable without GPU) and a documented `MuseTalkRenderer` stub (audio-frame buffer → inference →
  RGB frames → `rtc.VideoSource`). Compose override `infra/compose/avatar.compose.yml` (new file —
  do NOT edit docker-compose.yml).
- docs/avatar.md: provider comparison (Tavus vs open lip-sync: quality/latency/GPU needs/cost), setup
  for each, mock-renderer testing, honest GPU requirements.

## Part B — Agent-driven UI actions (Agent B)

### B1. Tool layer (`app/ui_actions.py` + `app/tools.py`)
- New module `ui_actions.py`: action models + validation. EXACT action set:
  - `{type:"navigate", path}` — path MUST start with `/`, no scheme/host (same-origin only).
  - `{type:"highlight", selector}` — CSS selector, sanitized: `[a-zA-Z0-9\-_#. :\[\]="'>]` only, ≤120 chars.
  - `{type:"prefill_booking", offering_id}` — uuid string.
- Register 3 tools in the tool layer: `navigate_to_page(path)`, `highlight_element(selector)`,
  `prefill_booking(offering_id)` — each validates via ui_actions and returns an ack; the action is
  attached to the turn's outgoing payload (NOT executed server-side).

### B2. Transport (`app/chat.py`)
- Buffered `/voice/chat` response gains additive `ui_actions: [...]` (validated actions from tools the
  LLM invoked this turn). SSE streaming mode emits `data: {"ui_action": {...}}` frames before `done`.
- System prompt addendum (1-2 sentences, appended in prompts.py ONLY IF that file stays untouched by
  others — check ownership; otherwise inject from chat.py): tells the agent it can offer to show pages
  / highlight the booking form.

### B3. Widget execution (`apps/admin-web/public/embed.js` + `app/embed/**`)
- embed.js: after each chat response (and on SSE ui_action frames), execute actions:
  navigate → `window.location.assign(path)` (same-origin guard); highlight → `querySelector`,
  `scrollIntoView({behavior:'smooth',block:'center'})` + 2s terracotta outline pulse class (inject the
  tiny CSS); prefill_booking → post the offering id into the booking form state (follow how the embed
  widget/page currently selects offerings; if no form API exists, dispatch a
  `CustomEvent('opendesk:prefill',{detail:{offering_id}})` and wire the embed page's booking form to
  listen for it). Never throws — wrap each action in try/catch.
- docs/widget-actions.md: action reference, security model (same-origin, selector sanitization,
  server-validated), vertical examples ("show me your rooms", "highlight the book button").

## Part C — MCP client (Agent C)

### C1. `app/mcp_client.py` (voice-agent-runtime)
- Hand-rolled MCP (Model Context Protocol) client, JSON-RPC 2.0 over streamable HTTP+SSE using httpx
  (already a dep — NO new deps): `initialize` (protocolVersion "2025-03-26", clientInfo
  opendesk-voice) → `notifications/initialized` → `tools/list` → `tools/call`.
- Config: env `MCP_SERVERS` JSON `[{"name":"n8n","url":"https://.../sse","headers":{...}}]` PLUS
  per-tenant pack-level `mcpServers` (Part C2) merged at session setup.
- `async def build_mcp_tools(cfg, tenant_ctx) -> list[ToolDef]`: per server, connect with 5s timeout;
  tools namespaced `mcp__{server}__{tool}`; JSON-schema passthrough for parameters; calls routed
  through the existing AsyncToolRunner timeout; ANY server failure → log warning, skip that server
  (never breaks the session).
- Wire into `app/plugin_tools.py::build_plugin_tools` (wrap: after customTools, append MCP tools) so
  both chat and voice paths get them without touching chat.py/tools.py.
- Tests (`app/tests/test_mcp_client.py` or sibling pattern): fake MCP server (httpx MockTransport or
  local aiohttp) covering initialize handshake, tools/list parsing → ToolDef shape, tools/call round
  trip, server-down graceful skip, namespacing.

### C2. Pack schema `mcpServers` passthrough
- `scripts/validate_pack.py`: allow optional `mcpServers: [{name, url}]` (name slug-regex, url https
  required). Must still pass for all 31 existing packs.
- Go pack loader: pass `mcpServers` through to the tenant context JSON exactly like
  consentText/languages/customTools (find that loader; additive). go build/vet/test green for the
  owning service (Go at /tmp/sdk/go/bin/go or reinstall; GOPROXY=https://goproxy.cn,direct).
- docs/mcp.md: what MCP is, n8n MCP server setup example, env + pack config reference, security notes
  (per-tenant scoping, https-only, timeouts, no credential passthrough in prompts).

## Part D — Hospitality pack + consistency (Agent D)

- `industries/hospitality.yaml` (31st pack): rooms/suites with kobo+USD pricing example, check-in/out
  terminology, offerings (standard/deluxe/suite night, airport pickup, breakfast package, event-hall
  day), deposit policy (first night), upsell-aware persona (friendly concierge, offers
  pickup/breakfast/late-checkout), knowledgeSeed (check-in/out times, amenities, pet policy, cancellation),
  reminders (pre-arrival 48h/24h), dashboardLabels (occupancy, ADR, RevPAR), languages [en, pcm].
  Validate with scripts/validate_pack.py.
- `industries/index.json` → 31 entries (same sha256 flow).
- `scripts/seed-industries.sh`: add `acme-hotel` tenant (Lagos, NGN) following existing pattern.
- `docs/industries.md`: add row (31 total).
- `apps/marketing/index.html`: update pack count claims 30→31 and add Hospitality to the verticals
  showcase (minimal, careful diff — keep everything else intact).

## Cross-agent notes
- A/B/C all touch voice-agent-runtime but disjoint files (see OWNERSHIP). prompts.py is NOT assigned —
  B injects the UI-action prompt addendum from chat.py.
- D's pack must pass the CURRENT validator before C's mcpServers change lands — coordinate via the
  validator's existing optional-block behavior (it already tolerates optional blocks; C's change is
  additive-only).

