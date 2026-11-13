# Receipt lakehouse — receipt ingest + `receipt_recon_gold.py` (SPEC-W62)

Receipt facts in the lakehouse: OCR'd receipt documents and receipt-recon
findings land in Iceberg **bronze** via the analytics-pipeline Kafka sink, a
scheduled Spark job rolls them into **gold** daily aggregates, dbt exposes
silver staging + gold passthrough views, and the knowledge-service NL→SQL
endpoint (`/v1/analytics/query`) answers questions over the gold tables.

W62 is a **delta wave on the existing lakehouse** (MinIO + Iceberg REST +
Trino + Spark + dbt, `infra/lakehouse/`): two new bronze tables, one new gold
Spark job, consumption allowlist entries. No new service, no new Kafka topic
(`opendesk.ocr.events` was provisioned in SPEC-W53,
`infra/kafka/create-topics.sh`), no booking-service changes.

## 1. Architecture

```
ocr-service ──▶ opendesk.ocr.events (com.opendesk.ocr.DocumentExtracted)
                     │                     kind='receipt' only; kyc_* dropped;
                     │                     extracted_fields whitelisted to
                     │                     amount_minor / currency / receipt_date
                     ▼
booking-service ──▶ opendesk.booking.events (com.opendesk.booking.ReceiptReconFinding)
                     │                     routed OUT of bronze.booking_events (G2 fix)
                     ▼
   analytics-pipeline bronze sink (aiokafka micro-batch, commit-after-append)
                     │
                     ▼
   bronze.receipt_documents        bronze.receipt_recon_findings
                     │                     (Iceberg, MinIO warehouse)
                     ▼
   receipt_recon_gold.py (spark-scheduler BATCH_JOBS, degrade-graceful,
                          dynamic partition overwrite)
                     │
                     ▼
   gold.receipt_recon_daily        gold.receipt_ingest_daily
                     │
                     ▼
   dbt silver staging + gold passthrough views (Trino)
                     │
                     ▼
   knowledge-service /v1/analytics/query  (sqlguard allowlist + tenant
   predicate injection)  ·  ad-hoc Trino/BI
```

All receipt channels (field-PWA capture, WhatsApp media inbox, admin upload)
pass through ocr-service, so the OCR event stream is the **complete** ingest
fact source for OCR'd documents — no operational-DB extract is needed for
document counts. See §6 for the channel-attribution gap this does NOT close.

## 2. Bronze inputs

### 2a. `bronze.receipt_documents` — from `opendesk.ocr.events`

ocr-service emits `com.opendesk.ocr.DocumentExtracted` for every processed
document (payload: `document_id`, `tenant_id`, `kind`, `status`,
`page_count`, `sha256`, `phash`, `model_version`, storage pointers,
`extracted_fields` — see `services/ocr-service/app/events.py`). The
analytics-pipeline mapper lands **receipts only** and **whitelists the
extracted fields** (SPEC-W62 gate G1):

- `kind = 'receipt'` documents are mapped; every other kind — including all
  `kyc_*` kinds — is **dropped entirely** (zero bronze rows). W58 R1 already
  emits `extracted_fields = {}` for kyc kinds; the mapper drop is
  belt-and-braces, because even a hash-only kyc row in the lake would be a
  processing record with no TTL mechanism (the source rows carry a 72h TTL).
- From `extracted_fields` exactly three scalar keys are whitelisted:
  `amount_minor` (integer minor units), `currency` (ISO-3), `receipt_date`
  (`YYYY-MM-DD`). Everything else the OCR model may return — vendor, note,
  line items, any future key — never leaves the event and never reaches the
  lake (free text may contain names → PII).
- Storage pointers (`storage_bucket`/`storage_key`) are **not** landed:
  document bytes stay in MinIO under ocr-service's retention/erasure control.

| Column | Type | Source | Notes |
|---|---|---|---|
| `event_id` | string | CloudEvent `id` | dedupe key (at-least-once sink; event_id dedupe idiom) |
| `tenant_id` | string | CE `tenantid` ext / `data.tenant_id` | required |
| `document_id` | string | `data.document_id` | ocr_documents UUID |
| `kind` | string | `data.kind` | always `receipt` in this table |
| `status` | string | `data.status` | e.g. `extracted`, `erased` |
| `page_count` | int | `data.page_count` | |
| `sha256` | string | `data.sha256` | hash-only content id (duplicate detection) |
| `phash` | string | `data.phash` | perceptual hash, nullable |
| `model_version` | string | `data.model_version` | OCR model that produced the fields |
| `amount_minor` | long | `extracted_fields.amount_minor` | nullable — OCR may not find an amount |
| `currency` | string | `extracted_fields.currency` | nullable, ISO-3 |
| `receipt_date` | string | `extracted_fields.receipt_date` | nullable, `YYYY-MM-DD` |
| `occurred_at` | timestamp | CE `time` | naive UTC, lakehouse convention |

### 2b. `bronze.receipt_recon_findings` — from `opendesk.booking.events` (G2 fix)

The receipt-recon sweep (booking-service, SPEC-W56) emits
`com.opendesk.booking.ReceiptReconFinding` on `opendesk.booking.events` with
the full finding payload (`services/booking-service/internal/recon/recon.go`).
**Before W62 these events landed sparsely in `bronze.booking_events`**: the
generic booking mapper put the tenant-slug CloudEvent `subject` into
`booking_id` when the payload lacked one, and the dbt `accepted_values` test
on `stg_booking_events.event_type` did not include `receiptreconfinding` — a
live latent dbt-test failure. W62 routes them to their **own** bronze table
(mapper skips them for `booking_events`; the existing
`stg_booking_events` accepted_values test is unchanged and now genuinely
true).

| Column | Type | Notes |
|---|---|---|
| `event_id` | string | CloudEvent `id`; exactly-once per finding at the producer (deduped inserts never write an outbox row) |
| `tenant_id` | string | required |
| `run_id` | string | sweep run id, nullable (from finding detail when present) |
| `finding_kind` | string | `receipt_without_payment` \| `payment_without_receipt` \| `amount_mismatch` \| `currency_unknown` |
| `confidence` | string | `exact` \| `heuristic` \| `none` |
| `capture_client_id` | string | nullable — field-capture client id of the receipt |
| `booking_id` | string | nullable booking UUID (findings may be unmatched by construction) |
| `attempt_tx_ref` | string | nullable payment attempt reference |
| `expected_amount_minor` | long | nullable |
| `captured_amount_minor` | long | nullable |
| `receipt_amount_major` | double | nullable — major units as read off the receipt |
| `receipt_currency` | string | nullable (from finding detail when present) |
| `occurred_at` | timestamp | naive UTC |

The operational row keeps a free-form `detail` JSONB; only the columns above
cross into the lake.

## 3. Gold job — `receipt_recon_gold.py`

`infra/lakehouse/spark/jobs/receipt_recon_gold.py`, run by the
**spark-scheduler** sidecar (compose profile `batch-scheduler`; added to the
default `BATCH_JOBS` in `infra/lakehouse/spark/run-scheduled.sh`). House
idioms kept: idempotent via **dynamic partition overwrite** (re-runs replace
only touched day partitions) and **degrade-graceful** — a missing/unreadable
bronze table logs a warning and exits 0 instead of failing the pass.

### `gold.receipt_recon_daily`

One row per `(tenant_id, day, finding_kind, confidence)`.

| Column | Type | Definition |
|---|---|---|
| `tenant_id` | varchar | |
| `day` | date | `to_date(occurred_at)` |
| `finding_kind` | varchar | recon kind enum (§2b) |
| `confidence` | varchar | `exact` \| `heuristic` \| `none` |
| `findings_count` | long | findings raised |
| `resolved_count` | long | findings resolved. Honest note: only NEW findings emit events today (no resolution event), so this is 0 until a resolution event lands — the column is pinned in the contract so the schema does not change when one does |
| `expected_sum_minor` | long | sum of expected amounts |
| `captured_sum_minor` | long | sum of captured amounts |
| `mismatch_abs_sum_minor` | long | sum of `abs(expected - captured)` |

### `gold.receipt_ingest_daily`

One row per `(tenant_id, day)` — receipt-document ingest volume and field
coverage from `bronze.receipt_documents`.

| Column | Type | Definition |
|---|---|---|
| `tenant_id` | varchar | |
| `day` | date | `to_date(occurred_at)` |
| `documents` | long | documents landed (deduped on `event_id`) |
| `pages` | long | summed `page_count` |
| `receipts` | long | rows with `kind = 'receipt'` |
| `receipts_with_amount` | long | receipts where `amount_minor` is non-null (OCR field-coverage signal) |

Run manually (usually unnecessary — the scheduler loops hourly):

```bash
docker compose -f infra/docker-compose.lakehouse.yml --profile batch-scheduler up -d spark-scheduler
# or a one-off:
docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
  --master spark://spark-master:7077 \
  --packages org.apache.iceberg:iceberg-spark-runtime-3.5_2.12:1.6.1,org.apache.iceberg:iceberg-aws-bundle:1.6.1 \
  /opt/spark-jobs/receipt_recon_gold.py
```

Verify in Trino:

```sql
SELECT * FROM iceberg.gold.receipt_recon_daily ORDER BY day DESC LIMIT 20;
SELECT day, receipts, receipts_with_amount
FROM iceberg.gold.receipt_ingest_daily
WHERE tenant_id = '<tenant>' ORDER BY day DESC LIMIT 30;
```

## 4. dbt + consumption

- **Silver** (`infra/lakehouse/dbt/models/silver/`): source entries for both
  bronze tables plus staging views; the recon staging model carries
  `accepted_values` tests on `finding_kind` (`receipt_without_payment`,
  `payment_without_receipt`, `amount_mismatch`, `currency_unknown`) and
  `confidence` (`exact`, `heuristic`, `none`). The pre-existing
  `stg_booking_events` accepted_values list is **unchanged** — after the G2
  routing fix it is true again.
- **Gold**: both gold tables are **Spark-written**; dbt exposes them as
  passthrough **views** (the `reco_pricing` precedent) so docs, lineage and
  schema tests live in one place. dbt remains manually scheduled
  (`make dbt`); the only scheduled tier is spark-scheduler.
- **NL→SQL**: knowledge-service `sqlguard.py` `ALLOWED_TABLES` gains
  `receipt_recon_daily` and `receipt_ingest_daily`, and `analytics.py`
  `GOLD_SCHEMA_PROMPT` describes both, so `POST /v1/analytics/query` answers
  receipt questions immediately. sqlguard still injects
  `tenant_id = '<tenant>'` top-level and rejects set operations /
  unscoped subselects (SPEC-W43 Y-02) — both tables are `tenant_id`-keyed, so
  the guardrail applies unchanged. Admin-web dashboard widgets are **out of
  scope** (optional follow-on); the operational recon admin page keeps
  reading Postgres directly.

## 5. PII / NDPA posture

What the lake holds for this pipeline: **hash-only identifiers**
(`sha256`, `phash`), UUIDs (`document_id`, `booking_id`, `event_id`), scalar
amounts/currencies/dates, and `tenant_id`+`day` aggregates. It must NOT hold
— and by construction does not:

- `kyc_*` anything (mapper drop, §2a; source events already redact
  `extracted_fields` per W58 R1, and kyc source rows carry a 72h TTL the
  lake cannot honour);
- receipt free text — vendor, note, line items are never mapped (the
  `extracted_fields` whitelist is exactly three scalars);
- `media_inbox.sender_phone` / `sender_phone_digits` (WhatsApp sender PII) —
  those tables emit no events and are never read by this pipeline;
- document bytes or storage pointers.

**Erasure follow-through.** Consent/subject erasure blanks the source rows
and deletes source bytes (W58 R2/R3, W61); because only hashes + scalars ever
landed, the lake row that remains is an **already-anonymous audit stub** —
the W58 tombstone idiom, no cascade consumer required. Tenant scoping is
query-time (Iceberg tables are day-partitioned; every gold table is
`tenant_id`-keyed with `not_null` dbt tests, and sqlguard enforces the
predicate — RECON Q5).

## 6. Known gaps (honestly registered)

1. **Channel attribution is not derivable from events today.** Receipts
   arrive via field-PWA capture (`field_captures`) and WhatsApp media
   (`media_inbox`), but those tables emit **no events** (the field-capture
   outbox is client-sync idempotency only). `gold.receipt_ingest_daily`
   therefore cannot split ingest by channel. **Rejected fix:** a Postgres
   superuser/JDBC extract — the federated Trino/JDBC connection authenticates
   as the `opendesk` superuser, which **bypasses FORCE RLS** (SPEC-W62 gate
   G3). **Follow-up candidate:** emit ingest events from the
   fieldcapture/media-inbox write paths and map them to bronze — the event
   idiom every other fact on this page already uses.
2. **No lake tenant-delete purge exists today.** `TenantDeleted` on
   `opendesk.identity.events` hard-deletes the tenant's Postgres rows and
   MinIO objects, but nothing consumes it in the lakehouse, so bronze rows
   and gold aggregates for a deleted tenant persist. Accepted residual: the
   content is hash-only scalars plus `tenant_id`+`day` aggregates (no free
   text, no subject identifiers), so the residual risk is low — but it is
   flagged, not hidden. **Follow-up candidate:** a lake purge job consuming
   `TenantDeleted` (delete bronze rows by `tenant_id`; gold day-partitions
   are rebuilt by the idempotent jobs).

## 7. Tests / verification

- `services/analytics-pipeline` pytest — mapper unit tests incl. the G1
  cases (kyc `DocumentExtracted` ⇒ zero bronze rows; receipt with extra
  `extracted_fields` keys ⇒ only whitelisted columns populated) and the G2
  routing cases.
- `services/knowledge-service` pytest — sqlguard allowlist tests for the two
  new gold tables.
- `python3 -m py_compile infra/lakehouse/spark/jobs/receipt_recon_gold.py`
  (no Spark runtime in CI — a live scheduled run is verified in the compose
  lakehouse tier).
- dbt model YAML parses; `dbt build` runs silver views + gold passthrough
  views + tests against Trino.
