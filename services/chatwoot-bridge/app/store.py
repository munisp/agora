"""Postgres store for the chatwoot-bridge (SPEC-W118 §4).

Three tenant-scoped tables in the chatwoot_bridge DB, all FORCE ROW LEVEL
SECURITY with the standard tenant_isolation policy — every statement runs
inside a transaction with app.tenant_id SET LOCAL (tenant_conn idiom,
mirroring ocr-service/app/store.py). The runtime ensure_schema() bootstrap
is byte-identical to infra/postgres/init-scripts/32-chatwoot-bridge-schema.sql
(test_store_ddl.py enforces the parity).

Tables:
- chatwoot_tenants       per-tenant Chatwoot account/inbox mapping + the
                         per-tenant webhook token + API credential.
- chatwoot_links         OpenDesk conversation <-> Chatwoot conversation
                         (+ contact source_id) linkage.
- chatwoot_sent_messages loop-prevention registry: source_id of every
                         message the bridge POSTED to Chatwoot, so the
                         webhook echo of our own writes is dropped.
"""

from __future__ import annotations

from contextlib import asynccontextmanager
from typing import Any

import asyncpg

from .logging import get_logger

log = get_logger(__name__)

DDL = """
CREATE TABLE IF NOT EXISTS chatwoot_tenants (
    tenant_id        UUID PRIMARY KEY,
    account_id       BIGINT NOT NULL,
    inbox_id         BIGINT NOT NULL,
    api_access_token TEXT NOT NULL,
    webhook_token    TEXT NOT NULL UNIQUE,
    webhook_id       BIGINT,
    hmac_secret      TEXT,
    enabled          BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS chatwoot_links (
    tenant_id                UUID NOT NULL,
    conversation_id          UUID NOT NULL,
    chatwoot_conversation_id BIGINT NOT NULL,
    chatwoot_contact_id      BIGINT NOT NULL,
    contact_source_id        TEXT NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, conversation_id),
    UNIQUE (tenant_id, chatwoot_conversation_id)
);

CREATE TABLE IF NOT EXISTS chatwoot_sent_messages (
    tenant_id           UUID NOT NULL,
    source_id           TEXT NOT NULL,
    chatwoot_message_id BIGINT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, source_id)
);

ALTER TABLE chatwoot_tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE chatwoot_tenants FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON chatwoot_tenants;
CREATE POLICY tenant_isolation ON chatwoot_tenants
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE chatwoot_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE chatwoot_links FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON chatwoot_links;
CREATE POLICY tenant_isolation ON chatwoot_links
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE chatwoot_sent_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE chatwoot_sent_messages FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON chatwoot_sent_messages;
CREATE POLICY tenant_isolation ON chatwoot_sent_messages
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
"""


class Database:
    def __init__(self, dsn: str):
        self._dsn = dsn
        self._pool: asyncpg.Pool | None = None

    async def connect(self) -> None:
        self._pool = await asyncpg.create_pool(self._dsn, min_size=1, max_size=8)
        log.info("postgres.pool_created")

    async def aclose(self) -> None:
        if self._pool:
            await self._pool.close()
            self._pool = None

    async def ping(self) -> bool:
        if not self._pool:
            return False
        try:
            async with self._pool.acquire() as conn:
                await conn.fetchval("SELECT 1")
            return True
        except Exception:
            return False

    async def ensure_schema(self) -> None:
        """Idempotent bootstrap DDL (ensure* idiom; parity-tested against
        infra/postgres/init-scripts/32-chatwoot-bridge-schema.sql)."""
        assert self._pool is not None, "database not connected"
        async with self._pool.acquire() as conn:
            await conn.execute(DDL)
        log.info("chatwoot_bridge.schema_ensured")

    @asynccontextmanager
    async def tenant_conn(self, tenant_id: str):
        """Yield a connection with `app.tenant_id` SET LOCAL for RLS."""
        assert self._pool is not None, "database not connected"
        async with self._pool.acquire() as conn:
            async with conn.transaction():
                await conn.execute(
                    "SELECT set_config('app.tenant_id', $1, true)", tenant_id
                )
                yield conn

    # --- tenant mappings ----------------------------------------------------
    async def upsert_tenant(
        self,
        tenant_id: str,
        *,
        account_id: int,
        inbox_id: int,
        api_access_token: str,
        webhook_token: str,
        webhook_id: int | None,
        hmac_secret: str | None,
    ) -> dict[str, Any]:
        """Register (or re-register) a tenant. The webhook_token is STABLE
        across re-registrations (Chatwoot points at a URL containing it), so
        it is only minted on first insert."""
        async with self.tenant_conn(tenant_id) as conn:
            row = await conn.fetchrow(
                """
                INSERT INTO chatwoot_tenants
                    (tenant_id, account_id, inbox_id, api_access_token,
                     webhook_token, webhook_id, hmac_secret)
                VALUES ($1, $2, $3, $4, $5, $6, $7)
                ON CONFLICT (tenant_id) DO UPDATE SET
                    account_id = EXCLUDED.account_id,
                    inbox_id = EXCLUDED.inbox_id,
                    api_access_token = EXCLUDED.api_access_token,
                    webhook_id = EXCLUDED.webhook_id,
                    hmac_secret = EXCLUDED.hmac_secret,
                    updated_at = now()
                RETURNING *
                """,
                tenant_id, account_id, inbox_id, api_access_token,
                webhook_token, webhook_id, hmac_secret,
            )
        return dict(row)

    async def get_tenant(self, tenant_id: str) -> dict[str, Any] | None:
        async with self.tenant_conn(tenant_id) as conn:
            row = await conn.fetchrow(
                "SELECT * FROM chatwoot_tenants WHERE tenant_id = $1", tenant_id
            )
        return dict(row) if row else None

    async def set_webhook_id(self, tenant_id: str, webhook_id: int | None) -> None:
        async with self.tenant_conn(tenant_id) as conn:
            await conn.execute(
                "UPDATE chatwoot_tenants SET webhook_id = $2, updated_at = now()"
                " WHERE tenant_id = $1",
                tenant_id, webhook_id,
            )

    async def delete_tenant(self, tenant_id: str) -> None:
        """Deregister: mapping + links + sent registry for the tenant."""
        async with self.tenant_conn(tenant_id) as conn:
            await conn.execute(
                "DELETE FROM chatwoot_sent_messages WHERE tenant_id = $1", tenant_id
            )
            await conn.execute(
                "DELETE FROM chatwoot_links WHERE tenant_id = $1", tenant_id
            )
            await conn.execute(
                "DELETE FROM chatwoot_tenants WHERE tenant_id = $1", tenant_id
            )

    # --- conversation links ---------------------------------------------------
    async def get_link(
        self, tenant_id: str, conversation_id: str
    ) -> dict[str, Any] | None:
        async with self.tenant_conn(tenant_id) as conn:
            row = await conn.fetchrow(
                "SELECT * FROM chatwoot_links"
                " WHERE tenant_id = $1 AND conversation_id = $2",
                tenant_id, conversation_id,
            )
        return dict(row) if row else None

    async def insert_link(
        self,
        tenant_id: str,
        conversation_id: str,
        *,
        chatwoot_conversation_id: int,
        chatwoot_contact_id: int,
        contact_source_id: str,
    ) -> dict[str, Any]:
        """Idempotent insert (ON CONFLICT DO NOTHING + re-read): safe under
        concurrent first-turn races for the same conversation."""
        async with self.tenant_conn(tenant_id) as conn:
            await conn.execute(
                """
                INSERT INTO chatwoot_links
                    (tenant_id, conversation_id, chatwoot_conversation_id,
                     chatwoot_contact_id, contact_source_id)
                VALUES ($1, $2, $3, $4, $5)
                ON CONFLICT (tenant_id, conversation_id) DO NOTHING
                """,
                tenant_id, conversation_id, chatwoot_conversation_id,
                chatwoot_contact_id, contact_source_id,
            )
            row = await conn.fetchrow(
                "SELECT * FROM chatwoot_links"
                " WHERE tenant_id = $1 AND conversation_id = $2",
                tenant_id, conversation_id,
            )
        assert row is not None
        return dict(row)

    async def get_link_by_chatwoot_conversation(
        self, tenant_id: str, chatwoot_conversation_id: int
    ) -> dict[str, Any] | None:
        async with self.tenant_conn(tenant_id) as conn:
            row = await conn.fetchrow(
                "SELECT * FROM chatwoot_links"
                " WHERE tenant_id = $1 AND chatwoot_conversation_id = $2",
                tenant_id, chatwoot_conversation_id,
            )
        return dict(row) if row else None

    # --- sent-message registry (loop prevention) -----------------------------
    async def sent_lookup(self, tenant_id: str, source_id: str) -> bool:
        async with self.tenant_conn(tenant_id) as conn:
            return bool(
                await conn.fetchval(
                    "SELECT EXISTS(SELECT 1 FROM chatwoot_sent_messages"
                    " WHERE tenant_id = $1 AND source_id = $2)",
                    tenant_id, source_id,
                )
            )

    async def sent_insert(
        self, tenant_id: str, source_id: str, chatwoot_message_id: int
    ) -> None:
        async with self.tenant_conn(tenant_id) as conn:
            await conn.execute(
                """
                INSERT INTO chatwoot_sent_messages
                    (tenant_id, source_id, chatwoot_message_id)
                VALUES ($1, $2, $3)
                ON CONFLICT (tenant_id, source_id) DO NOTHING
                """,
                tenant_id, source_id, chatwoot_message_id,
            )
