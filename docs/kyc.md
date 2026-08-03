# kyc-service — consent-gated BVN/NIN resolution (SPEC-W12 §5, Agent C)

Go service on **:7013**, Dapr app-id **`kyc`**, layout mirrors booking-service
(chi router, pgx store with RLS bootstrap, Dapr sidecar pub/sub).

## API

### `POST /v1/kyc/resolve`

```json
{ "tenant_id": "<uuid|slug>", "subject_phone": "+2348012345678",
  "id_type": "bvn", "id_value": "22223333444" }
```

→ `200` (response contract §5):

```json
{ "status": "verified", "reference": "kyc_<uuid5>", "latency_ms": 42 }
```

| Status | Meaning |
|---|---|
| `verified` | provider confirmed the identity |
| `mismatch` | provider answered: does not match |
| `pending` | provider unreachable/ambiguous — safe to retry (same `reference`) |

Error paths: `400` validation · **`403` no active consent** · `502` consent
gate (identity-service) unreachable · `500` audit write failed.

## Flow

1. **Consent gate (contract §5).** Before any resolution the service calls
   identity `GET /internal/consents/check?subject=<phone>&purpose=kyc` with
   `X-Tenant-ID`/`X-Tenant-Slug` (uuid refs become `X-Tenant-ID`, slugs
   `X-Tenant-Slug`). `403`/denied → the request is refused with
   `403 {"error":"consent_required"}`; transport failure → `502`. No consent,
   no lookup, no audit row.
2. **Resolve.** Default is the deterministic mock (`KYC_MOCK=1`, contract §5):
   `id_value` all digits and length ≥ 10 → `verified`, else `mismatch`.
3. **Audit.** Exactly one `kyc_audit` row per attempt (who/what/when/result).
   Raw BVN/NIN is **never stored** — only `sha256(id_value)` hex. The request
   fails (`500`) if the audit write fails: no audit, no resolution.
4. **Event.** CloudEvent `com.opendesk.kyc.Resolved` on topic
   **`opendesk.kyc.resolved.v1`** (`KYC_EVENTS_TOPIC`), best-effort Dapr
   publish (identity-service pattern); the audit row is the durable record a
   reconciler can republish from. Event data carries `id_value_hash`, never
   the raw value.

`reference` is deterministic (`uuid5(tenant|subject|id_type|hash)`): retries
of the same request correlate; every attempt still writes its own audit row.

## kyc_audit table

Bootstrapped at boot (idempotent DDL), `FORCE ROW LEVEL SECURITY` +
`tenant_isolation` policy (`app.tenant_id`, booking-service `withTenant`
idiom). Columns: `audit_id, tenant_id, actor (X-Actor header),
subject_phone, id_type (CHECK bvn|nin), id_value_hash,
status (CHECK verified|mismatch|pending), reference, latency_ms, created_at`.

## Configuration (contract §8 naming)

| Env | Default | Notes |
|---|---|---|
| `PORT` | `7013` | |
| `DATABASE_URL` | — | required; kyc DB (see "Provisioning") |
| `KYC_MOCK` | `1` | `0` requires `KYC_PROVIDER_URL` |
| `KYC_PROVIDER_URL` | — | live provider base URL (**ASSUMPTION**, below) |
| `KYC_PROVIDER_API_KEY` | — | bearer token for the live provider |
| `KYC_RESOLVE_TIMEOUT_SECONDS` | `8` | per-resolution budget |
| `IDENTITY_APP_ID` | `identity` | Dapr app-id for the consent gate |
| `IDENTITY_BASE_URL` | — | direct identity URL, bypasses Dapr (tests/dev) |
| `KYC_EVENTS_TOPIC` | `opendesk.kyc.resolved.v1` | |
| `DAPR_HOST` / `DAPR_HTTP_PORT` / `DAPR_PUBSUB_NAME` | `daprd-kyc` / `3500` / `pubsub-kafka` | |

**Live provider (ASSUMPTION — no live keys in this wave):** the client
expects `POST {KYC_PROVIDER_URL}/resolve {"id_type","id_value"}` with
`Authorization: Bearer …` answering `{"status":"verified|mismatch|pending"}`.
Any transport error, non-2xx, or unknown status degrades to `pending`
(+ error log) — never a fabricated hard verdict. Confirm the real provider
(Prembly/Dojah/Stripe-Identity-NG) contract before setting `KYC_MOCK=0`.

## Performance

p95 target **≤ 8s** end-to-end (contract §5): dominated by the provider call
(`KYC_RESOLVE_TIMEOUT_SECONDS=8` caps it); consent gate and audit insert are
local-network/DB ops (~ms). Mock mode resolves in µs; the timeout only
bounds the live path. A `pending` on timeout is the designed escape hatch.

## Provisioning notes (cross-agent)

- Kafka topic `opendesk.kyc.resolved.v1`: `infra/kafka/create-topics.sh`
  (Agent D, contract §7).
- docker-compose service + daprd sidecar blocks: Agent D (contract §7 env
  passthrough).
- A dedicated `kyc` database (+ `app_kyc` role) in
  `infra/postgres/init-scripts/00-create-dbs.sql` / `05-app-roles.sql` is
  NOT part of this wave's file ownership — the service bootstraps its own
  table in whatever database `DATABASE_URL` points at, but a dedicated DB +
  least-privilege role should be added there (flagged to Wave-12 lead).

## Tests

`go build/vet/test ./...` green (go1.23.4): httptest handler tests (mock
determinism, consent 403/502, validation, audit-once, publish-failure
tolerance, reference determinism, consent client vs. identity stub),
embedded-Postgres store tests (insert + CHECK constraints + RLS policy
presence). Store tests skip under `-short`.
