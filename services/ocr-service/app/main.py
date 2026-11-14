"""ocr-service FastAPI app (port 7020, SPEC-W53 Scope A).

Routes:
  POST /v1/ocr/extract           multipart file OR {"document_url"} + kind
                                 (receipt|document|knowledge_scan) -> MinIO
                                 `documents` bucket (tenant-prefixed keys),
                                 llama-ocr sidecar extraction, ocr_documents
                                 row (uploaded->extracted|failed), ocr_page
                                 UsageRecord + DocumentExtracted CloudEvent.
  GET  /v1/ocr/documents/{id}    metadata + extracted fields (NO bytes).
  POST /v1/ocr/documents/{id}/erase  internauth per-document hard erase
                                 (SPEC-W61 G2; booking PrivacyConsumer).
  POST /v1/internal/ocr/backfill-document-events
                                 internauth D9 backfill reconciler —
                                 deterministic-id, historical-time
                                 DocumentExtracted re-emission with keyset
                                 pagination (SPEC-W66 WS1-A).
  POST /v1/internal/ocr/backfill-document-registry
                                 internauth registry backfill — replays
                                 historical extracted rows through the
                                 SPEC-W67 cross-tenant registry upsert
                                 (same fn as the live path), keyset
                                 pagination + dry_run (SPEC-W67).
  GET  /healthz
  GET  /metrics
  GET  /dapr/subscribe           Dapr programmatic subscriptions (W58 P3):
                                 consent erasure + TenantDeleted topics.
  POST /v1/ocr/events/consent-erasure   GATE R2 erasure consumer (kyc_*).
  POST /v1/ocr/events/tenant-deleted    GATE R2 tenant cascade (all kinds).

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

import asyncio
import hmac
import uuid
from contextlib import asynccontextmanager
from datetime import UTC, datetime, timedelta
from typing import Any

import httpx
from fastapi import Depends, FastAPI, HTTPException, Request, Response
from fastapi.responses import JSONResponse, PlainTextResponse
from prometheus_client import CONTENT_TYPE_LATEST, Counter, generate_latest

from . import erasure, events, registry, sweeper
from .av import AvClient, AvUnavailable, Infected, InfectedError
from .config import ALLOWED_MIMES, DOCUMENT_KINDS, is_kyc_kind, settings
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

# SPEC-W59 P6: first custom Prometheus metric in the service (zero new
# deps — prometheus-client already pinned). result: clean|infected|
# error|skipped.
AV_SCANS = Counter(
    "opendesk_ocr_av_scans_total",
    "Document-upload malware scans (clamd zINSTREAM) by result.",
    ("result",),
)

# Response schema shared with booking-service (A<->B) and admin-web (A<->C):
# POST /v1/ocr/extract -> {document_id, status, extracted_fields, page_count,
#                          extracted_text}
# (extracted_text is the W53-D additive key C's knowledge flow consumes;
# docs/api/openapi/ocr.yaml is the normative contract).


class AppState:
    db: Database
    documents: DocumentStore
    objects: sweeper.MinioObjectDeleter
    ocr: OcrClient
    dapr: DaprClient
    av: AvClient | None  # SPEC-W59: None == AV disabled (CLAMAV_HOST empty)


def get_state(request: Request) -> AppState:
    return request.app.state.deps


def _build_av_client() -> AvClient | None:
    """SPEC-W59 P10: CLAMAV_HOST empty disables scanning — CRITICAL, loud
    (XTTS_MOCK/OCR_MOCK loud-default idiom), never a silent unscanned
    window. Default is `clamav`; disabling requires explicit emptying."""
    if not settings.clamav_host:
        log.critical(
            "clamav.disabled",
            msg="CLAMAV_HOST is empty — upload malware scanning DISABLED. "
            "DEV SANDBOX ONLY. NEVER ship this to production.",
        )
        return None
    return AvClient(
        host=settings.clamav_host,
        port=settings.clamav_port,
        timeout_s=settings.clamav_timeout_s,
    )


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
    # SPEC-W59 P1: clamd zINSTREAM sidecar client (None == disabled).
    deps.av = _build_av_client()
    # SPEC-W67 G1: registry posture logged ONCE at startup — disabled
    # (OCR_REGISTRY_HMAC_SECRET unset) is the fail-closed default.
    registry.log_startup_posture(settings.ocr_registry_hmac_secret)
    # SPEC-W58 P3/P4: object deletion for the erasure consumers + TTL
    # sweeper (storage.py is upload-only by design).
    deps.objects = sweeper.MinioObjectDeleter(
        endpoint=settings.minio_endpoint,
        access_key=settings.minio_access_key,
        secret_key=settings.minio_secret_key,
        bucket=settings.minio_bucket,
        secure=settings.minio_secure,
    )
    await deps.db.connect()
    try:
        await deps.db.ensure_ocr_documents_table()
    except Exception as exc:
        log.error("ocr_documents.bootstrap_failed", error=str(exc))
    app.state.deps = deps
    # GATE R3: periodic TTL sweeper (kind-generic; only kyc_* rows carry
    # expires_at today). Task is cancelled in the lifespan shutdown path.
    sweep_task: asyncio.Task | None = None
    if settings.kyc_sweep_minutes > 0:
        sweep_task = asyncio.create_task(
            sweeper.sweeper_loop(
                deps.db, deps.objects, interval_minutes=settings.kyc_sweep_minutes
            )
        )
    log.info(
        "service.started",
        port=settings.port,
        mock=settings.ocr_mock,
        llm_base_url=settings.llm_base_url,
        bucket=settings.minio_bucket,
        kyc_ttl_hours=settings.kyc_ttl_hours,
        kyc_sweep_minutes=settings.kyc_sweep_minutes,
        av_enabled=deps.av is not None,
    )
    try:
        yield
    finally:
        log.info("service.shutting_down")
        if sweep_task is not None:
            sweep_task.cancel()
        if deps.av is not None:
            await deps.av.aclose()
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
async def _read_upload(
    request: Request,
) -> tuple[str, bytes, str | None, str | None, Any, str | None]:
    """Extract (filename, data, kind, tenant, expires_in_hours raw,
    subject_phone) from multipart OR JSON body.

    The tenant selector is `tenant_id` (A<->B contract; the booking-service
    fieldcapture client sends it as a form field alongside X-Tenant-Slug);
    the legacy `tenant` key stays accepted. Enforces the 25 MB hard cap
    while reading. `expires_in_hours` (SPEC-W61 G2) is returned RAW and
    validated by the caller (_parse_expires_in_hours). `subject_phone`
    (SPEC-W65 G2) is the OPTIONAL E.164 consent subject forwarded by
    kyc-service; it is persisted ONLY for kyc_* kinds (dropped + logged
    otherwise) and never appears in any response.
    """
    ctype = request.headers.get("content-type", "")
    if ctype.startswith("multipart/form-data"):
        form = await request.form()
        upload = form.get("file")
        kind = form.get("kind")
        tenant = _as_str(form.get("tenant_id")) or _as_str(form.get("tenant"))
        expires_raw = form.get("expires_in_hours")
        subject_phone = _as_str(form.get("subject_phone"))
        if upload is None or not getattr(upload, "filename", None):
            raise HTTPException(status_code=400, detail="multipart field 'file' required")
        data = await upload.read()
        return upload.filename, data, _as_str(kind), tenant, expires_raw, subject_phone
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
    expires_raw = body.get("expires_in_hours")
    subject_phone = _as_str(body.get("subject_phone"))
    if not url or not isinstance(url, str):
        raise HTTPException(status_code=400, detail="document_url required")
    filename, data = await _fetch_document_url(url)
    return filename, data, _as_str(kind), tenant, expires_raw, subject_phone


def _parse_expires_in_hours(raw: Any) -> int | None:
    """SPEC-W61 G2: optional `expires_in_hours` (int 1..8760) on
    POST /v1/ocr/extract. Absent => None (byte-identical legacy behavior —
    the kyc TTL path is untouched). Present but invalid => 400."""
    if raw is None:
        return None
    if isinstance(raw, bool) or isinstance(raw, float):
        # JSON true/false must never coerce to 1/0; fractional floats are
        # not integer hours (a form field always arrives as str).
        raise HTTPException(
            status_code=400, detail="expires_in_hours must be an integer in 1..8760"
        )
    try:
        hours = int(raw.strip()) if isinstance(raw, str) else int(raw)
    except (TypeError, ValueError):
        raise HTTPException(
            status_code=400, detail="expires_in_hours must be an integer in 1..8760"
        ) from None
    if not 1 <= hours <= 8760:
        raise HTTPException(
            status_code=400, detail="expires_in_hours must be an integer in 1..8760"
        )
    return hours


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
    mime: str,
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
        # SPEC-W64: the validated content type rides the event (not PII).
        mime=mime,
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


async def _publish_av_alert(deps: AppState, event: dict[str, Any]) -> None:
    """SPEC-W59 P5: best-effort ops alert via the existing DaprClient —
    publishing NEVER blocks the 415/503 (the block is the primary signal)."""
    try:
        await deps.dapr.publish(
            settings.dapr_pubsub_name, events.TOPIC_OPS_ALERTS, event
        )
    except Exception as exc:
        log.warning("av.alert_publish_failed", error=str(exc))


async def _scan_upload(
    deps: AppState, *, tenant_id: str, kind: str, data: bytes, digest: str
) -> None:
    """SPEC-W59 P1/P2: clamd zINSTREAM scan, gating the FIRST durable side
    effect (MinIO put). Infected => InfectedError (415); clamd down =>
    AvUnavailable (503) unless the fail posture allows a loud skip."""
    if deps.av is None:
        # CLAMAV_HOST empty (P10): skip + metric; the CRITICAL boot log
        # fired at startup.
        AV_SCANS.labels(result="skipped").inc()
        return
    try:
        result = await deps.av.scan(data)
    except AvUnavailable as exc:
        log.error(
            "av.unavailable",
            tenant_id=tenant_id,
            kind=kind,
            sha256=digest,
            error=str(exc),
        )
        # P2: kyc_* fail-closed ALWAYS (hard-coded, regardless of
        # CLAMAV_REQUIRED); receipt may fail-open ONLY when
        # CLAMAV_REQUIRED=false — and a skip is never silent (metric+alert).
        if is_kyc_kind(kind) or settings.clamav_required:
            AV_SCANS.labels(result="error").inc()
            raise
        AV_SCANS.labels(result="skipped").inc()
        await _publish_av_alert(
            deps,
            events.av_scan_skipped_event(
                tenant_id=tenant_id,
                kind=kind,
                sha256=digest,
                reason="av_unavailable",
            ),
        )
        return
    if isinstance(result, Infected):
        AV_SCANS.labels(result="infected").inc()
        log.warning(
            "av.infected",
            tenant_id=tenant_id,
            kind=kind,
            sha256=digest,
            signature=result.signature,
        )
        await _publish_av_alert(
            deps,
            events.av_alert_event(
                tenant_id=tenant_id,
                kind=kind,
                sha256=digest,
                signature=result.signature,
            ),
        )
        raise InfectedError(result.signature)
    AV_SCANS.labels(result="clean").inc()


async def _emit_registry_match(
    deps: AppState,
    *,
    tenant_id: str,
    document_id: Any,
    kind: str,
    sha256: str,
    result: dict[str, Any],
) -> None:
    """SPEC-W67 G5/G6: emit the pinned CrossTenantDocumentMatch for the
    LATER tenant. The platform-only audit line (registry_key + both sides'
    tenant ids + ts) NEVER rides a tenant-visible path; the event payload
    itself carries own-identifiers + one integer only."""
    registry.REGISTRY_MATCHES.inc()
    log.info(
        "ocr_registry.cross_tenant_match",  # platform-only audit (NDPA R8)
        registry_key=result["registry_key"],
        later_tenant_id=tenant_id,
        other_tenant_ids=result["other_tenant_ids"],
        tenant_count=result["tenant_count"],
    )
    event = events.cross_tenant_match_event(
        tenant_id=tenant_id,
        document_id=document_id,
        kind=kind,
        sha256=sha256,
        tenant_count=result["tenant_count"],
    )
    await deps.dapr.publish(
        settings.dapr_pubsub_name, settings.ocr_events_topic, event
    )


async def _registry_register_and_alert(
    deps: AppState,
    *,
    tenant_id: str,
    document_id: uuid.UUID,
    kind: str,
    sha256: str,
    first_seen: Any,
) -> None:
    """SPEC-W67 live write path — post-commit registry upsert + at-most-one
    match emission. BEST-EFFORT: the extract/upload response is INVARIANT
    whether or not a match fires (G5 timing/disclosure gate), so a registry
    failure is logged and left for the backfill reconciler, never raised."""
    if not settings.ocr_registry_hmac_secret:
        # G1 fail-closed-disabled: the upsert is a no-op, the extract path
        # is unaffected (the disabled posture logged once at startup).
        return
    try:
        result = await registry.register_document(
            deps.db,
            secret=settings.ocr_registry_hmac_secret,
            tenant_id=tenant_id,
            sha256=sha256,
            first_seen=first_seen,
        )
    except Exception as exc:
        log.error(
            "ocr_registry.upsert_failed",
            document_id=str(document_id),
            error=str(exc),
        )
        return
    if result["match"]:
        try:
            await _emit_registry_match(
                deps,
                tenant_id=tenant_id,
                document_id=document_id,
                kind=kind,
                sha256=sha256,
                result=result,
            )
        except Exception as exc:
            # At-most-one stands (the alerted flag is already committed);
            # the audit line above is the platform reconciliation source.
            log.error(
                "ocr_registry.match_publish_failed",
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
    expires_in_hours: int | None = None,
    subject_phone: str | None = None,
) -> dict[str, Any]:
    ext, mime = validate_upload(filename, data, max_bytes=settings.ocr_max_bytes)
    digest = sha256_hex(data)
    phash = perceptual_hash(data, mime)
    document_id = uuid.uuid4()
    storage_key = f"{tenant_id}/{document_id}{ext}"
    kyc = is_kyc_kind(kind)
    # SPEC-W65 G2: subject_phone (the consent-registry subject key) is
    # persisted ONLY for kyc_* kinds — PII minimization. For non-kyc kinds
    # it is silently dropped + logged (compat: a 400 would break old
    # callers during rolling deploys). It is NEVER echoed in responses.
    if subject_phone is not None and not kyc:
        log.info(
            "ocr.subject_phone_dropped_non_kyc",
            tenant_id=tenant_id,
            kind=kind,
        )
        subject_phone = None
    # SPEC-W61 G2: a caller-pinned expires_in_hours sets expires_at for ANY
    # kind (whatsapp media anchor flow) and overrides the kyc default.
    # SPEC-W58 P4 (GATE R3), UNCHANGED when the field is absent: expires_at
    # is set ONLY for kyc_* kinds; the periodic sweeper deletes the row +
    # MinIO object once reached.
    if expires_in_hours is not None:
        expires_at = datetime.now(UTC) + timedelta(hours=expires_in_hours)
    else:
        expires_at = (
            datetime.now(UTC) + timedelta(hours=settings.kyc_ttl_hours) if kyc else None
        )

    # SPEC-W59: AV scan AFTER validation+hashing (never spend a scan on a
    # 415; sha256 feeds the ops alert) and strictly BEFORE any bytes touch
    # MinIO or the pdftoppm rasterizer. Infected => nothing stored: no
    # MinIO object, no ocr_documents row, no quarantine problem.
    await _scan_upload(deps, tenant_id=tenant_id, kind=kind, data=data, digest=digest)

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
                "expires_at": expires_at,
                "subject_phone": subject_phone,  # kyc_* only (SPEC-W65 G2)
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
    # SPEC-W58 A5 (GATES R2/R3 minimization): kyc_* kinds never persist the
    # full transcript — it is pure PII with no downstream consumer (the
    # knowledge-scan flow is the only extracted_text consumer). Fields stay
    # persisted (TTL-bound) so the consent-gated review surface works.
    async with deps.db.tenant_conn(tenant_id) as conn:
        await deps.db.mark_extracted(
            conn,
            document_id,
            page_count=extraction.page_count,
            model_version=extraction.model_version,
            extracted_text=None if kyc else extraction.text,
            extracted_fields=extraction.fields,
        )
        # SPEC-W67: the registry's first_seen MUST be the document row's
        # created_at (true chronology for the backfill), never wall-clock.
        doc_row = await deps.db.get_document(conn, document_id)
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
        # SPEC-W64: validated content type (from validate_upload above)
        # rides the DocumentExtracted event.
        mime=mime,
    )
    # SPEC-W67: cross-tenant registry hook — strictly POST-COMMIT (the
    # mark_extracted tenant_conn transaction above has committed; failed
    # uploads never reach this) and best-effort: the response below is
    # byte-invariant whether or not a match fires.
    await _registry_register_and_alert(
        deps,
        tenant_id=tenant_id,
        document_id=document_id,
        kind=kind,
        sha256=digest,
        first_seen=doc_row["created_at"],
    )
    return {
        "document_id": str(document_id),
        "status": "extracted",
        "extracted_fields": extraction.fields,
        "page_count": extraction.page_count,
        # W53-D additive key: the full markdown/text body (C's knowledge
        # flow POSTs it to knowledge-service /v1/documents). SPEC-W58 A5:
        # suppressed ("") for kyc_* kinds — transcript is never persisted
        # nor echoed for KYC PII.
        "extracted_text": "" if kyc else extraction.text,
    }


@app.post("/v1/ocr/extract", status_code=200)
async def extract_document(request: Request, deps: AppState = Depends(get_state)):
    try:
        filename, data, kind, tenant, expires_raw, subject_phone = await _read_upload(request)
        if kind not in DOCUMENT_KINDS:
            raise HTTPException(
                status_code=400,
                detail=f"kind must be one of {list(DOCUMENT_KINDS)}",
            )
        # SPEC-W61 G2: optional TTL field, validated BEFORE any side effect.
        expires_in_hours = _parse_expires_in_hours(expires_raw)
        tenant_id = await _authenticate(request, tenant, deps)
        return await _run_extraction(
            deps, tenant_id=tenant_id, kind=kind, filename=filename, data=data,
            expires_in_hours=expires_in_hours, subject_phone=subject_phone,
        )
    except ValidationError as exc:
        msg = str(exc)
        code = 413 if "hard cap" in msg else 415
        raise HTTPException(
            status_code=code,
            detail=f"{msg} (allowed mime types: {list(ALLOWED_MIMES)})",
        )
    except InfectedError as exc:
        # SPEC-W59 P7: 415 sibling of the ValidationError mapping. The
        # signature name aids false-positive triage; no bytes are logged
        # or echoed. Nothing was stored (scan gates the MinIO put).
        raise HTTPException(
            status_code=415,
            detail={
                "error": "av_infected",
                "detail": f"rejected by malware scan: {exc.signature}",
                "signature": exc.signature,
            },
        )
    except AvUnavailable:
        # SPEC-W59 P2/P7: fail-closed posture (kyc_* always; others per
        # CLAMAV_REQUIRED) — 503 mirrors the internauth fail-closed idiom.
        raise HTTPException(status_code=503, detail={"error": "av_unavailable"})
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
        "expires_at": row["expires_at"].isoformat() if row.get("expires_at") else None,
        "created_at": row["created_at"].isoformat(),
        "updated_at": row["updated_at"].isoformat(),
    }


# ---------------------------------------------------------------------------
# SPEC-W61 G2: internauth per-document hard erase. Invoked from booking's
# PrivacyConsumer for media_inbox-linked documents after a subject anonymize
# (the row tombstone is authoritative; this deletes the BYTES + row here).
# Internauth ONLY (K2 fail-closed, W58 ruling): X-Internal-Token vs
# OCR_INTERNAL_TOKEN, constant-time; unset => 503, missing/wrong => 401.
# Never takes the gateway tenant-binding path. Idempotent: a missing or
# already-erased document answers 204. Byte delete precedes row delete
# (sweeper idiom); an object-store failure keeps the row for retry (502).
# ---------------------------------------------------------------------------
@app.post("/v1/ocr/documents/{document_id}/erase")
async def erase_document_endpoint(
    document_id: str,
    request: Request,
    deps: AppState = Depends(get_state),
):
    if not settings.internal_token:
        log.error("document_erase.unconfigured", msg="OCR_INTERNAL_TOKEN unset (fail closed)")
        raise HTTPException(
            status_code=503,
            detail="internal auth not configured (OCR_INTERNAL_TOKEN unset)",
        )
    token = request.headers.get("x-internal-token")
    if token is None or not hmac.compare_digest(token, settings.internal_token):
        raise HTTPException(status_code=401, detail="invalid internal token")
    try:
        doc_uuid = uuid.UUID(document_id)
    except ValueError:
        raise HTTPException(status_code=400, detail="invalid document id")
    # Cross-tenant sweep scope (app.ocr_internal_sweep escape): the caller
    # is a token-authenticated machine addressing the document by uuid only
    # — the booking consumer does not hold the ocr tenant id.
    async with deps.db.sweep_conn() as conn:
        row = await deps.db.get_document_for_erase(conn, doc_uuid)
    if row is None:
        # Idempotent (SPEC-W61): already-erased / never-existed => 204.
        return Response(status_code=204)
    try:
        await deps.objects.delete(row["storage_key"])
    except Exception as exc:
        log.error("document_erase.object_delete_failed", document_id=document_id, error=str(exc))
        raise HTTPException(status_code=502, detail="object storage delete failed") from exc
    async with deps.db.sweep_conn() as conn:
        await deps.db.erase_document(conn, doc_uuid)
    log.info(
        "document_erase.erased",
        document_id=document_id,
        tenant_id=str(row["tenant_id"]),
    )
    return Response(status_code=204)


# ---------------------------------------------------------------------------
# SPEC-W66 WS1-A: D9 backfill reconciler. Re-emits
# com.opendesk.ocr.DocumentExtracted for historical status='extracted'
# rows so graph-sync can project the hash-only (:Document) stubs that
# pre-D9 documents never got (Kafka offset.reset=latest gap, W55 closeout).
#
# Internauth ONLY (K2 fail-closed, erase-endpoint idiom): X-Internal-Token
# vs OCR_INTERNAL_TOKEN, constant-time; unset => 503, missing/wrong => 401.
# Never takes the gateway tenant-binding path.
#
# Idempotency (stateless): event id = uuid5(URL-ns,
# "ocr-backfill:{document_id}") and event time = document created_at, so a
# re-run re-emits byte-identical events; graph MERGE + processed-marker
# dedup + gold partitionBy(event_id) absorb replays. Keyset pagination on
# (created_at, id) IS the resumability story — pass next_cursor back until
# it returns null. dry_run=true scans and counts but publishes NOTHING.
#
# RLS (G3): tenant enumeration rides the EXISTING sweep_conn() GUC escape;
# every row read is under tenant_conn(). No new GUC, no superuser reads.
# ---------------------------------------------------------------------------
BACKFILL_DEFAULT_LIMIT = 500
BACKFILL_MAX_LIMIT = 5000


def _parse_rfc3339(value: Any, field: str) -> datetime | None:
    """Optional RFC3339 bound; must be timezone-aware (a naive timestamp is
    an operator bug, never silently assumed UTC)."""
    if value is None:
        return None
    if not isinstance(value, str) or not value.strip():
        raise HTTPException(status_code=400, detail=f"{field} must be an RFC3339 timestamp")
    try:
        dt = datetime.fromisoformat(value.strip().replace("Z", "+00:00"))
    except ValueError:
        raise HTTPException(
            status_code=400, detail=f"{field} must be an RFC3339 timestamp"
        ) from None
    if dt.tzinfo is None:
        raise HTTPException(
            status_code=400, detail=f"{field} must include a timezone offset"
        )
    return dt


def _parse_backfill_limit(value: Any) -> int:
    """limit: default 500, hard max 5000 (clamped, never an error)."""
    if value is None:
        return BACKFILL_DEFAULT_LIMIT
    if isinstance(value, bool) or not isinstance(value, int):
        raise HTTPException(status_code=400, detail="limit must be an integer in 1..5000")
    if value < 1:
        raise HTTPException(status_code=400, detail="limit must be an integer in 1..5000")
    return min(value, BACKFILL_MAX_LIMIT)


def _encode_backfill_cursor(created_at: datetime, document_id: Any) -> str:
    return f"{created_at.isoformat()}|{document_id}"


def _parse_backfill_cursor(raw: Any) -> tuple[datetime, uuid.UUID] | None:
    if raw is None:
        return None
    if not isinstance(raw, str) or "|" not in raw:
        raise HTTPException(status_code=400, detail="invalid cursor")
    ts_raw, _, id_raw = raw.rpartition("|")
    try:
        created = datetime.fromisoformat(ts_raw.strip().replace("Z", "+00:00"))
        doc_id = uuid.UUID(id_raw.strip())
    except ValueError:
        raise HTTPException(status_code=400, detail="invalid cursor") from None
    if created.tzinfo is None:
        raise HTTPException(status_code=400, detail="invalid cursor")
    return created, doc_id


@app.post("/v1/internal/ocr/backfill-document-events")
async def backfill_document_events(request: Request, deps: AppState = Depends(get_state)):
    if not settings.internal_token:
        log.error("backfill.unconfigured", msg="OCR_INTERNAL_TOKEN unset (fail closed)")
        raise HTTPException(
            status_code=503,
            detail="internal auth not configured (OCR_INTERNAL_TOKEN unset)",
        )
    token = request.headers.get("x-internal-token")
    if token is None or not hmac.compare_digest(token, settings.internal_token):
        raise HTTPException(status_code=401, detail="invalid internal token")

    try:
        body = await request.json()
    except Exception:
        body = {}
    if body is None:
        body = {}
    if not isinstance(body, dict):
        raise HTTPException(status_code=400, detail="invalid JSON body")

    tenant_filter = body.get("tenant_id")
    if tenant_filter is not None:
        if not isinstance(tenant_filter, str):
            raise HTTPException(status_code=400, detail="tenant_id must be a uuid string")
        try:
            tenant_filter = str(uuid.UUID(tenant_filter))
        except ValueError:
            raise HTTPException(
                status_code=400, detail="tenant_id must be a uuid string"
            ) from None
    since = _parse_rfc3339(body.get("since"), "since")
    until = _parse_rfc3339(body.get("until"), "until")
    if since is not None and until is not None and since > until:
        raise HTTPException(status_code=400, detail="since must be <= until")
    cursor = _parse_backfill_cursor(body.get("cursor"))
    limit = _parse_backfill_limit(body.get("limit"))
    dry_run = bool(body.get("dry_run", False))

    # Tenant enumeration: the pinned tenant, or DISTINCT tenants holding
    # extracted rows in the window via the EXISTING sweep GUC escape.
    if tenant_filter is not None:
        tenants = [tenant_filter]
    else:
        async with deps.db.sweep_conn() as conn:
            tenants = await deps.db.list_backfill_tenant_ids(conn, since, until)

    # Merge one globally (created_at, id)-ordered page across tenants. Each
    # per-tenant read is RLS-scoped (tenant_conn) and already cursor-bound;
    # fetching up to `limit` per tenant and merge-truncating keeps the
    # keyset exact when rows interleave across tenants.
    merged: list[dict[str, Any]] = []
    exhausted = True
    for tid in tenants:
        async with deps.db.tenant_conn(tid) as conn:
            rows = await deps.db.list_documents_for_backfill(
                conn, since, until, cursor, limit
            )
        if len(rows) >= limit:
            exhausted = False
        merged.extend(rows)
    merged.sort(key=lambda r: (r["created_at"], str(r["id"])))
    page = merged[:limit]
    truncated = len(merged) > limit

    scanned = len(page)
    emitted = 0
    skipped = 0
    for row in page:
        # Defensive skip (keeps emitted + skipped == scanned): sha256 is
        # NOT NULL and created_at is NOT NULL in the schema, so this should
        # never trip — but a reconciler must never emit a hash-less stub.
        if not row.get("sha256") or row.get("created_at") is None:
            skipped += 1
            log.warning("backfill.row_skipped", document_id=str(row.get("id")))
            continue
        if dry_run:
            skipped += 1  # counted, never published
            continue
        event = events.document_backfill_event(
            tenant_id=str(row["tenant_id"]),
            document_id=row["id"],
            kind=row["kind"],
            status=row["status"],
            page_count=row["page_count"],
            sha256=row["sha256"],
            phash=row["phash"],
            model_version=row["model_version"],
            created_at=row["created_at"],
            extracted_fields=row.get("extracted_fields"),
        )
        try:
            await deps.dapr.publish(
                settings.dapr_pubsub_name, settings.ocr_events_topic, event
            )
        except Exception as exc:
            # Re-runs are idempotent (deterministic ids), so failing the
            # page is safe: retry with the SAME cursor.
            log.error(
                "backfill.publish_failed",
                document_id=str(row["id"]),
                error=str(exc),
            )
            raise HTTPException(
                status_code=502,
                detail="event publish failed; retry with the same cursor (idempotent)",
            ) from exc
        emitted += 1

    next_cursor = None
    if page and (truncated or not exhausted):
        last = page[-1]
        next_cursor = _encode_backfill_cursor(last["created_at"], last["id"])
    log.info(
        "backfill.page",
        tenants=len(tenants),
        scanned=scanned,
        emitted=emitted,
        skipped=skipped,
        dry_run=dry_run,
        done=next_cursor is None,
    )
    return {
        "emitted": emitted,
        "scanned": scanned,
        "skipped": skipped,
        "next_cursor": next_cursor,
    }


# ---------------------------------------------------------------------------
# SPEC-W67: cross-tenant registry backfill. Replays historical
# status='extracted' rows through the SAME registry upsert fn as the live
# path (registry.register_document) so pre-existing cross-tenant duplicates
# register + alert once (the alerted flag makes re-runs free). Separate
# endpoint — the W66 event endpoint contract above is frozen.
#
# Internauth ONLY (W66 idiom exactly): X-Internal-Token vs
# OCR_INTERNAL_TOKEN, constant-time; unset => 503, missing/wrong => 401.
# G1 fail-closed: OCR_REGISTRY_HMAC_SECRET unset => 503 (the registry is
# DISABLED; running a backfill would be a silent no-op).
#
# Keyset (created_at, id) IS first-use-wins order; pass next_cursor back
# until it returns null. dry_run=true scans and counts but writes NOTHING
# and emits NOTHING. RLS: tenant enumeration rides the EXISTING
# sweep_conn() GUC escape; row reads stay under tenant_conn() (no registry
# GUC on those reads); registry writes go through the registry module's
# dedicated connection only.
#
# NOTE on the 502 retry: a match event publish failure after a committed
# membership is NOT re-emitted on retry (the alerted flag is the
# at-most-one record per (tenant, hash); the platform audit line is the
# reconciliation source). Everything else is replay-safe.
# ---------------------------------------------------------------------------
@app.post("/v1/internal/ocr/backfill-document-registry")
async def backfill_document_registry(request: Request, deps: AppState = Depends(get_state)):
    if not settings.internal_token:
        log.error("registry_backfill.unconfigured", msg="OCR_INTERNAL_TOKEN unset (fail closed)")
        raise HTTPException(
            status_code=503,
            detail="internal auth not configured (OCR_INTERNAL_TOKEN unset)",
        )
    token = request.headers.get("x-internal-token")
    if token is None or not hmac.compare_digest(token, settings.internal_token):
        raise HTTPException(status_code=401, detail="invalid internal token")
    if not settings.ocr_registry_hmac_secret:
        # G1 fail-closed: the registry is DISABLED — refuse loudly instead
        # of scanning and writing nothing.
        log.error(
            "registry_backfill.disabled",
            msg="OCR_REGISTRY_HMAC_SECRET unset (fail closed)",
        )
        raise HTTPException(
            status_code=503,
            detail="registry disabled (OCR_REGISTRY_HMAC_SECRET unset)",
        )

    try:
        body = await request.json()
    except Exception:
        body = {}
    if body is None:
        body = {}
    if not isinstance(body, dict):
        raise HTTPException(status_code=400, detail="invalid JSON body")

    tenant_filter = body.get("tenant_id")
    if tenant_filter is not None:
        if not isinstance(tenant_filter, str):
            raise HTTPException(status_code=400, detail="tenant_id must be a uuid string")
        try:
            tenant_filter = str(uuid.UUID(tenant_filter))
        except ValueError:
            raise HTTPException(
                status_code=400, detail="tenant_id must be a uuid string"
            ) from None
    since = _parse_rfc3339(body.get("since"), "since")
    until = _parse_rfc3339(body.get("until"), "until")
    if since is not None and until is not None and since > until:
        raise HTTPException(status_code=400, detail="since must be <= until")
    cursor = _parse_backfill_cursor(body.get("cursor"))
    limit = _parse_backfill_limit(body.get("limit"))
    dry_run = bool(body.get("dry_run", False))

    # Tenant enumeration: the pinned tenant, or DISTINCT tenants holding
    # extracted rows in the window via the EXISTING sweep GUC escape.
    if tenant_filter is not None:
        tenants = [tenant_filter]
    else:
        async with deps.db.sweep_conn() as conn:
            tenants = await deps.db.list_backfill_tenant_ids(conn, since, until)

    # Merge one globally (created_at, id)-ordered page across tenants —
    # identical to the W66 reconciler above; that order IS first-use-wins
    # order for the registry.
    merged: list[dict[str, Any]] = []
    exhausted = True
    for tid in tenants:
        async with deps.db.tenant_conn(tid) as conn:
            rows = await deps.db.list_documents_for_backfill(
                conn, since, until, cursor, limit
            )
        if len(rows) >= limit:
            exhausted = False
        merged.extend(rows)
    merged.sort(key=lambda r: (r["created_at"], str(r["id"])))
    page = merged[:limit]
    truncated = len(merged) > limit

    scanned = len(page)
    upserted = 0
    memberships_added = 0
    matches = 0
    skipped = 0
    for row in page:
        # Defensive skip (keeps upserted + skipped == scanned): sha256 and
        # created_at are NOT NULL in the schema, so this should never trip.
        if not row.get("sha256") or row.get("created_at") is None:
            skipped += 1
            log.warning("registry_backfill.row_skipped", document_id=str(row.get("id")))
            continue
        if dry_run:
            skipped += 1  # counted, never written, never emitted
            continue
        result = await registry.register_document(
            deps.db,
            secret=settings.ocr_registry_hmac_secret,
            tenant_id=str(row["tenant_id"]),
            sha256=row["sha256"],
            first_seen=row["created_at"],
        )
        upserted += 1
        if result["membership_added"]:
            memberships_added += 1
        if result["match"]:
            try:
                await _emit_registry_match(
                    deps,
                    tenant_id=str(row["tenant_id"]),
                    document_id=row["id"],
                    kind=row["kind"],
                    sha256=row["sha256"],
                    result=result,
                )
            except Exception as exc:
                log.error(
                    "registry_backfill.publish_failed",
                    document_id=str(row["id"]),
                    error=str(exc),
                )
                raise HTTPException(
                    status_code=502,
                    detail="event publish failed; see endpoint contract note "
                    "on alerted-flag at-most-one semantics",
                ) from exc
            matches += 1

    next_cursor = None
    if page and (truncated or not exhausted):
        last = page[-1]
        next_cursor = _encode_backfill_cursor(last["created_at"], last["id"])
    log.info(
        "registry_backfill.page",
        tenants=len(tenants),
        scanned=scanned,
        upserted=upserted,
        memberships_added=memberships_added,
        matches=matches,
        skipped=skipped,
        dry_run=dry_run,
        done=next_cursor is None,
    )
    return {
        "upserted": upserted,
        "memberships_added": memberships_added,
        "matches": matches,
        "scanned": scanned,
        "skipped": skipped,
        "next_cursor": next_cursor,
    }


# ---------------------------------------------------------------------------
# SPEC-W58 GATE R2 (P3): Dapr pubsub consumers (consent erasure + tenant
# delete). Programmatic subscription via /dapr/subscribe; Dapr POSTs the
# CloudEvent envelope to each route. Handler failure => 500 => Dapr
# redelivers (handlers are idempotent); malformed/foreign events => DROP.
# These routes are sidecar-facing only (not in the APISIX api-ocr route).
# ---------------------------------------------------------------------------
@app.get("/dapr/subscribe")
async def dapr_subscriptions():
    return [
        {
            "pubsubname": settings.dapr_pubsub_name,
            "topic": events.TOPIC_CONSENT_ERASURE,
            "route": "/v1/ocr/events/consent-erasure",
        },
        {
            "pubsubname": settings.dapr_pubsub_name,
            "topic": events.TOPIC_IDENTITY_EVENTS,
            "route": "/v1/ocr/events/tenant-deleted",
        },
    ]


@app.post("/v1/ocr/events/consent-erasure")
async def consent_erasure_event(request: Request, deps: AppState = Depends(get_state)):
    event = await request.json()
    if isinstance(event, dict) and event.get("type") != events.TYPE_CONSENT_ERASURE:
        # Another event type on the topic is not ours to process.
        return {"status": "DROP"}
    try:
        result = await erasure.handle_erasure_event(deps.db, deps.objects, event)
    except Exception as exc:
        log.error("consent_erasure.handler_failed", error=str(exc))
        raise HTTPException(status_code=500, detail="erasure handler failed") from exc
    return {"status": "SUCCESS", **result}


@app.post("/v1/ocr/events/tenant-deleted")
async def tenant_deleted_event(request: Request, deps: AppState = Depends(get_state)):
    event = await request.json()
    if isinstance(event, dict) and event.get("type") not in (
        None,
        events.TYPE_TENANT_DELETED,
    ):
        # opendesk.identity.events carries other identity lifecycle events.
        return {"status": "DROP"}
    try:
        result = await erasure.handle_tenant_deleted(deps.db, deps.objects, event)
    except Exception as exc:
        log.error("tenant_deleted.handler_failed", error=str(exc))
        raise HTTPException(status_code=500, detail="tenant-delete handler failed") from exc
    return {"status": "SUCCESS", **result}
