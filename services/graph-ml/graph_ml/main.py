"""FastAPI entry point (SPEC-W29 §3 WS-A): health, manual rescore, status,
plus the APScheduler interval sweep (SCORE_INTERVAL_MINUTES, default 60).

The service is a batch scorer; this HTTP API (:7016, internal-only in
compose) exists for on-demand rescore and ops visibility.
"""

from __future__ import annotations

import logging
import os
import re
import threading
from contextlib import asynccontextmanager
from dataclasses import asdict
from typing import Any

from apscheduler.schedulers.background import BackgroundScheduler
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse
from pydantic import BaseModel

from . import MODEL_VERSION_GNN_PREFIX, gnn_train
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


class ScoreTrainRequest(BaseModel):
    tenant_id: str | None = None


class ScoreTrainResponse(BaseModel):
    run_id: int
    trained: list[dict[str, Any]]
    skipped: list[dict[str, Any]]
    ok: bool


class _AppState:
    """Mutable run state guarded by a lock (scheduler + HTTP share it)."""

    def __init__(self, settings: Settings, backend: str) -> None:
        self.settings = settings
        self.backend = backend
        self.lock = threading.Lock()
        self.running = False
        self.run_count = 0
        self.train_count = 0
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
        train_minutes = getattr(settings, "train_interval_minutes", 0)
        if enable_scheduler and (settings.score_interval_minutes > 0 or train_minutes > 0):
            scheduler = BackgroundScheduler(daemon=True)
            if settings.score_interval_minutes > 0:
                scheduler.add_job(
                    _scheduled_sweep,
                    "interval",
                    minutes=settings.score_interval_minutes,
                    id="full-sweep",
                    max_instances=1,
                    coalesce=True,
                )
                log.info(
                    "sweep scheduler started",
                    extra={"interval_minutes": settings.score_interval_minutes},
                )
            if train_minutes > 0:
                scheduler.add_job(
                    _scheduled_train,
                    "interval",
                    minutes=train_minutes,
                    id="gnn-train",
                    max_instances=1,
                    coalesce=True,
                )
                log.info(
                    "gnn train scheduler started",
                    extra={"interval_minutes": train_minutes},
                )
            scheduler.start()
            state.scheduler = scheduler
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

    def execute_train(tenant_id: str | None) -> dict[str, Any]:
        """Train per-tenant GNN models (SPEC-W31 §2). Per-tenant failures
        (undersized graph, no data, train error) land in ``skipped`` — they
        NEVER fail the run; heuristic scoring stays the fallback path."""
        with state.lock:
            if state.running:
                raise RuntimeError("a scoring/training run is already in progress")
            state.running = True
        trained: list[dict[str, Any]] = []
        skipped: list[dict[str, Any]] = []
        try:
            graph_client = get_graph_client()
            if tenant_id:
                tenants = [tenant_id]
            else:
                try:
                    tenants = graph_client.list_tenants()
                except Exception as exc:  # noqa: BLE001
                    log.exception("tenant discovery failed")
                    skipped.append(
                        {
                            "tenant_id": "*",
                            "reason": f"tenant discovery: {type(exc).__name__}: {exc}",
                        }
                    )
                    tenants = []
            for tid in tenants:
                try:
                    graph = graph_client.fetch_tenant_graph(tid)
                except Exception as exc:  # noqa: BLE001
                    log.exception("tenant extraction failed", extra={"tenant_id": tid})
                    skipped.append(
                        {"tenant_id": tid, "reason": f"no data: {type(exc).__name__}: {exc}"}
                    )
                    continue
                try:
                    result = gnn_train.train_tenant(graph, tid, settings)
                    trained.append(
                        {
                            "tenant_id": tid,
                            "model_version": result.model_version,
                            "final_loss": result.final_loss,
                        }
                    )
                except gnn_train.GNNInsufficientData as exc:
                    log.info(
                        "tenant below GNN min-size gate; skipped",
                        extra={"tenant_id": tid},
                    )
                    skipped.append({"tenant_id": tid, "reason": f"insufficient data: {exc}"})
                except gnn_train.GNNBackendUnavailable as exc:
                    log.warning("gnn backend unavailable; tenant skipped", extra={"tenant_id": tid})
                    skipped.append({"tenant_id": tid, "reason": f"backend unavailable: {exc}"})
                except Exception as exc:  # noqa: BLE001 - isolation: skip, never fail the run
                    log.exception("tenant training failed", extra={"tenant_id": tid})
                    skipped.append(
                        {"tenant_id": tid, "reason": f"train error: {type(exc).__name__}: {exc}"}
                    )
            with state.lock:
                state.train_count += 1
                run_id = state.train_count
            return {"run_id": run_id, "trained": trained, "skipped": skipped, "ok": True}
        finally:
            with state.lock:
                state.running = False

    def _scheduled_train() -> None:
        if state.backend != "gnn":
            return  # train scheduler is a no-op in heuristic mode
        try:
            execute_train(None)
        except Exception:  # noqa: BLE001 - scheduler must never die
            log.exception("scheduled gnn train failed")

    state.execute_train = execute_train  # type: ignore[attr-defined]

    @app.get("/healthz")
    def healthz() -> dict[str, Any]:
        body: dict[str, Any] = {
            "status": "ok",
            "service": "graph-ml",
            "backend": state.backend,
            "gnn_available": GNN_AVAILABLE,
        }
        # W31: GNN model-registry visibility. Best-effort — any filesystem
        # error omits these fields; /healthz must never 500 on it.
        try:
            model_dir = settings.model_dir
            version_dir = re.compile(rf"^{re.escape(MODEL_VERSION_GNN_PREFIX)}\d+$")
            tenants_with_models = 0
            if os.path.isdir(model_dir):
                for entry in os.listdir(model_dir):
                    tenant_path = os.path.join(model_dir, entry)
                    if os.path.isdir(tenant_path) and any(
                        version_dir.match(d) for d in os.listdir(tenant_path)
                    ):
                        tenants_with_models += 1
            body["gnn_models_dir"] = model_dir
            body["gnn_tenants_with_models"] = tenants_with_models
        except Exception:  # noqa: BLE001 - best-effort fields only
            log.debug("healthz gnn model stats unavailable", exc_info=True)
        return body

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

    @app.post("/v1/score/train")
    def score_train(body: ScoreTrainRequest):
        """Manual GNN training trigger (SPEC-W31 §2). Honest degradation:
        409 when the gnn backend is not enabled; per-tenant failures are
        reported in ``skipped`` and never fail the run."""
        if state.backend != "gnn":
            return JSONResponse(
                status_code=409, content={"error": "gnn backend not enabled"}
            )
        result = execute_train(body.tenant_id)
        return ScoreTrainResponse(
            run_id=result["run_id"],
            trained=result["trained"],
            skipped=result["skipped"],
            ok=result["ok"],
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
