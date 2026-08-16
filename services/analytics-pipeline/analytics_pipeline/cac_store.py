"""asyncpg persistence for the CAC realtime rollups (SPEC-W13 §5).

Tables live in the ``analytics_meta`` database (one DB per service, SPEC §7)
and every table is tenant-RLS'd with the OpenDesk idiom:

    ENABLE + FORCE ROW LEVEL SECURITY
    POLICY tenant_isolation USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)

Every tenant-scoped statement runs inside a transaction that first does
``SELECT set_config('app.tenant_id', $1, true)`` (SET LOCAL) — same pattern
as conversation-service app/db.py ``_tenant_tx``.

Dual-path note (contract §5): these realtime rollups feed
``GET /v1/cac/summary``; the lakehouse gold tables
(``cac_gold.daily_cac_by_channel`` / ``daily_cac_by_lga``, Iceberg) are the
batch-verified source reconciled nightly by the Spark job.
"""

from __future__ import annotations

import uuid
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from datetime import date
from decimal import Decimal
from typing import Any

import asyncpg
import structlog

from .cac_events import FunnelEvent
from .config import Settings

log = structlog.get_logger()

# Bootstrap DDL is a superuser migration path (runs WITHOUT app.tenant_id).
# CREATE POLICY has no IF NOT EXISTS -> pg_policies existence check, same as
# booking-service internal/store/incidents.go.
_CAC_DDL = """
CREATE TABLE IF NOT EXISTS cac_rollup_channel (
    tenant_id   UUID NOT NULL,
    day         DATE NOT NULL,
    channel     TEXT NOT NULL,
    leads       INTEGER NOT NULL DEFAULT 0,
    conversions INTEGER NOT NULL DEFAULT 0,
    revenue_ngn NUMERIC(18,2) NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, day, channel)
);
ALTER TABLE cac_rollup_channel ENABLE ROW LEVEL SECURITY;
ALTER TABLE cac_rollup_channel FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'cac_rollup_channel' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON cac_rollup_channel
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS cac_rollup_lga (
    tenant_id   UUID NOT NULL,
    day         DATE NOT NULL,
    lga_id      INTEGER NOT NULL,
    leads       INTEGER NOT NULL DEFAULT 0,
    conversions INTEGER NOT NULL DEFAULT 0,
    revenue_ngn NUMERIC(18,2) NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, day, lga_id)
);
ALTER TABLE cac_rollup_lga ENABLE ROW LEVEL SECURITY;
ALTER TABLE cac_rollup_lga FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'cac_rollup_lga' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON cac_rollup_lga
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

-- First-seen campaign -> channel attribution (from funnel events), used by
-- the summary spend join: booking-service stores spend per campaign, so we
-- need to know which channel each campaign belongs to.
CREATE TABLE IF NOT EXISTS cac_campaign_channel (
    tenant_id      UUID NOT NULL,
    campaign_id    UUID NOT NULL,
    channel        TEXT NOT NULL,
    first_seen_day DATE NOT NULL,
    PRIMARY KEY (tenant_id, campaign_id)
);
ALTER TABLE cac_campaign_channel ENABLE ROW LEVEL SECURITY;
ALTER TABLE cac_campaign_channel FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'cac_campaign_channel' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON cac_campaign_channel
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;

-- Consumer idempotency (contract §2 idempotency_key): one row per processed
-- event key; recorded in the SAME transaction as the rollup upserts so a
-- crash mid-batch replays cleanly (at-least-once + dedupe = effectively once).
CREATE TABLE IF NOT EXISTS cac_processed_events (
    tenant_id        UUID NOT NULL,
    idempotency_key  TEXT NOT NULL,
    processed_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, idempotency_key)
);
ALTER TABLE cac_processed_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE cac_processed_events FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'cac_processed_events' AND policyname = 'tenant_isolation') THEN
        CREATE POLICY tenant_isolation ON cac_processed_events
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;
"""


def _database_dsn(settings: Settings) -> str:
    """Append/override the database on the base PG_DSN (conversation-service
    convention: PG_DSN base + PG_DATABASE)."""
    base = settings.pg_dsn.rstrip("/")
    # strip an existing trailing db path segment
    if base.count("/") >= 3:
        head = base.rsplit("/", 1)[0]
        if "://" in head:
            base = head
    return f"{base}/{settings.pg_database}"


class CacStore:
    def __init__(self, settings: Settings):
        self._settings = settings
        self._pool: asyncpg.Pool | None = None

    async def connect(self) -> None:
        self._pool = await asyncpg.create_pool(
            _database_dsn(self._settings),
            min_size=self._settings.pg_min_size,
            max_size=self._settings.pg_max_size,
        )
        log.info("cac.postgres_pool_created", database=self._settings.pg_database)

    async def close(self) -> None:
        if self._pool is not None:
            await self._pool.close()
            self._pool = None

    async def ping(self) -> None:
        async with self._pool_acquire() as conn:
            await conn.fetchval("SELECT 1")

    async def ensure_schema(self) -> None:
        """Idempotent DDL — safe to run on every startup."""
        async with self._pool_acquire() as conn:
            await conn.execute(_CAC_DDL)
        log.info("cac.schema_ensured")

    def _pool_acquire(self) -> Any:
        assert self._pool is not None, "CacStore.connect() not called"
        return self._pool.acquire()

    @asynccontextmanager
    async def _tenant_tx(self, tenant_id: str) -> AsyncIterator[asyncpg.Connection]:
        """Acquire a connection inside a transaction with app.tenant_id set."""
        async with self._pool_acquire() as conn:
            async with conn.transaction():
                await conn.execute(
                    "SELECT set_config('app.tenant_id', $1, true)", str(tenant_id)
                )
                yield conn

    # ------------------------------------------------------------------
    # consumer path
    # ------------------------------------------------------------------

    async def record_event(self, event: FunnelEvent) -> bool:
        """Apply one funnel event to the rollups, idempotently.

        Returns True when the event was applied, False when it was a replay
        (same tenant + idempotency_key already processed). The idempotency
        marker and the rollup upserts commit in ONE transaction.
        """
        tenant_uuid = uuid.UUID(event.tenant_id)
        leads = 1 if event.is_lead else 0
        conversions = 1 if event.is_conversion else 0
        revenue = event.revenue_ngn
        async with self._tenant_tx(event.tenant_id) as conn:
            applied = await conn.fetchval(
                """
                INSERT INTO cac_processed_events (tenant_id, idempotency_key)
                VALUES ($1, $2)
                ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
                RETURNING 1
                """,
                tenant_uuid,
                event.idempotency_key,
            )
            if applied is None:
                return False  # replay — already rolled up

            if event.channel:
                if leads or conversions or revenue:
                    await conn.execute(
                        """
                        INSERT INTO cac_rollup_channel
                            (tenant_id, day, channel, leads, conversions, revenue_ngn)
                        VALUES ($1, $2, $3, $4, $5, $6)
                        ON CONFLICT (tenant_id, day, channel) DO UPDATE SET
                            leads = cac_rollup_channel.leads + EXCLUDED.leads,
                            conversions = cac_rollup_channel.conversions + EXCLUDED.conversions,
                            revenue_ngn = cac_rollup_channel.revenue_ngn + EXCLUDED.revenue_ngn,
                            updated_at = now()
                        """,
                        tenant_uuid,
                        event.day,
                        event.channel,
                        leads,
                        conversions,
                        revenue,
                    )
                if event.campaign_id:
                    try:
                        campaign_uuid = uuid.UUID(event.campaign_id)
                    except ValueError:
                        campaign_uuid = None
                    if campaign_uuid is not None:
                        await conn.execute(
                            """
                            INSERT INTO cac_campaign_channel
                                (tenant_id, campaign_id, channel, first_seen_day)
                            VALUES ($1, $2, $3, $4)
                            ON CONFLICT (tenant_id, campaign_id) DO NOTHING
                            """,
                            tenant_uuid,
                            campaign_uuid,
                            event.channel,
                            event.day,
                        )
            if event.lga_id is not None and (leads or conversions or revenue):
                await conn.execute(
                    """
                    INSERT INTO cac_rollup_lga
                        (tenant_id, day, lga_id, leads, conversions, revenue_ngn)
                    VALUES ($1, $2, $3, $4, $5, $6)
                    ON CONFLICT (tenant_id, day, lga_id) DO UPDATE SET
                        leads = cac_rollup_lga.leads + EXCLUDED.leads,
                        conversions = cac_rollup_lga.conversions + EXCLUDED.conversions,
                        revenue_ngn = cac_rollup_lga.revenue_ngn + EXCLUDED.revenue_ngn,
                        updated_at = now()
                    """,
                    tenant_uuid,
                    event.day,
                    event.lga_id,
                    leads,
                    conversions,
                    revenue,
                )
            return True

    # ------------------------------------------------------------------
    # summary path (GET /v1/cac/summary)
    # ------------------------------------------------------------------

    async def fetch_channel_rollup(
        self, tenant_id: str, date_from: date | None, date_to: date | None
    ) -> list[asyncpg.Record]:
        """Per-channel totals over [from, to] (inclusive; NULL = unbounded)."""
        async with self._tenant_tx(tenant_id) as conn:
            return await conn.fetch(
                """
                SELECT channel,
                       SUM(leads)::int AS leads,
                       SUM(conversions)::int AS conversions,
                       SUM(revenue_ngn) AS revenue_ngn
                FROM cac_rollup_channel
                WHERE ($2::date IS NULL OR day >= $2)
                  AND ($3::date IS NULL OR day <= $3)
                GROUP BY channel
                ORDER BY channel
                """,
                uuid.UUID(tenant_id),
                date_from,
                date_to,
            )

    async def fetch_lga_rollup(
        self, tenant_id: str, date_from: date | None, date_to: date | None
    ) -> list[asyncpg.Record]:
        """Per-LGA totals over [from, to] (inclusive; NULL = unbounded)."""
        async with self._tenant_tx(tenant_id) as conn:
            return await conn.fetch(
                """
                SELECT lga_id,
                       SUM(leads)::int AS leads,
                       SUM(conversions)::int AS conversions,
                       SUM(revenue_ngn) AS revenue_ngn
                FROM cac_rollup_lga
                WHERE ($2::date IS NULL OR day >= $2)
                  AND ($3::date IS NULL OR day <= $3)
                GROUP BY lga_id
                ORDER BY lga_id
                """,
                uuid.UUID(tenant_id),
                date_from,
                date_to,
            )

    async def list_campaign_channels(self, tenant_id: str) -> list[asyncpg.Record]:
        """All known campaign -> channel attributions for the tenant (the
        spend join iterates these; first-touch mapping, never overwritten)."""
        async with self._tenant_tx(tenant_id) as conn:
            return await conn.fetch(
                """
                SELECT campaign_id, channel
                FROM cac_campaign_channel
                ORDER BY first_seen_day, campaign_id
                """
            )

    async def rollup_day_bounds(self, tenant_id: str) -> tuple[date | None, date | None]:
        """(min day, max day) present in the channel rollup — used for the
        payback period length when the caller left from/to open-ended."""
        async with self._tenant_tx(tenant_id) as conn:
            row = await conn.fetchrow(
                "SELECT MIN(day) AS min_day, MAX(day) AS max_day FROM cac_rollup_channel"
            )
            if row is None:
                return (None, None)
            return (row["min_day"], row["max_day"])


def decimal_or_none(value: Any) -> Decimal | None:
    if value is None:
        return None
    if isinstance(value, Decimal):
        return value
    return Decimal(str(value))
