# ocr-service — MiniCPM-OCR document intelligence (SPEC-W53)

Python FastAPI service (Dapr app-id **`ocr`**, default port **7020**) that
turns uploaded document images and PDFs into text/markdown plus structured
fields, backed by a MiniCPM-OCR GGUF served by a llama.cpp `llama-server`
sidecar. First document-bytes ingestion surface on the platform — read the
[Security](#security) section before exposing it.

Consumers this wave:

- **Knowledge scans** — admin-web uploads a scan/photo/PDF, OCR output is
  reviewed and ingested into the existing knowledge pipeline
  (`POST /api/knowledge/v1/documents`; no knowledge-service changes).
- **Receipt/invoice OCR** — staff attach a receipt to a field capture via
  booking-service fieldcapture; extracted `{vendor, amount, date, items}` is
  stored on the capture and displayed in admin-web.
- **KYC document OCR (SPEC-W58)** — NIN slip / driver's license /
  international passport / voter's card / CAC document, consent-gated
  through kyc-service as an assistive pre-fill for HUMAN review. See
  [KYC document kinds](#kyc-document-kinds-spec-w58) — the NDPA gates there
  (event redaction, erasure consumer, 72h TTL) are load-bearing: kyc_* kinds
  ship together with them or not at all.

Deferred to later waves: WhatsApp media pipeline, fraud detector D9 (the
`phash` column is already populated to prepare it).

## Architecture

```
admin-web ──BFF──▶ APISIX (api-ocr, OIDC) ──▶ ocr-service (FastAPI)
fieldcapture (booking-service) ──X-Internal-Token──▶ ocr-service
                                                   │
                                                   ├─▶ llama-ocr sidecar (llama.cpp llama-server,
                                                   │    OpenAI-compatible /v1/chat/completions,
                                                   │    MiniCPM-OCR GGUF + mmproj on `ocr-models` volume)
                                                   ├─▶ MinIO `documents` bucket (tenant-prefixed keys)
                                                   ├─▶ Postgres `ocr` DB (`ocr_documents`, FORCE RLS)
                                                   └─▶ Dapr pubsub: opendesk.usage.events (ocr_page)
                                                      + opendesk.ocr.events (DocumentExtracted)
```

- The **sidecar runs the model**, not the service (RECON §3): llama-server
  is a single static binary speaking the OpenAI-compatible API every other
  consumer already uses (Ollama-shaped `OCR_LLM_BASE_URL`, default
  `http://llama-ocr:8080/v1`). No torch/transformers in any Python image
  (I5 slim-image discipline).
- The sidecar lives in the opt-in overlay `infra/compose/ocr.compose.yml`
  (voices.compose.yml pattern), CPU by default with a commented GPU
  `capabilities: [gpu]` block for production. Expect multi-second per page
  on CPU at ctx 40960 — plan SLOs accordingly; e2e uses `OCR_MOCK`.
- Weights are **volume-cached** (`ocr-models` volume, mms-models precedent)
  and are never downloaded at request time. See the provisioning runbook.
- PDFs are rasterized page-by-page with `pdftoppm` (poppler-utils in the
  ocr-service image — the one deliberate slim-image exception) because the
  llama.cpp vision path consumes images; each page is a separate
  chat-completion and a separate metered `ocr_page` unit.
- Metadata lives in the `ocr_documents` table (dedicated `ocr` database,
  FORCE RLS + `tenant_isolation`, index on `(tenant_id, sha256)`); document
  bytes live only in MinIO. `GET /v1/ocr/documents/{id}` never returns bytes.

## Model provisioning runbook (one-time per environment)

Exact GGUF filenames and their sha256 digests are pinned in
`services/ocr-service/README.md` — treat that file as the source of truth;
this runbook is the procedure.

1. **Pin the exact model + version first, then re-read its LICENSE.**
   MiniCPM licensing varies by model version: newer MiniCPM-V 4.x /
   MiniCPM-OCR 1.0 weights are Apache-2.0 per the OpenBMB model cards, but
   older MiniCPM-V 2.6 weights use the MiniCPM Model Community License
   (commercial use gated on a registration questionnaire, <1M DAU,
   "Built with MiniCPM" attribution). Do not assume — open the LICENSE file
   shipped next to the exact GGUF revision you download and record the
   conclusion in the README before shipping.
2. Download the model GGUF and the mmproj projector GGUF from the pinned
   Hugging Face revision **on an operator workstation**, not from the
   container.
3. Verify both files against the pinned sha256 (`sha256sum file.gguf`) —
   mismatch ⇒ discard and re-download; never provision unverified weights.
4. Copy them into the `ocr-models` volume at `/models/` (e.g. via a
   throwaway `mc`/alpine container with the volume mounted). The sidecar
   starts with `--model /models/<file>.gguf --mmproj <projector>.gguf`.
5. Start the overlay (`infra/compose/ocr.compose.yml`) and confirm the
   sidecar healthcheck, then run one real extraction against a sample image
   with `OCR_MOCK=false` before announcing the route.
6. Re-provisioning = repeat with a new pinned revision. Model *governance*
   (promote/rollback) is registered as a family in model-registry; the
   registry stores only the `artifact_uri` metadata — the bytes stay in the
   volume.

## `OCR_MOCK` semantics

- **Safe-default-OFF** (`OCR_MOCK=false` in `.env.example` and compose).
  Production always calls the real sidecar.
- When `true`, the service logs a **CRITICAL** line at startup (XTTS_MOCK
  precedent) and serves deterministic fixture extractions (fixed
  vendor/amount/date, fixed markdown body) without touching llama-server or
  MinIO. Use only for dev loops and e2e (CPU sandbox latency makes real
  extraction impractical in CI).
- No other mock path exists; there is no silent fallback to fixtures when
  the sidecar is down — failures become `status=failed` rows + `502`.

## Usage metering

Every extracted page appends a `com.opendesk.usage.UsageRecord`
(`metric: "ocr_page"`, `meta: {document_id, service, model_version}`) to
topic **`opendesk.usage.events`** via the UsageExtra outbox pattern — same
transaction as the document row, so metering survives publish outages.
analytics-pipeline consumes it unchanged (metric is a free-text column) into
`bronze.usage_events` → `gold.usage_daily`. OCR pages are therefore billable
with zero consumer work.

## Events

On successful extraction the service emits CloudEvent
**`com.opendesk.ocr.DocumentExtracted`** on topic **`opendesk.ocr.events`**
(registered in `infra/kafka/create-topics.sh`), best-effort Dapr publish;
the `ocr_documents` row is the durable record a reconciler can republish
from. The event carries ids + extracted fields, never document bytes —
**except for kyc_* kinds (SPEC-W58 GATE R1): the event emits
`extracted_fields: {}` so KYC PII never touches the bus** (consumers
graph-sync + fraud D9 work unchanged on the hash-only projection). The
stored `phash` + `sha256` enable a future fraud detector (duplicate/
tampered-document signal) without re-reading MinIO.

## Security

Uploaded bytes are **PII** (receipts, IDs, scanned letters). Posture:

- **Storage**: MinIO `documents` bucket, keys tenant-prefixed
  `{tenant_id}/{uuid}.<ext>`; no anonymous access; bytes are never logged.
  Metadata table under FORCE RLS; `GET /v1/ocr/documents/{id}` returns
  metadata + extracted fields only.
- **Validation**: extension **and** magic-byte allow-list
  (`image/jpeg`, `image/png`, `image/webp`, `application/pdf`), hard
  **25 MB** byte cap (`OCR_MAX_BYTES`), per-tenant rate limit at the
  gateway. admin-web mirrors the cap client-side but the service cap is the
  enforcement point.
- **Ingress**: public traffic only via APISIX (`api-ocr` route, OIDC) →
  admin-web BFF (binary passthrough); service-to-service callers
  (fieldcapture) authenticate with `X-Internal-Token`, constant-time,
  fail-closed.
- **No antivirus scanner — documented gap.** There is no ClamAV anywhere in
  the platform; OCR upload is the first user-content binary surface. The
  allow-list + byte cap + rasterize-then-infer pipeline limits the blast
  radius (payloads are decoded to pixels, never executed or served back),
  but this is an accepted residual risk that matters MORE now that KYC
  identity documents ride this surface (RECON §5 R9). **Follow-up (do not
  drop again):** add a ClamAV sidecar to `infra/compose/ocr.compose.yml`
  and scan before storage.
- **NDPA note**: storing document *images* (not just extracted text) is a
  deliberate policy change relative to kyc-service's never-store-raw-IDs
  posture. Deployments must cover document storage in their privacy notice,
  define retention, and wire GDPR export/erasure to include and delete
  document objects. For **kyc_* kinds** retention and erasure are
  enforced in-service (see below); receipt/knowledge_scan documents remain
  residual until the sweeper is extended to them.

## API summary

Public ingress prefix: `/api/ocr` (APISIX) → service root.

### `POST /v1/ocr/extract`

Multipart `file` **or** JSON `{document_url}`; form/JSON param
`kind ∈ {receipt, document, knowledge_scan, kyc_nin_slip,
kyc_drivers_license, kyc_passport, kyc_voters_card, kyc_cac_document}`.
Validates extension + magic bytes + size, stores to MinIO, rasterizes PDFs,
extracts via the sidecar, persists an `ocr_documents` row, meters
`ocr_page`, emits the event. **kyc_* kinds are intended to be reached via
kyc-service** (consent gate + audit, service-to-service) — direct public
`/api/ocr` upload of kyc_* kinds bypasses consent and MUST NOT be exposed.

`200` →

```json
{ "document_id": "<uuid>", "status": "extracted",
  "extracted_fields": { "vendor": "…", "amount": 12500, "date": "…", "items": [] },
  "page_count": 2,
  "extracted_text": "# Receipt\n…full markdown/text body…" }
```

(receipt kinds populate vendor/amount/date/items tolerantly — any may be
null; knowledge scans return markdown text + best-effort fields).
Failures: `400` validation · `413` over cap · `502` sidecar/extraction
failure (row persisted as `status=failed`; no retry loop). Bounded
timeouts: sidecar call 120 s.

### `GET /v1/ocr/documents/{id}`

Metadata + extracted fields for one document (RLS-scoped). **Never returns
document bytes.**

## Receipt OCR via fieldcapture

booking-service fieldcapture owns the staff upload surface:
`POST /v1/field/captures/{id}/receipt` forwards image/PDF bytes to the
ocr-service internally (`OCR_SERVICE_URL` + `X-Internal-Token`, 150 s
bounded client timeout — OCR is slow on CPU — no retries, mime allow-list
re-checked client-side). The returned `document_id` + extracted fields are
stored on the capture (`receipt_document_id`, `receipt_fields jsonb`,
additive columns). Fail-open: ocr-service down ⇒ `502` with code
`OCR_UNAVAILABLE` and the capture row stays intact; receipt OCR never blocks
a payment or a capture write. admin-web displays `receipt_fields`
(vendor/amount/date/items + a "Receipt OCR" marker) on the person-360 lead
captures surface, defensively optional — there is deliberately no upload
button in admin-web this wave.

## Knowledge-scan flow

1. Admin opens Knowledge → **Upload scan**, picks a
   `.jpg/.jpeg/.png/.webp/.pdf` (≤ 25 MB client-side guard).
2. Browser POSTs multipart to `/api/ocr/v1/ocr/extract` with
   `kind=knowledge_scan` through the BFF (binary passthrough, W52-D).
3. The dialog shows progress + an "OCR is slow on CPU" hint, then an
   extraction summary (page count, first structured fields, text preview).
4. On **Review & add**, the extracted text pre-fills the existing
   Add-document dialog; confirming POSTs to the unchanged
   `POST /api/knowledge/v1/documents` textarea path — OCR is a pure upstream
   text producer, knowledge-service needed zero changes.

## KYC document kinds (SPEC-W58)

Five kinds extract identity-document fields as an **assistive pre-fill for
human review — OCR never auto-verifies**:

| Kind | Whitelisted fields (tolerant parse, null-fallback; everything else dropped) |
|---|---|
| `kyc_nin_slip` | `full_name, dob, nin, address?` |
| `kyc_drivers_license` | `full_name, dob, license_no, expiry` |
| `kyc_passport` | `full_name, dob, passport_no, nationality, expiry, mrz_valid` |
| `kyc_voters_card` | `full_name, vin, dob?` |
| `kyc_cac_document` | `company_name, rc_number, registration_date, address?` |

Entry path (3-hop, SPEC-W58 P1): admin-web → booking-service staff routes
(owner/admin only) → kyc-service `POST /v1/kyc/documents` (consent gate
`purpose=kyc_document` + hash-only `kyc_documents` record + append-only
`kyc_audit`) → this service's existing `/v1/ocr/extract` with
`kind = doc_type` verbatim. The durable kyc-service record stores HMAC-SHA256
per-field hashes only — raw field values exist ONLY in this service's DB
under the TTL below. Reviewers read fields back through
`GET /v1/kyc/documents/{id}` (owner/admin, view-audited); after expiry the
envelope honestly returns `fields_available: false`.

### NDPA gates (load-bearing — kyc_* kinds ship with these or not at all)

- **R1 event redaction** — for any kind starting `kyc_`,
  `DocumentExtracted` carries `extracted_fields: {}`. KYC PII never rides
  the Kafka bus.
- **R2 erasure consumer** — the service consumes
  `com.opendesk.consent.ErasureRequested` (`opendesk.consent.erasure.v1`)
  and identity `TenantDeleted`: for kyc_* rows it blanks
  `extracted_fields`/`extracted_text`, sets `status='erased'` and DELETES
  the MinIO object; `sha256`/`phash` are retained for audit. TenantDeleted
  hard-deletes ALL of the tenant's rows + objects (all kinds).
- **R3 retention TTL** — kyc_* rows are inserted with
  `expires_at = now() + OCR_KYC_TTL_HOURS` (default **72h**). A periodic
  sweeper (interval `OCR_KYC_SWEEP_MINUTES`, default **15**) deletes expired
  rows + MinIO objects (kind-generic mechanism, kyc-scoped data). A MinIO
  bucket lifecycle rule on the `documents` bucket is the documented
  belt-and-braces layer — it cannot be provisioned from this repo, so
  operators must set it per environment.
- **Minimization** — `extracted_text` (the full transcript) is never
  persisted for kyc_* kinds; there is no downstream text consumer.
- **Human-review terminal** — kyc_documents.review_state defaults to
  `pending_review`; only an owner/admin approve/reject action (audited)
  moves it.

### MiniCPM capability honesty

The model **transcribes printed fields only**. Nothing in this pipeline
claims, and no caller may represent:

- **MRZ verification by the model** — for `kyc_passport` the MRZ check
  digits are validated by deterministic code (ICAO 9303) in the parse layer
  and surfaced as `mrz_valid: true|false|null`; the model's transcription is
  the input, never the verdict.
- **Barcode/PDF417 decoding** (driver's-license reverse) — no decoder
  exists; the model sees pixels only.
- **Forensic authenticity** (holograms, UV, microprint, screen-recapture) —
  the D9 hash signals are byte-level tamper signals, not forensics.
- **Face matching or liveness** — no comparator or selfie pipeline exists.

Accuracy on Nigerian document layouts is UNVERIFIED (no golden corpus);
the human review step is the mitigation, and the review verdict remains
kyc-service provider resolution plus reviewer sign-off.
