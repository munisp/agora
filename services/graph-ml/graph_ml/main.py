"""FastAPI entry point (SPEC-W29 §3 WS-A): health, manual rescore, status,
plus the APScheduler interval sweep (SCORE_INTERVAL_MINUTES, default 60).

The service is a batch scorer; this HTTP API (:7016, internal-only in
compose) exists for on-demand rescore and ops visibility.
"""

from __future__ import annotations

import logging
import threading
from contextlib import asynccontextmanager
from dataclasses import asdict
from typing import Any

from apscheduler.schedulers.background import BackgroundScheduler
from fastapi import FastAPI, Request
from pydantic import BaseModel

from .config import Settings, load_settings
from .extract import GraphClient, client_from_settings
from .gnn import GNN_AVAILABLE, resolve_backend
from .score import SweepResult, run_sweep
from .writeback import HttpWritebackClient, WritebackClient

log = logging.getLogger(__name__)


class ScoreRunRequest(BaseModel):
    tenant_id: str | None = None


class ScoreRunResponse(BaseModel):
    run_id: int
    backend: str
    tenants: list[dict[str, Any]]
    ok: bool


class _AppState:
    """Mutable run state guarded by a lock (scheduler + HTTP share it)."""

    def __init__(self, settings: Settings, backend: str) -> None:
        self.settings = settings
        self.backend = backend
        self.lock = threading.Lock()
        self.running = False
        self.run_count = 0
        self.last_sweep: SweepResult | None = None
        self.scheduler: BackgroundScheduler | None = None


def _build_writer(settings: Settings) -> WritebackClient:
    return HttpWritebackClient(
        base_url=settings.graph_service_url,
        internal_token=settings.internal_token,
        chunk_size=settings.writeback_chunk_size,
        timeout_s=settings.http_timeout_s,
    )


def create_app(
    settings: Settings | None = None,
    graph_client: GraphClient | None = None,
    writer: WritebackClient | None = None,
    enable_scheduler: bool = True,
) -> FastAPI:
    settings = settings or load_settings()
    backend = resolve_backend(settings)  # gnn -> heuristic fallback warns here
    state = _AppState(settings, backend)

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        if enable_scheduler and settings.score_interval_minutes > 0:
            scheduler = BackgroundScheduler(daemon=True)
            scheduler.add_job(
                _scheduled_sweep,
                "interval",
                minutes=settings.score_interval_minutes,
                id="full-sweep",
                max_instances=1,
                coalesce=True,
            )
            scheduler.start()
            state.scheduler = scheduler
            log.info(
                "sweep scheduler started",
                extra={"interval_minutes": settings.score_interval_minutes},
            )
        yield
        if state.scheduler is not None:
            state.scheduler.shutdown(wait=False)

    app = FastAPI(title="graph-ml", version="0.1.0", lifespan=lifespan)
    app.state.graph_ml = state

    # Injected seams (tests) or lazily-built real clients (production).
    injected_graph = graph_client
    injected_writer = writer

    def get_graph_client() -> GraphClient:
        if injected_graph is not None:
            return injected_graph
        if getattr(state, "_graph_client", None) is None:
            state._graph_client = client_from_settings(settings)
        return state._graph_client

    def get_writer() -> WritebackClient:
        if injected_writer is not None:
            return injected_writer
        if getattr(state, "_writer", None) is None:
            state._writer = _build_writer(settings)
        return state._writer

    def execute_sweep(tenant_id: str | None) -> SweepResult:
        with state.lock:
            if state.running:
                raise RuntimeError("a scoring run is already in progress")
            state.running = True
        try:
            sweep = run_sweep(settings, get_graph_client(), get_writer(), tenant_id=tenant_id)
            with state.lock:
                state.run_count += 1
                state.last_sweep = sweep
            return sweep
        finally:
            with state.lock:
                state.running = False

    def _scheduled_sweep() -> None:
        try:
            execute_sweep(None)
        except Exception:  # noqa: BLE001 - scheduler must never die
            log.exception("scheduled sweep failed")

    state.execute_sweep = execute_sweep  # type: ignore[attr-defined]

    @app.get("/healthz")
    def healthz() -> dict[str, Any]:
        return {
            "status": "ok",
            "service": "graph-ml",
            "backend": state.backend,
            "gnn_available": GNN_AVAILABLE,
        }

    @app.post("/v1/score/run", response_model=ScoreRunResponse)
    def score_run(body: ScoreRunRequest) -> ScoreRunResponse:
        """Manual trigger: full sweep, or one tenant when tenant_id is set."""
        sweep = execute_sweep(body.tenant_id)
        return ScoreRunResponse(
            run_id=state.run_count,
            backend=sweep.backend,
            tenants=[asdict(t) for t in sweep.tenants],
            ok=sweep.ok,
        )

    @app.get("/v1/score/status")
    def score_status(request: Request) -> dict[str, Any]:
        with state.lock:
            last = state.last_sweep
            next_run = None
            if state.scheduler is not None:
                job = state.scheduler.get_job("full-sweep")
                if job is not None and job.next_run_time is not None:
                    next_run = job.next_run_time.isoformat()
            return {
                "backend": state.backend,
                "gnn_available": GNN_AVAILABLE,
                "running": state.running,
                "run_count": state.run_count,
                "score_interval_minutes": settings.score_interval_minutes,
                "top_k": settings.top_k,
                "next_scheduled_run": next_run,
                "last_sweep": (
                    None
                    if last is None
                    else {
                        "started_at": last.started_at,
                        "finished_at": last.finished_at,
                        "ok": last.ok,
                        "tenants": [asdict(t) for t in last.tenants],
                    }
                ),
            }

    return app


app = create_app()
