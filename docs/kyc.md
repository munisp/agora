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

## KYC document extraction (SPEC-W58)

Assistive pre-fill for HUMAN review of KYC documents (NIN slip, driver's
license, international passport, voter's card, CAC document). **OCR never
auto-verifies** — `review_state` defaults to `pending_review` and only a
reviewer decision moves it (P7).

### API (all under the `/v1/kyc/*` internauth — X-Internal-Token)

**`POST /v1/kyc/documents`** — multipart form: `file` (≤ 25 MB;
`image/jpeg|png|webp`, `application/pdf`, magic-byte checked), `tenant_id`
(uuid or slug), `subject_phone` (E.164), `doc_type`
(`kyc_nin_slip|kyc_drivers_license|kyc_passport|kyc_voters_card|kyc_cac_document`),
optional `consent_id` (recorded in the audit row, P6). → `200`:

```json
{ "document_id": "<uuid>", "reference": "kyc_doc_<uuid5>",
  "status": "extracted", "review_state": "pending_review",
  "fields_available": true }
```

Errors: `400` validation · `401/503` internauth · **`403`
consent_required** (no active consent for `purpose=kyc_document`) · **`403`
id_binding_violation** (same document number under a different phone, K22)
· `413` over 25 MB · `415` mime/magic rejection · `502` consent gate or
ocr-service down · `503` OCR upstream not configured (fail-closed).

**`GET /v1/kyc/documents/{id}?tenant_id=<uuid>`** (or `X-Tenant-ID`
header) → the hash-only row (`document_id, reference, tenant_id,
subject_phone, doc_type, ocr_document_id, sha256, phash, id_value_hash,
field_hashes, status, review_state, reviewer_actor, reviewed_at,
created_at`) plus `fields` + `fields_available` proxied from ocr-service
`GET /v1/ocr/documents/{id}`. Cleartext fields are **TTL-bound upstream**
(default 72 h): after the sweeper/erasure removes the ocr row the response
honestly says `"fields_available": false` and serves the hash-only record
only. Every view emits a view-audit log line (actor, document_id, tenant —
R8). ocr DOWN (transport) is `502`, distinct from fields EXPIRED.

**`POST /v1/kyc/documents/{id}/review`** — `{ "tenant_id": "<uuid>",
"decision": "approved|rejected", "reason": "..." }` → transitions
`review_state` (+ `reviewer_actor` from `X-Actor`, `reviewed_at`), writes
exactly one audit row carrying the decision and reason, `200` with the
envelope. Re-review (correction) is allowed and audited.

### Flow + sequencing (pinned)

1. **Validate** — multipart shape, doc_type enum, E.164 phone, 25 MB cap,
   mime allow-list + magic bytes.
2. **Consent gate (P6)** — identity
   `GET /internal/consents/check?subject=<phone>&purpose=kyc_document`
   (403 denied / 502 gate-down). `id_hash` is EMPTY at this step by
   construction: the document number only exists after OCR extraction.
3. **Extract** — bytes forwarded unchanged to ocr-service
   `POST /v1/ocr/extract` with `kind = doc_type` verbatim
   (`KYC_OCR_BASE_URL` + `X-Internal-Token`, 150 s bounded, NO retries).
   Failure ⇒ one audit row (`status=failed, detail=ocr_unavailable`) +
   `502`.
4. **Hash (P8)** — per-field HMAC-SHA256 (`KYC_HASH_SECRET`) digests into
   `field_hashes`; the document-number field for the doc_type (`nin` /
   `license_no` / `passport_no` / `vin` / `rc_number`, trimmed +
   upper-cased) becomes `id_value_hash`. Raw values are never stored,
   never logged, never in the POST/review response. If the OCR output has
   no document number, `id_value_hash` is "" and the binding is skipped.
5. **K22 document binding (P8), POST-extraction PRE-commit** — the first
   committed `(doc_type, id_value_hash)` row binds the document number to
   that `subject_phone`; a match under a DIFFERENT phone ⇒ audited
   `403 id_binding_violation`. The check cannot run earlier because the
   hash comes from the OCR output.
6. **Commit** — one `kyc_documents` row (sha256 of the uploaded bytes,
   phash corroborated from ocr metadata) + one `kyc_audit` row. Failure ⇒
   `500` (no durable record, fail).

### Hash-only durable record (P8)

`kyc_documents` (bootstrapped idempotently, FORCE RLS + `tenant_isolation`)
stores ONLY hashes + pointers: `document_id, tenant_id, subject_phone,
doc_type, ocr_document_id, sha256, phash, id_value_hash, field_hashes
(jsonb), status (uploaded|extracted|failed), review_state
(pending_review|approved|rejected), reviewer_actor, reviewed_at,
created_at`. Cleartext fields + document bytes live exclusively in
ocr-service (MinIO + ocr DB) under `OCR_KYC_TTL_HOURS`. Mutable ONLY on
`review_state/reviewer_actor/reviewed_at` — enforced by the
`kyc_documents_review_only` trigger (binds every role, like the kyc_audit
append-only trigger); DELETE is forbidden. Erasure wipes the ocr row +
MinIO object and leaves the hash tombstone — NDPA accountability preserved
without retaining PII.

`kyc_audit` is reused for document actions (GF7 append-only preserved):
the `id_type` CHECK is widened to the `kyc_*` doc types, `status` accepts
`uploaded|extracted|failed|approved|rejected` alongside the resolve
statuses, and additive `consent_id` + `detail` (review reason) columns
record the consent receipt and action context (P6). The widening is a
drop+re-add CHECK rebuild (W32 idiom) — idempotent and safe to re-run.

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
subject_phone, id_type (CHECK bvn|nin|kyc_nin_slip|kyc_drivers_license|
kyc_passport|kyc_voters_card|kyc_cac_document — SPEC-W58 widened),
id_value_hash, status (CHECK verified|mismatch|pending|uploaded|extracted|
failed|approved|rejected — SPEC-W58 widened), reference, consent_id,
detail, latency_ms, created_at`. Append-only (SPEC-W34 GF7): REVOKE +
BEFORE UPDATE/DELETE trigger.

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
| `KYC_OCR_BASE_URL` | — | ocr-service base URL for the document pipeline (SPEC-W58 P12); empty ⇒ `/v1/kyc/documents*` fail closed 503 |
| `OCR_INTERNAL_TOKEN` | — | shared ocr service-to-service token (existing var — the booking block sends the same one; X-Internal-Token on the kyc→ocr hop) |
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

SPEC-W58 document pipeline: httptest tests cover consent 403/502 on
`purpose=kyc_document`, validation (doc_type enum, E.164, 25 MB cap 413,
mime/magic 415), internauth 401, ocr-down 502 + audited failure, OCR-not-
configured 503 fail-closed, the K22 document binding (same document number
under a different phone ⇒ 403), the GET fields passthrough + honest
`fields_available:false` post-TTL/erasure, review transitions + audit rows,
and a CANARY assertion that the raw NIN/name/address never appear in
stored rows or POST/review responses; the OCR client is contract-tested
against an httptest stub. Store tests cover the kyc_documents DDL (RLS,
review-only-mutable trigger, DELETE ban, binding lookup, CHECK enums,
bootstrap idempotency).
