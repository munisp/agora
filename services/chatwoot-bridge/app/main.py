"""chatwoot-bridge service entrypoint (SPEC-W118).

FastAPI app wiring: Postgres store (ensure* DDL bootstrap), Chatwoot +
conversation-service clients, the outbound Kafka consumer (SYNC_ENABLED),
webhook + admin routers, /healthz.

Test seam: create_app(..., store=, chatwoot_client=, conv_client=) — DI
overrides used ONLY by tests; production main() never passes them.
"""

from __future__ import annotations

import contextlib

import uvicorn
from fastapi import FastAPI
from fastapi.responses import JSONResponse

from . import logging as logging_mod
from .admin import router as admin_router
from .client import ChatwootClient
from .config import Settings, load
from .consumer import BridgeConsumer
from .conversations import ConversationServiceClient
from .store import Database
from .webhook import router as webhook_router


def create_app(
    cfg: Settings | None = None,
    *,
    store=None,
    chatwoot_client=None,
    conv_client=None,
) -> FastAPI:
    cfg = cfg or Settings()

    @contextlib.asynccontextmanager
    async def lifespan(app: FastAPI):
        logging_mod.setup()
        if store is not None:
            app.state.store = store
        else:
            real_store = Database(cfg.pg_dsn)
            await real_store.connect()
            await real_store.ensure_schema()
            app.state.store = real_store

        app.state.chatwoot = chatwoot_client or ChatwootClient(
            cfg.chatwoot_base_url, timeout=cfg.chatwoot_timeout_s
        )
        app.state.conv_client = conv_client or ConversationServiceClient(
            cfg.conversation_service_url,
            token=cfg.chatwoot_conversation_internal_token,
            timeout=cfg.conversation_timeout_s,
        )

        consumer = None
        if cfg.sync_enabled:
            consumer = BridgeConsumer(
                cfg, app.state.store, app.state.chatwoot, app.state.conv_client
            )
            consumer.start()
        app.state.consumer = consumer
        try:
            yield
        finally:
            if consumer is not None:
                await consumer.stop()
            if chatwoot_client is None:
                await app.state.chatwoot.aclose()
            if conv_client is None:
                await app.state.conv_client.aclose()
            if store is None:
                await app.state.store.aclose()

    app = FastAPI(title="opendesk-chatwoot-bridge", lifespan=lifespan)
    app.state.cfg = cfg
    app.include_router(webhook_router)
    app.include_router(admin_router)

    @app.get("/healthz")
    async def healthz():
        ok = await app.state.store.ping()
        return JSONResponse(
            {"status": "ok" if ok else "degraded"},
            status_code=200 if ok else 503,
        )

    return app


def main() -> None:
    cfg = load()
    app = create_app(cfg)
    uvicorn.run(app, host="0.0.0.0", port=cfg.port)


if __name__ == "__main__":
    main()
