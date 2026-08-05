# graph-sync

Kafka consumer group `graph-sync` that materializes the per-tenant knowledge
graph (SPEC-W28) into FalkorDB via Redis-protocol Cypher. Mirrors the
crm-sync-service layout (multi-topic consumers, tiny Prometheus registry,
/healthz + /metrics sidecar) and the W24 booking-service consumer patterns
(explicit commits, DLQ after 3 attempts, idempotency markers).

## What it does (SPEC-W28 §4 WS-A)

- **Idempotent upserts by `event_id`** — a `ProcessedEvent` marker node is
  MERGEd in the graph; duplicates are skipped (W24 pattern).
- **tenant_id is mandatory on every node and edge** (SPEC §5 gate 1);
  tenant-less events are poison and dead-letter immediately.
- **Phones are stored only as `phone_hash`** — SHA-256(salt|tenant|phone),
  the same salted-hash posture as the leads dedupe scheme
  (`booking-service/internal/leads.DedupeKey`). `name` is the only
  plaintext PII in the graph and is deleted on erasure.
- **Entity resolution** — exact `phone_hash` match (same tenant) auto-merges
  into the existing Person node. Ollama `nomic-embed-text` embedding
  similarity over name+channel context (cosine ≥ 0.92) creates a
  `MERGE_CANDIDATE` edge only — never a merge. If Ollama is unreachable the
  service degrades gracefully: embeddings are skipped (logged + metric
  `embeddings_skipped`), exact-hash merges keep working.
- **Erasure** — `opendesk.consent.erasure.v1` tombstones DETACH DELETE the
  Person subgraph (Person + its Consents + Contacts; Bookings/Offerings are
  kept as transactional records) and emit an audit CloudEvent on
  `opendesk.graph.erasure.done.v1`.
- **Enrichment** — `opendesk.graph.enrichment.v1` (nightly spark
  `graph_enrichment.py`) applies per-Person property rows
  (`bookings_total`, `ltv_cents`, `no_show_rate`, `cac_channel_ngn_30d`,
  `propensity_*`, …) onto EXISTING Person nodes only; unknown/erased
  persons are dropped (docs/graph.md §4 — no resurrection).
- **Quarantine** — imported, consent-unverified persons carry
  `quarantine: true`; the flag is monotonic (never cleared by later
  non-quarantine events).

## FalkorDB access

Writes go through the `graph.Client` interface (`internal/graph/graph.go`);
the concrete implementation speaks RESP `GRAPH.QUERY` via `go-redis` — the
documented alternative to `github.com/FalkorDB/falkordb-go` (FalkorDB is a
Redis-protocol graph store; parameters are sent as escaped Cypher literals
in the `CYPHER k=v` query prefix, so event data never reaches the query
string unescaped). Tests run against an in-memory fake.

## Configuration (env)

| Var | Default | Purpose |
|---|---|---|
| `PORT` | `7015` | HTTP sidecar (`/healthz`, `/metrics`) |
| `KAFKA_BROKERS` | `kafka:9092` | broker list |
| `GRAPH_SYNC_GROUP` | `graph-sync` | consumer group |
| `GRAPH_SYNC_BOOKING_TOPIC` | `opendesk.booking.events` | bookings → Person/Booking/Offering |
| `GRAPH_SYNC_IDENTITY_TOPIC` | `opendesk.identity.events` | tenants, contact captures, consents |
| `GRAPH_SYNC_TRANSCRIPTS_TOPIC` | `opendesk.conversation.transcripts` | callers → Person (voice) |
| `GRAPH_SYNC_ERASURE_TOPIC` | `opendesk.consent.erasure.v1` | erasure tombstones |
| `GRAPH_SYNC_ENRICHMENT_TOPIC` | `opendesk.graph.enrichment.v1` | nightly gold→graph Person property rows (spark `graph_enrichment.py`); empty = skip |
| `GRAPH_SYNC_CAC_TOPIC` | _(empty = skip)_ | optional funnel/CAC lead events (W13) |
| `GRAPH_SYNC_DLQ_TOPIC` | `opendesk.dlq` | poison messages (after 3 attempts) |
| `GRAPH_ERASURE_DONE_TOPIC` | `opendesk.graph.erasure.done.v1` | erasure audit events |
| `FALKORDB_ADDR` | `graph-db:6379` | FalkorDB Redis-protocol address |
| `FALKORDB_GRAPH` | `opendesk` | graph name |
| `PHONE_HASH_SALT` | _(empty, warn)_ | SHA-256 phone-hash salt (required in prod) |
| `OLLAMA_BASE_URL` | `http://localhost:11434/v1` | OpenAI-compatible endpoint |
| `OLLAMA_EMBED_MODEL` | `nomic-embed-text` | embedding model |
| `GRAPH_SYNC_MERGE_THRESHOLD` | `0.92` | cosine floor for MERGE_CANDIDATE |
| `CONSUMER_ENABLED` | `true` | gate all consumers |
| `SHUTDOWN_TIMEOUT_SECONDS` | `20` | graceful shutdown budget |

## Consumed event types (CloudEvents 1.0)

- `com.opendesk.booking.Booking{Created,Confirmed,Rescheduled,Cancelled,Completed,NoShow}`
- `com.opendesk.identity.{TenantProvisioned,ContactCaptured,ConsentGranted,ConsentRevoked}`
- `com.opendesk.conversation.SessionEnded` (transcripts topic)
- `com.opendesk.leads.LeadCreated` / `com.opendesk.identity.ContactCaptured` (CAC topic)
- `com.opendesk.consent.ErasureRequested` (erasure topic)
- `com.opendesk.graph.PersonEnrichment` (enrichment topic; properties applied
  via tenant-scoped `MATCH`+`SET p += $props` — rows for unknown/erased
  persons are dropped, never resurrected: metric
  `enrichment_dropped_unknown_person` + debug log)

Unknown types on a consumed topic are acknowledged and skipped
(forward-compatible). Emits `com.opendesk.graph.ErasureDone`.

## Development

```sh
go build ./... && go vet ./... && go test ./...
```
