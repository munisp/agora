# Leads, Attribution & Campaign Spend (Wave 13, CAC App)

booking-service owns the growth-core write path: Lead storage with 24h
dedup, first-touch attribution, the lead status machine emitting
`FunnelEvent`s onto Kafka `cac.events`, promo codes + public redemption,
and campaign spend entry — plus the internal spend-sum endpoint consumed by
analytics-service. Implements SPEC-W13 Agent A (contracts §1–§7).

---

## 1. Lead entity (contract §1)

Table `leads` (RLS `tenant_isolation`, like every tenant table):

```
lead_id uuid PK, tenant_id uuid, phone_e164 text,
channel_of_first_touch text  -- voice|whatsapp|telegram|web|sms|webhook|ussd|qr|promo|field
campaign_id uuid null,       -- no FK by design (may reference campaigns or W8 geo_campaigns)
promo_code text null, utm jsonb null, lga_id int null,
status text                  -- new|contacted|qualified|converted|lost
consent_id uuid null, dedupe_key text, created_at, updated_at
UNIQUE (tenant_id, dedupe_key)
```

**Dedupe key (24h dedup, CAC FR-009):**
`dedupe_key = sha256(tenant_id|lower(phone)|channel|YYYY-MM-DD)` (UTC day of
creation). Insert is `ON CONFLICT (tenant_id, dedupe_key) DO NOTHING` → the
EXISTING lead is returned (`created=false`). Attribution is written once, at
first touch, and never overwritten — only `status`/`updated_at` ever change
(`LEAD_ATTRIBUTION_FIRST_TOUCH_ONLY=true`, contract §7, default on).

## 2. Attribution precedence (contract §3)

Resolved in `leads.ResolveAttribution`:

```
explicit promo_code  >  UTM (utm_source/medium/campaign)  >  QR slug (?ref=)  >  channel_of_first_touch
```

- **promo_code** → `channel_of_first_touch = "promo"`, `promo_code` stored,
  `campaign_id` taken from the promo code (redeem path) or as given (create).
- **UTM** → channel stays as observed (e.g. `web`, `whatsapp`); the utm map
  is persisted for campaign matching downstream.
- **QR slug** → `channel_of_first_touch = "qr"` and a utm triple is
  synthesized: `{utm_source:"qr", utm_medium:"offline", utm_campaign:<slug>}`
  (mirrors the Agent E QR landing redirect).
- **bare channel** → stored as given (default `web`).

Frontend (embed.js / widget bridge, Agent E) merely forwards hints; the
backend enforces first-touch — replays with different UTM/promo are deduped
away without touching the stored attribution.

## 3. Status machine & funnel events (contract §2)

```
new → contacted → qualified → converted | lost      (converted/lost terminal)
```

Every transition emits one CloudEvent `com.opendesk.cac.FunnelEvent` on
Kafka topic `cac.events` (`CAC_EVENTS_TOPIC`) via the transactional outbox
(same dispatcher as booking/usage events):

```json
{"event_id":"uuid","tenant_id":"uuid","entity_type":"lead","entity_id":"uuid",
 "event_name":"lead_created|contacted|qualified|converted|lost",
 "event_ts":"rfc3339","channel":"...","campaign_id":"uuid|null","lga_id":null,
 "amount_ngn":null,"idempotency_key":"lead:{lead_id}:{event_name}"}
```

`idempotency_key` is deterministic (lead × event) — the analytics-service
consumer (Agent B, group `analytics-cac`) dedupes on it. Exactly one
`lead_created` fires per lead: dedupe hits and promo-redemption replays
emit nothing. Emission is best-effort after commit (enqueue failures are
logged for reconciliation; the lead row is always durable).

## 4. REST API

Tenant-scoped (`X-Tenant-Slug` middleware). Mutations require Permify
`manage_bookings`; reads require `view_analytics` (docs/security/roles.md).

| Method & path | Perm | Purpose |
|---|---|---|
| `POST /v1/leads` | manage_bookings | Create lead (`{phone_e164, channel, promo_code?, utm?, ref?, campaign_id?, lga_id?, consent_id?}`) → `201 {lead, created}` (`200` on dedupe hit, existing lead returned) |
| `GET /v1/leads?status=&channel=&campaign_id=&from=&to=` | view_analytics | List (newest first, ≤500) → `{leads:[...]}` |
| `GET /v1/leads/{id}` | view_analytics | Detail → `{lead}` |
| `POST /v1/leads/{id}/status` | manage_bookings | Transition `{status}` → `{lead}`; illegal transition → `409` |
| `POST /v1/promo` | manage_bookings | Upsert promo code `{code, campaign_id?, discount_ngn?, max_redemptions}` (0 = unlimited) → `201 {promo_code}` |
| `GET /v1/promo` | view_analytics | List codes → `{promo_codes:[{code, campaign_id, discount_ngn, max_redemptions, redeemed_count, created_at}]}` |
| `POST /v1/campaigns` | manage_bookings | Create campaign `{name, channel, start_ts?, end_ts?}` → `201 {campaign}` |
| `GET /v1/campaigns` | view_analytics | List with lifetime spend → `{campaigns:[{id, name, channel, spend_ngn, start_ts, end_ts, created_at}]}` |
| `POST /v1/campaigns/{id}/spend` | manage_bookings | Record spend `{amount_ngn, channel, day:"YYYY-MM-DD"}` → `201 {spend}` |

**Spend write semantics:** `campaign_spend` is keyed
`(tenant_id, campaign_id, day, channel)` with SET semantics — reposting the
same key REPLACES the amount, so retried posts never double-count.

### Public promo redemption (contract §6)

`POST /v1/promo/redeem {code, phone}` — NO tenant middleware (the
unguessable code resolves its owning tenant server-side, same pattern as
public site-slug resolution). Rate-limited (10/min per code+phone, 60/min
per IP). Idempotent per code+phone via the `promo_redemptions`
`(tenant_id, code, phone_e164)` anchor: a replay returns the original lead
and does NOT bump `redeemed_count`. Creates the lead with promo attribution
(channel `promo`, campaign from the code) when it is the phone's first
touch of the day, emitting `lead_created` exactly then.
`max_redemptions` enforced under `SELECT ... FOR UPDATE` → `409 promo code
exhausted`. Unknown code → `404`.

### Internal spend-sum (Agent B coordination)

`GET /internal/campaigns/{id}/spend-sum?from&to` — Dapr-invoked by
analytics-service; usual `X-Tenant-Slug` middleware (like
`/internal/contacts`), no Permify guard. `from`/`to` are RFC3339 or
YYYY-MM-DD day bounds (inclusive, optional).

```json
{"campaign_id":"uuid","from":"2026-01-01T00:00:00Z","to":null,
 "spend_ngn":15500.0,"by_channel":[{"channel":"field","spend_ngn":12500.0},
 {"channel":"qr","spend_ngn":3000.0}]}
```

## 5. Downstream (context)

- **analytics-service** (Agent B) consumes `cac.events` into
  `cac_rollup_channel` / `cac_rollup_lga` and serves `GET /v1/cac/summary`;
  spend is joined via the internal endpoint above.
- **lakehouse** (Agent C) recomputes the same rollups nightly from the
  `cac.events` bronze into Iceberg gold tables
  (`cac_gold.daily_cac_by_channel`, `cac_gold.daily_cac_by_lga`) —
  batch-verified source of truth (realtime rollup + nightly reconcile).

## 6. Config (contract §7)

| Env | Default | Purpose |
|---|---|---|
| `CAC_EVENTS_TOPIC` | `cac.events` | Funnel topic (empty disables emission) |
| `LEAD_ATTRIBUTION_FIRST_TOUCH_ONLY` | `true` | First-touch attribution guard |
| `CAC_EVENTS_GROUP` | `analytics-cac` | Consumer group (analytics-service side) |

## 7. Tests

- `internal/store/leads_test.go` — dedupe/first-touch, status + filters,
  promo redeem tx (idempotency, exhaustion, cross-tenant), spend SET
  semantics + bounded sums (embedded Postgres).
- `internal/leads/leads_test.go` — pure: dedupe key, status machine,
  attribution precedence, FunnelEvent shape.
- `internal/leads/service_test.go` — create + full transition chain emits
  exactly `lead_created/contacted/qualified/converted` on `cac.events`;
  promo redeem end-to-end (embedded Postgres, port 5546).
- `internal/httpapi/leads_routes_test.go` — route wiring: public redeem
  bypasses tenant middleware, internal spend-sum requires it, admin API
  perm-guarded.
