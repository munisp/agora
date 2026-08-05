# graph-service (SPEC-W28 WS-B, port 7014)

Tenant-scoped, consent-gated read API over the per-tenant knowledge graph in
FalkorDB. Every query injects the authenticated `tenant_id` at the query
layer — there is no exempt path.

## Endpoints

| Route | Description |
|---|---|
| `POST /v1/graph/segments` | Save declarative segment DSL; compiled Cypher (mandatory consent + tenant + quarantine filters) stored with it |
| `POST /v1/graph/segments/count` | Live count preview for UNSAVED DSL (Segment Builder); identical mandatory gates, persists nothing |
| `GET /v1/graph/segments` | List caller's segments (tenant-isolated) |
| `GET /v1/graph/segments/{id}/count` | Consent-passing count preview (quarantined excluded) |
| `POST /v1/graph/segments/{id}/audience` | Materialize consent-passing audience → `audience_id` + member refs (`{person_id, phone_hash, lead_id}`; see below) |
| `GET /v1/graph/audiences/{id}` | Fetch materialized audience (notification-worker handoff, WS-C) |
| `POST /v1/graph/ask` | NL→Cypher GraphRAG via Ollama: LLM selects an allowlisted read-only template + params (strict JSON); service renders canonical Cypher (tenant filter injected post-generation, rows capped at 100); 503 + reason when Ollama is down |
| `GET /v1/graph/persons/{id}` | Person 360 (contacts, bookings, consents, referrals, messages); 404 cross-tenant |
| `POST /v1/graph/cypher` | Template-allowlisted parameterized queries ONLY — raw Cypher rejected (compliance gate 5) |
| `GET /healthz`, `GET /metrics` | Workforce conventions |

## Auth (workforce seam)

- `JWT_PUBLIC_KEY` set → `Authorization: Bearer <jwt>` required; signature
  verified (`JWT_ALGORITHM`, HS256/RS256/ES256); tenant = JWT `sub` claim.
- `JWT_PUBLIC_KEY` unset → dev mode: `X-Tenant-Id` header supplies the tenant
  (dev compose / tests only). Bearer tokens are rejected in this mode.

## Segment DSL

```json
{
  "name": "Lapsed Alimosho customers",
  "purpose": "marketing",
  "filter": {
    "has_consent": "marketing",
    "last_booking_before": "2026-01-01",
    "lga": "Alimosho",
    "not_messaged_since_days": 30
  }
}
```

- `has_consent` defaults to `purpose`; the compiled query ALWAYS requires a
  purpose-matching unrevoked `CONSENTED` edge (gate 2) and excludes
  quarantined persons (gate 4 — `include_quarantined` exists only to make
  this explicit; `true` is rejected).
- `last_booking_before`: persons with ≥1 booking whose most recent booking
  precedes the date (lapsed customers).

## Audience member shape (orchestrator contract)

Every materialized audience member is:

```json
{"person_id": "pa1", "phone_hash": "hash-pa1", "lead_id": "lead1b"}
```

- `lead_id` comes from the Person's `HAS_CONTACT` edge → `Contact.lead_id`
  (the booking-service lead the contact was captured as). **Resolution rule:
  the MOST RECENT Contact (by `captured_at`) with a non-null `lead_id` wins;
  `lead_id` is `null` when the person has no such Contact.**
- The graph stays raw-PII-free: no phone numbers in graph-service responses.
  Phone resolution happens downstream (notification-worker → booking-service).
- `GET /v1/graph/audiences/{id}` returns the same member shape (stored record).

## Segment/audience persistence — documented choice

**JSON file store** (`SEGMENT_STORE_DIR`, atomic tmp+rename writes, in-memory
index rebuilt at startup). Chosen over FalkorDB-persistence because segment
definitions are operational metadata, not tenant graph relationships, and the
spec mandates a Postgres-free option; files back up cleanly alongside
FalkorDB RDB snapshots. Records carry `tenant_id`; all lookups are
tenant-scoped (cross-tenant → 404).

## NL→Cypher design (gate 5 aligned)

The LLM never produces executable Cypher. It answers a schema-prompted
question by selecting one of the read-only templates in `app/templates.py`
plus params. The service renders the parameterized Cypher itself — this is
how the tenant filter is injected post-generation and why prompt injection
cannot reach the graph store. The generated Cypher is returned with the rows.

## Backends

`GRAPH_BACKEND=falkordb` (default) runs canonical Cypher against FalkorDB
(`falkordb` driver imported lazily). `GRAPH_BACKEND=memory` swaps in an
in-memory property graph that evaluates the same query plans — the pytest
suite needs no live graph DB.

## Environment

| Var | Default | Notes |
|---|---|---|
| `PORT` / `HOST` | `7014` / `0.0.0.0` | |
| `GRAPH_BACKEND` | `falkordb` | `falkordb` or `memory` |
| `FALKORDB_HOST` / `FALKORDB_PORT` | `localhost` / `6379` | |
| `FALKORDB_GRAPH` | `agora_tenants` | graph name per deployment |
| `FALKORDB_USERNAME` / `FALKORDB_PASSWORD` | empty | |
| `JWT_PUBLIC_KEY` | empty (dev mode) | PEM (RS256/ES256) or HMAC secret (HS256) |
| `JWT_ALGORITHM` | `RS256` | |
| `OLLAMA_BASE_URL` | `http://localhost:11434/v1` | OpenAI-compatible |
| `GRAPH_ASK_MODEL` | `qwen2.5:7b-instruct` | |
| `OLLAMA_API_KEY` / `OLLAMA_TIMEOUT_S` | `ollama` / `30` | |
| `SEGMENT_STORE_DIR` | `./data/graph-service` | |
| `ASK_ROW_CAP` / `QUERY_ROW_CAP` / `SEGMENT_ROW_CAP` | `100` / `100` / `10000` | |

## Tests

`python -m pytest` — 54 tests incl. tenant-isolation negatives for every
endpoint, consent-gating (no-consent person never in any audience),
quarantine exclusion, Cypher-injection rejection, and ask allowlist
enforcement.
