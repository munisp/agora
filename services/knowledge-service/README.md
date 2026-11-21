# knowledge-service (port 7008)

Knowledge-base ingest + hybrid retrieval for OpenDesk (SPEC §10). FastAPI + asyncpg +
sentence-transformers (`all-MiniLM-L6-v2`, 384-dim) + OpenSearch.

## What it does

- `POST /v1/documents` — ingest a document: store in Postgres (`knowledge.documents` +
  `knowledge.chunks`, SPEC §7), chunk body to ~500 tokens with overlap (word-based
  approximation, 375 words ≈ 500 tokens, 64-word overlap), embed with
  `all-MiniLM-L6-v2`, bulk-index into OpenSearch `kb-chunks` (knn_vector, HNSW/lucene,
  cosinesimil).
- `POST /v1/documents/upload` — binary file ingest (SPEC-W107): JSON base64 upload
  converted by the docling doc-intel sidecar (layout-aware Markdown, RapidOCR for
  scanned content) and fed through the same ingest pipeline. 400/403/413/415/422/503
  taxonomy, formats, limits and runbook: `docs/document-intelligence.md`.
- `DELETE /v1/documents/{id}?tenant=` — delete doc + chunks (Postgres cascade) and
  delete-by-query in OpenSearch.
- `GET /v1/search?q=&tenant=&k=` — hybrid search: BM25 (`multi_match` on content/title)
  and kNN legs are over-fetched, then fused client-side with **reciprocal rank fusion**
  (`score = Σ 1/(60 + rank)`, `app/search.py:reciprocal_rank_fusion`).
- `GET /v1/context?tenant=&q=` — top-k snippets plus a preformatted `prompt_block` for
  agent system-prompt injection (used by voice-agent-runtime).
- `GET /healthz` — Postgres + OpenSearch reachability.

`tenant` accepts a UUID or a slug; slugs are resolved server-side via Dapr service
invocation to identity-service `GET /v1/tenants/{slug}` — callers never pass tenant
IDs obtained from a model. RLS is enforced by setting `app.tenant_id` per transaction
(`SET LOCAL` via `app/db.py`).

### SPEC-W107 upload settings

| Var | Default | Description |
|---|---|---|
| DOC_INTEL_URL | http://doc-intel:5001 | docling doc-intel sidecar base URL |
| DOC_INTEL_API_KEY | (empty) | X-Api-Key for the sidecar (matches DOCLING_SERVE_API_KEY) |
| DOC_INTEL_TIMEOUT_S | 300 | conversion timeout (>= sidecar sync wait) |
| DOC_INTEL_MAX_BYTES | 10485760 | decoded upload ceiling (413) |
| DOC_INTEL_MAX_CHARS | 1000000 | extracted-text ceiling (413) |

## Environment variables

| Var | Default | Description |
|---|---|---|
| `PORT` | `7008` | HTTP listen port |
| `LOG_LEVEL` | `INFO` | structlog level |
| `DATABASE_URL` | `postgres://opendesk:opendesk@postgres:5432/knowledge` | knowledge DB DSN |
| `OPENSEARCH_URL` | `http://opensearch:9200` | OpenSearch endpoint |
| `OPENSEARCH_INDEX` | `kb-chunks` | chunk index name (SPEC §10) |
| `OPENSEARCH_USERNAME` / `OPENSEARCH_PASSWORD` | empty | basic auth (dev: security plugin off) |
| `EMBED_MODEL` | `all-MiniLM-L6-v2` | sentence-transformers model |
| `EMBED_DIM` | `384` | embedding dimension (knn_vector mapping) |
| `SENTENCE_TRANSFORMERS_HOME` | `/models/hf` | HF cache dir (pre-populated in image) |
| `CHUNK_WORDS` | `375` | words per chunk (~500 tokens) |
| `CHUNK_OVERLAP_WORDS` | `64` | overlap between chunks |
| `DEFAULT_K` / `MAX_K` | `5` / `50` | search size bounds |
| `RRF_K` | `60` | RRF constant |
| `DAPR_HOST` / `DAPR_HTTP_PORT` | `daprd-knowledge` / `3500` | Dapr sidecar |
| `IDENTITY_APP_ID` | `identity` | Dapr app-id of identity-service |
| `LLM_BASE_URL` | `http://ollama:11434/v1` | OpenAI-compatible endpoint for text-to-SQL (SPEC-W3 §3) |
| `LLM_MODEL` | `qwen3:8b` | LLM model for question→SQL translation |
| `LLM_API_KEY` | `ollama` | Bearer token for the LLM endpoint |
| `TRINO_URL` | `http://trino:8080` | Trino HTTP API for guarded SQL execution |
| `TRINO_USER` | `opendesk-analytics` | `X-Trino-User` header |

## Text-to-SQL (SPEC-W3 §3, innovation 8)

`POST /v1/analytics/query {"tenant": "<slug|uuid>", "question": "..."}` translates
the question with the configured OpenAI-compatible LLM (system prompt embeds the
gold dbt schema), hardens it with `app/sqlguard.py` (single SELECT, gold-table
allowlist, no comments/chaining/DDL/DML outside strings, server-side
`tenant_id` predicate injection, `LIMIT 500` cap) and executes it via Trino's
HTTP API (20s budget, `X-Trino-Catalog: iceberg`, `X-Trino-Schema: gold`).
Returns `{sql, columns, rows, truncated}`; any failure is a clean 400 including
the LLM's raw SQL for debugging. Tests: `pytest tests/test_sqlguard.py`.

## Model downloads

The embedding model (~90 MB) is downloaded **at image build time** by the Dockerfile
(`SentenceTransformer('all-MiniLM-L6-v2')`) into `/models/hf`. Running outside Docker,
the first embed call downloads from Hugging Face Hub into
`SENTENCE_TRANSFORMERS_HOME`.

## Run

```bash
pip install .            # or: docker build -t opendesk/knowledge-service .
uvicorn app.main:app --port 7008
```

Tests (pure logic, no infra): `pip install .[dev] && pytest tests/`

## Dapr

The service expects a sidecar (`daprd-knowledge` on the compose network) with
`--app-id knowledge`; it is only used for service invocation to identity-service.
No pub/sub usage.

## Privacy note (SPEC-W3 §2)

Privacy erasure (GdprEraseWorkflow / PrivacyEraseRequested) does NOT span
knowledge documents: this service stores no contact data (documents/chunks
are business content), so no erasure hook is registered here.
