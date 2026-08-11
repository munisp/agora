"""conversation-service entrypoint (SPEC §7 conversation schema, §4 topics).

FastAPI + asyncpg + structlog. On startup: Postgres pool, transcript sink
(Fluvio/Kafka), Dapr client, transcript indexer task. Graceful shutdown via
the ASGI lifespan (uvicorn forwards SIGINT/SIGTERM).
"""

from __future__ import annotations

import contextlib
from collections.abc import AsyncIterator
from dataclasses import dataclass

import uvicorn
from fastapi import FastAPI
from fastapi.responses import JSONResponse

from .config import Config, load
from .dapr_client import DaprClient
from .db import Database
from .agent_db import AgentStore
from .agent_routes import router as agent_router
from .capture import CaptureExtractor
from .indexer import TranscriptIndexer
from .logging import get_logger, setup
from .privacy import PrivacyEraseConsumer
from .quality import CallQualityEnricher
from .retention import RetentionSweeper
from .routes import router
from .sinks import KafkaSink, TranscriptSink, build_sink
from .tenants import TenantResolver


@dataclass
class State:
    cfg: Config
    db: Database
    dapr: DaprClient
    sink: TranscriptSink
    intel_sink: TranscriptSink
    quality_sink: TranscriptSink
    indexer: TranscriptIndexer | None
    quality_enricher: CallQualityEnricher | None
    capture_extractor: CaptureExtractor | None
    privacy: PrivacyEraseConsumer | None
    retention: RetentionSweeper | None
    log: object


@contextlib.asynccontextmanager
async def lifespan(app: FastAPI) -> AsyncIterator[None]:
    setup()
    log = get_logger("conversation-service")
    cfg = load()

    db = Database(cfg)
    await db.connect()
    # SPEC-W3 §4 innovation 3: idempotent ALTER for enrichment columns.
    try:
        await db.ensure_intel_columns()
    except Exception as exc:
        log.error("intel column bootstrap failed; enrichment inserts will fail",
                  error=str(exc))
    # SPEC-W3 §3: idempotent ALTER + unique partial index for turn
    # Idempotency-Key dedupe.
    try:
        await db.ensure_turn_idempotency()
    except Exception as exc:
        log.error("idempotency bootstrap failed; keyed turn appends will 500",
                  error=str(exc))

    # SPEC-W12 contract §2: ussd channel enum on conversations + the GDPR
    # contact column (USSD caller number → incident IDP callback_number).
    # ensure_contact_column also runs under privacy_enabled below; both are
    # idempotent.
    try:
        await db.ensure_contact_column()
        await db.ensure_ussd_channel()
    except Exception as exc:
        log.error("ussd bootstrap failed; ussd conversations will fail",
                  error=str(exc))

    # SPEC-W38 F1/F3: agents registry + capture tables (init script
    # 07-agents-capture-schema.sql is authoritative on fresh installs; the
    # idempotent ensure covers already-initialized databases).
    agent_store = AgentStore(db)
    try:
        await agent_store.ensure_agent_tables()
    except Exception as exc:
        log.error("agents/capture table bootstrap failed; /v1/agents will fail",
                  error=str(exc))

    dapr = DaprClient(cfg.dapr_host, cfg.dapr_http_port, cfg.dapr_pubsub_name)

    # Tenant slug -> UUID resolution for ?tenant=<slug> (admin-web passes the
    # org slug). identity-service via Dapr invoke, TTL-cached; every success
    # is written through to the tenant_slugs projection so /v1/agents/resolve
    # can answer tenant_id -> slug for the voice runtime.
    tenant_resolver = TenantResolver(
        dapr,
        cfg.identity_app_id,
        cfg.tenant_cache_ttl_seconds,
        remember=agent_store.remember_tenant_slug,
    )

    sink = build_sink(cfg)
    try:
        await sink.start()
    except Exception as exc:
        # Transcript sink is a streaming optimization; Postgres is the source
        # of truth. Degrade to "log-only" rather than refusing to start.
        log.error("transcript sink start failed; turns will only hit Postgres+Dapr",
                  error=str(exc))
        sink = _NullSink()

    # Enriched turns (call intelligence) → opendesk.conversation.enriched.
    intel_sink: TranscriptSink = KafkaSink(cfg.kafka_brokers, cfg.enriched_topic)
    try:
        await intel_sink.start()
    except Exception as exc:
        log.error("enriched sink start failed; enrichment stays in Postgres",
                  error=str(exc))
        intel_sink = _NullSink()

    # CallQualityEnriched (avg sentiment) → opendesk.conversation.quality
    # (Wave 5 #2). Separate topic so enriched events never retrigger the
    # SessionEnded consumers on opendesk.conversation.events.
    quality_sink: TranscriptSink = KafkaSink(cfg.kafka_brokers, cfg.quality_topic)
    try:
        await quality_sink.start()
    except Exception as exc:
        log.error("quality sink start failed; CallQualityEnriched will not be published",
                  error=str(exc))
        quality_sink = _NullSink()

    indexer: TranscriptIndexer | None = None
    if cfg.indexer_enabled:
        indexer = TranscriptIndexer(cfg, db)
        indexer.start()

    quality_enricher: CallQualityEnricher | None = None
    if cfg.quality_enrich_enabled:
        quality_enricher = CallQualityEnricher(cfg, db, quality_sink)
        quality_enricher.start()

    # SPEC-W38 F3: post-call capture extraction (LLM, degrade = skip).
    # Publishes CaptureExtracted via the Dapr pubsub path (like transcripts)
    # so daprd owns broker delivery to opendesk.conversation.captures.
    capture_extractor: CaptureExtractor | None = None
    if cfg.capture_enabled:
        capture_extractor = CaptureExtractor(cfg, db, agent_store, dapr)
        capture_extractor.start()

    privacy: PrivacyEraseConsumer | None = None
    if cfg.privacy_enabled:
        try:
            await db.ensure_contact_column()
        except Exception as exc:
            log.error("contact column bootstrap failed; privacy erase will fail",
                      error=str(exc))
        privacy = PrivacyEraseConsumer(cfg, db)
        privacy.start()

    # NDPA/GDPR storage limitation: hourly hard-delete of aged turns
    # (RETENTION_DAYS, default 365; NDPA profile sets 180).
    retention: RetentionSweeper | None = None
    if cfg.retention_enabled:
        retention = RetentionSweeper(cfg, db)
        retention.start()

    app.state.cfg = cfg
    app.state.db = db
    app.state.agent_store = agent_store
    app.state.dapr = dapr
    app.state.tenant_resolver = tenant_resolver
    app.state.sink = sink
    app.state.intel_sink = intel_sink
    app.state.quality_sink = quality_sink
    app.state.log = log
    log.info("conversation-service started", port=cfg.port, sink=cfg.transcript_sink,
             intel_llm=cfg.intel_llm, quality_enrich=cfg.quality_enrich_enabled,
             capture=cfg.capture_enabled,
             retention_days=cfg.retention_days if cfg.retention_enabled else None)

    try:
        yield
    finally:
        log.info("conversation-service shutting down")
        if indexer is not None:
            await indexer.stop()
        if quality_enricher is not None:
            with contextlib.suppress(Exception):
                await quality_enricher.stop()
        if capture_extractor is not None:
            with contextlib.suppress(Exception):
                await capture_extractor.stop()
        if privacy is not None:
            with contextlib.suppress(Exception):
                await privacy.stop()
        if retention is not None:
            with contextlib.suppress(Exception):
                await retention.stop()
        with contextlib.suppress(Exception):
            await sink.close()
        with contextlib.suppress(Exception):
            await intel_sink.close()
        with contextlib.suppress(Exception):
            await quality_sink.close()
        with contextlib.suppress(Exception):
            await dapr.close()
        with contextlib.suppress(Exception):
            await db.close()


class _NullSink:
    async def publish(self, record: dict) -> None:
        return None

    async def close(self) -> None:
        return None


app = FastAPI(title="OpenDesk conversation-service", version="0.1.0", lifespan=lifespan)
app.include_router(router)
app.include_router(agent_router)


@app.get("/healthz")
async def healthz() -> JSONResponse:
    try:
        await app.state.db.ping()
    except Exception as exc:
        return JSONResponse(
            {"status": "unavailable", "error": str(exc)}, status_code=503
        )
    return JSONResponse({"status": "ok"})


def main() -> None:
    cfg = load()
    uvicorn.run(
        "app.main:app",
        host="0.0.0.0",
        port=cfg.port,
        log_config=None,  # structlog owns logging
        timeout_graceful_shutdown=15,
    )


if __name__ == "__main__":
    main()
