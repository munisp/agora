# Twenty CRM Integration

Agora syncs tenants, contacts and bookings **bidirectionally** with a
self-hosted [Twenty](https://twenty.com) CRM: forward (Agora → Twenty)
via Kafka event consumers, and reverse (Twenty → Agora) via HMAC-verified
webhooks and a dedicated reverse worker. This guide covers the architecture,
setup, event mapping, loop prevention, rate limits and day-2 operations.

## Architecture

```
                        ┌───────────────────────── OpenDesk compose ──────────────────────────┐
                        │                                                                      │
 identity-service ──┐   │                                                                      │
 booking-service  ──┼──►│  Kafka topics                                                        │
 conversation-svc ──┘   │   opendesk.identity.events ─┐                                        │
                        │   opendesk.booking.events ──┼──► crm-sync (:7010, daprd-crm-sync)    │
                        │   opendesk.conversation.events ┘   │  consumer group "crm-sync"       │
                        │                                    │  token bucket 90 req/min        │
                        │   opendesk.dlq ◄── poison messages─┘  (3 attempts, then DLQ)         │
                        │                                                                      │
                        │   FORWARD:  crm-sync ── REST (Bearer API key) ──► twenty-api (:3000) │
                        │        │                                  │     ▲                    │
                        │        │ sync_map (crm_sync DB)           │     │ Bull-MQ jobs       │
                        │        ▼                                  ▼     │                    │
                        │     postgres                        twenty-worker ──► twenty-redis   │
                        │   (DBs: twenty, crm_sync, booking)                                     │
                        │                                                                      │
                        │   REVERSE: Twenty webhook ──► POST :7010/webhooks/twenty (HMAC)      │
                        │        └─► Kafka opendesk.crm.events ──► reverse worker              │
                        │              (consumer group "crm-sync-reverse", DLQ after 3)        │
                        │        └─► Dapr invoke booking-service:                              │
                        │              POST /internal/contacts/upsert      (person → contact)  │
                        │              POST /internal/bookings/{id}/crm-note (task DONE)       │
                        └──────────────────────────────────────────────────────────────────────┘

 Browser:  CRM UI  http://localhost:3100        (linked from admin-web sidebar)
 Gateway:  APISIX  /crm/*  ──proxy-rewrite──►  twenty-api:3000   (openid-connect bearer_only)
```

### Compose services (root `docker-compose.yml`)

| Service | Image / build | Ports | Purpose |
|---|---|---|---|
| `twenty-api` | `twentycrm/twenty:v1.3.2` | `3100:3000` | Twenty server (REST + frontend) |
| `twenty-worker` | `twentycrm/twenty:v1.3.2` (`yarn worker:prod`) | — | Background job worker (Bull-MQ) |
| `twenty-redis` | `redis:7-alpine` | internal | Queue + cache for Twenty |
| `crm-sync` | `services/crm-sync-service` (Go 1.23) | `7010` | Agora ⇄ Twenty bridge |
| `daprd-crm-sync` | daprd sidecar (`DAPR_APP_ID=crm-sync`) | internal | Pub/sub + service invocation |

Twenty stores its data in the shared `postgres` container (database
`twenty`, created by `infra/postgres/init-scripts/00-create-dbs.sql`); crm-sync
keeps its own `crm_sync` database for the `sync_map` table. Twenty env:
`PG_DATABASE_URL=postgres://opendesk:opendesk@postgres:5432/twenty`,
`REDIS_URL=redis://twenty-redis:6379`, `SERVER_URL`/`FRONT_BASE_URL=
http://localhost:3100`, `MESSAGE_QUEUE_TYPE=bull-mq`, `STORAGE_TYPE=local`,
plus token secrets with `-dev` fallbacks
(`ACCESS_TOKEN_SECRET`, `LOGIN_TOKEN_SECRET`, `REFRESH_TOKEN_SECRET`,
`FILE_TOKEN_SECRET`, `APP_SECRET`).

## Creating a Twenty API key

1. Open **http://localhost:3100** and complete the workspace setup (first user
   becomes the workspace admin).
2. Go to **Settings → API & Webhooks** (⚙ → Developers in some versions).
3. **Create API key** — give it a name (`opendesk-crm-sync`) and copy the
   token; it is shown once.
4. Set it on the sync service in the root compose file (or `.env`):

   ```yaml
   crm-sync:
     environment:
       TWENTY_API_KEY: eyJhbGciOi...   # replace the placeholder dev value
   ```

5. `docker compose up -d crm-sync` to pick up the new key.

All Twenty calls use `Authorization: Bearer <key>` against
`TWENTY_API_URL` (`http://twenty-api:3000` in-compose).

## Sync mapping (Agora → Twenty)

| Kafka topic | CloudEvent type | Twenty action | sync_map rows |
|---|---|---|---|
| `opendesk.identity.events` | `TenantProvisioned` | Upsert **Company** `{name, domainName: "<slug>.opendesk.local"}` | `kind=tenant` |
| `opendesk.booking.events` | `BookingCreated` / `BookingConfirmed` | `findPerson` by contact email/phone (`GET /rest/people?filter=emails.primaryEmail[eq]...`), then POST or PATCH **Person**; create **Task** `"{offering} appointment at {starts_at}"` linked to the person | `kind=contact`, `kind=booking`, `kind=booking_task`, `kind=booking_contact` |
| `opendesk.booking.events` | `BookingCancelled` | PATCH the Task — status done + cancellation note | `kind=booking` (updated) |
| `opendesk.booking.events` | `BookingRescheduled` | PATCH the Task `dueDate` | `kind=booking` (updated) |
| `opendesk.conversation.events` | `ToolInvoked(book_appointment)` | Optional **Note** on the person: “Booked via AI receptionist” | — |
| `opendesk.conversation.events` | `SessionEnded` (with `quality`) | Optional **Note** on the person: “📞 AI call summary — …” (see *Call quality signals*) | reads `kind=contact_phone` (written at booking sync) |

Every forward-sync `sync_map` write also stamps `last_synced_at` — the input
for reverse echo suppression (below). `kind=booking_task` (booking id → task
id) exists so the reverse worker can resolve a `task.updated` webhook back to
its Agora booking.

Upserts are keyed by `sync_map`
`UNIQUE(kind, opendesk_id, tenant_id)`, so re-delivered events are idempotent.
Events for unknown tenants/companies are retried (transient ordering) and
dead-lettered after 3 attempts — see Troubleshooting.

## Call quality signals

When an AI voice session ends, voice-agent-runtime enriches the
`com.opendesk.conversation.SessionEnded` CloudEvent with a `quality` object
built from a per-session accumulator (`app/metrics.py` `SessionMetrics`,
fed by the same call sites as the Prometheus registry: STT/TTS stages, the
LLM tool loop + fallback chain, and the tool layer). crm-sync turns it into
a **Note** on the caller's Twenty person.

### Event shape

```jsonc
{
  "type": "com.opendesk.conversation.SessionEnded",
  "source": "voice-agent-runtime",
  "tenantid": "<tenant-uuid>",
  "subject": "<tenant-slug>",
  "data": {
    "conversationId": "…",
    "channel": "voice",
    "siteSlug": "acme-salon",
    "quality": {                     // ABSENT when the session recorded no signals
      "duration_s": 95.2,
      "turn_count": 6,
      "tool_calls": {"book_appointment": 1, "lookup_appointment": 2},
      "avg_llm_latency_ms": 820,     // null when no LLM call was timed
      "max_llm_latency_ms": 1400,    // null when no LLM call was timed
      "stt_calls": 6,
      "tts_calls": 5,
      "llm_fallback_used": false,
      "escalated": true,
      "confirmed_phone": "+1555000111"  // null when the caller never confirmed a number
    }
  }
}
```

### Resulting Twenty note

Title `📞 AI call summary`, body (optional segments omitted when the payload
lacks them):

```
📞 AI call summary — duration 95s, 6 turns, tools: book_appointment×1,
lookup_appointment×2, avg LLM 820ms (max 1400ms), stt 6 calls, tts 5 calls,
escalated: yes, fallback used: no
```

The note is created via `POST /rest/notes` and linked to the person via
`POST /rest/noteTargets` — the same pattern as the AI-booking note.

### Person resolution & skip rules

1. `FindPerson` by `confirmed_phone` (Twenty `phones.primaryPhoneNumber[eq]`).
2. Fallback: `sync_map kind=contact_phone` (phone → person id), written at
   booking sync whenever a booking carries a phone — covers phone-format
   mismatches between the confirmed number and the Twenty record.
3. **Skip + ack** (logged, no retry, no DLQ) when: the event has no
   `quality` object (session recorded no signals); `confirmed_phone` is null
   (caller never confirmed — no person can be resolved and an orphaned note
   would pollute the CRM); or the phone resolves to no person (contact never
   synced — the note is best-effort).

### Limitations

* **Sentiment is not included.** The voice runtime has no per-turn
  sentiment; that signal is computed by conversation-service (`app/intel.py`)
  and lives in the **OpenSearch `conversations` index** (per-turn
  sentiment/intent/entities columns), not in the SessionEnded event. The
  consumer accepts an optional `avg_sentiment` field for future enrichment
  and omits the segment when it is absent (voice-agent-runtime never sends
  it today).
* **LLM latency is null on the pure voice path.** The LiveKit worker's LLM
  node (livekit-plugins-openai) is not timed by the in-process metrics, so
  `avg_llm_latency_ms`/`max_llm_latency_ms` are `null` for voice sessions
  today; the accumulator fields are populated by sessions whose LLM calls go
  through the instrumented tool loop / fallback chain. `llm_fallback_used`
  likewise reflects the circuit-broken fallback chain, not the worker's
  job-level retry.
* `turn_count` counts committed user utterances
  (`user_speech_committed`); if the LiveKit event surface changes, turns
  degrade to 0 rather than breaking session teardown.

## Reverse sync (Twenty → Agora)

The reverse direction is fully wired: Twenty webhook → HMAC intake →
`opendesk.crm.events` → reverse worker (Kafka consumer group
`crm-sync-reverse`, DLQ after 3 attempts) → Dapr service invocation into
booking-service internal endpoints (no Permify — internal, Dapr-invoked
only; tenant resolution via the usual `X-Tenant-Slug` middleware).

### Setup: register the webhook

```bash
export TWENTY_API_KEY=eyJhbGciOi...     # Twenty Settings → API & Webhooks
export TWENTY_WEBHOOK_SECRET=...        # must equal crm-sync's TWENTY_WEBHOOK_SECRET
./infra/twenty/setup-webhooks.sh        # idempotent; skips when already registered
```

The script creates one webhook (`targetUrl
http://crm-sync:7010/webhooks/twenty`, operations `person.created`,
`person.updated`, `task.updated`) via `POST /rest/webhooks`, after a
`GET /rest/webhooks` presence check. Field names follow Twenty's v1 REST
webhook object schema and are **version-sensitive** — see the script
comments and [infra/twenty/README.md](../../infra/twenty/README.md).

crm-sync verifies the `X-Twenty-Webhook-Signature` HMAC header against
`TWENTY_WEBHOOK_SECRET` and emits the payload as a CloudEvent
(`com.opendesk.crm.twenty.<event>`) to `opendesk.crm.events`. Requests with
a missing/invalid signature get `401` — check `docker compose logs crm-sync`
for `invalid webhook signature` entries.

### Reverse mapping

| Webhook event | Reverse worker action | booking-service endpoint |
|---|---|---|
| `person.created` / `person.updated` | Fetch full Person (`GET /rest/people/{id}`), resolve tenant slug, upsert contact keyed by phone OR email with `source='twenty'`, `external_id=<personId>` | `POST /internal/contacts/upsert` (+ `GET /internal/contacts?phone=\|email=` lookup helper) |
| `task.updated` (status `DONE`) | Resolve booking via `sync_map kind=booking_task` (reverse lookup by Twenty task id), append a CRM note to the booking's `crm_notes` JSONB array | `POST /internal/bookings/{id}/crm-note` |

Tenant slug resolution order for persons: (1) `sync_map` contact mapping
(person → contact → tenant → company domain `<slug>.opendesk.local` via
`GET /rest/companies/{id}`); (2) fallback via the person's company when the
company is a mapped tenant (`sync_map kind=tenant`) — domain → slug. Events
whose tenant cannot be resolved are **skipped + acked** (foreign-workspace
records are not poison).

booking-service schema additions are bootstrapped idempotently at startup
(`ALTER TABLE ... IF NOT EXISTS`): nullable `contacts.source` /
`contacts.external_id` (+ index) and `bookings.crm_notes JSONB DEFAULT '[]'`.

### Loop prevention

* **No outbound events from the reverse write path.** Contacts have no
  outbox in booking-service (only booking lifecycle mutations do), and the
  crm-note append deliberately bypasses the outbox — so a reverse-synced
  change can never re-enter the forward event flow.
* **Echo suppression.** Every forward person write stamps
  `sync_map.last_synced_at`. An inbound `person.created`/`person.updated`
  webhook for a person whose mapping was stamped within the last **10s**
  (`REVERSE_ECHO_WINDOW_SECONDS`) is our own write echoing back and is
  skipped + acked (metric `reverse_echo_suppressed`).
* **`task.updated` gating.** Only tasks with a `sync_map kind=booking_task`
  mapping (i.e. tasks Agora created) produce crm-notes; tasks created
  inside Twenty are ignored.

**Remaining race (accepted, documented):** a person edited by a human in
Twenty within the same 10s window as a forward-sync write is suppressed
together with the echo and only converges on the next Twenty-side edit
(forward last-write-wins within the window). One known echo the window does
NOT cover: the forward sync closes cancelled bookings as task `DONE`, which
fires `task.updated` — the reverse worker then appends a "marked DONE"
crm-note to an already-cancelled booking. The note is cosmetic (the booking
status is untouched); deduplicating it would require tracking forward task
patches in `sync_map`, which we deliberately did not add.

### Reverse worker configuration

| Env | Default | Purpose |
|---|---|---|
| `REVERSE_CONSUMER_GROUP` | `crm-sync-reverse` | Kafka group for `opendesk.crm.events` |
| `BOOKING_APP_ID` | `booking` | Dapr app-id of booking-service |
| `REVERSE_ECHO_WINDOW_SECONDS` | `10` | Echo suppression window |

## Rate limits & batching

Twenty Cloud defaults are generous, but the self-hosted API is guarded by the
sync client regardless:

* **Token bucket, 90 requests/min** (`TWENTY_RATE_PER_MIN=90`) — all outbound
  REST calls pass through the limiter.
* **Batch endpoints: max 60 objects per call** — when bulk-creating
  people/tasks, split payloads into batches of ≤ 60.
* **Retries:** exponential backoff with jitter on `429` and `5xx`; other 4xx
  responses are treated as poison and go to the DLQ after the attempt budget.

Metrics are on `:7010/metrics`: per-event-type counters
(`crm_sync_events_total{type=...}`) and Twenty call latency
(`crm_sync_twenty_call_duration_seconds`).

## sync_map & DLQ operations

```bash
# What is mapped where:
docker exec postgres psql -U opendesk -d crm_sync -c \
  "SELECT kind, opendesk_id, twenty_id, tenant_id, updated_at FROM sync_map
   ORDER BY updated_at DESC LIMIT 20;"

# Unmapped tenants (should be empty in steady state):
docker exec postgres psql -U opendesk -d identity -c "SELECT id, slug FROM tenants;" \
  # vs sync_map rows with kind='tenant'
```

Full DLQ replay procedure for crm-sync (including the rekey procedure and
Twenty upgrades) lives in [docs/runbooks/operations.md §7](../runbooks/operations.md).

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `401` from Twenty in crm-sync logs | Placeholder/expired `TWENTY_API_KEY` | Create a key (above), set env, restart `crm-sync` |
| Companies created, people/tasks not | `BookingCreated` arrived before `TenantProvisioned` was processed | Transient — consumer retries; check `opendesk.dlq` only if it persists |
| Events in `opendesk.dlq` with `404` from Twenty | Object deleted in Twenty, or bad payload | Inspect DLQ message, fix root cause, replay to original topic (runbook §7) |
| `429` bursts, rising call latency | Rate limiter too tight or a retry loop | Verify `TWENTY_RATE_PER_MIN`; check `/metrics` latency histogram |
| Webhook `401 invalid signature` | `TWENTY_WEBHOOK_SECRET` ≠ secret configured in Twenty | Re-copy the webhook secret; both sides must match byte-for-byte |
| Twenty UI up but API unreachable in-compose | `twenty-worker` down (migrations run there) | `docker compose logs twenty-worker`; worker runs DB migrations on boot |
| `domainName` conflicts on Company upsert | Two tenants sharing a slug after a rekey | Re-map via sync_map (runbook §7 rekey procedure) |
| Nothing syncing at all | crm-sync or sidecar down / Kafka lag | `docker compose ps crm-sync daprd-crm-sync`; `kafka-consumer-groups.sh --describe --group crm-sync` |
