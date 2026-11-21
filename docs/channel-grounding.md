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

## Per-turn grounding (W109)

`/voice/chat` (web chat + the WhatsApp/Telegram bridges) now grounds EACH
turn: the post-translation user message becomes the `/v1/context` query
(turn snippets lead the bootstrap snippets in the system prompt, deduped,
capped at `knowledge_snippet_count`). Best-effort under
`KNOWLEDGE_TURN_TIMEOUT_S` (default 1.5s): any error/timeout/empty result
keeps the bootstrap grounding, and a turn never fails on grounding.
`KNOWLEDGE_TURN_QUERIES=0` restores bootstrap-only behavior.

The LiveKit REALTIME voice path is deliberately unchanged: its per-utterance
latency budget is sub-second and the prompt is built at session start; the
`knowledge_search` slow tool (filler ack + 4s tool timeout) covers explicit
lookups there.

## LLM-compressed USSD answers (W110)

Raw snippet text is accurate but terse. With `USSD_KB_LLM=on` (default
**off**) the grounded snippets are composed into a short natural answer by
an OpenAI-compatible LLM (`USSD_KB_LLM_MODEL`, default qwen3:8b via Ollama,
temperature 0, ≤120 tokens, facts-only prompt). The fallback chain is
**LLM answer → raw snippet → static ack**; the response payload gains
`"composed": true|false`.

Why default off: the compose call runs AFTER the 0.8s KB fetch and a cold
model can stretch the turn toward the ~2s aggregator budget documented in
docs/channels-ussd.md. Operators enabling it should watch turn latency.
Defense in depth: compose_answer never raises (None on any failure) AND the
W108 grounding block's own fallback net catches anything that escapes.

## Grounding-quality metrics (W112)

Both grounding surfaces emit bounded-cardinality counters so operators can
see whether the KB actually answers users, per tenant and channel:

- `voice_knowledge_grounding_total{tenant,channel,result}` — one increment
  per eligible /voice/chat turn lookup; result is `hit` (snippets
  returned), `miss` (empty), `timeout`, or `error`. Disabled grounding and
  empty queries are not counted.
- `conversation_grounding_turns_total{tenant,channel,result}` — one
  increment per eligible USSD text-mode turn; result is `composed`, `raw`,
  `compose_fallback`, `ungrounded`, `error`, or `disabled` (flag off).
  Menu turns and empty first turns are not counted.

Tenant label values are sanitized and capped at
`GROUNDING_METRICS_MAX_TENANTS` (default 100) distinct tenants; beyond the
cap all tenants aggregate under `tenant="overflow"` so a multi-tenant
fleet cannot blow up Prometheus cardinality. A high `miss`/`ungrounded`
rate means the tenant corpus is thin; a rising `timeout`/`error` rate
means the knowledge leg is unhealthy. Metrics recording never fails a
turn.

Retrieval quality itself is addressed by the W113 cross-encoder rerank
stage — see docs/rag-reranker.md. Its uplift shows up in these counters as
falling `miss`/`ungrounded` rates.

## Honest limits

- The bootstrap knowledge leg still runs once per tenant-context TTL with
  the static `KNOWLEDGE_QUERY` — it grounds the session persona; per-turn
  relevance comes from W109.
- USSD answers default to raw KB text (W110 LLM compose is opt-in) —
  good for facts (hours, pricing, policies), terse by design.
- USSD grounding quality depends on the KB corpus: tenants with no
  documents ingest nothing and always get the static ack (grounded=false).
