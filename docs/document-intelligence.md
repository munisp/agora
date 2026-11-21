# Document Intelligence (SPEC-W107 + W111)

How binary documents become conversational knowledge in OpenDesk/Agora.

## Architecture

```
POST /v1/documents/upload  (knowledge-service, JSON base64)
  -> tenant binding (SPEC-W43 C1, X-Tenant-Slugs)
  -> wait=true (default): doc-intel sync convert (W107, unchanged)
  -> wait=false: upload_jobs row + 202; background submit -> poll -> result (W111)
  -> doc-intel sidecar (docling-serve, infra/compose/doc-intel.compose.yml)
       docling layout-aware parse -> Markdown
       RapidOCR (PaddleOCR ONNX models) for scanned pages / images
       TableFormer table structure -> Markdown tables
  -> _ingest_document (UNCHANGED): chunk (~500 tok, sentence-boundary)
       -> Embedder (all-MiniLM-L6-v2, 384-dim)
       -> OpenSearch kb-chunks (BM25 + kNN, client-side RRF k=60)
```

The sidecar is the **only** extraction engine. There is no in-process
fallback parser: when the sidecar is unreachable the sync endpoint answers
**503** — a silent text-only degradation would be indistinguishable from a
mock. Async jobs record the same failure honestly on the job row.

## Formats

| Format | Path |
|---|---|
| PDF (digital) | docling layout pipeline -> Markdown |
| PDF (scanned) | layout pipeline + RapidOCR on bitmap content |
| DOCX / PPTX / XLSX | docling office backends -> Markdown |
| HTML / MD / TXT / AsciiDoc | docling markup backends |
| PNG / JPG / TIFF | OCR path (RapidOCR) |

Supported extensions (415 otherwise): `pdf docx pptx xlsx html htm md txt
asciidoc adoc png jpg jpeg tiff tif`.

## Async ingest for very large scans (SPEC-W111)

The W107 sync path remains the default (`wait` omitted or `true`) and is
byte-identical. For scans that can outlive a request budget, submit with
`"wait": false`:

```
POST /v1/documents/upload
{"tenant": "...", "filename": "annual-report.pdf",
 "content_base64": "...", "wait": false}
-> 202 {"job_id": "...", "status": "pending", "tenant_id": "...", "title": "..."}

GET /v1/documents/upload/{job_id}?tenant=...
-> {"status": "pending|running|done|failed", "document_id": ..., "chars": ...,
    "error": ...}
```

The background runner submits docling's `/v1/convert/source/async`, polls
`/v1/status/poll/{task_id}` every `DOC_INTEL_POLL_S` (default 2s) until
`DOC_INTEL_JOB_TIMEOUT_S` (default 900s), fetches `/v1/result/{task_id}`, then
runs the **same unchanged `_ingest_document` pipeline** as sync uploads.
Terminal failures mark the job `failed` with the honest error; the job never
throws into unrelated requests. Job status reads are tenant-bound like every
other knowledge route: another tenant receives **404**, not an existence
oracle.

`upload_jobs` is created idempotently at service startup with RLS enabled and
forced (kb_suggestions precedent). A bootstrap failure is logged and does not
break sync uploads; `wait=false` requests will fail honestly until the table
exists.

### Stale-running reaper (W112)

A crash or restart strands `wait=false` jobs in `running` forever — the
background task is gone but the row never settles. knowledge-service runs a
reaper: one sweep at startup, then one every `DOC_INTEL_REAPER_INTERVAL_S`
(default 60s). Any row still `running` whose `updated_at` is older than
`DOC_INTEL_REAPER_STALE_S` (default 960s = job timeout + 60s grace) is
marked `failed` with `interrupted: service restarted while job was running;
resubmit upload`. Pending rows (the task may not have started) and terminal
rows are never touched. Because `upload_jobs` uses FORCE RLS, the sweep runs
on a maintenance connection with `app.upload_reaper=on` (the
`upload_job_reaper` UPDATE-only policy); request connections remain
tenant-bound. Set `DOC_INTEL_REAPER_ENABLED=off` to disable.

## OCR — PaddleOCR models via RapidOCR

knowledge-service always requests `ocr_engine=rapidocr`: the ONNX Runtime
build of the PaddleOCR detection/recognition models. It runs CPU-only inside
the sidecar. Scanned documents extract through it automatically; if a PDF has
no text layer and OCR yields nothing, the upload fails **422** with an honest
message (never an empty knowledge document).

## VLM pipeline (opt-in, heavy)

For hard layouts (multi-column scans, forms, degraded images) docling offers a
VLM pipeline (e.g. the granite-docling preset) that replaces the classical
layout+OCR stages with a vision-language model. It is **not enabled** in the
overlay: CPU cost is an order of magnitude higher per page. To evaluate it,
set the docling-serve pipeline env (see docling-serve release notes for the
current env name) in a dedicated overlay and point a second knowledge-service
at it — do not flip the shared sidecar.

## RAG surfaces (user directive: chat, voice AND channels)

| Surface | Consumer | Status |
|---|---|---|
| Voice | voice-agent-runtime `/v1/context` (X-Tenant-Slugs bound) + async_tools `knowledge_search` (filler ack while the slow tool runs) | WIRED |
| Chat | voice-agent-runtime text path: per-turn `/v1/context` query merged with bootstrap snippets before prompt build (SPEC-W109) | WIRED |
| Channels (USSD / messaging) | conversation-service USSD text turns ground from `/v1/context` (SPEC-W108) with optional LLM compression (SPEC-W110); WhatsApp/Telegram ride the voice bridge | WIRED |

All surfaces share the same embeddings — there is one knowledge base, not
three.

## Errors

| Code | Cause |
|---|---|
| 400 | invalid base64 |
| 403 | tenant not in caller's X-Tenant-Slugs |
| 413 | decoded upload > DOC_INTEL_MAX_BYTES (10 MiB) or extracted text > DOC_INTEL_MAX_CHARS (1M) |
| 415 | extension outside the allowlist |
| 422 | docling conversion failed, or no text extracted (OCR pointer) |
| 503 | sidecar unreachable / timeout |

## Limits & honest caveats

- Sync conversion waits up to `DOCLING_SERVE_MAX_SYNC_WAIT=600`s on the
  sidecar and `DOC_INTEL_TIMEOUT_S=300`s in knowledge-service. Very large
  scans should use `wait=false` (above) instead of stretching HTTP budgets.
- Async jobs poll every `DOC_INTEL_POLL_S` and give up after
  `DOC_INTEL_JOB_TIMEOUT_S`; a timeout marks the row `failed`, never retries
  silently.
- Tables become **Markdown text** inside chunks — cell-level structure is not
  queryable as data (text-to-SQL analytics is the structured path).
- Reading order is docling's heuristic; exotic layouts can interleave.
- Multipart upload is not exposed (JSON base64 matches the house API idiom);
  the practical ceiling is the 10 MiB decoded limit.
- First conversion in a fresh `doc-intel-models` volume downloads layout /
  TableFormer / RapidOCR weights once. Air-gapped deploys must pre-seed the
  volume (see the overlay header runbook) — the container needs no network at
  request time after seeding.

## Runbook

```
docker compose -f docker-compose.yml -f infra/compose/doc-intel.compose.yml up -d doc-intel
curl -s http://localhost:9080/... # via gateway; direct port is NOT published (GF4)
```

knowledge-service env: `DOC_INTEL_URL` (default `http://doc-intel:5001`),
`DOC_INTEL_API_KEY` (must match the sidecar's `DOCLING_SERVE_API_KEY` when
set), `DOC_INTEL_TIMEOUT_S` (300; keep >= the sidecar sync wait),
`DOC_INTEL_MAX_BYTES`, `DOC_INTEL_MAX_CHARS`, `DOC_INTEL_POLL_S` (2), and
`DOC_INTEL_JOB_TIMEOUT_S` (900).
