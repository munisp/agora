"""Entrypoint: bootstrap Iceberg, start Kafka consumer + sidecar HTTP server.

Startup is retry-loop resilient: in the dev compose the Iceberg REST catalog and
Kafka may still be booting when this container starts.
"""

from __future__ import annotations

import asyncio
import signal

import structlog
import uvicorn

from .cac_consumer import CacConsumer
from .cac_store import CacStore
from .cac_summary import BookingSpendClient
from .config import load_settings
from .consumer import BronzeConsumer
from .dapr_client import DaprClient
from .iceberg_tables import IcebergSink, ensure_bronze, load_rest_catalog
from .server import CacDeps, create_app
from .tenants import TenantResolver

log = structlog.get_logger()


def configure_logging() -> None:
    structlog.configure(
        processors=[
            structlog.contextvars.merge_contextvars,
            structlog.processors.add_log_level,
            structlog.processors.TimeStamper(fmt="iso", utc=True),
            structlog.processors.StackInfoRenderer(),
            structlog.processors.format_exc_info,
            structlog.processors.JSONRenderer(),
        ],
        wrapper_class=structlog.make_filtering_bound_logger(0),
        cache_logger_on_first_use=True,
    )


async def _with_retry(fn, what: str, settings) -> None:
    for attempt in range(1, settings.startup_max_attempts + 1):
        try:
            await fn()
            return
        except Exception as exc:
            log.warning(
                "startup.retry",
                dependency=what,
                attempt=attempt,
                error=f"{type(exc).__name__}: {exc}",
            )
            await asyncio.sleep(settings.startup_retry_seconds)
    raise RuntimeError(f"{what} not reachable after {settings.startup_max_attempts} attempts")


async def amain() -> None:
    configure_logging()
    settings = load_settings()
    log.info("startup.settings", **{
        "kafka": settings.kafka_bootstrap_servers,
        "group": settings.kafka_group_id,
        "batch_size": settings.batch_size,
        "flush_interval": settings.flush_interval_seconds,
        "iceberg_rest": settings.iceberg_rest_uri,
        "warehouse": settings.iceberg_warehouse,
        "s3_endpoint": settings.aws_endpoint_url,
        "port": settings.port,
    })

    # Iceberg catalog + bronze tables (blocking client calls -> thread).
    catalog = load_rest_catalog(settings)
    if settings.auto_create_tables:
        async def _ensure():
            await asyncio.to_thread(ensure_bronze, catalog)
        await _with_retry(_ensure, "iceberg-rest", settings)
        log.info("iceberg.bronze_ready")

    sink = IcebergSink(catalog)
    consumer = BronzeConsumer(settings, sink)
    await _with_retry(consumer.start, "kafka", settings)

    # SPEC-W13: CAC rollup module (cac.events -> Postgres analytics_meta).
    cac_deps: CacDeps | None = None
    cac_consumer: CacConsumer | None = None
    dapr: DaprClient | None = None
    if settings.cac_consumer_enabled:
        cac_store = CacStore(settings)
        await _with_retry(cac_store.connect, "postgres", settings)
        await _with_retry(cac_store.ensure_schema, "postgres-ddl", settings)
        dapr = DaprClient(settings.dapr_host, settings.dapr_http_port)
        cac_deps = CacDeps(
            store=cac_store,
            spend=BookingSpendClient(dapr, settings.booking_app_id),
            tenants=TenantResolver(
                dapr, settings.identity_app_id, settings.tenant_cache_ttl_seconds
            ),
        )
        cac_consumer = CacConsumer(settings, cac_store)
        await _with_retry(cac_consumer.start, "kafka-cac", settings)
    else:
        log.info("cac.disabled")

    ready_flag = {"ready": True}
    app = create_app(consumer, ready_flag, settings, cac_deps)
    server = uvicorn.Server(
        uvicorn.Config(app, host=settings.host, port=settings.port,
                       log_level="info", access_log=False)
    )

    stop_event = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(sig, stop_event.set)

    consumer_task = asyncio.create_task(consumer.run(stop_event))
    server_task = asyncio.create_task(server.serve())
    cac_task = (
        asyncio.create_task(cac_consumer.run(stop_event))
        if cac_consumer is not None
        else None
    )

    await stop_event.wait()
    log.info("shutdown.requested")
    ready_flag["ready"] = False
    server.should_exit = True
    await consumer.stop()
    if cac_consumer is not None:
        await cac_consumer.stop()
    tasks = [consumer_task, server_task] + ([cac_task] if cac_task else [])
    await asyncio.gather(*tasks, return_exceptions=True)
    if cac_deps is not None:
        await cac_deps.store.close()
    if dapr is not None:
        await dapr.aclose()
    log.info("shutdown.complete")


def main() -> None:
    try:
        asyncio.run(amain())
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
