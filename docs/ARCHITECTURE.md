# Architecture — Agora (Open-Source AI Receptionist Platform)

Fully open-source, multi-tenant AI receptionist / front-desk SaaS. Every appointment-based
business gets a branded public booking page with a voice+text AI concierge that answers
questions and books/reschedules/cancels appointments live — plus a tenant dashboard for staff.
Superset of the YouTube baseline (Next.js + Clerk + Convex + ElevenLabs) with zero proprietary
dependencies and an enterprise middleware backbone.

---

## 1. System diagram

```
                         ┌─────────────────────────────────────────────────────────┐
                         │                      EDGE                               │
                         │  open-appsec (WAF) → APISIX gateway :9080               │
                         │  (jwt-auth, rate limit, routing)                        │
                         └───────┬───────────────────────┬─────────────────────────┘
                                 │ /api/* /p/*           │ /ws /voice/*
            ┌────────────────────┼───────────────────────┼─────────────────────┐
            ▼                    ▼                       ▼                     ▼
   apps/admin-web        booking-service         ws-gateway (Rust)     voice-agent-runtime
   Next.js :3001         Go :7002                :7005                 Python :7006
   /p/{slug} public      REST /v1                Redis pub/sub         LiveKit agents
   /app/{slug} dash      Temporal client         JWT verify            whisper/piper/ollama
            │                    │                       │                     │
            │                    ▼                       │                     ▼
            │            identity-service                │            conversation-service
            │            Go :7001                        │            Python :7007
            │            tenants/packs                   │            turns / OpenSearch
            │                    │                       │                     │
            ▼                    ▼                       ▼                     ▼
   ┌────────────────────────────────────────────────────────────────────────────────┐
   │                          SERVICE / DATA MIDDLEWARE                              │
   │  Dapr sidecars (pub/sub, invoke, state, bindings, workflow)                     │
   │  Kafka (+ Fluvio edge) · Temporal · Postgres(+RLS) · Redis · OpenSearch         │
   │  Keycloak (OIDC) · Permify (ReBAC) · Mojaloop · TigerBeetle                     │
   └────────────────────────────────────────────────────────────────────────────────┘
            │                    │                       │                     │
            ▼                    ▼                       ▼                     ▼
   notification-worker   payments-service      knowledge-service      analytics-pipeline
   Go/Temporal :7003     Rust :7004            Python :7008           Python :7009
   email/sms/whatsapp    ledger + rails        RAG search/embed       dbt/Trino consume
            │                    │
            ▼                    ▼
   ┌─────────────────── LAKEHOUSE ───────────────────┐    ┌──────────┐    ┌─────────────┐
   │ MinIO (S3) · Iceberg REST · Spark · Trino · dbt  │    │ Fluvio   │    │ Twenty CRM  │
   │ bronze → silver → gold marts                     │    │ PII SM   │    │ :3100       │
   └──────────────────────────────────────────────────┘    └──────────┘    └─────────────┘
```

All synchronous service-to-service calls go through **Dapr service invocation**; all
async integration goes through **Kafka** via the Dapr pub/sub component `pubsub-kafka`
(and booking/payments also write Kafka directly with an outbox). Every mutating saga is a
**Temporal** workflow with compensation.

---

## 2. Service inventory

| Service | Lang | Port | Dapr app-id | Responsibility |
|---|---|---|---|---|
| identity-service | Go | 7001 | `identity` | Tenants, site config, industry packs, consent erasure events |
| booking-service | Go | 7002 | `booking` | Catalog, availability engine, bookings REST, outbox→Kafka, saga start, ledger balance proxy, industry packs registry |
| notification-worker | Go | 7003 | `notification` | Temporal worker: BookingSaga, GdprExport/Erase; email/SMS/WhatsApp sends; DLQ consumer |
| payments-service | Rust | 7004 | `payments` | Invoice/charge/refund, Paystack+Mojaloop rails, TigerBeetle ledger, outbox, payment DLQ consumer |
| ws-gateway | Rust | 7005 | — | WebSocket fan-out (booking events) with JWT + tenant isolation |
| voice-agent-runtime | Python | 7006 | `voice-runtime` | `/voice/chat` text turns, `/voice/session` LiveKit token, 6 agent tools via Dapr invoke, eval harness |
| conversation-service | Python | 7007 | `conversation` | Turn persistence, intel (sentiment/NER), GDPR erase consumer, OpenSearch indexer |
| knowledge-service | Python | 7008 | `knowledge` | RAG: docs CRUD + vector search + text-to-SQL analytics answers |
| analytics-pipeline | Python | 7009 | `analytics` | Kafka→Iceberg bronze consumers, silver/gold Spark jobs, ISM, rollup API, lag metrics |
| crm-sync | Go | 7010 | — | Twenty CRM bidirectional sync with echo suppression, reverse webhook worker, GDPR erase consumer |

(Internal ports are not host-mapped except 7001/7002/7009 for dev convenience; all external
traffic enters through APISIX :9080.)

### Middleware (all containerized under `infra/`)

| Component | Role | Notes |
|---|---|---|
| APISIX + etcd | API gateway | standalone config `infra/apisix/apisix.yaml`, jwt-auth + prometheus plugins |
| open-appsec | WAF | compose `appsec` profile, layered over gateway |
| Keycloak | AuthN | realm `opendesk` import; public client for web, confidential for services |
| Permify | AuthZ (ReBAC) | gRPC :3476 / REST :3478; org/team/site schema |
| Kafka (KRaft) | Event backbone | single broker dev; topics in SPEC §4 |
| Fluvio | Edge streaming | PII-redaction smart module (Rust/WASM) |
| Temporal | Durable workflows | namespace `opendesk`; saga orchestration |
| Postgres 16 | System of record | 4 DBs: keycloak/permify/booking(+knowledge)/conversation; RLS everywhere |
| Redis 7 | Cache/pub-sub | ws fan-out + hot availability cache |
| Mojaloop | Payment rails | account-lookup + quoting + ml-api + central-ledger (sim) |
| OpenSearch | Search | knowledge vectors + conversations index + ISM rollover |
| TigerBeetle | Financial ledger | double-entry; sim in-memory fallback in payments-service |
| LiveKit | WebRTC SFU | voice calls; worker = voice-agent-runtime |
| Ollama | Local LLM | compose `voice` profile; vLLM-compatible env override |
| Lakehouse | Analytics | MinIO + Iceberg REST + Spark + Trino + dbt |
| Twenty CRM | CRM | upstream `twentycrm/twenty` image; crm-sync bridges |

---

## 3. Request flows

### 3.1 Public booking page (text chat)

1. Visitor opens `/p/{slug}` (Next.js page, fetches `GET /v1/public/sites/{slug}` via gateway).
2. Chat widget → `POST /voice/chat {session_id, site_slug, text}` (APISIX route `voice-runtime`).
3. voice-agent-runtime resolves tenant **server-side** from the site slug via Dapr invoke to
   identity-service, loads the industry pack (persona, terminology, booking policy, knowledge
   scope), persists the user turn via conversation-service, and calls the LLM with 6 function
   tools.
4. Tool calls execute as Dapr invocations (`booking` / `identity` app-ids):
   `get_business_info`, `get_availability`, `book_appointment`, `lookup_appointment`,
   `reschedule_appointment`, `cancel_appointment`.
5. `book_appointment` → `POST /v1/bookings` on booking-service:
   - validates the **phone-confirmation policy** (browser sessions carry no caller ID),
   - writes the booking row in Postgres inside an RLS-scoped transaction (`SET LOCAL app.tenant_id`),
   - inserts a matching **outbox row** in the same transaction,
   - starts the `BookingSaga` Temporal workflow,
   - an outbox dispatcher publishes `BookingCreated` CloudEvents to Kafka (≥1 delivery).
6. ws-gateway consumes `opendesk.booking.events` and fans out to `/ws` subscribers for the
   tenant's dashboard (JWT-verified, tenant-claim bound).

### 3.2 Voice call (LiveKit)

1. Widget posts `POST /voice/session {site_slug}` → returns a LiveKit access token (HS256,
   dev key/secret from env) + room `site-{slug}`.
2. Browser joins LiveKit over WebRTC; `voice-worker` (LiveKit Agents) accepts the job:
   VAD → whisper STT → LLM (Ollama/vLLM; ElevenLabs adapter optional) → piper TTS.
3. Same 6 tools as text mode. Barge-in handled by the agents framework; browser text chat
   falls back to `/voice/chat` when LiveKit is unreachable.

### 3.3 Booking saga (Temporal)

`BookingSagaWorkflow(booking_id, tenant_id)` in notification-worker:

1. `FetchBookingContext` — booking, offering, member, tenant info (Dapr invoke).
2. `SendBookingConfirmation` — email + SMS + WhatsApp via SMTP2GO/Twilio (or log stubs).
3. `ScheduleReminder` — a Temporal timer at `starts_at - 24h` (dev: 2min) → `SendReminder`.
4. **Compensation**: if the booking is cancelled before the timer fires, the saga cancels the
   reminder and sends a cancellation notice. Workflow IDs are deterministic
   (`booking-saga-{booking_id}`) so replays are idempotent.

### 3.4 Payments & ledger

`POST /v1/invoices` → payments-service writes invoice + outbox (same tx) →
`POST /v1/invoices/{id}/charge` → rail adapter (`paystack` default, `mojaloop` optional)
→ on success a **double-entry transfer** posts to the ledger:

- `tigerbeetle` backend when `TB_ADDRESSES` set (accounts: tenant receivable ↔ platform clearing);
- otherwise an in-memory `sim` backend (dev default, clearly logged).

Webhook from the rail → signature verify → mark paid → `PaymentSucceeded` event via outbox.
`GET /v1/ledger/balance?tenant_id=` is exposed for dashboards (also proxied by booking-service).

### 3.5 Knowledge / RAG

`knowledge-service` owns docs per tenant (`POST /v1/knowledge/docs`). Embeddings: OpenSearch
kNN when available; deterministic hash-embeddings in dev. `POST /v1/knowledge/search` does
vector search + keyword fallback. The agent's `get_business_info` consults it via Dapr invoke.

### 3.6 Analytics lakehouse

`analytics-pipeline` consumes `opendesk.booking.events` + `opendesk.payment.events` →
writes **bronze** JSON/parquet to MinIO (`s3://lake/bronze/...`) → Spark job
`silver_clean_bookings.py` dedups/normalizes into **silver** Iceberg tables → `dbt run` builds
**gold** marts (`gold_daily_bookings`, `gold_channel_revenue`, `gold_containment`).
Trino exposes SQL over Iceberg; dashboards call `GET /v1/analytics/summary` (and
`POST /v1/analytics/query` for guarded text-to-SQL answers).

### 3.7 CRM sync (Twenty)

`crm-sync` consumes booking/contact events → upserts People/Appointments in Twenty (REST
:3000). Reverse direction: Twenty webhooks → `POST /webhooks/twenty` → emits
`com.opendesk.crm.ContactUpdated` to `opendesk.crm.events`; echo suppression via a sync_map
hash table so forward-applied changes don't boomerang.

### 3.8 Industry packs

YAML packs (`industries/*.yaml`) per vertical: terminology ("appointment"→"session",
"staff"→"coach"), booking policy (min notice, buffer, cancellation window), agent persona
prompt, starter knowledge docs, Temporal workflow variant hints. Resolved per tenant at
request time; provision/seed applies pack defaults; `POST /internal/tenants/{slug}/seed-pack`
seeds offerings + knowledge from the pack. Registry: booking-service
`GET /internal/packs` (admin-registered, persisted in `industry_packs` table, hot-reloadable).

---

## 4. Multi-tenancy & isolation

- **Tenant id (UUID)** on every row; **RLS policies** on every tenant table (Postgres
  `FORCE ROW LEVEL SECURITY`, `app.tenant_id` GUC set per-transaction).
- **Slug** is the external handle: `/p/{slug}`, `/app/{slug}`, `X-Tenant-Slug` header on
  internal calls. Slug→tenant resolution happens server-side only; agent tools never accept
  a tenant id from the model.
- **JWT** (Keycloak realm `opendesk`): dashboard users belong to org groups
  `/tenants/{slug}`; gateway enforces `jwt-auth`; services re-verify the tenant slug against
  the path/header.
- **Permify ReBAC**: organization → team → site relations; permission checks
  (`can_manage_bookings`, `can_view_dashboard`) via Dapr invoke to identity-service or direct
  Permify gRPC from booking-service middleware.
- **Cross-tenant reads are impossible by construction**: RLS + per-request tenant context +
  no superuser role in app DB users.

## 5. AuthN/Z summary

| Layer | Mechanism |
|---|---|
| Browser → gateway | Keycloak OIDC authorization-code (public client `admin-web`) |
| Gateway | APISIX `jwt-auth` (consumers `admin`, `svc-*`), `limit-count`, route ACLs |
| Service-to-service | Dapr invocation with mTLS; `X-Tenant-Slug` carried as input, never trusted from callers for admin ops |
| Authorization | Permify relationship checks; cached 60s in booking middleware |
| Public endpoints | `/v1/public/*`, `/voice/*` — tenant resolved from slug; rate-limited at edge |

## 6. Event model (CloudEvents 1.0)

Envelope: `{specversion, id, source, type, subject, time, tenantid (extension), data}`.

| Topic | Type(s) | Producer | Consumers |
|---|---|---|---|
| `opendesk.booking.events` | BookingCreated/Updated/Cancelled | booking-service (outbox) | ws-gateway, analytics, crm-sync |
| `opendesk.payment.events` | PaymentSucceeded/Failed/Refunded | payments-service (outbox) | analytics, notification (receipt) |
| `opendesk.crm.events` | ContactUpserted/AppointmentUpserted | crm-sync | Twenty webhook worker |
| `opendesk.notifications.outbox` | NotificationCommand/Result | notification-worker | send adapters |
| `opendesk.dlq` | any dead-lettered | all consumers | notification DLQ logger, alerts |
| `opendesk.usage.events` | UsageRecord (metering) | billing-enabled services | billing-engine (W17) |
| `opendesk.privacy.events` | PrivacyEraseRequested | booking `/v1/privacy/erase` | booking, conversation, crm-sync, knowledge |
| `opendesk.transcripts.raw` | TranscriptChunk (PII pre-redaction) | conversation-service | Fluvio |
| `opendesk.transcripts.clean` | TranscriptChunk (post-redaction) | Fluvio smart module | OpenSearch indexer |

**Delivery semantics:** outbox → dispatcher polls `unsent` rows → Kafka (idempotent producer,
keyed by tenant) → consumers with **idempotency keys** (`event_id` dedup tables in
conversation/crm-sync). DLQ after N redeliveries with exponential backoff.

## 7. Data & storage

| Store | Contents | Retention |
|---|---|---|
| Postgres `booking` | tenants, sites, offerings, team, availability, bookings, outbox, sync_map | forever; daily `pg_dump` (`infra/backups/backup.sh`) |
| Postgres `conversation` | conversations, turns, intel, idempotency | turns TTL 90d via job; erasure on privacy events |
| Postgres `keycloak`/`permify` | auth stores | provider-managed |
| Redis | ws pub/sub channels `tenant:{id}:events`, availability cache | volatile |
| OpenSearch | `knowledge` vectors, `conversations` transcripts | ISM rollover 30d/50GB → delete 180d |
| TigerBeetle | accounts + transfers (ledger) | immutable by design |
| MinIO `lake` | bronze/silver/gold Iceberg | bronze 1y, gold forever |
| MinIO `exports` | GDPR export bundles | manual purge after delivery |

## 8. Resilience & delivery guarantees

- **Idempotent commands:** booking create dedup key `(tenant, contact, offering, starts_at)`;
  payment charge idempotency-key header; saga workflow IDs deterministic.
- **Outbox pattern** everywhere Kafka is produced → no lost events on crash between DB commit
  and publish.
- **Retries with jittered backoff** in outbox dispatcher, send adapters, rail webhooks;
  poison messages → `opendesk.dlq` with the original payload + error.
- **Graceful degradation matrix** (documented in runbook): Permify down → 503 on mutations
  (never fail-open); LiveKit down → text chat fallback; OpenSearch down → keyword search;
  lakehouse down → dashboard reads stale rollups, writes unaffected.
- **Availability engine** computes slots from `availability_rules` − `bookings` in one SQL
  query per (member, day); correctness covered by unit tests with DST edge cases.

## 9. Security

- Edge: open-appsec WAF (modsec rules), APISIX rate limits per consumer/IP.
- Secrets: env-only in dev (`.env.example`); SOPS/Vault pattern documented for prod
  (`docs/runbooks/secrets.md`).
- PII: Fluvio smart module redacts phone/email from transcript streams before indexing;
  voice runtime never logs message bodies at INFO.
- GDPR: `POST /v1/privacy/export|erase` (booking-service) → Temporal workflows gather/
  tombstone across Postgres, OpenSearch, CRM, MinIO (see §3 flows and
  `docs/runbooks/security.md`).

## 10. Repository layout

```
opendesk/
  docker-compose.yml          # root: core services + includes infra fragments
  Makefile                    # up/down/seed/smoke/topics/trino + industry seed
  .env.example                # every tunable documented
  apps/admin-web/             # Next.js 15: /p/{slug} public page, /app/{slug} dashboard
  services/                   # 10 microservices (Go×3, Rust×2, Python×4, Go crm-sync)
  infra/
    docker-compose.core.yml   # postgres, redis, kafka, temporal, keycloak, permify, ...
    docker-compose.edge.yml   # apisix, etcd, open-appsec
    docker-compose.lakehouse.yml
    apisix/  keycloak/  permify/  postgres/  kafka/  fluvio/  temporal/
    tigerbeetle/  mojaloop/  opensearch/  lakehouse/  livekit/  backups/
  industries/                 # YAML packs (salon, clinic, consultancy, support-desk)
  docs/                       # this file, ADRs, OpenAPI, runbooks, runbook per wave
  scripts/                    # seed-demo.sh, seed-industries.sh, smoke-test.sh
  tests/e2e/                  # pytest end-to-end suite (compose fixtures)
  SPEC.md                     # (repo root parent) the platform contract — sacred
```

## 11. Deploy shapes

- **Dev:** root `docker-compose.yml` (single host, ~30 containers).
- **Prod (reference):** `docs/ADRs/0008-production-topology.md` — Kafka rf=3, Patroni PG,
  3-node TigerBeetle, Temporal multi-node, APISIX HA behind LB, lakehouse on separate nodes.
- **Edge appliance (MVP):** `deploy/k3s/` kustomize base — single-node k3s with the core
  subset + MirrorMaker2 store-forward to cloud.
