# conversation-service

Conversation session + turn persistence and transcript streaming for OpenDesk
(SPEC §4 topics, §7 conversation schema, §11 voice pipeline). Python 3.12,
FastAPI, asyncpg, structlog.

## Endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/healthz` | Liveness (pings Postgres) |
| POST | `/v1/conversations` | Start a conversation `{tenant_id, site_slug, channel}` (channel: voice\|chat\|phone\|api) |
| POST | `/v1/conversations/{id}/turns` | Append a turn `{role, text, tool_calls?, audio_url?}` (role: user\|agent\|system\|tool); seq assigned atomically |
| GET | `/v1/conversations?tenant=<uuid>` | List tenant conversations (paged) |
| GET | `/v1/conversations/{id}?tenant=<uuid>` | Conversation with all turns |

**Tenant scope** is required on every request via `?tenant=<uuid>` or the
`X-Tenant-ID` header. The schema enforces `FORCE ROW LEVEL SECURITY`
(`app.tenant_id` setting), so the service sets it per transaction — requests
without tenant scope are rejected `400`; cross-tenant rows are invisible.

## Turn pipeline (per accepted turn)

1. Insert into Postgres (`turns`, next `seq`, in a transaction with an
   advisory lock per conversation).
2. Publish the **raw record** `{conversationId, tenantId, role, text, ts}` to
   topic `opendesk.transcripts-raw` through a `TranscriptSink` (SPEC §5):
   - `TRANSCRIPT_SINK=fluvio` → `FluvioSink` (official fluvio python client,
     import guarded, sync client wrapped in `asyncio.to_thread`)
   - `TRANSCRIPT_SINK=kafka` (default) → `KafkaSink` (aiokafka producer,
     same topic name on Kafka) — also the automatic fallback when the
     optional `fluvio` package is missing.
3. Always publish a **CloudEvent** (`ConversationTurn` per SPEC §4) to Kafka
   topic `opendesk.conversation.transcripts` via the Dapr HTTP pubsub
   component `pubsub-kafka` (`application/cloudevents+json`).

Postgres is the source of truth: streaming/publish failures are logged and do
not fail the API request.

## Indexer

Background task (aiokafka consumer, group `conversation-service-indexer`)
reading `opendesk.conversation.transcripts` and bulk-indexing documents into
the OpenSearch index `conversations`, conforming to the mapping in
`infra/opensearch/setup-indices.sh` (`tenant_id, conversation_id, site_slug,
channel, role, text, audio_url, ts`). `site_slug`/`channel` are enriched from
Postgres; `redacted` is set by the index's `pii-safe` default ingest pipeline.
Batched flushes (size/time), explicit offset commits after each flush.

## Environment variables

| Var | Default | Description |
|---|---|---|
| `PORT` | `7007` | HTTP listen port |
| `PG_DSN` | `postgres://opendesk:opendesk@postgres:5432` | Base Postgres DSN |
| `PG_DATABASE` | `conversation` | Database name (appended to PG_DSN) |
| `PG_MIN_SIZE` / `PG_MAX_SIZE` | `1` / `10` | Pool sizes |
| `DAPR_HOST` / `DAPR_HTTP_PORT` | `daprd-conversation` / `3500` | daprd sidecar |
| `DAPR_PUBSUB_NAME` | `pubsub-kafka` | Dapr pubsub component |
| `TRANSCRIPTS_TOPIC` | `opendesk.conversation.transcripts` | CloudEvent topic |
| `TRANSCRIPT_SINK` | `kafka` | Raw transcript sink: `fluvio` or `kafka` |
| `FLUVIO_TOPIC` | `opendesk.transcripts-raw` | Raw transcript topic |
| `KAFKA_BROKERS` | `kafka:9092` | Broker list (sink + indexer) |
| `OPENSEARCH_ADDR` | `http://opensearch:9200` | OpenSearch address |
| `CONVERSATIONS_INDEX` | `conversations` | Target index |
| `INDEXER_ENABLED` | `true` | Run the indexer task |
| `INDEXER_GROUP` | `conversation-service-indexer` | Consumer group |
| `INDEXER_BULK_SIZE` / `INDEXER_BULK_FLUSH_SECONDS` | `100` / `2` | Bulk batching |
| `QUALITY_ENRICH_ENABLED` | `true` | Run the call-quality sentiment enricher (Wave 5 #2) |
| `CONVERSATION_EVENTS_TOPIC` | `opendesk.conversation.events` | SessionEnded intake topic |
| `QUALITY_EVENTS_TOPIC` | `opendesk.conversation.quality` | CallQualityEnriched egress topic |
| `QUALITY_ENRICH_GROUP` | `conversation-sentiment` | Enricher consumer group |
| `RETENTION_ENABLED` | `true` | Run the retention sweeper (NDPA/GDPR storage limitation) |
| `RETENTION_DAYS` | `365` | Hard-delete turns older than this many days (`180` in the NDPA profile) |
| `RETENTION_SWEEP_SECONDS` | `3600` | Sweep interval (also runs once at startup) |
| `RETENTION_BATCH_SIZE` | `1000` | Max turns deleted per batch statement |

## Run

```bash
python -m venv .venv && . .venv/bin/activate
pip install -e .            # add [fluvio] for the Fluvio sink
python -m app.main
# or
docker build -t opendesk/conversation-service .
```

## Notes

- The fluvio dependency is optional (`pip install -e .[fluvio]`); the import
  is guarded so the image runs without it (default `kafka` sink).
- The Dockerfile builds with `pip install .` (core deps only); install the
  `fluvio` extra in a custom image when `TRANSCRIPT_SINK=fluvio`.

## Call-quality sentiment enrichment (Wave 5 #2)

Background task (`app/quality.py`, direct aiokafka like the indexer,
consumer group `conversation-sentiment`) consuming
`com.opendesk.conversation.SessionEnded` from
`opendesk.conversation.events`. When the event carries a `quality` payload
**and** `quality.confirmed_phone`, it averages the per-turn `sentiment`
scores of that conversation (turns table, written by app/intel.py) and
republishes the payload as `com.opendesk.conversation.CallQualityEnriched`
on the dedicated topic **`opendesk.conversation.quality`** — a separate
topic on purpose, so the enriched event can never retrigger SessionEnded
consumers. The enriched data adds `avg_sentiment` + `turn_sentiment_count`
(and sets `quality.avg_sentiment`). Skips (acked, never retried): no
quality, no confirmed phone, malformed ids, and conversations with zero
sentiment-scored turns. crm-sync-service merges this into the Twenty call
summary note (see its README for the eventual-consistency design).

## GDPR contact marker + privacy erasure (SPEC-W3 §2, innovation 13)

- `conversations.contact_phone` (nullable, idempotent
  `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` at startup + index) marks the
  data subject behind a conversation. It is populated at creation time from
  the caller's site/session metadata (`POST /v1/conversations` with
  `contact_phone`) when the visitor's phone (or e-mail) is known — anonymous
  sessions stay NULL. It is deliberately a single flat marker column instead
  of scanning turn text (PII minimization + cheap indexed lookups).
- `GET /v1/conversations?tenant=<uuid>&contact=<phone|email>` filters on
  this marker (used by `GdprExportWorkflow`).
- The privacy erase consumer (`app/privacy.py`, direct aiokafka like the
  indexer) consumes `PrivacyEraseRequested` tombstones from
  `opendesk.privacy.events` and deletes all turns of matching conversations,
  then clears the marker; conversation shells are kept for referential
  integrity. Env: `PRIVACY_ENABLED` (default true), `PRIVACY_EVENTS_TOPIC`,
  `PRIVACY_EVENTS_GROUP`.

## Data retention (NDPA 2023 / GDPR storage limitation)

`app/retention.py` runs a background sweeper (startup + hourly) that
**hard-deletes turns older than `RETENTION_DAYS`** (default 365; the NDPA
profile in `infra/privacy/ndpa-profile.env` sets 180 — see
docs/compliance/ndpa.md):

- Per-tenant batches: tenants are enumerated from the conversations table
  and each delete runs inside a tenant-scoped transaction
  (`app.tenant_id` set), so FORCE ROW LEVEL SECURITY confines every batch to
  its tenant. Enumeration requires a role that bypasses RLS — with the
  default `opendesk` superuser DSN this works out of the box; under an
  RLS-enforced role (`app_conversation_login`) the sweep finds no tenants
  and no-ops, so run retention with the superuser DSN or a maintenance role.
- The age cutoff is computed with the **database clock** (`now()` in SQL),
  so app-side clock skew can never extend the retention window.
- Deletes are batched (`RETENTION_BATCH_SIZE` per statement) to bound lock
  time; one tenant's failure is logged and does not stop the others.
- Orthogonal to GDPR/NDPA erasure: the privacy erase consumer deletes a data
  subject's turns immediately; the sweeper only removes aged rows erasure
  did not cover. Conversation shells are kept (referential integrity); only
  turn content is deleted. Indexed copies in OpenSearch are NOT covered —
  see docs/compliance/ndpa.md for the indexer caveat.

## Advanced search (SPEC-W117)

`GET /v1/conversations/search` (`app/search.py`) is the OpenSearch-backed
advanced search over the turn-level `conversations` index (mapping +
TranscriptIndexer from the Indexer section above — no re-index, no mapping
change).

- **Auth**: identical to the list endpoint (`_require_tenant` — the
  X-Tenant-Slugs gateway binding). The resolved tenant is a **mandatory
  `term` filter, always the first filter in the bool query** — tenant
  isolation is enforced at the query level, never post-filtered
  client-side.
- **Params**: `q` (full-text, optional; `match` on `text` with
  `operator=and`, `fuzziness=AUTO`), `channel`, `site_slug`,
  `role` (user|agent|tool|system), `conversation_id` (uuid),
  `from_ts`/`to_ts` (ISO-8601; invalid => 400; `from>to` => 400),
  `limit` (1..100, default 25), `offset` (>=0, default 0,
  `limit+offset <= 10000` result window).
- **Pure builder**: `build_search_query()` assembles the whole body
  without touching a cluster — unit-tested standalone; endpoint tests use
  a fake async OpenSearch client and assert the exact query that reached
  the cluster (including the tenant term).
- **Fail closed**: when OpenSearch is not configured (or the cluster
  errors) the route answers **503 — no silent Postgres LIKE fallback**.
  A degraded duplicate of the index is worse than a loud gap.
- **Response**: `{hits: [{conversation_id, site_slug, channel, role,
  text, audio_url, ts}], total, limit, offset}` sorted by `ts` desc.
- `OPENSEARCH_ADDR` is read at startup; a boot-time client-construction
  failure is non-fatal (the rest of the service keeps working; only
  search 503s).

**Admin surface**: `apps/admin-web/app/app/[orgSlug]/conversations/` (SPEC
-W117 §5) gates on `canViewConversations` (owner/admin/staff), uses the
reusable `components/advanced-search-bar.tsx` + pure
`lib/advanced-search.ts` (chips, inclusive day bounds, paging clamps) and
calls the endpoint through the existing `/api/conversations/*` APISIX
wildcard. On 503 the screen shows a loud "search temporarily unavailable"
note and renders **no substitute list**. Follow-on data-heavy screens
(bookings/contacts/leads) adopt the same bar + lib once their Go backends
grow search endpoints (V-FUNDS).
