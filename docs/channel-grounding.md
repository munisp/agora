# Channel Grounding (SPEC-W108)

Every response surface now answers from the same knowledge base. One KB, one
embedding space (all-MiniLM-L6-v2, OpenSearch hybrid BM25+kNN) — three
surfaces:

| Surface | Path | Status |
|---|---|---|
| Voice | voice-agent-runtime tenant-context knowledge leg (`GET /v1/context`) + prompts `{knowledge}` block | REPAIRED in W108 (was silently dead) |
| WhatsApp / Telegram | messaging-gateway bridge -> voice `/voice/chat` -> same tenant-context leg | REPAIRED by the same fix |
| USSD (text mode) | conversation-service grounding in `/v1/ussd/turns` | NEW in W108 |
| Chat (admin) | `knowledge_packs` tags = config only | unchanged |

## The W108 repair (Part A)

Since SPEC-W43 C1, knowledge-service binds every request to the
`X-Tenant-Slugs` header (401 without it; the standalone-dev escape is off in
compose). The voice runtime's knowledge leg invoked `GET /v1/context`
WITHOUT the header, inside a best-effort try/except — so grounding 401'd on
every call and only a warning log marked it. Voice sessions and the
WhatsApp/Telegram bridges (which ride `/voice/chat`) were answering
ungrounded.

W108 passes `X-Tenant-Slugs: <tenant_slug>` on the invoke. Trust posture:
the header is gateway-injected for EXTERNAL callers; internal services on
the Dapr network self-pass it — the same trust level as existing
service-to-service invokes that carry tenant ids in the body (e.g.
`/v1/ussd/turns`). External binding enforcement is unchanged.

## USSD text-mode grounding (Part B)

In pass-through text mode (no tenant-pack menu), a non-empty free-text
selection triggers a KB lookup:

- `GET v1/context` on app-id `knowledge` with `tenant=<uuid>`,
  `q=<selection>`, header `X-Tenant-Slugs: <uuid>` (UUID short-circuits
  slug resolution server-side).
- Tight per-call timeout (`USSD_KB_TIMEOUT_S`, default **0.8s** — the USSD
  turn budget is ~1s) via a per-request httpx timeout override.
- Top snippets are whitespace-collapsed, joined (max 2), and truncated at a
  word boundary with "…" at `USSD_KB_MAX_REPLY_CHARS` (default 300).
- **Never fails a callback**: KB error, timeout, or empty snippets fall back
  to the pre-W108 static ack, byte-identical. The response payload gains
  `"grounded": true|false` (additive; the gateway ignores unknown keys).
- Menu mode and the empty session-start turn never call the KB.

Config: `KNOWLEDGE_APP_ID`, `USSD_KB_GROUNDING` (default on),
`USSD_KB_TIMEOUT_S`, `USSD_KB_MAX_REPLY_CHARS`, `USSD_KB_MAX_SNIPPETS`.

## Honest limits

- The voice knowledge leg runs once per tenant-context TTL with a STATIC
  bootstrap query (`KNOWLEDGE_QUERY`, default "opening hours services
  pricing") — it grounds the session persona, not each turn. Per-turn
  grounding queries are a separate wave.
- USSD answers are raw KB text, not LLM-composed — good for facts (hours,
  pricing, policies), terse by design. LLM compression is a documented
  follow-up.
- USSD grounding quality depends on the KB corpus: tenants with no
  documents ingest nothing and always get the static ack (grounded=false).
