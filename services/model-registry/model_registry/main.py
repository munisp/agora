"""FastAPI entry point (SPEC-W33 §4 C1/C2/C3/C5): registry REST, A/B
assignment + report, drift sweep + nightly training schedulers, /healthz.

Scheduler pattern mirrors services/graph-ml/graph_ml/main.py (BackgroundScheduler
started in lifespan, jobs wrapped so they never crash the scheduler — I1).
"""

from __future__ import annotations

import logging
from contextlib import asynccontextmanager
from datetime import datetime, timezone
from typing import Any, Mapping

from apscheduler.schedulers.background import BackgroundScheduler
from fastapi import FastAPI, Query, Request
from fastapi.responses import JSONResponse, PlainTextResponse
from prometheus_client import CONTENT_TYPE_LATEST, CollectorRegistry, Gauge, generate_latest
from pydantic import BaseModel, Field

from . import ab
from .alerts import AlertPublisher, build_publisher
from .config import Settings, load_settings
from .drift import DirectoryManifestProvider, DriftJob, ManifestProvider
from .store import Conflict, NotFound, RegistryStore
from .trainer import FamilyTrainer, run_nightly_tick

log = logging.getLogger(__name__)


# --------------------------------------------------------------------- models
class RegisterRequest(BaseModel):
    family: str
    tenant_id: str
    artifact_uri: str
    metrics: dict[str, Any] = Field(default_factory=dict)
    seed: int | None = None
    dataset_hash: str | None = None
    git_sha: str | None = None
    version: int | None = None


class PromoteRequest(BaseModel):
    family: str
    tenant_id: str
    version: int


class RollbackRequest(BaseModel):
    family: str
    tenant_id: str


class ExperimentRequest(BaseModel):
    family: str
    tenant_id: str
    champion_version: int
    challenger_version: int
    pct: int = Field(ge=0, le=100)
    starts_at: datetime | None = None
    ends_at: datetime | None = None


class OutcomeRequest(BaseModel):
    tenant_id: str
    person_id: str
    assigned_arm: str = Field(pattern="^(champion|challenger)$")
    predicted_label: int = Field(ge=0, le=1)
    predicted_score: float = Field(ge=0.0, le=1.0)
    true_label: int | None = Field(default=None, ge=0, le=1)


class ScoresRequest(BaseModel):
    family: str
    tenant_id: str
    scores: list[float]


class FeaturesRequest(BaseModel):
    family: str
    tenant_id: str
    features: dict[str, list[float]]


# ------------------------------------------------------------------ app state
class _AppState:
    def __init__(self, settings: Settings, store: RegistryStore,
                 publisher: AlertPublisher,
                 trainers: Mapping[str, FamilyTrainer],
                 manifests: ManifestProvider, gauge: Gauge) -> None:
        self.settings = settings
        self.store = store
        self.publisher = publisher
        self.trainers = trainers
        self.manifests = manifests
        self.gauge = gauge
        self.scheduler: BackgroundScheduler | None = None


def create_app(
    settings: Settings | None = None,
    store: RegistryStore | None = None,
    publisher: AlertPublisher | None = None,
    trainers: Mapping[str, FamilyTrainer] | None = None,
    manifests: ManifestProvider | None = None,
    metric_registry: CollectorRegistry | None = None,
    enable_scheduler: bool = True,
) -> FastAPI:
    settings = settings or load_settings()
    store = store or RegistryStore(settings.pg_dsn)
    publisher = publisher or build_publisher(
        kafka_enabled=settings.kafka_enabled,
        bootstrap_servers=settings.kafka_bootstrap_servers)
    trainers = trainers or {}
    manifests = manifests or DirectoryManifestProvider(settings.drift_manifest_dir)
    metric_registry = metric_registry or CollectorRegistry()
    gauge = Gauge("opendesk_model_drift_psi",
                  "PSI of serving distributions vs training reference "
                  "(SPEC-W33 §4 C2); >0.25 alerts on ops.alerts",
                  ["family", "tenant"], registry=metric_registry)
    state = _AppState(settings, store, publisher, trainers, manifests, gauge)

    def _drift_tick() -> None:
        try:
            job = DriftJob(store, manifests, publisher,
                           threshold=settings.drift_psi_threshold,
                           gauge=gauge,
                           window_minutes=settings.drift_interval_minutes,
                           alerts_topic=settings.alerts_topic)
            run = job.run_once()
            log.info("drift sweep: checked=%d findings=%d skipped=%d",
                     run.checked, len(run.findings), len(run.skipped))
        except Exception:  # noqa: BLE001 — I1: never crash the scheduler
            log.exception("drift sweep tick failed")

    def _train_tick() -> None:
        try:
            results = run_nightly_tick(
                store, trainers, alerter=publisher,
                git_sha=settings.git_sha,
                brier_max=settings.train_brier_max,
                aucpr_tolerance=settings.train_aucpr_tolerance,
                alerts_topic=settings.alerts_topic)
            log.info("nightly training tick: %s",
                     [(r.family, r.decision) for r in results])
        except Exception:  # noqa: BLE001 — I1
            log.exception("nightly training tick failed")

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        if enable_scheduler and (settings.drift_interval_minutes > 0
                                 or settings.train_enabled):
            scheduler = BackgroundScheduler(daemon=True)
            if settings.drift_interval_minutes > 0:
                scheduler.add_job(
                    _drift_tick, "interval",
                    minutes=settings.drift_interval_minutes,
                    id="drift-sweep", max_instances=1, coalesce=True)
                log.info("drift scheduler started (every %d min)",
                         settings.drift_interval_minutes)
            if settings.train_enabled:
                scheduler.add_job(
                    _train_tick, "cron", hour=settings.train_cron_hour,
                    id="nightly-train", max_instances=1, coalesce=True)
                log.info("nightly training scheduler started (hour=%d)",
                         settings.train_cron_hour)
            scheduler.start()
            state.scheduler = scheduler
        yield
        if state.scheduler is not None:
            state.scheduler.shutdown(wait=False)

    app = FastAPI(title="opendesk-model-registry",
                  version="0.1.0", lifespan=lifespan)

    # ------------------------------------------------------------ errors (honest)
    @app.exception_handler(NotFound)
    async def _nf(_: Request, exc: NotFound):
        return JSONResponse(status_code=404, content={"detail": str(exc)})

    @app.exception_handler(Conflict)
    async def _cf(_: Request, exc: Conflict):
        return JSONResponse(status_code=409, content={"detail": str(exc)})

    # ------------------------------------------------------------------ health
    @app.get("/healthz")
    def healthz():
        if state.store.health():
            return {"status": "ok"}
        return JSONResponse(status_code=503, content={"status": "db-unreachable"})

    @app.get("/metrics")
    def metrics():
        return PlainTextResponse(generate_latest(metric_registry),
                                 media_type=CONTENT_TYPE_LATEST)

    # ----------------------------------------------------------- C1 registry
    @app.post("/v1/registry/register", status_code=201)
    def register(req: RegisterRequest):
        return state.store.register_version(
            family=req.family, tenant_id=req.tenant_id,
            artifact_uri=req.artifact_uri, metrics=req.metrics,
            seed=req.seed, dataset_hash=req.dataset_hash,
            git_sha=req.git_sha, version=req.version)

    @app.post("/v1/registry/promote")
    def promote(req: PromoteRequest):
        return state.store.promote(req.family, req.tenant_id, req.version)

    @app.post("/v1/registry/rollback")
    def rollback(req: RollbackRequest):
        return state.store.rollback(req.family, req.tenant_id)

    @app.get("/v1/registry/{family}/{tenant_id}/production")
    def production(family: str, tenant_id: str):
        row = state.store.get_production(family, tenant_id)
        if row is None:
            raise NotFound(f"no production version for {family}/{tenant_id}")
        return row

    @app.get("/v1/registry/{family}/{tenant_id}/versions")
    def versions(family: str, tenant_id: str):
        return {"family": family, "tenant_id": tenant_id,
                "versions": state.store.list_versions(family, tenant_id)}

    # ---------------------------------------------------------------- C3 A/B
    @app.post("/v1/registry/experiments", status_code=201)
    def create_experiment(req: ExperimentRequest):
        return state.store.create_experiment(
            family=req.family, tenant_id=req.tenant_id,
            champion_version=req.champion_version,
            challenger_version=req.challenger_version, pct=req.pct,
            starts_at=req.starts_at, ends_at=req.ends_at)

    @app.get("/v1/registry/experiments/assignment")
    def assignment(family: str = Query(...), tenant_id: str = Query(...),
                   person_id: str = Query(...)):
        """Per-request assignment for scoring services. FAIL-CLOSED to
        champion on missing experiment or any error."""
        now = datetime.now(timezone.utc)
        try:
            experiment = state.store.get_active_experiment(family, tenant_id, now)
        except Exception:  # noqa: BLE001 — fail-closed (I1)
            log.exception("assignment lookup failed; failing closed to champion")
            experiment = None
        arm = ab.assign_arm(experiment, tenant_id=tenant_id,
                            person_id=person_id, now=now)
        version = ab.arm_version(experiment, arm)
        if version is None:
            prod = state.store.get_production(family, tenant_id)
            version = prod["version"] if prod else None
        return {
            "family": family, "tenant_id": tenant_id, "person_id": person_id,
            "experiment_id": experiment["id"] if experiment else None,
            "arm": arm, "version": version,
        }

    @app.post("/v1/registry/experiments/{experiment_id}/outcomes",
              status_code=201)
    def record_outcome(experiment_id: str, req: OutcomeRequest):
        return state.store.record_outcome(
            experiment_id=experiment_id, tenant_id=req.tenant_id,
            person_id=req.person_id, assigned_arm=req.assigned_arm,
            predicted_label=req.predicted_label,
            predicted_score=req.predicted_score, true_label=req.true_label)

    @app.get("/v1/registry/experiments/{experiment_id}/report")
    def experiment_report(experiment_id: str):
        """Champion vs challenger precision/recall/Brier over labeled
        outcomes. 404 when the experiment does not exist (honest empty)."""
        experiment = state.store.get_experiment(experiment_id)
        if experiment is None:
            raise NotFound(f"experiment {experiment_id} not found")
        arms = state.store.experiment_report(experiment_id,
                                             experiment["tenant_id"])
        return {
            "experiment_id": experiment_id,
            "family": experiment["family"],
            "tenant_id": experiment["tenant_id"],
            "champion_version": experiment["champion_version"],
            "challenger_version": experiment["challenger_version"],
            "pct": experiment["pct"],
            "status": experiment["status"],
            "arms": arms,
            "note": "promotion of a winner is MANUAL via /v1/registry/promote",
        }

    # ------------------------------------------- drift observation write path
    @app.post("/v1/registry/observations/scores", status_code=202)
    def record_scores(req: ScoresRequest):
        n = state.store.record_scores(family=req.family,
                                      tenant_id=req.tenant_id,
                                      scores=req.scores)
        return {"recorded": n}

    @app.post("/v1/registry/observations/features", status_code=202)
    def record_features(req: FeaturesRequest):
        n = state.store.record_features(family=req.family,
                                        tenant_id=req.tenant_id,
                                        features=req.features)
        return {"recorded": n}

    return app


app = create_app()
