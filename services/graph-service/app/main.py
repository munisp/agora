"""graph-service FastAPI app (SPEC-W28 §4 WS-B, port 7014).

Routes (ALL tenant-scoped via the workforce auth seam — JWT sub, or
X-Tenant-Id in dev mode; every query injects the tenant filter):
  POST /v1/graph/segments                    save declarative segment DSL
  GET  /v1/graph/segments                    list tenant's segments
  GET  /v1/graph/segments/{id}/count         consent-passing count preview
  POST /v1/graph/segments/{id}/audience      materialize consent-passing audience
  GET  /v1/graph/audiences/{id}              fetch materialized audience (notification-worker handoff)
  POST /v1/graph/ask                         NL->Cypher GraphRAG via Ollama (allowlisted shapes)
  GET  /v1/graph/persons/{id}                person 360 (404 cross-tenant)
  POST /v1/graph/cypher                      template-allowlisted queries ONLY (gate 5)
  GET  /v1/graph/segments/schema             filterable-field catalog (incl. score filters)
  POST /v1/graph/internal/scores             W29 score write-back (X-Internal-Token only)
  POST /v1/graph/internal/recommendations    W29 RECOMMENDED_FOR write-back (internal)
  GET  /v1/graph/alerts[/{id}]               W30 fraud alert list/detail
  POST /v1/graph/alerts/{id}/resolve         W30 adjudication + audit CloudEvent
  POST /v1/graph/internal/fixtures/seed      dev/e2e fixture seeder (E2E_FIXTURES=1 only)
  GET  /healthz, GET /metrics
"""

from __future__ import annotations

import time
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any

import structlog
from fastapi import Depends, FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse, PlainTextResponse
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest
from pydantic import BaseModel, Field, ValidationError

from . import metrics
from .ask import (
    AskLLM,
    AskUnavailable,
    AskUnanswerable,
    OllamaAskLLM,
    build_prompt,
    parse_answer,
    validate_selection,
)
from .auth import current_tenant
from .backend import FalkorBackend, GraphBackend, GraphError, InMemoryBackend
from .config import Settings, load_settings
from .dsl import SegmentCreate
from .events import (
    AlertEventPublisher,
    KafkaAlertEventPublisher,
    NoopAlertEventPublisher,
)
from .plans import CompiledQuery
from .routers import alerts as alerts_router
from .routers import internal_fixtures as internal_fixtures_router
from .routers import internal_scores as internal_scores_router
from .routers import segments as segments_router
from .segment.compiler import compile_segment_query
from .store import SegmentStore
from .templates import TemplateError, compile_template

log = structlog.get_logger("graph-service")


@dataclass
class Deps:
    settings: Settings
    backend: GraphBackend
    llm: AskLLM | None
    store: SegmentStore
    events: AlertEventPublisher | None = None


class AskRequest(BaseModel):
    question: str = Field(min_length=1, max_length=2000)


class CypherRequest(BaseModel):
    """Compliance gate 5: template-allowlisted parameterized queries ONLY.
    A ``cypher`` key in the payload is rejected outright."""

    template: str = Field(min_length=1, max_length=100)
    params: dict[str, Any] = Field(default_factory=dict)


class AudienceRequest(BaseModel):
    campaign_id: str | None = Field(default=None, max_length=100)


def get_deps(request: Request) -> Deps:
    return request.app.state.deps


def create_app(
    settings: Settings,
    backend: GraphBackend | None = None,
    llm: AskLLM | None = None,
    store: SegmentStore | None = None,
    events: AlertEventPublisher | None = None,
) -> FastAPI:
    if backend is None:
        if settings.graph_backend == "memory":
            backend = InMemoryBackend()
        else:
            backend = FalkorBackend(
                host=settings.falkordb_host,
                port=settings.falkordb_port,
                graph_name=settings.falkordb_graph,
                username=settings.falkordb_username,
                password=settings.falkordb_password,
            )
    if llm is None:
        llm = OllamaAskLLM(
            base_url=settings.ollama_base_url,
            model=settings.graph_ask_model,
            api_key=settings.ollama_api_key,
            timeout_s=settings.ollama_timeout_s,
        )
    if store is None:
        store = SegmentStore(settings.segment_store_dir)
    if events is None:
        if settings.kafka_bootstrap_servers:
            events = KafkaAlertEventPublisher(settings.kafka_bootstrap_servers)
        else:
            events = NoopAlertEventPublisher()

    app = FastAPI(title="opendesk-graph-service", version="0.1.0")
    app.state.settings = settings
    app.state.deps = Deps(
        settings=settings, backend=backend, llm=llm, store=store, events=events
    )

    # W29 internal write-back + W30 alerts/fixtures routers. The fixture
    # seeder exists ONLY in dev/e2e (E2E_FIXTURES=1); production 404s it.
    app.include_router(internal_scores_router.router)
    app.include_router(alerts_router.router)
    app.include_router(segments_router.router)
    if settings.e2e_fixtures:
        app.include_router(internal_fixtures_router.router)

    @app.middleware("http")
    async def _metrics_middleware(request: Request, call_next):
        start = time.monotonic()
        response = await call_next(request)
        metrics.http_requests.labels(
            method=request.method,
            route=request.url.path,
            status=response.status_code,
        ).inc()
        _ = start  # latency covered per graph query below
        return response

    async def _run(
        deps: Deps, kind: str, query: CompiledQuery, tenant_id: str
    ) -> list[dict[str, Any]]:
        try:
            with metrics.graph_query_latency.labels(kind=kind).time():
                rows = await deps.backend.execute(query, tenant_id)
        except GraphError as exc:
            metrics.graph_queries.labels(kind=kind, result="error").inc()
            log.warning("graph.query_failed", kind=kind, error=str(exc))
            raise HTTPException(status_code=502, detail=str(exc)) from exc
        metrics.graph_queries.labels(kind=kind, result="ok").inc()
        return rows

    # ------------------------------------------------------------------ meta
    @app.get("/healthz")
    async def healthz(deps: Deps = Depends(get_deps)) -> JSONResponse:
        graph_ok = await deps.backend.ping()
        body = {
            "status": "ok" if graph_ok else "degraded",
            "graph": graph_ok,
            "graph_backend": deps.settings.graph_backend,
            "ask_model": deps.settings.graph_ask_model,
        }
        return JSONResponse(body, status_code=200 if graph_ok else 503)

    @app.get("/metrics")
    async def metrics_endpoint() -> PlainTextResponse:
        return PlainTextResponse(
            generate_latest().decode("utf-8"), media_type=CONTENT_TYPE_LATEST
        )

    # -------------------------------------------------------------- segments
    @app.post("/v1/graph/segments", status_code=201)
    async def create_segment(
        payload: SegmentCreate,
        tenant_id: str = Depends(current_tenant),
        deps: Deps = Depends(get_deps),
    ) -> dict[str, Any]:
        """Save a declarative segment. Compilation happens at save time so an
        invalid segment can never be persisted; the stored query always
        carries the mandatory consent + tenant filters."""
        compiled = compile_segment_query(
            payload, projection="ids", row_cap=deps.settings.segment_row_cap
        )
        record = deps.store.create_segment(
            tenant_id,
            payload={
                "name": payload.name,
                "purpose": payload.purpose,
                "description": payload.description,
                "consent_purpose": payload.consent_purpose,
                "filter": payload.filter.model_dump(mode="json"),
            },
            compiled_cypher=compiled.cypher,
        )
        return record

    @app.post("/v1/graph/segments/count")
    async def preview_segment_count(
        payload: SegmentCreate,
        tenant_id: str = Depends(current_tenant),
        deps: Deps = Depends(get_deps),
    ) -> dict[str, Any]:
        """Live count preview for UNSAVED segment DSL (admin-web Segment
        Builder). Accepts the same DSL body as POST /v1/graph/segments,
        compiles it through the identical mandatory gates (tenant filter,
        purpose-matching unrevoked CONSENTED edge, quarantine exclusion) and
        returns the consent-passing count WITHOUT persisting anything."""
        compiled = compile_segment_query(payload, projection="count")
        rows = await _run(deps, "segment_count_preview", compiled, tenant_id)
        return {"count": rows[0]["count"] if rows else 0}

    @app.get("/v1/graph/segments")
    async def list_segments(
        tenant_id: str = Depends(current_tenant),
        deps: Deps = Depends(get_deps),
    ) -> dict[str, Any]:
        return {"segments": deps.store.list_segments(tenant_id)}

    def _load_segment(deps: Deps, tenant_id: str, segment_id: str) -> SegmentCreate:
        record = deps.store.get_segment(tenant_id, segment_id)
        if record is None:
            raise HTTPException(status_code=404, detail="segment not found")
        return SegmentCreate(
            name=record["name"],
            purpose=record["purpose"],
            description=record.get("description"),
            filter=record.get("filter") or {},
        )

    @app.get("/v1/graph/segments/{segment_id}/count")
    async def segment_count(
        segment_id: str,
        tenant_id: str = Depends(current_tenant),
        deps: Deps = Depends(get_deps),
    ) -> dict[str, Any]:
        """Consent-passing count preview (quarantined excluded, gate 4)."""
        segment = _load_segment(deps, tenant_id, segment_id)
        compiled = compile_segment_query(segment, projection="count")
        rows = await _run(deps, "segment_count", compiled, tenant_id)
        return {
            "segment_id": segment_id,
            "count": rows[0]["count"] if rows else 0,
            "consent_purpose": segment.consent_purpose,
            "cypher": compiled.cypher,
        }

    @app.post("/v1/graph/segments/{segment_id}/audience", status_code=201)
    async def create_audience(
        segment_id: str,
        payload: AudienceRequest | None = None,
        tenant_id: str = Depends(current_tenant),
        deps: Deps = Depends(get_deps),
    ) -> dict[str, Any]:
        """Materialize the consent-passing audience (gate 2: no person
        without a purpose-matching CONSENTED edge; gate 4: no quarantined
        person). Returns audience_id + member refs (person_id, phone_hash,
        lead_id — raw PII never leaves the graph; lead_id is the most recent
        Contact's lead_id or null)."""
        segment = _load_segment(deps, tenant_id, segment_id)
        compiled = compile_segment_query(
            segment, projection="ids", row_cap=deps.settings.segment_row_cap
        )
        members = await _run(deps, "segment_audience", compiled, tenant_id)
        campaign_id = payload.campaign_id if payload else None
        audience = deps.store.create_audience(
            tenant_id, segment_id, campaign_id, members
        )
        metrics.audience_members.observe(len(members))
        return {
            "audience_id": audience["id"],
            "segment_id": segment_id,
            "campaign_id": campaign_id,
            "member_count": len(members),
            "members": members,
        }

    @app.get("/v1/graph/audiences/{audience_id}")
    async def get_audience(
        audience_id: str,
        tenant_id: str = Depends(current_tenant),
        deps: Deps = Depends(get_deps),
    ) -> dict[str, Any]:
        """Handoff seam for notification-worker (SPEC-W28 §4 WS-C)."""
        record = deps.store.get_audience(tenant_id, audience_id)
        if record is None:
            raise HTTPException(status_code=404, detail="audience not found")
        return record

    # ------------------------------------------------------------------- ask
    @app.post("/v1/graph/ask")
    async def ask(
        payload: AskRequest,
        tenant_id: str = Depends(current_tenant),
        deps: Deps = Depends(get_deps),
    ) -> dict[str, Any]:
        """NL->Cypher GraphRAG. The LLM only selects an allowlisted read-only
        template + params; the service renders the Cypher itself (tenant
        filter injected post-generation, rows capped at ASK_ROW_CAP)."""
        assert deps.llm is not None
        try:
            answer = await deps.llm.complete(build_prompt(payload.question))
        except AskUnavailable as exc:
            metrics.ask_requests.labels(result="unavailable").inc()
            raise HTTPException(
                status_code=503,
                detail={"reason": "ollama_unavailable", "message": str(exc)},
            ) from exc
        try:
            name, params = validate_selection(parse_answer(answer))
        except AskUnanswerable as exc:
            metrics.ask_requests.labels(result="unanswerable").inc()
            raise HTTPException(status_code=422, detail=str(exc)) from exc
        try:
            compiled = compile_template(
                name, params, row_cap=deps.settings.ask_row_cap
            )
        except TemplateError as exc:
            metrics.ask_requests.labels(result="invalid_params").inc()
            raise HTTPException(status_code=422, detail=str(exc)) from exc
        rows = await _run(deps, "ask", compiled, tenant_id)
        rows = rows[: deps.settings.ask_row_cap]
        metrics.ask_requests.labels(result="ok").inc()
        return {
            "question": payload.question,
            "template": name,
            "params": compiled.plan.params,
            "cypher": compiled.cypher,
            "row_count": len(rows),
            "rows": rows,
        }

    # --------------------------------------------------------------- persons
    @app.get("/v1/graph/persons/{person_id}")
    async def person_360(
        person_id: str,
        tenant_id: str = Depends(current_tenant),
        deps: Deps = Depends(get_deps),
    ) -> dict[str, Any]:
        """Person 360: contacts, bookings, consents, referrals, messages.
        Tenant-scoped by construction; cross-tenant ids answer 404."""
        parts: dict[str, Any] = {}
        for part, template_name in (
            ("person", "person_by_id"),
            ("contacts", "person_contacts"),
            ("bookings", "person_bookings"),
            ("consents", "person_consents"),
            ("referrals", "person_referrals"),
            ("messages", "person_messages"),
        ):
            compiled = compile_template(
                template_name,
                {"person_id": person_id},
                row_cap=deps.settings.query_row_cap,
            )
            parts[part] = await _run(deps, "person_360", compiled, tenant_id)
        if not parts["person"]:
            raise HTTPException(status_code=404, detail="person not found")
        parts["person"] = parts["person"][0]
        return parts

    # ---------------------------------------------------------------- cypher
    @app.post("/v1/graph/cypher")
    async def cypher(
        request: Request,
        tenant_id: str = Depends(current_tenant),
        deps: Deps = Depends(get_deps),
    ) -> dict[str, Any]:
        """Template-allowlisted parameterized queries ONLY (gate 5)."""
        body = await request.json()
        if not isinstance(body, dict):
            raise HTTPException(status_code=400, detail="JSON object body required")
        if "cypher" in body or "query" in body:
            raise HTTPException(
                status_code=400,
                detail="raw Cypher is not accepted; use an allowlisted template",
            )
        try:
            payload = CypherRequest.model_validate(body)
        except ValidationError as exc:
            raise HTTPException(status_code=400, detail=f"invalid request: {exc}") from exc
        try:
            compiled = compile_template(
                payload.template, payload.params, row_cap=deps.settings.query_row_cap
            )
        except TemplateError as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc
        rows = await _run(deps, "cypher", compiled, tenant_id)
        return {
            "template": payload.template,
            "cypher": compiled.cypher,
            "row_count": len(rows),
            "rows": rows,
        }

    return app


def main() -> None:
    import uvicorn

    structlog.configure(
        processors=[
            structlog.contextvars.merge_contextvars,
            structlog.processors.add_log_level,
            structlog.processors.TimeStamper(fmt="iso", utc=True),
            structlog.processors.JSONRenderer(),
        ],
        cache_logger_on_first_use=True,
    )
    settings = load_settings()
    app = create_app(settings)
    uvicorn.run(app, host=settings.host, port=settings.port, log_level="info")


if __name__ == "__main__":
    main()
