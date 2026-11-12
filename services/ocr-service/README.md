# ocr-service (SPEC-W53 Scope A)

Document-intelligence foundation: upload validation, MinIO `documents`
object storage, MiniCPM-OCR extraction via a `llama.cpp llama-server`
sidecar, RLS metadata table, `ocr_page` usage metering, CloudEvents.

## Topology

```
admin-web / BFF ──APISIX api-ocr (/api/ocr/*, OIDC)──┐
booking-service fieldcapture ──X-Internal-Token──────┤
                                                     ▼
                                              ocr-service (:7020)
                                                │    │    │
                            MinIO documents ◄───┘    │    └──► Postgres `ocr`
                            {tenant_id}/{uuid}.ext   │         ocr_documents (FORCE RLS)
                                                     ▼
                                        llama-ocr sidecar (:8080)
                                        llama-server, OpenAI-compatible
                                        /v1/chat/completions, GGUF on the
                                        ocr-models volume (CPU default,
                                        GPU block commented in compose)
                                                     │
                            Dapr pubsub-kafka ◄──────┘
                            opendesk.usage.events (ocr_page UsageRecord)
                            opendesk.ocr.events (com.opendesk.ocr.DocumentExtracted)
```

The sidecar lives in a separate opt-in overlay,
`infra/compose/ocr.compose.yml` (voices.compose.yml precedent):

```sh
docker compose -f docker-compose.yml -f infra/compose/ocr.compose.yml up -d
```

## Model provisioning runbook (ONE-TIME, offline-first)

Model weights are NEVER downloaded at request time. Provision once into
the `ocr-models` docker volume:

| File | Role | sha256 |
|---|---|---|
| `MiniCPM-OCR-1_0-Q8_0.gguf` | language model weights (~0.8 GB) | `<TODO at provisioning: sha256sum of the downloaded file>` |
| `mmproj-model-f16-4x.gguf` | vision projector (mmproj, ~0.86 GB) | `<TODO at provisioning>` |

Sidecar image: `ghcr.io/ggml-org/llama.cpp:server-b6992` (pinned tag;
re-verify the tag against ggml-org releases before pulling).

1. On a machine with network access, download the two GGUFs from the
   pinned OpenBMB MiniCPM-OCR 1.0 release.
2. `sha256sum` both files and record the digests in the table above
   (replace the `<TODO>` placeholders).
3. Copy them into the volume:
   ```sh
   docker run --rm -v agora_ocr-models:/models -v "$PWD:/host" alpine:3.20 \
     sh -c 'cp /host/MiniCPM-OCR-1_0-Q8_0.gguf /host/mmproj-model-f16-4x.gguf /models/'
   ```
4. Start the overlay (command above). `llama-ocr` healthchecks on
   `GET /health`; first load takes a minute on CPU.

> **LICENSE WARNING (RECON §5 — verify before ship):** MiniCPM licensing
> VARIES BY VERSION. Newer MiniCPM-V 4.x / MiniCPM-OCR 1.0 weights are
> Apache-2.0 per the OpenBMB model cards, but older MiniCPM-V 2.6 weights
> use the MiniCPM Model Community License (commercial use gated on a
> registration questionnaire, <1M DAU, "Built with MiniCPM" attribution).
> **Pin the exact model+version above and RE-READ its LICENSE file at
> provisioning time.** Do not substitute a different MiniCPM release
> without a fresh license review.

## MinIO client choice

Python writes to MinIO via the official `minio` pip package
(`minio==7.2.15`, pure-Python ~1 MB wheel + certifi/urllib3/argon2-cffi/
pycryptodome). The Go hand-rolled SigV4 precedent
(`notification-worker/internal/activities/s3.go`) exists because adding a
Go module was the heavier option there; in Python the maintained official
client is smaller and safer than a hand-rolled SigV4 implementation, and
stays within the slim-image budget (I5) — total image delta is dominated
by poppler-utils (~35 MB, see Dockerfile comment), not this client.

## Security posture

- Uploaded bytes are PII (NDPA): tenant-prefixed MinIO keys
  `{tenant_id}/{uuid}.<ext>`, bytes never logged, metadata table under
  `ENABLE`+`FORCE ROW LEVEL SECURITY` (`tenant_isolation`, NULLIF
  current_setting idiom).
- Validation: extension + magic-byte allow-list (`image/jpeg`,
  `image/png`, `image/webp`, `application/pdf`; the extension and sniffed
  magic MUST agree), 25 MB hard cap (`OCR_MAX_BYTES`), APISIX
  `limit-count` on the public route.
- **No AV scanner exists anywhere in the platform** — this upload surface
  is the first user-content binary ingress. Documented-acceptance for
  W53; ClamAV sidecar is a follow-up (RECON §5).
- Internal callers: `X-Internal-Token`, constant-time compare,
  fail-closed 503 when `OCR_INTERNAL_TOKEN` is unset. APISIX global rules
  strip client-supplied `x-internal-token`, so gateway calls can never
  forge internal auth.

## OCR_MOCK (DEV ONLY)

`OCR_MOCK=true` selects a deterministic fixture extraction (fixed
vendor/amount/date — see `app/llm.py MOCK_*`), no sidecar, no rasterizer,
no network. Safe-default-OFF; a CRITICAL startup log fires when enabled
(XTTS_MOCK precedent). NEVER set in production. Compose wires
`${OCR_MOCK:-false}`.

## API summary (normative: `docs/api/openapi/ocr.yaml`)

- `POST /v1/ocr/extract` — multipart `file` (+ form `kind`, `tenant`) OR
  JSON `{"document_url", "kind", "tenant"}`; `kind` in
  `receipt|document|knowledge_scan`. Response:
  `{document_id, status, extracted_fields, page_count}` — receipt
  `extracted_fields`: `{vendor, amount, currency, date, items[]}` (nulls
  allowed, tolerant parse). Upstream failure → row `status=failed` + 502
  (no retries).
- `GET /v1/ocr/documents/{id}` — metadata + extracted fields (NO bytes).
- `GET /healthz`, `GET /metrics`.

## Usage metering

Every extracted page emits one `com.opendesk.usage.UsageRecord`
(`metric=ocr_page`, `value=page_count`, `meta={document_id, service,
model_version, kind}`) to `opendesk.usage.events` — the analytics-pipeline
bronze.usage_events consumer needs no changes (metric is free-text).

## Tests

```sh
pip install -r requirements-dev.txt
cd services/ocr-service && python -m pytest tests -q
```

Offline: no network, no model download. The real-sidecar path is tested
against a local stub HTTP server; Postgres/RLS tests skip cleanly unless
`OCR_TEST_DATABASE_URL` is set.
