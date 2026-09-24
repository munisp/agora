"""conversation-service entrypoint (SPEC §7 conversation schema, §4 topics).

FastAPI + asyncpg + structlog. On startup: Postgres pool, transcript sink
(Fluvio/Kafka), Dapr client, transcript indexer task. Graceful shutdown via
the ASGI lifespan (uvicorn forwards SIGINT/SIGTERM).
"""

from __future__ import annotations

import contextlib
from collections.abc import AsyncIterator
from dataclasses import dataclass

import httpx
import uvicorn
from fastapi import FastAPI
from fastapi.responses import JSONResponse, PlainTextResponse
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest

from .config import Config, load
from .dapr_client import DaprClient
from .db import Database
from .agent_db import AgentStore
from .agent_routes import router as agent_router
from .capture import CaptureExtractor
from .helpdesk import HelpdeskAutomation
from .indexer import TranscriptIndexer
from . import incidents as incidents_mod
from .internal_routes import router as internal_router
from .logging import get_logger, setup
from .privacy import PrivacyEraseConsumer
from .quality import CallQualityEnricher
from .outbox import OutboxRelay
from .retention import RetentionSweeper
from .routes import router
from .sinks import KafkaSink, TranscriptSink, build_sink
from .tenant_lifecycle import TenantLifecycleConsumer
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
    helpdesk: HelpdeskAutomation | None
    tenant_lifecycle: TenantLifecycleConsumer | None
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

    # SPEC-W43 Y-03/Y-06/Y-08: durable relay tables (incident_counters +
    # incident_emitted + conversation_outbox) created at boot OUTSIDE any
    # tenant-scoped tx, with fail-closed RLS + FORCE (NULLIF idiom).
    try:
        await db.ensure_relay_tables()
    except Exception as exc:
        log.error("relay table bootstrap failed; incident dedupe and the turn "
                  "outbox will fail", error=str(exc))

    # W46-F (P-DATA DDL #4/#9): hot-path indexes — incident unsent poll +
    # retention sweep by turns.ts. Idempotent; failure is non-fatal (the
    # queries still work, just slower) and logged loudly.
    try:
        await db.ensure_perf_indexes()
    except Exception as exc:
        log.error("perf index bootstrap failed; relay poll and retention "
                  "sweep will seq-scan", error=str(exc))

    # W46-F P10: shared app-lifetime httpx client for the intel LLM NER
    # (keep-alive across turns; previously one client per turn = TCP/TLS
    # handshake per persist). Created only when INTEL_LLM=on.
    intel_client: httpx.AsyncClient | None = None
    if cfg.intel_llm:
        intel_client = httpx.AsyncClient(
            timeout=httpx.Timeout(cfg.intel_llm_timeout_s)
        )

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
    # can answer tenant_id -> slug for the voice dial-plan without a second
    # invoke.
    resolver = TenantResolver(
        dapr,
        identity_app_id=cfg.identity_app_id,
        ttl_s=cfg.tenant_cache_ttl_s,
        store=agent_store,
        internal_token=cfg.identity_internal_token,
        base_url=cfg.identity_base_url,
    )

    # Transcript sinks (SPEC §5): default both to Fluvio; the knowledge
    # indexer consumes intel_sink.
    sink = build_sink(cfg.fluvio_profile_path, cfg.fluvio_topic, cfg.kafka_brokers)
    intel_sink = build_sink(cfg.fluvio_profile_path, cfg.fluvio_intel_topic,
                            cfg.kafka_brokers)
    quality_sink = build_sink(cfg.fluvio_profile_path, cfg.fluvio_quality_topic,
                              cfg.kafka_brokers)

    # Knowledge indexer (SPEC-W3 §4 innovation 4): consumes
    # opendesk.conversation.intel.
    indexer = None
    if cfg.indexer_enabled:
        indexer = TranscriptIndexer(cfg, intel_sink, log)
        await indexer.start()

    # Call-quality enrichment (SPEC-W3 §4 innovation 5): subscribes
    # TurnEnded on conversation.events via Dapr /subscribe, publishes
    # CallQuality metrics to opendesk.conversation.quality.
    quality_enricher = None
    if cfg.quality_enabled:
        quality_enricher = CallQualityEnricher(cfg, db, quality_sink, log)

    # Field-capture extraction (SPEC-W16 contract §5): subscribes the same
    # TurnEnded consumer group; POSTs to booking /v1/field/capture.
    capture_extractor = None
    if cfg.capture_enabled:
        capture_extractor = CaptureExtractor(cfg, db, log)

    # SPEC-W44 W-06: the booking-persistence path (Graph EntityExtractor +
    # Projector, app/entities.py) was removed this wave — read models now
    # persist via graph-sync consuming opendesk.conversation.events
    # (services/graph-sync/app/graph_conversations.py). DB plumbing deleted:
    # _ENTITY_TABLES_DDL, Database.ensure_entities_schema,
    # Database.upsert_graph_entity, Database.upsert_graph_edge.

    # SPEC-W43 Y-06: transactional-outbox relay for turn CloudEvents (Turns
    # land on opendesk.conversation.events via conversation_outbox; the relay
    # republishes until published_at, then the direct Dapr publish in
    # db.add_turn is the fast path).
    outbox_relay = OutboxRelay(db, dapr, cfg, log)
    await outbox_relay.start()

    # GDPR erasure consumer (opendesk.privacy.events → anonymization).
    privacy = None
    if cfg.privacy_enabled:
        await db.ensure_contact_column()
        privacy = PrivacyEraseConsumer(cfg, db, log)
        await privacy.start()

    # Conversation retention sweeper (SPEC-W3 §3 innovation 3).
    retention = None
    if cfg.retention_enabled:
        retention = RetentionSweeper(cfg, db, log)
        await retention.start()

    # Incident auto-detection sweep (SPEC-W11 Part A): republishes IDPs that
    # were emitted to incident_emitted but never published (Y-03 durable
    # gate: the turn path records first, this sweep drains stragglers).
    incident_sweeper = incidents_mod.IncidentSweeper(db, dapr, cfg, log)
    await incident_sweeper.start()

    # Helpdesk automation (SPEC-W19 contract §6): consumes
    # TurnEnded and drives auto-ticket creation + escalations.
    helpdesk = None
    if cfg.helpdesk_enabled:
        helpdesk = HelpdeskAutomation(cfg, db, dapr, log)
        await helpdesk.start()

    # Tenant lifecycle (SPEC-W44 W-D-3): twin Deleted tombstones purge the
    # tenant's rows and drop its tenant_slugs projection entry.
    tenant_lifecycle = None
    if cfg.tenant_lifecycle_enabled:
        tenant_lifecycle = TenantLifecycleConsumer(cfg, db, agent_store, log)
        await tenant_lifecycle.start()

    app.state.cfg = cfg
    app.state.db = db
    app.state.dapr = dapr
    app.state.sink = sink
    app.state.intel_sink = intel_sink
    app.state.quality_sink = quality_sink
    app.state.log = log
    app.state.resolver = resolver
    app.state.helpdesk = helpdesk
    app.state.outbox_relay = outbox_relay
    app.state.agent_store = agent_store
    app.state.incident_sweeper = incident_sweeper

    yield

    await incident_sweeper.stop()
    if tenant_lifecycle is not None:
        await tenant_lifecycle.stop()
    if helpdesk is not None:
        await helpdesk.stop()
    if retention is not None:
        await retention.stop()
    if privacy is not None:
        await privacy.stop()
    if capture_extractor is not None:
        await capture_extractor.stop()
    if indexer is not None:
        await indexer.stop()
    await outbox_relay.stop()
    if intel_client is not None:
        await intel_client.aclose()
    await dapr.close()
    await sink.close()
    if intel_sink is not sink:
        await intel_sink.close()
    if quality_sink is not sink and quality_sink is not intel_sink:
        await quality_sink.close()
    await db.close()


app = FastAPI(title="conversation-service", lifespan=lifespan)
app.include_router(router)
app.include_router(agent_router)
app.include_router(internal_router)


@app.get("/healthz")
async def healthz() -> JSONResponse:
    try:
        await app.state.db.ping()
        return JSONResponse({"status": "ok"})
    except Exception as exc:  # noqa: BLE001 - honest 503
        return JSONResponse({"status": "db-unreachable", "error": str(exc)},
                            status_code=503)


@app.get("/metrics")
async def metrics() -> PlainTextResponse:
    return PlainTextResponse(generate_latest(), media_type=CONTENT_TYPE_LATEST)


def main() -> None:
    cfg = load()
    uvicorn.run(
        "app.main:app",
        host="0.0.0.0",
        port=cfg.port,
        log_level=cfg.log_level,
    )


if __name__ == "__main__":
    main()
