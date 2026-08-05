# Agora Architecture

Agora is an open-source, multi-tenant AI receptionist / front-desk SaaS.
It is a SOTA superset of the YouTube demo "Switchboard" (Next.js + Clerk + Convex + ElevenLabs), rebuilt with zero proprietary dependencies and an enterprise middleware backbone.

## Baseline → Agora mapping

| Baseline (Switchboard) | Agora (open source) | Why it's better |
|---|---|---|
| Clerk (auth/orgs/billing) | **Keycloak** (OIDC, groups=tenants, realm roles) + **Permify** (ReBAC fine-grained authz) | Self-hosted, standards-based OIDC; relationship-based permissions (owner/admin/staff/viewer per org) instead of opaque SaaS roles |
| Convex (reactive DB) | **Postgres** (RLS multi-tenancy) + **Redis** + **Kafka** events + WebSocket fan-out (`gateway-edge`) | Full SQL, row-level security, explicit event backbone instead of black-box reactivity |
| ElevenLabs agent | **LiveKit Agents** + **faster-whisper** (STT) + **Piper** (TTS) + **Ollama/vLLM** (LLM), optional ElevenLabs adapter | Fully self-hostable voice pipeline, swappable per stage |
| 6 client tools | Same 6 tool contracts, executed via **Dapr service invocation** → booking-service | Tenant-safe by construction (server resolves org from site slug), idempotent commands through Kafka |
| No durable flows | **Temporal** sagas (BookingSaga, Reminder, NoShowFollowup, TenantOnboarding) | Compensating transactions, retries, timers — bookings can't get lost |
| Clerk billing | **TigerBeetle** double-entry ledger + **Mojaloop** payout rails | Real accounting invariants, interop payment rails |
| No search/RAG | **OpenSearch** hybrid BM25 + k-NN knowledge RAG | Grounded answers from tenant KB |
| No analytics | **Lakehouse**: MinIO + Iceberg + Spark + Trino + dbt | Bronze/silver/gold marts: bookings, revenue, no-show rate, agent containment |
| No edge security | **APISIX** gateway + **OpenAppSec** WAF, rate-limit, OIDC/jwt | Single secured entry point |
| — | **Fluvio** streaming with WASM smart module (PII redaction) | Edge/telephony ingestion with in-stream compliance |

## Request path (public booking via voice agent)

```
Visitor (browser /p/{slug})
   │  HTTPS / WSS
   ▼
APISIX (9080) ── OpenAppSec WAF ── OIDC (browser) / jwt-auth (services) ── limit-count (Redis)
   │
   ├── /voice/chat ─────────► voice-agent-runtime (Python, LiveKit Agents)
   │                            │  Dapr invoke: identity (tenant ctx), knowledge (RAG)
   │                            │  6 tools ──Dapr invoke──► booking-service public endpoints
   │                            └── CloudEvents ──► Kafka opendesk.conversation.events
   ├── /api/bookings/* ──────► booking-service (Go)
   │                            │  Permify check (authz) → Postgres (RLS) → outbox
   │                            │  Temporal: BookingSagaWorkflow
   │                            │      ├─ ReserveSlot (booking)
   │                            │      ├─ HoldDeposit (payments) ──► TigerBeetle (hold)
   │                            │      ├─ ConfirmBooking
   │                            │      └─ SendConfirmation (notification) ──► Dapr smtp/twilio bindings
   │                            │  outbox dispatcher ──► Kafka opendesk.booking.events
   ├── /api/payments/* ──────► payments-service (Rust) ──► TigerBeetle / Mojaloop
   ├── /ws ──────────────────► gateway-edge (Rust) ◄── Kafka booking.events + Fluvio transcripts
   └── /* ───────────────────► admin-web (Next.js)

Async fan-out:
  conversation-service ──► Fluvio opendesk.transcripts-raw ──► [WASM pii-redact] ──► OpenSearch `conversations`
  analytics-pipeline ◄── Kafka (booking/payment/transcript events) ──► Iceberg bronze ──► Spark silver ──► dbt gold ──► Trino
```

## Middleware roles (the full requested stack)

| Middleware | Role in Agora |
|---|---|
| **Kafka** | Primary event backbone: commands, domain events, notifications outbox, DLQ |
| **Dapr** | Sidecar per service: pub/sub abstraction, service invocation, state, output bindings (smtp/twilio), secrets |
| **Fluvio** | High-throughput transcript ingestion + WASM smart module for in-stream PII redaction; live tail to dashboards |
| **Temporal** | Durable orchestration: booking saga with compensation, reminders, no-show follow-up, tenant onboarding |
| **Postgres** | System of record per service; RLS tenant isolation; Temporal/Keycloak/Permify/Permify backends |
| **Keycloak** | OIDC authn, orgs-as-groups, realm roles, service accounts |
| **Permify** | ReBAC authz: org/offering/booking/site permissions checked on every mutation |
| **Redis** | Rate limiting (APISIX), Dapr state store, sessions, presence |
| **Mojaloop** | FSPIOP quoting/transfer adapter for tenant payouts (interop rails) |
| **OpenSearch** | Hybrid search + k-NN RAG (knowledge), conversation search, analytics indices |
| **OpenAppSec** | WAF at the gateway (web attack protection + API discovery), detect→prevent |
| **APISIX** | API gateway: routing, OIDC, JWT, rate limit, prometheus, OTel |
| **TigerBeetle** | Double-entry ledger: deposits, captures, refunds, no-show fees, payouts |
| **Lakehouse** | MinIO + Iceberg REST catalog + Spark + Trino + dbt (bronze/silver/gold) |

## Multi-tenancy model
- Every business table carries `tenant_id`; Postgres RLS enforces isolation (`app.tenant_id` GUC).
- Keycloak group `/tenants/{slug}` ↔ tenant; `tenant_slugs` claim in access token.
- Permify relationships scope permissions per organization resource.
- Agent sessions never receive org IDs from the model — tools resolve tenant from the public site slug server-side.

## Reliability patterns
- **Outbox** (booking, payments): atomic DB write + async Kafka publish.
- **Saga** (Temporal): compensation order ReleaseSlot → VoidHold on any failure.
- **Idempotency**: `idempotency_key` unique on bookings; transfer IDs in the ledger; consumer-side dedupe.
- **DLQ**: `opendesk.dlq` for poison messages across all consumers.

## ADRs
- docs/ADRs/0007 — ledger client fallback & gateway/WAF simplifications.
