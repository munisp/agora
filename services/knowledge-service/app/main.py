"""knowledge-service FastAPI app (port 7008, SPEC §10).

Routes:
  POST   /v1/documents          ingest: store, chunk (~500 tokens), embed, bulk index
  DELETE /v1/documents/{id}     delete doc + chunks from Postgres and OpenSearch
  GET    /v1/search             hybrid BM25 + kNN with reciprocal rank fusion
  GET    /v1/context            top snippets formatted for agent prompt injection
  GET    /v1/suggestions        self-improving KB review queue (SPEC-W3 §4)
  POST   /v1/suggestions/{id}/approve   approve -> ingests a real document
  DELETE /v1/suggestions/{id}   reject a suggestion
  GET    /healthz
"""

from __future__ import annotations

import uuid
from contextlib import asynccontextmanager
from typing import Any

from fastapi import Depends, FastAPI, HTTPException, Query, Request
from fastapi.responses import PlainTextResponse
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest
from pydantic import BaseModel, Field

from . import analytics
from .chunking import chunk_text
from .config import settings
from .dapr_client import DaprClient
from .db import Database, bind_tenant_value, parse_tenant_slugs, resolve_tenant_id
from .embeddings import Embedder
from .logging import configure_logging, get_logger
from .rrf import reciprocal_rank_fusion
from .search import SearchStore

configure_logging(settings.log_level)
log = get_logger("knowledge-service")


class IngestRequest(BaseModel):
    tenant: str = Field(..., description="tenant slug or UUID")
    title: str
    body: str
    source_url: str | None = None


class AppState:
    db: Database
    search: SearchStore
    embedder: Embedder
    dapr: DaprClient


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
    await deps.db.connect()
    # SPEC-W3 §4 innovation 4: kb_suggestions bootstrap DDL (idempotent).
    try:
        await deps.db.ensure_suggestions_table()
    except Exception as exc:
        log.error("kb_suggestions.bootstrap_failed", error=str(exc))
    await deps.search.ensure_index()
    app.state.deps = deps
    log.info("service.started", port=settings.port)
    try:
        yield
    finally:
        log.info("service.shutting_down")
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
) -> list[dict[str, Any]]:
    fetch_k = max(k * 2, 10)  # over-fetch each leg before fusion
    bm25_hits = await deps.search.bm25_search(tenant_id, q, fetch_k)
    [vector] = await deps.embedder.embed([q])
    knn_hits = await deps.search.knn_search(tenant_id, vector, fetch_k)
    return reciprocal_rank_fusion(
        [bm25_hits, knn_hits], rrf_k=settings.rrf_k, size=k
    )


@app.get("/v1/search")
async def search(
    q: str = Query(..., min_length=1),
    tenant: str = Query(..., description="tenant slug or UUID"),
    k: int = Query(settings.default_k, ge=1, le=settings.max_k),
    request: Request = None,
    deps: AppState = Depends(get_state),
):
    tenant_id = await _bound_tenant_id(request, tenant, deps)
    hits = await _hybrid(deps, tenant_id, q, k)

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
        "results": [
            {
                "chunk_id": h["_id"],
                "document_id": h["_source"]["document_id"],
                "title": h["_source"].get("title"),
                "content": h["_source"]["content"],
                "rrf_score": h["_rrf_score"],
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
    hits = await _hybrid(deps, tenant_id, q, k)
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
