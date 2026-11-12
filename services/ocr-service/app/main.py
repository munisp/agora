"""ocr-service FastAPI app (port 7020, SPEC-W53 Scope A).

Routes:
  POST /v1/ocr/extract           multipart file OR {"document_url"} + kind
                                 (receipt|document|knowledge_scan) -> MinIO
                                 `documents` bucket (tenant-prefixed keys),
                                 llama-ocr sidecar extraction, ocr_documents
                                 row (uploaded->extracted|failed), ocr_page
                                 UsageRecord + DocumentExtracted CloudEvent.
  GET  /v1/ocr/documents/{id}    metadata + extracted fields (NO bytes).
  GET  /healthz
  GET  /metrics

Auth: two caller shapes —
  * service-to-service: X-Internal-Token (constant-time compare, FAIL-CLOSED
    503 when unset; APISIX strips client-supplied x-internal-token, so a
    gateway call can never forge this),
  * gateway (APISIX api-ocr route): SPEC-W43 C1 tenant binding via the
    X-Tenant-Slugs header (+ identity-service slug resolution).

Tenant resolution precedence (W53-D integration fix — the booking-service
fieldcapture client sends the tenant uuid BOTH ways, geo-enrich precedent):
  1. X-Tenant-Slug header (singular),
  2. tenant_id multipart form field / JSON key (legacy `tenant` accepted),
  3. gateway C1 claims (X-Tenant-Slugs binding) for browser calls.
Header and form field must AGREE when both are present (400 on conflict);
an authenticated internal call with no resolvable tenant is a 400, never a
silent global/no-tenant bucket prefix.

OCR_MOCK=true selects a deterministic fixture extraction (fixed
vendor/amount/date), CRITICAL-logged at startup, NEVER the default.
"""

from __future__ import annotations

import hmac
import uuid
from contextlib import asynccontextmanager
from typing import Any

import httpx
from fastapi import Depends, FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse, PlainTextResponse
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest

from . import events
from .config import ALLOWED_MIMES, DOCUMENT_KINDS, settings
from .dapr_client import DaprClient
from .llm import OcrClient, OcrUpstreamError, mock_extraction
from .logging import configure_logging, get_logger
from .storage import DocumentStore, StorageError
from .store import Database
from .tenant import bind_request_tenant, resolve_tenant_id
from .validate import (
    ValidationError,
    perceptual_hash,
    pdf_page_count,
    sha256_hex,
    validate_upload,
)

configure_logging(settings.log_level)
log = get_logger("ocr-service")

# Response schema shared with booking-service (A<->B) and admin-web (A<->C):
# POST /v1/ocr/extract -> {document_id, status, extracted_fields, page_count,
#                          extracted_text}
# (extracted_text is the W53-D additive key C's knowledge flow consumes;
# docs/api/openapi/ocr.yaml is the normative contract).


class AppState:
    db: Database
    documents: DocumentStore
    ocr: OcrClient
    dapr: DaprClient


def get_state(request: Request) -> AppState:
    return request.app.state.deps


@asynccontextmanager
async def lifespan(app: FastAPI):
    if settings.ocr_mock:
        # XTTS_MOCK precedent (RECON §4): loud, unmissable startup signal.
        log.critical(
            "ocr_mock.enabled",
            msg="OCR_MOCK=true — deterministic fixture extraction, NO real "
            "model calls. DEV/E2E ONLY. NEVER set in production.",
        )
    deps = AppState()
    deps.db = Database(settings.database_url)
    deps.documents = DocumentStore(
        endpoint=settings.minio_endpoint,
        access_key=settings.minio_access_key,
        secret_key=settings.minio_secret_key,
        bucket=settings.minio_bucket,
        secure=settings.minio_secure,
    )
    deps.ocr = OcrClient(
        base_url=settings.llm_base_url,
        model=settings.ocr_model,
        timeout_s=settings.ocr_timeout_s,
    )
    deps.dapr = DaprClient(settings.dapr_host, settings.dapr_http_port)
    await deps.db.connect()
    try:
        await deps.db.ensure_ocr_documents_table()
    except Exception as exc:
        log.error("ocr_documents.bootstrap_failed", error=str(exc))
    app.state.deps = deps
    log.info(
        "service.started",
        port=settings.port,
        mock=settings.ocr_mock,
        llm_base_url=settings.llm_base_url,
        bucket=settings.minio_bucket,
    )
    try:
        yield
    finally:
        log.info("service.shutting_down")
        await deps.dapr.aclose()
        await deps.ocr.aclose()
        await deps.db.aclose()


app = FastAPI(title="opendesk-ocr-service", version="0.1.0", lifespan=lifespan)


@app.get("/healthz")
async def healthz(deps: AppState = Depends(get_state)):
    db_ok = await deps.db.ping()
    status = "ok" if db_ok else "degraded"
    return JSONResponse(
        {
            "status": status,
            "postgres": db_ok,
            "mock": settings.ocr_mock,
        },
        status_code=200 if db_ok else 503,
    )


@app.get("/metrics")
async def metrics() -> PlainTextResponse:
    return PlainTextResponse(
        generate_latest().decode("utf-8"), media_type=CONTENT_TYPE_LATEST
    )


# ---------------------------------------------------------------------------
# auth / tenancy
# ---------------------------------------------------------------------------
async def _authenticate(request: Request, explicit_tenant: str | None, deps: AppState) -> str:
    """Return the bound tenant UUID for either caller shape (module docstring).

    Precedence (module docstring): X-Tenant-Slug header, then the explicit
    tenant_id/tenant selector. Both present + disagreeing is a 400 — an
    ambiguous tenant must never silently pick one (PII bucket prefix).
    """
    header_tenant = _as_str(request.headers.get("x-tenant-slug"))
    if header_tenant and explicit_tenant and header_tenant != explicit_tenant:
        raise HTTPException(
            status_code=400,
            detail="conflicting tenant selectors: X-Tenant-Slug header and "
            "tenant_id form field must agree",
        )
    explicit_tenant = header_tenant or explicit_tenant
    internal = request.headers.get("x-internal-token")
    if internal is not None:
        # Service-to-service caller (booking-service fieldcapture, future
        # kyc-service). SPEC-W44 K2: constant-time compare; FAIL-CLOSED when
        # the token is not configured (503, kyc/model-registry idiom).
        if not settings.internal_token:
            raise HTTPException(
                status_code=503,
                detail="internal auth not configured (OCR_INTERNAL_TOKEN unset)",
            )
        if not hmac.compare_digest(internal, settings.internal_token):
            raise HTTPException(status_code=401, detail="invalid internal token")
        if not explicit_tenant:
            raise HTTPException(status_code=400, detail="tenant required")
        tenant = explicit_tenant
    else:
        tenant = bind_request_tenant(request, explicit_tenant)
    try:
        return await resolve_tenant_id(
            deps.dapr, settings.identity_app_id, tenant, settings.identity_internal_token
        )
    except LookupError as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc


# ---------------------------------------------------------------------------
# upload acquisition
# ---------------------------------------------------------------------------
async def _read_upload(request: Request) -> tuple[str, bytes, str | None, str | None]:
    """Extract (filename, data, kind, tenant) from multipart OR JSON body.

    The tenant selector is `tenant_id` (A<->B contract; the booking-service
    fieldcapture client sends it as a form field alongside X-Tenant-Slug);
    the legacy `tenant` key stays accepted. Enforces the 25 MB hard cap
    while reading.
    """
    ctype = request.headers.get("content-type", "")
    if ctype.startswith("multipart/form-data"):
        form = await request.form()
        upload = form.get("file")
        kind = form.get("kind")
        tenant = _as_str(form.get("tenant_id")) or _as_str(form.get("tenant"))
        if upload is None or not getattr(upload, "filename", None):
            raise HTTPException(status_code=400, detail="multipart field 'file' required")
        data = await upload.read()
        return upload.filename, data, _as_str(kind), tenant
    try:
        body = await request.json()
    except Exception:
        raise HTTPException(
            status_code=400,
            detail="expected multipart/form-data file upload or JSON {document_url}",
        )
    if not isinstance(body, dict):
        raise HTTPException(status_code=400, detail="invalid JSON body")
    url = body.get("document_url")
    kind = body.get("kind")
    tenant = _as_str(body.get("tenant_id")) or _as_str(body.get("tenant"))
    if not url or not isinstance(url, str):
        raise HTTPException(status_code=400, detail="document_url required")
    filename, data = await _fetch_document_url(url)
    return filename, data, _as_str(kind), tenant


def _as_str(value: Any) -> str | None:
    return value if isinstance(value, str) and value else None


async def _fetch_document_url(url: str) -> tuple[str, bytes]:
    """Bounded fetch of {document_url} (http/https only, hard cap enforced
    mid-stream; 25s total budget so a slow origin cannot pin a worker)."""
    if not url.lower().startswith(("http://", "https://")):
        raise HTTPException(status_code=400, detail="document_url must be http(s)")
    name = url.rstrip("/").rsplit("/", 1)[-1].split("?", 1)[0] or "document"
    try:
        async with httpx.AsyncClient(timeout=25.0, follow_redirects=False) as client:
            async with client.stream("GET", url) as resp:
                if resp.status_code != 200:
                    raise HTTPException(
                        status_code=400,
                        detail=f"document_url fetch failed: HTTP {resp.status_code}",
                    )
                buf = bytearray()
                async for chunk in resp.aiter_bytes(65536):
                    buf += chunk
                    if len(buf) > settings.ocr_max_bytes:
                        raise ValidationError(
                            f"upload exceeds hard cap of {settings.ocr_max_bytes} bytes"
                        )
    except httpx.HTTPError as exc:
        raise HTTPException(status_code=400, detail=f"document_url fetch failed: {exc}")
    return name, bytes(buf)


# ---------------------------------------------------------------------------
# extraction pipeline
# ---------------------------------------------------------------------------
async def _emit_events(
    deps: AppState,
    *,
    tenant_id: str,
    document_id: uuid.UUID,
    kind: str,
    status: str,
    page_count: int,
    sha256: str,
    phash: str | None,
    model_version: str,
    storage_key: str,
    extracted_fields: dict[str, Any],
) -> None:
    """Best-effort Dapr publishes (kyc idiom: the durable DB row is the
    reconciliation source — a publish failure never fails the request)."""
    usage = events.usage_record_event(
        tenant_id=tenant_id,
        document_id=document_id,
        page_count=page_count,
        model_version=model_version,
        kind=kind,
    )
    doc_event = events.document_extracted_event(
        tenant_id=tenant_id,
        document_id=document_id,
        kind=kind,
        status=status,
        page_count=page_count,
        sha256=sha256,
        phash=phash,
        model_version=model_version,
        storage_bucket=settings.minio_bucket,
        storage_key=storage_key,
        extracted_fields=extracted_fields,
    )
    try:
        await deps.dapr.publish(settings.dapr_pubsub_name, settings.usage_topic, usage)
        await deps.dapr.publish(
            settings.dapr_pubsub_name, settings.ocr_events_topic, doc_event
        )
    except Exception as exc:
        log.warning(
            "events.publish_failed",
            document_id=str(document_id),
            error=str(exc),
        )


async def _run_extraction(
    deps: AppState,
    *,
    tenant_id: str,
    kind: str,
    filename: str,
    data: bytes,
) -> dict[str, Any]:
    ext, mime = validate_upload(filename, data, max_bytes=settings.ocr_max_bytes)
    digest = sha256_hex(data)
    phash = perceptual_hash(data, mime)
    document_id = uuid.uuid4()
    storage_key = f"{tenant_id}/{document_id}{ext}"

    # 1) PII bytes -> MinIO `documents` (tenant-prefixed key).
    await deps.documents.put(storage_key, data, mime)

    # 2) metadata row, status=uploaded (durable reconciliation source).
    page_count_hint = pdf_page_count(data) if mime == "application/pdf" else 1
    async with deps.db.tenant_conn(tenant_id) as conn:
        await deps.db.insert_document(
            conn,
            {
                "id": document_id,
                "tenant_id": tenant_id,
                "kind": kind,
                "storage_bucket": settings.minio_bucket,
                "storage_key": storage_key,
                "mime": mime,
                "bytes": len(data),
                "sha256": digest,
                "phash": phash,
                "page_count": page_count_hint,
            },
        )

    # 3) extraction — mock fixture or the real llama-ocr sidecar.
    try:
        if settings.ocr_mock:
            extraction = mock_extraction(kind, data, mime, settings.ocr_model)
        else:
            extraction = await deps.ocr.extract(
                kind=kind, data=data, mime=mime, max_pages=settings.ocr_max_pages
            )
    except OcrUpstreamError as exc:
        async with deps.db.tenant_conn(tenant_id) as conn:
            await deps.db.mark_failed(conn, document_id, error=str(exc))
        log.error(
            "ocr.extraction_failed",
            document_id=str(document_id),
            tenant_id=tenant_id,
            error=str(exc),
        )
        raise HTTPException(
            status_code=502,
            detail={
                "error": "ocr_upstream_failed",
                "document_id": str(document_id),
                "status": "failed",
            },
        )

    # 4) persist extraction + 5) meter/emit.
    async with deps.db.tenant_conn(tenant_id) as conn:
        await deps.db.mark_extracted(
            conn,
            document_id,
            page_count=extraction.page_count,
            model_version=extraction.model_version,
            extracted_text=extraction.text,
            extracted_fields=extraction.fields,
        )
    log.info(
        "ocr.extracted",
        document_id=str(document_id),
        tenant_id=tenant_id,
        kind=kind,
        pages=extraction.page_count,
        model=extraction.model_version,
    )
    await _emit_events(
        deps,
        tenant_id=tenant_id,
        document_id=document_id,
        kind=kind,
        status="extracted",
        page_count=extraction.page_count,
        sha256=digest,
        phash=phash,
        model_version=extraction.model_version,
        storage_key=storage_key,
        extracted_fields=extraction.fields,
    )
    return {
        "document_id": str(document_id),
        "status": "extracted",
        "extracted_fields": extraction.fields,
        "page_count": extraction.page_count,
        # W53-D additive key: the full markdown/text body (C's knowledge
        # flow POSTs it to knowledge-service /v1/documents).
        "extracted_text": extraction.text,
    }


@app.post("/v1/ocr/extract", status_code=200)
async def extract_document(request: Request, deps: AppState = Depends(get_state)):
    try:
        filename, data, kind, tenant = await _read_upload(request)
        if kind not in DOCUMENT_KINDS:
            raise HTTPException(
                status_code=400,
                detail=f"kind must be one of {list(DOCUMENT_KINDS)}",
            )
        tenant_id = await _authenticate(request, tenant, deps)
        return await _run_extraction(
            deps, tenant_id=tenant_id, kind=kind, filename=filename, data=data
        )
    except ValidationError as exc:
        msg = str(exc)
        code = 413 if "hard cap" in msg else 415
        raise HTTPException(
            status_code=code,
            detail=f"{msg} (allowed mime types: {list(ALLOWED_MIMES)})",
        )
    except StorageError as exc:
        log.error("document.store_failed", error=str(exc))
        raise HTTPException(status_code=502, detail="document storage failed")


@app.get("/v1/ocr/documents/{document_id}")
async def get_document(
    document_id: str,
    request: Request,
    tenant: str | None = None,
    deps: AppState = Depends(get_state),
):
    """Metadata + extracted fields. Document BYTES are never served here."""
    try:
        doc_uuid = uuid.UUID(document_id)
    except ValueError:
        raise HTTPException(status_code=400, detail="invalid document id")
    tenant_id = await _authenticate(request, tenant, deps)
    async with deps.db.tenant_conn(tenant_id) as conn:
        row = await deps.db.get_document(conn, doc_uuid)
    if row is None:
        raise HTTPException(status_code=404, detail="document not found")
    return {
        "document_id": str(row["id"]),
        "tenant_id": str(row["tenant_id"]),
        "kind": row["kind"],
        "mime": row["mime"],
        "bytes": row["bytes"],
        "sha256": row["sha256"],
        "phash": row["phash"],
        "page_count": row["page_count"],
        "model_version": row["model_version"],
        "storage_bucket": row["storage_bucket"],
        "storage_key": row["storage_key"],
        "status": row["status"],
        "error": row["error"],
        "extracted_text": row["extracted_text"],
        "extracted_fields": row["extracted_fields"],
        "created_at": row["created_at"].isoformat(),
        "updated_at": row["updated_at"].isoformat(),
    }
