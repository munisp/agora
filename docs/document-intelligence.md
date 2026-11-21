# Document Intelligence (SPEC-W107)

How binary documents become conversational knowledge in OpenDesk/Agora.

## Architecture

```
POST /v1/documents/upload  (knowledge-service, JSON base64)
  -> tenant binding (SPEC-W43 C1, X-Tenant-Slugs)
  -> doc-intel sidecar (docling-serve, infra/compose/doc-intel.compose.yml)
       docling layout-aware parse -> Markdown
       RapidOCR (PaddleOCR ONNX models) for scanned pages / images
       TableFormer table structure -> Markdown tables
  -> _ingest_document (UNCHANGED): chunk (~500 tok, sentence-boundary)
       -> Embedder (all-MiniLM-L6-v2, 384-dim)
       -> OpenSearch kb-chunks (BM25 + kNN, client-side RRF k=60)
```

The sidecar is the **only** extraction engine. There is no in-process
fallback parser: when the sidecar is unreachable the endpoint answers **503**
— a silent text-only degradation would be indistinguishable from a mock.

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
| Voice | voice-agent-runtime `/v1/context` + async_tools `knowledge_search` (filler ack while the slow tool runs) | WIRED |
| Chat | admin agent wizard `knowledge_packs` tags are config only; runtime grounding rides the voice-agent-runtime text path | PARTIAL |
| Channels (USSD / messaging) | conversation-service / messaging-gateway | NOT WIRED — queued follow-up wave "channel grounding" (`/v1/context` lookup for channel auto-responses) |

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

- Sync conversion only; the sidecar waits up to
  `DOCLING_SERVE_MAX_SYNC_WAIT=600`s. Very large scans should use docling's
  async endpoints — a documented follow-up, not wired.
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
`DOC_INTEL_MAX_BYTES`, `DOC_INTEL_MAX_CHARS`.
