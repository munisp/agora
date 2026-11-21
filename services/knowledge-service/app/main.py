"""knowledge-service FastAPI app (port 7008, SPEC §10).

Routes:
  POST   /v1/documents          ingest: store, chunk (~500 tokens), embed, bulk index
  POST   /v1/documents/upload   file ingest via docling doc-intel sidecar (SPEC-W107)
  GET    /v1/documents          tenant-scoped document list from Postgres (SPEC-W47)
  DELETE /v1/documents/{id}     delete doc + chunks from Postgres and OpenSearch
  GET    /v1/search             hybrid BM25 + kNN with reciprocal rank fusion
  GET    /v1/context            top snippets formatted for agent prompt injection
  GET    /v1/suggestions        self-improving KB review queue (SPEC-W3 §4)
  POST   /v1/suggestions/{id}/approve   approve -> ingests a real document
  DELETE /v1/suggestions/{id}   reject a suggestion
  GET    /healthz
"""

from __future__ import annotations

import asyncio
import base64
import binascii
import uuid
from contextlib import asynccontextmanager
from typing import Any

from fastapi import Depends, FastAPI, HTTPException, Query, Request
from fastapi.responses import JSONResponse, PlainTextResponse
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest
from pydantic import BaseModel, Field

from . import analytics
from . import suggestions as kb_suggestions
from .chunking import chunk_text
from .config import settings
from .dapr_client import DaprClient
from .db import Database, bind_tenant_value, parse_tenant_slugs, resolve_tenant_id
from .docintel import (
    DocIntelClient,
    DocIntelEmpty,
    DocIntelRejected,
    DocIntelUnavailable,
)
from .upload_jobs import UploadJobsStore
from .embeddings import Embedder
from .logging import configure_logging, get_logger
from .reranker import CrossEncoderReranker
from .rrf import reciprocal_rank_fusion
from .search import SearchStore

configure_logging(settings.log_level)
log = get_logger("knowledge-service")


class IngestRequest(BaseModel):
    tenant: str = Field(..., description="tenant slug or UUID")
    title: str
    body: str
    source_url: str | None = None


# SPEC-W107: extensions the docling doc-intel sidecar can convert. Images go
# through the OCR path (RapidOCR / PaddleOCR ONNX models).
ALLOWED_UPLOAD_EXTENSIONS = frozenset({
    "pdf", "docx", "pptx", "xlsx", "html", "htm", "md", "txt",
    "asciidoc", "adoc", "png", "jpg", "jpeg", "tiff", "tif",
})


class UploadRequest(BaseModel):
    tenant: str = Field(..., description="tenant slug or UUID")
    filename: str = Field(..., min_length=1, max_length=255)
    content_base64: str = Field(..., min_length=1)
    title: str | None = None
    # SPEC-W111: wait=false submits an async docling task and returns 202
    # with a job id (poll GET /v1/documents/upload/{job_id}); the default
    # keeps the W107 sync path byte-identical.
    wait: bool = True


class AppState:
    db: Database
    search: SearchStore
    embedder: Embedder
    dapr: DaprClient
    docintel: DocIntelClient
    upload_jobs: UploadJobsStore
    reranker: CrossEncoderReranker
    bg_tasks: set[asyncio.Task]


def get_state(request: Request) -> AppState:
    return request.app.state.deps


async def _bound_tenant_id(request: Request, explicit: str, deps: AppState) -> str:
    """SPEC-W43 C1: bind the caller's tenant selector (query param or body
    field) to the gateway-injected X-Tenant-Slugs header (exact match, else
    403; no header + no dev escape -> 401), THEN resolve slug -> UUID via
    identity-service as before."""
    value = bind_tenant_value(
        parse_tenant_slugs(request.headers.get("x-tenant-slugs")),
        explicit,
        trust_direct=settings.trust_direct_tenant,
    )
    try:
        return await resolve_tenant_id(
            deps.dapr, settings.identity_app_id, value, settings.identity_internal_token
        )
    except LookupError as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc


@asynccontextmanager
async def lifespan(app: FastAPI):
    deps = AppState()
    deps.db = Database(settings.database_url)
    deps.search = SearchStore(
        settings.opensearch_url,
        settings.opensearch_index,
        settings.embed_dim,
        settings.opensearch_username,
        settings.opensearch_password,
    )
    deps.embedder = Embedder(settings.embed_model, settings.sentence_transformers_home)
    deps.dapr = DaprClient(settings.dapr_host, settings.dapr_http_port)
    deps.docintel = DocIntelClient(
        settings.doc_intel_url,
        api_key=settings.doc_intel_api_key,
        timeout_s=settings.doc_intel_timeout_s,
    )
    deps.upload_jobs = UploadJobsStore(deps.db)
    deps.reranker = CrossEncoderReranker(
        settings.rerank_model,
        cache_dir=settings.sentence_transformers_home,
        max_chars=settings.rerank_max_chars,
    )
    deps.bg_tasks = set()
    await deps.db.connect()
    # SPEC-W3 §4 innovation 4: kb_suggestions bootstrap DDL (idempotent).
    try:
        await deps.db.ensure_suggestions_table()
    except Exception as exc:
        log.error("kb_suggestions.bootstrap_failed", error=str(exc))
    # SPEC-W111: upload_jobs bootstrap (idempotent). Failure must NOT break
    # the sync upload path — logged, and wait=false requests will 503.
    try:
        await deps.db.ensure_upload_jobs_table()
    except Exception as exc:
        log.error("upload_jobs.bootstrap_failed", error=str(exc))
    # SPEC-W112: stale-running reaper. One sweep at startup (jobs stranded
    # by a previous crash/restart), then periodically. Reaper failures are
    # logged, never fatal to startup or ingest.
    if settings.doc_intel_reaper_enabled:
        reaper_task = asyncio.create_task(_reaper_loop(deps))
        deps.bg_tasks.add(reaper_task)
        reaper_task.add_done_callback(deps.bg_tasks.discard)
    await deps.search.ensure_index()
    app.state.deps = deps
    log.info("service.started", port=settings.port)
    try:
        yield
    finally:
        log.info("service.shutting_down")
        for task in deps.bg_tasks:
            task.cancel()
        if deps.bg_tasks:
            await asyncio.gather(*deps.bg_tasks, return_exceptions=True)
        await deps.dapr.aclose()
        await deps.search.aclose()
        await deps.db.aclose()


app = FastAPI(title="opendesk-knowledge-service", version="0.1.0", lifespan=lifespan)
# SPEC-W3 §3 innovation 8: text-to-SQL analytics (POST /v1/analytics/query).
app.include_router(analytics.router)


@app.get("/healthz")
async def healthz(deps: AppState = Depends(get_state)):
    db_ok = await deps.db.ping()
    os_ok = await deps.search.ping()
    status = "ok" if (db_ok and os_ok) else "degraded"
    code = 200 if (db_ok and os_ok) else 503
    from fastapi.responses import JSONResponse

    return JSONResponse(
        {"status": status, "postgres": db_ok, "opensearch": os_ok}, status_code=code
    )


@app.get("/metrics")
async def metrics() -> PlainTextResponse:
    """W44: Prometheus scrape endpoint (default registry: process + platform
    collectors) on the existing app port — closes the F15-02 scrape gap."""
    return PlainTextResponse(
        generate_latest().decode("utf-8"), media_type=CONTENT_TYPE_LATEST
    )


async def _ingest_document(
    deps: AppState, tenant_id: str, title: str, body: str, source_url: str | None
) -> dict[str, Any]:
    """Shared ingest pipeline (POST /v1/documents and suggestion approval)."""
    chunks = chunk_text(
        body,
        chunk_words=settings.chunk_words,
        overlap_words=settings.chunk_overlap_words,
    )
    doc_id = uuid.uuid4()
    chunk_ids = [uuid.uuid4() for _ in chunks]

    async with deps.db.tenant_conn(tenant_id) as conn:
        await conn.execute(
            "INSERT INTO documents (id, tenant_id, title, body, source_url)"
            " VALUES ($1, $2, $3, $4, $5)",
            doc_id, uuid.UUID(tenant_id), title, body, source_url,
        )
        for cid, seq, content in zip(chunk_ids, range(len(chunks)), chunks):
            await conn.execute(
                "INSERT INTO chunks (id, document_id, seq, content) VALUES ($1, $2, $3, $4)",
                cid, doc_id, seq, content,
            )

    vectors = await deps.embedder.embed(chunks) if chunks else []
    os_docs = [
        {
            "tenant_id": tenant_id,
            "document_id": str(doc_id),
            "chunk_id": str(cid),
            "seq": seq,
            "title": title,
            "content": content,
            "embedding": vector,
        }
        for cid, seq, content, vector in zip(chunk_ids, range(len(chunks)), chunks, vectors)
    ]
    indexed = await deps.search.bulk_index_chunks(os_docs)
    log.info(
        "document.ingested",
        document_id=str(doc_id), tenant_id=tenant_id,
        chunks=len(chunks), indexed=indexed,
    )
    return {
        "id": str(doc_id),
        "tenant_id": tenant_id,
        "title": title,
        "chunk_count": len(chunks),
        "indexed": indexed,
    }


@app.post("/v1/documents", status_code=201)
async def ingest_document(
    req: IngestRequest, request: Request, deps: AppState = Depends(get_state)
):
    tenant_id = await _bound_tenant_id(request, req.tenant, deps)
    return await _ingest_document(deps, tenant_id, req.title, req.body, req.source_url)


@app.post("/v1/documents/upload", status_code=201)
async def upload_document(
    req: UploadRequest, request: Request, deps: AppState = Depends(get_state)
):
    """SPEC-W107: binary document ingest via the docling doc-intel sidecar.

    The sidecar (docling layout-aware parsing; RapidOCR/PaddleOCR models for
    scanned content; tables -> Markdown) is the ONLY extraction engine. There
    is no silent local fallback: sidecar down -> 503, conversion refused or
    empty -> 422, oversized -> 413, unsupported type -> 415.
    """
    ext = req.filename.rsplit(".", 1)[-1].lower() if "." in req.filename else ""
    if ext not in ALLOWED_UPLOAD_EXTENSIONS:
        raise HTTPException(
            status_code=415,
            detail=(
                f"unsupported file type '.{ext or '(none)'}'; allowed: "
                f"{sorted(ALLOWED_UPLOAD_EXTENSIONS)}"
            ),
        )
    try:
        data = base64.b64decode(req.content_base64, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise HTTPException(status_code=400, detail=f"invalid base64: {exc}") from exc
    if len(data) > settings.doc_intel_max_bytes:
        raise HTTPException(
            status_code=413,
            detail=(
                f"decoded upload is {len(data)} bytes; limit is "
                f"{settings.doc_intel_max_bytes}"
            ),
        )
    tenant_id = await _bound_tenant_id(request, req.tenant, deps)
    title = req.title or req.filename.rsplit(".", 1)[0]
    if not req.wait:
        # SPEC-W111: async ingest for very large scans — submit a docling
        # task and let a background job poll/ingest; the caller polls the
        # job row. 202 + job id; the job NEVER propagates failure to other
        # requests (row marked failed with the honest error).
        job_id = await deps.upload_jobs.create(tenant_id, req.filename, title)
        task = asyncio.create_task(
            _run_upload_job(
                deps, tenant_id, job_id, req.filename, data, title
            )
        )
        deps.bg_tasks.add(task)
        task.add_done_callback(deps.bg_tasks.discard)
        return JSONResponse(
            status_code=202,
            content={
                "job_id": job_id,
                "status": "pending",
                "tenant_id": tenant_id,
                "title": title,
            },
        )
    try:
        extraction = await deps.docintel.convert(req.filename, data)
    except DocIntelUnavailable as exc:
        raise HTTPException(status_code=503, detail=str(exc)) from exc
    except (DocIntelRejected, DocIntelEmpty) as exc:
        raise HTTPException(status_code=422, detail=str(exc)) from exc
    if len(extraction.text) > settings.doc_intel_max_chars:
        raise HTTPException(
            status_code=413,
            detail=(
                f"extracted text is {len(extraction.text)} chars; limit is "
                f"{settings.doc_intel_max_chars}"
            ),
        )
    result = await _ingest_document(
        deps, tenant_id, title, extraction.text, f"upload:{req.filename}"
    )
    result["extraction"] = {
        "engine": "docling",
        "processing_time_s": extraction.processing_time_s,
        "chars": len(extraction.text),
    }
    return result


async def _run_upload_job(
    deps: AppState,
    tenant_id: str,
    job_id: str,
    filename: str,
    data: bytes,
    title: str,
) -> None:
    """SPEC-W111 background job: submit -> poll -> ingest -> mark done.

    Every failure marks the job row failed with the honest error; nothing
    propagates. CancelledError (shutdown) propagates so the lifespan gather
    is prompt — the row stays 'running' and the SPEC-W112 reaper fails it
    honestly once it goes stale.
    """
    try:
        await deps.upload_jobs.set_running(tenant_id, job_id)
        task_id = await deps.docintel.submit(filename, data)
        deadline = asyncio.get_running_loop().time() + settings.doc_intel_job_timeout_s
        status = "pending"
        while status not in ("success", "failure"):
            if asyncio.get_running_loop().time() > deadline:
                raise DocIntelUnavailable(
                    f"doc-intel job timed out after {settings.doc_intel_job_timeout_s}s"
                )
            await asyncio.sleep(settings.doc_intel_poll_s)
            status = await deps.docintel.poll(task_id)
        if status == "failure":
            raise DocIntelRejected("doc-intel task reported failure")
        extraction = await deps.docintel.result(task_id)
        if len(extraction.text) > settings.doc_intel_max_chars:
            raise DocIntelRejected(
                f"extracted text is {len(extraction.text)} chars; limit is "
                f"{settings.doc_intel_max_chars}"
            )
        result = await _ingest_document(
            deps, tenant_id, title, extraction.text, f"upload:{filename}"
        )
        await deps.upload_jobs.set_done(
            tenant_id, job_id, result["id"], len(extraction.text)
        )
        log.info("upload_job.done", job_id=job_id, document_id=result["id"])
    except asyncio.CancelledError:
        raise
    except Exception as exc:  # noqa: BLE001 - job failure is row state
        log.warning("upload_job.failed", job_id=job_id, error=str(exc)[:200])
        try:
            await deps.upload_jobs.set_failed(tenant_id, job_id, str(exc))
        except Exception as exc2:  # noqa: BLE001 - never raise from a job
            log.error("upload_job.fail_mark_failed", job_id=job_id, error=str(exc2))


async def _reaper_loop(deps: AppState) -> None:
    """SPEC-W112: fail stale `running` upload jobs honestly.

    One sweep immediately (covers jobs stranded by a previous crash or
    restart — the background task is gone but the row still says running),
    then one every settings.doc_intel_reaper_interval_s. Only stale running
    rows are touched; pending and terminal rows are never modified. A sweep
    failure is logged and retried next interval — the loop only exits on
    cancellation (shutdown).
    """
    while True:
        try:
            reaped = await deps.upload_jobs.reap_stale(
                settings.doc_intel_reaper_stale_s
            )
            if reaped:
                log.warning("upload_reaper.reaped", count=reaped)
        except asyncio.CancelledError:
            raise
        except Exception as exc:  # noqa: BLE001 - reaper never dies on error
            log.error("upload_reaper.sweep_failed", error=str(exc)[:200])
        await asyncio.sleep(settings.doc_intel_reaper_interval_s)


@app.get("/v1/documents/upload/{job_id}")
async def get_upload_job(
    job_id: str,
    request: Request,
    tenant: str = Query(..., description="tenant slug or UUID"),
    deps: AppState = Depends(get_state),
):
    """SPEC-W111: tenant-bound async upload job status (404 cross-tenant)."""
    tenant_id = await _bound_tenant_id(request, tenant, deps)
    try:
        job = await deps.upload_jobs.get(tenant_id, job_id)
    except ValueError:
        raise HTTPException(status_code=404, detail="upload job not found") from None
    if job is None:
        raise HTTPException(status_code=404, detail="upload job not found")
    return {
        "job_id": job.id,
        "tenant_id": job.tenant_id,
        "filename": job.filename,
        "title": job.title,
        "status": job.status,
        "document_id": job.document_id,
        "chars": job.chars,
        "error": job.error,
        "created_at": job.created_at.isoformat(),
        "updated_at": job.updated_at.isoformat(),
    }


@app.get("/v1/documents")
async def list_documents(
    request: Request,
    tenant: str = Query(..., description="tenant slug or UUID"),
    limit: int = Query(50, ge=1, le=200, description="page size (default 50)"),
    offset: int = Query(0, ge=0, description="page offset"),
    deps: AppState = Depends(get_state),
):
    """SPEC-W47 (UIWEB-04 + DB-08): tenant-scoped document list read from
    POSTGRES (the source of truth for stored documents — the admin Knowledge
    page lists what the tenant actually ingested).

    DRIFT NOTE: this reflects the PG `documents`/`chunks` tables only. The
    OpenSearch `kb-chunks` index is written on ingest and removed on delete,
    but there is NO reconciliation job — PG and OpenSearch can drift silently
    (e.g. a failed bulk index after a committed insert). /v1/search answers
    from OpenSearch, so search results may lag or diverge from this list;
    `chunk_count` here is the PG truth. OpenSearch remains the k-NN/BM25
    query engine (unchanged).

    Reads run under tenant_conn (RLS `app.tenant_id` context) AND an explicit
    tenant_id predicate — belt and braces; cross-tenant enumeration is
    impossible even if the RLS policy is ever relaxed.
    """
    tenant_id = await _bound_tenant_id(request, tenant, deps)
    tenant_uuid = uuid.UUID(tenant_id)
    async with deps.db.tenant_conn(tenant_id) as conn:
        rows = await conn.fetch(
            "SELECT d.id, d.title, d.body, d.source_url, d.created_at,"
            " (SELECT COUNT(*) FROM chunks c WHERE c.document_id = d.id)"
            "   AS chunk_count"
            " FROM documents d"
            " WHERE d.tenant_id = $1"
            " ORDER BY d.created_at DESC, d.id"
            " LIMIT $2 OFFSET $3",
            tenant_uuid, limit, offset,
        )
        total = await conn.fetchval(
            "SELECT COUNT(*) FROM documents WHERE tenant_id = $1", tenant_uuid
        )
    items = [
        {
            "id": str(r["id"]),
            "tenant_id": tenant_id,
            "title": r["title"],
            "body": r["body"],
            "source_url": r["source_url"],
            "created_at": r["created_at"].isoformat(),
            "chunk_count": int(r["chunk_count"]),
        }
        for r in rows
    ]
    return {
        "tenant_id": tenant_id,
        "items": items,
        "limit": limit,
        "offset": offset,
        "count": len(items),
        "total": int(total or 0),
    }


@app.delete("/v1/documents/{document_id}", status_code=204)
async def delete_document(
    document_id: str,
    request: Request,
    tenant: str = Query(..., description="tenant slug or UUID"),
    deps: AppState = Depends(get_state),
):
    tenant_id = await _bound_tenant_id(request, tenant, deps)
    try:
        doc_uuid = uuid.UUID(document_id)
    except ValueError:
        raise HTTPException(status_code=400, detail="invalid document id")

    async with deps.db.tenant_conn(tenant_id) as conn:
        deleted = await conn.execute(
            "DELETE FROM documents WHERE id = $1", doc_uuid
        )  # chunks cascade (SPEC §7 FK)
    if deleted == "DELETE 0":
        raise HTTPException(status_code=404, detail="document not found")
    await deps.search.delete_document(tenant_id, document_id)
    log.info("document.deleted", document_id=document_id, tenant_id=tenant_id)


async def _hybrid(
    deps: AppState, tenant_id: str, q: str, k: int
) -> tuple[list[dict[str, Any]], bool]:
    fetch_k = max(k * 2, 10)  # over-fetch each leg before fusion
    bm25_hits = await deps.search.bm25_search(tenant_id, q, fetch_k)
    [vector] = await deps.embedder.embed([q])
    knn_hits = await deps.search.knn_search(tenant_id, vector, fetch_k)
    # SPEC-W113: over-fuse to the rerank candidate pool so the cross-encoder
    # re-scores a real pool, not just the final k. Disabled config keeps the
    # W112 behavior byte-identical (fuse straight to k, reranker untouched).
    fused = reciprocal_rank_fusion(
        [bm25_hits, knn_hits],
        rrf_k=settings.rrf_k,
        size=max(k, settings.rerank_candidates) if settings.rerank_enabled else k,
    )
    if not settings.rerank_enabled:
        return fused, False
    candidates = fused[: settings.rerank_candidates]
    try:
        reranked = await asyncio.wait_for(
            deps.reranker.rerank(q, candidates),
            timeout=settings.rerank_timeout_s,
        )
    except Exception as exc:  # noqa: BLE001 - reranking is best-effort
        # Timeout, model-load failure, OOM — anything: keep the RRF order
        # and log. Search NEVER fails because of reranking.
        log.warning("rerank.failed_rrf_fallback", error=str(exc)[:200])
        return fused[:k], False
    return reranked[:k], True


@app.get("/v1/search")
async def search(
    q: str = Query(..., min_length=1),
    tenant: str = Query(..., description="tenant slug or UUID"),
    k: int = Query(settings.default_k, ge=1, le=settings.max_k),
    request: Request = None,
    deps: AppState = Depends(get_state),
):
    tenant_id = await _bound_tenant_id(request, tenant, deps)
    hits, reranked = await _hybrid(deps, tenant_id, q, k)

    # Self-improving KB (SPEC-W3 §4, innovation 4): weak answer to a
    # question-shaped query -> record a suggestion for staff review.
    top_score = hits[0]["_rrf_score"] if hits else None
    if kb_suggestions.should_suggest(top_score, q, settings.suggest_threshold):
        try:
            async with deps.db.tenant_conn(tenant_id) as conn:
                row = await kb_suggestions.record_suggestion(
                    conn, uuid.UUID(tenant_id), q, top_score
                )
            if row:
                log.info("kb_suggestion.recorded", tenant_id=tenant_id,
                         suggestion_id=str(row["id"]), top_score=top_score)
        except Exception as exc:  # best-effort; never fail the search
            log.warning("kb_suggestion.record_failed", error=str(exc))

    return {
        "query": q,
        "tenant_id": tenant_id,
        "reranked": reranked,
        "results": [
            {
                "chunk_id": h["_id"],
                "document_id": h["_source"]["document_id"],
                "title": h["_source"].get("title"),
                "content": h["_source"]["content"],
                "rrf_score": h["_rrf_score"],
                # SPEC-W113: present only when the hit was cross-encoder
                # reranked; consumers must tolerate its absence.
                **({"rerank_score": h["_rerank_score"]} if "_rerank_score" in h else {}),
            }
            for h in hits
        ],
    }


@app.get("/v1/context")
async def context(
    q: str = Query(..., min_length=1),
    tenant: str = Query(..., description="tenant slug or UUID"),
    k: int = Query(settings.default_k, ge=1, le=20),
    request: Request = None,
    deps: AppState = Depends(get_state),
):
    """Top snippets formatted for injection into an agent system prompt."""
    tenant_id = await _bound_tenant_id(request, tenant, deps)
    hits, _reranked = await _hybrid(deps, tenant_id, q, k)
    snippets = [h["_source"]["content"] for h in hits]
    prompt_block = "\n".join(f"- {s}" for s in snippets)
    return {
        "query": q,
        "tenant_id": tenant_id,
        "snippets": snippets,
        "prompt_block": f"Relevant knowledge base facts:\n{prompt_block}" if snippets else "",
    }


# ---------------------------------------------------------------------------
# Self-improving KB (SPEC-W3 §4, innovation 4): suggestion review queue
# ---------------------------------------------------------------------------
class ApproveRequest(BaseModel):
    title: str | None = None  # defaults to the suggested question
    body: str = Field(min_length=1)


@app.get("/v1/suggestions")
async def list_kb_suggestions(
    request: Request,
    tenant: str = Query(..., description="tenant slug or UUID"),
    status: str | None = Query(default=None, description="pending|approved|rejected"),
    deps: AppState = Depends(get_state),
):
    tenant_id = await _bound_tenant_id(request, tenant, deps)
    if status is not None and status not in ("pending", "approved", "rejected"):
        raise HTTPException(status_code=400, detail="invalid status filter")
    async with deps.db.tenant_conn(tenant_id) as conn:
        rows = await kb_suggestions.list_suggestions(conn, uuid.UUID(tenant_id), status)
    return {
        "tenant_id": tenant_id,
        "suggestions": [
            {**r, "id": str(r["id"]), "tenant_id": str(r["tenant_id"]),
             "created_at": r["created_at"].isoformat()}
            for r in rows
        ],
    }


@app.post("/v1/suggestions/{suggestion_id}/approve")
async def approve_kb_suggestion(
    suggestion_id: str,
    req: ApproveRequest,
    request: Request,
    tenant: str = Query(..., description="tenant slug or UUID"),
    deps: AppState = Depends(get_state),
):
    """Approve: ingest the answer as a real document, then mark approved."""
    tenant_id = await _bound_tenant_id(request, tenant, deps)
    try:
        sid = uuid.UUID(suggestion_id)
    except ValueError:
        raise HTTPException(status_code=400, detail="invalid suggestion id")

    async with deps.db.tenant_conn(tenant_id) as conn:
        suggestion = await kb_suggestions.get_suggestion(conn, sid)
    if suggestion is None:
        raise HTTPException(status_code=404, detail="suggestion not found")
    if suggestion["status"] != "pending":
        raise HTTPException(
            status_code=409, detail=f"suggestion already {suggestion['status']}"
        )

    doc = await _ingest_document(
        deps,
        tenant_id,
        req.title or suggestion["question"],
        req.body,
        source_url=f"kb-suggestion:{suggestion_id}",
    )
    async with deps.db.tenant_conn(tenant_id) as conn:
        await kb_suggestions.set_status(conn, sid, "approved")
    log.info("kb_suggestion.approved", suggestion_id=suggestion_id,
             document_id=doc["id"])
    return {"status": "approved", "suggestion_id": suggestion_id, "document": doc}


@app.delete("/v1/suggestions/{suggestion_id}")
async def reject_kb_suggestion(
    suggestion_id: str,
    request: Request,
    tenant: str = Query(..., description="tenant slug or UUID"),
    deps: AppState = Depends(get_state),
):
    """Reject: mark the suggestion rejected (kept for audit)."""
    tenant_id = await _bound_tenant_id(request, tenant, deps)
    try:
        sid = uuid.UUID(suggestion_id)
    except ValueError:
        raise HTTPException(status_code=400, detail="invalid suggestion id")
    async with deps.db.tenant_conn(tenant_id) as conn:
        updated = await kb_suggestions.set_status(conn, sid, "rejected")
    if not updated:
        raise HTTPException(status_code=404, detail="suggestion not found")
    log.info("kb_suggestion.rejected", suggestion_id=suggestion_id)
    return {"status": "rejected", "suggestion_id": suggestion_id}
