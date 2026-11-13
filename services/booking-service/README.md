# booking-service

The heart of OpenDesk: catalog, team availability, the slot engine, bookings
with transactional outbox, the booking saga activities and the voice-agent
command consumer (SPEC §4/§6/§7). Go 1.23, chi, pgx/v5, zap, Temporal SDK,
segmentio/kafka-go.

## Endpoints

Tenant API — tenant resolved from `X-Tenant-Slug` header or the JWT
`tenant_slugs` claim (validated by middleware against identity-service via
Dapr invocation); mutating routes check Permify (`manage_catalog` /
`manage_bookings`, subject = JWT `sub` or `X-User-Id`, resource
`organization:{tenantID}`):

| Method | Path | Permify |
|---|---|---|
| GET/POST | `/v1/offerings` | POST: `manage_catalog` |
| GET/PUT/DELETE | `/v1/offerings/{id}` | PUT/DELETE: `manage_catalog` |
| GET/POST | `/v1/team-members` | POST: `manage_catalog` |
| GET/PUT/DELETE | `/v1/team-members/{id}` | PUT/DELETE: `manage_catalog` |
| PUT/GET | `/v1/team-members/{id}/availability` | PUT: `manage_catalog` |
| GET | `/v1/availability?offering_id&team_member_id&from&to[&optimize=true]` | — |
| GET | `/v1/availability/optimize?offering_id&team_member_id&date=YYYY-MM-DD` — top 3 slots `{starts_at, ends_at, score, reason}` ranked by calendar fragmentation (Wave 5 #4) | — |
| GET/PUT | `/v1/site` (auto-creates default on first read; slug immutable; theme jsonb) | PUT: `manage_catalog` |
| GET/POST, GET/PUT/DELETE `/{id}` | `/v1/contacts` | mutations: `manage_bookings` |
| POST/GET | `/v1/bookings` | POST: `manage_bookings` |
| GET | `/v1/bookings/{id}` | — |
| POST | `/v1/bookings/{id}/reschedule`, `/v1/bookings/{id}/cancel` | `manage_bookings` |

Public (no auth; the site slug resolves the tenant server-side — tenant-safe
by construction):

- `GET /public/sites/{slug}/context`
- `GET /public/sites/{slug}/availability?...` — `optimize=true` returns every
  slot annotated with `{score, reason}` (chronological order)
- `POST /public/sites/{slug}/bookings`

Customer self-service portal (Wave 5 #7 — magic-code login, no account):

- `POST /public/sites/{slug}/portal/request` `{phone|email}` — 6-digit login
  code. Always 202 (anti-enumeration); rate-limited to 5/hour per contact
  (in-memory sliding window). The code's SHA-256 hash lands in
  `portal_tokens` (10-min TTL); the plaintext code is published as a
  `com.opendesk.notifications.SendPortalCode` CloudEvent to
  `opendesk.notifications.outbox` (via `pubsub-kafka` — the component scope
  already covers both `booking` and `notification`, so no Dapr change is
  needed) and delivered by notification-worker through the smtp/twilio
  bindings.
- `POST /public/sites/{slug}/portal/verify` `{phone|email, code}` — 5 wrong
  codes lock the token (429); success consumes it and returns a 15-minute
  HS256 portal JWT (`PORTAL_SECRET`; claims `sub`=contact_id, `tid`,
  `tsl`).
- `GET /portal/bookings`, `POST /portal/bookings/{id}/reschedule`,
  `POST /portal/bookings/{id}/cancel` — portal-JWT middleware, contact-scoped
  (another contact's booking is a 404); reschedule/cancel reuse the exact
  `bookingops` logic of the staff endpoints (same availability rules and
  outbox events). APISIX exposes these at `/api/bookings/portal/*` without
  openid-connect (booking-service verifies the portal JWT itself; the
  admin-web BFF strips `Authorization`, so browsers may send the token as
  `X-Portal-Token` instead).

Temporal saga activity callbacks (invoked by notification-worker via Dapr):

- `POST /activities/reserve-slot` · `/confirm-booking` · `/release-slot` (compensation) · `/mark-no-show`

Internal: `POST /internal/sites` (seeded by `TenantOnboardingWorkflow`).

Receipt ↔ payment reconciliation (SPEC-W56 Scope A):

- `GET /v1/recon/receipts?status=open|resolved|all&limit=` — `manage_bookings`.
  Returns `{findings: [...], count, status}`: `receipt_recon_findings` rows
  newest-first, each carrying the linked capture's `receipt_fields` verbatim
  (the first read path for receipt data). `status` defaults to `open`
  (unresolved); `limit` defaults to 100, capped at 500.
- `POST /v1/internal/recon/receipts` — manual sweep trigger, guarded by
  `X-Internal-Token` vs `BOOKING_INTERNAL_TOKEN` (constant-time; 503 when
  unset — fail closed, 401 on missing/wrong). Runs one sweep synchronously
  and returns `{run_id, tenants_scanned, tenants_failed, findings_new,
  findings_deduped}`.

A finding's `kind` is one of `receipt_without_payment` (no candidate, or
ambiguous winning rung — candidates listed in `detail`),
`payment_without_receipt` (only for tenants with ≥1 receipt-bearing capture
in the window), `amount_mismatch` (paper-vs-rail; DISTINCT from the W49
rail-vs-price mismatch), `currency_unknown` (standalone ONLY when the
missing receipt amount/currency blocks an otherwise-exact pair).
`confidence` is `exact` (checkin contact chain), `heuristic` (lead-phone
chain or amount+date) or `none`. Receipts carry no currency: every
finding's `detail` carries `currency_unknown: true` and amount comparisons
assume the rail/offering currency (`assumed_currency` in `detail`). Each
NEW finding emits one `com.opendesk.booking.ReceiptReconFinding` CloudEvent
to `opendesk.booking.events` in the same transaction (dedupe key
`(tenant_id, kind, capture_client_id, booking_id)`; replays emit nothing).

Reverse CRM sync (invoked by crm-sync-service via Dapr, tenant via
`X-Tenant-Slug`, no Permify — internal only; **no outbox events**, so the
Twenty → OpenDesk direction can never loop):

- `POST /internal/contacts/upsert` — create-or-update a contact keyed by
  phone OR email within the tenant; stamps `source`/`external_id` (e.g.
  `twenty` / Twenty person id).
- `GET /internal/contacts?phone=|email=` — lookup helper.
- `POST /internal/bookings/{id}/crm-note` — append `{at, source, text}` to
  the booking's `crm_notes` JSONB array (e.g. "Twenty task … marked DONE").

## Behavior notes

- **Phone-confirmation policy**: booking creation (all paths — REST, public,
  Kafka commands) is rejected `422` without a contact phone (SPEC §1/§11).
- **Idempotency**: `idempotency_key` is UNIQUE; replays return the original
  booking. The Kafka consumer uses the CloudEvent `id` as the key.
- **Outbox**: booking mutations insert outbox rows in the same transaction; a
  dispatcher goroutine publishes them as CloudEvents to
  `opendesk.booking.events` via Dapr pubsub `pubsub-kafka` and marks them sent.
- **Saga**: booking creation starts `BookingSagaWorkflow` (workflow ID
  `booking-saga-{bookingID}`, idempotent) on task queue `opendesk-main`;
  bookings stay `pending` until the saga confirms them.
- **Availability engine** (`internal/availability`): pure function over weekly
  rules + existing bookings + buffers + capacity; extensively unit-tested
  (`availability_test.go`: overlap, partial overlap, buffer, capacity,
  effective ranges, timezones, dedup).
- **Kafka command channel**: `opendesk.booking.commands` consumed via a direct
  broker connection (kafka-go, NOT Dapr). Poison messages are dead-lettered to
  `opendesk.dlq` with error metadata after bounded retries; deterministic
  validation errors go straight to the DLQ.

## Environment variables

| Var | Default | Description |
|---|---|---|
| `PORT` | `7002` | HTTP listen port |
| `DATABASE_URL` | — (required) | Postgres DSN for the `booking` DB |
| `PERMIFY_URL` | `http://permify:3476` | Permify HTTP API |
| `AUTHZ_DISABLED` | `false` | Dev escape hatch: skip Permify checks |
| `AUTHZ_OUTAGE_POLICY` | `fail_closed` | `fail_closed` (Permify error → 502) or `fail_open` (log CRITICAL + allow; dev only) |
| `REDIS_ADDR` | — (cache off) | Redis for the availability cache |
| `CACHE_TTL_SECONDS` | `120` | Availability cache entry TTL |
| `CACHE_STALE_TTL_SECONDS` | `900` | Serve-stale window beyond the fresh TTL (`X-Cache: stale`) |
| `TENANT_CACHE_TTL_SECONDS` | `300` | Identity tenant-context cache TTL; stale-served on identity outage |
| `USAGE_EVENTS_TOPIC` | `opendesk.usage.events` | Usage-metering topic (empty disables metering) |
| `DAPR_HOST` / `DAPR_HTTP_PORT` | `daprd-booking` / `3500` | daprd sidecar |
| `DAPR_PUBSUB_NAME` | `pubsub-kafka` | Dapr pubsub component |
| `BOOKING_EVENTS_TOPIC` | `opendesk.booking.events` | Outbox topic |
| `IDENTITY_APP_ID` | `identity` | Dapr app-id of identity-service (tenant resolution) |
| `TEMPORAL_HOST_PORT` / `TEMPORAL_NAMESPACE` / `TEMPORAL_TASK_QUEUE` | `temporal:7233` / `opendesk` / `opendesk-main` | Saga client |
| `KAFKA_BROKERS` | `kafka:9092` | Direct broker list for the command consumer |
| `BOOKING_COMMANDS_TOPIC` | `opendesk.booking.commands` | Command topic |
| `BOOKING_COMMANDS_GROUP` | `booking-service-commands` | Consumer group |
| `DLQ_TOPIC` | `opendesk.dlq` | Dead-letter topic |
| `CONSUMER_ENABLED` | `true` | Run the command consumer |
| `OUTBOX_POLL_INTERVAL_SECONDS` | `2` | Outbox poll cadence |
| `SHUTDOWN_TIMEOUT_SECONDS` | `20` | Graceful shutdown budget |
| `PORTAL_SECRET` | dev fallback | HMAC secret signing 15-min portal JWTs — override in prod |
| `NOTIFICATIONS_TOPIC` | `opendesk.notifications.outbox` | Topic for `SendPortalCode` delivery |
| `RECON_SWEEP_MINUTES` | `60` | Receipt↔payment reconciliation sweep cadence (SPEC-W56); `0` disables the ticker (manual trigger stays) |
| `RECON_LOOKBACK_DAYS` | `30` | Trailing sweep window |
| `RECON_MATCH_WINDOW_DAYS` | `7` | ± contact-chain appointment match window |
| `RECON_AMOUNT_WINDOW_DAYS` | `3` | ± amount+date rung window |

## Schema notes

- Uses SPEC §7 tables: `offerings`, `team_members`, `availability_rules`,
  `contacts`, `bookings`, `outbox`.
- **`sites`** (`id, tenant_id, tenant_slug, slug UNIQUE, display_name,
  published, created_at`) is not defined in SPEC §7 but is required by the
  public booking page; the service creates it idempotently at startup
  (`CREATE TABLE IF NOT EXISTS`). Rows are seeded by the onboarding workflow
  or `POST /internal/sites`.
- **Reverse CRM sync columns** (SPEC-CRM §B): nullable `contacts.source` /
  `contacts.external_id` (+ index on `(tenant_id, external_id)`) and
  `bookings.crm_notes JSONB DEFAULT '[]'` are added idempotently at startup
  (`ALTER TABLE IF EXISTS ... ADD COLUMN IF NOT EXISTS`).
- **`portal_tokens`** (`id, tenant_id, contact_id, token_hash, channel,
  attempts, expires_at, consumed_at, created_at`) backs the customer portal
  (Wave 5 #7); bootstrapped idempotently at startup with the same RLS
  `tenant_isolation` policy as `waitlist`.
- **`receipt_recon_findings`** (SPEC-W56): recon findings (`kind`,
  confidence ladder, amounts, `detail JSONB`, `resolved_at`), bootstrapped
  idempotently by `internal/recon` with RLS ENABLE+FORCE `tenant_isolation`;
  dedupe `UNIQUE(tenant_id, kind, capture_client_id, booking_id)` plus a
  COALESCE expression unique index so NULL halves dedupe too. The W56
  additive `booking_payment_attempts.amount_minor BIGINT` / `currency TEXT`
  columns are filled at capture-transition time (`paymentTransitionLocked`)
  and are NULL on pre-W56 rows.

## Auth trust boundary

JWT *signature* verification happens at the APISIX gateway (`jwt-auth` /
`openid-connect` plugins, SPEC §8/§12). booking-service only decodes the
payload claims (`sub`, `tenant_slugs`) inside the cluster network; Permify
enforces per-tenant permissions on every mutation.

## Run

```bash
go build ./... && go test ./...
DATABASE_URL=postgres://opendesk:opendesk@localhost:5432/booking ./server
# or
docker build -t opendesk/booking-service .
```
