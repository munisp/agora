"""asyncpg persistence for the W38 agents registry + capture tables
(SPEC-W38 F1/F3, schema in infra/postgres/init-scripts/07-agents-capture-schema.sql).

RLS note: like app/db.py, every tenant-scoped statement runs inside a
transaction with app.tenant_id set via set_config(..., true) (LOCAL), so the
FORCE ROW LEVEL SECURITY policies (tenant_id = current_setting(
'app.tenant_id', true)::uuid) apply. The store borrows the existing
Database pool/transaction helper instead of opening a second pool.
"""

from __future__ import annotations

import json
import re
import uuid
from typing import Any

import asyncpg

from .db import Database, NotFoundError
from .logging import get_logger

log = get_logger(__name__)

__all__ = ["AgentStore", "NotFoundError", "DuplicatePhoneError", "DuplicateSlugError"]


class DuplicatePhoneError(Exception):
    """agents.phone_number is UNIQUE per tenant (uq_agents_tenant_phone)."""


class DuplicateSlugError(Exception):
    """agents.slug is UNIQUE per tenant."""


_SLUG_RE = re.compile(r"[^a-z0-9]+")


def slugify(name: str) -> str:
    """Default agent slug from its display name (stable, URL-safe)."""
    slug = _SLUG_RE.sub("-", name.strip().lower()).strip("-")
    return slug or "agent"


def _agent_dict(row: Any) -> dict[str, Any]:
    d = dict(row)
    if isinstance(d.get("definition"), str):
        d["definition"] = json.loads(d["definition"])
    return d


def _schema_dict(row: Any) -> dict[str, Any]:
    d = dict(row)
    if isinstance(d.get("schema"), str):
        d["schema"] = json.loads(d["schema"])
    return d


def _record_dict(row: Any) -> dict[str, Any]:
    d = dict(row)
    if isinstance(d.get("data"), str):
        d["data"] = json.loads(d["data"])
    return d


class AgentStore:
    """Tenant-scoped store for agents / capture_schemas / capture_records."""

    def __init__(self, db: Database) -> None:
        self._db = db

    async def ensure_agent_tables(self) -> None:
        """Idempotent bootstrap for existing deployments (the init script
        07-agents-capture-schema.sql is authoritative on fresh installs;
        this mirrors the ensure_intel_columns pattern so upgrades to an
        already-initialized database get the tables too).

        SQL-001: the bootstrap ALSO ensures FORCE ROW LEVEL SECURITY +
        tenant_isolation policies + app_conversation GRANTs (verbatim from
        07-agents-capture-schema.sql) — previously a database bootstrapped
        ONLY by this method had the tables without any RLS, so the
        app.tenant_id GUC was set but never enforced. Policies/grants are
        guarded by pg_policies/pg_roles existence checks; tenant_slugs is
        deliberately NOT RLS-scoped (resolve runs cross-tenant by design).
        Safe on every startup."""
        ddl = [
            """
            CREATE TABLE IF NOT EXISTS agents (
                id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
                tenant_id    UUID NOT NULL,
                name         TEXT NOT NULL,
                slug         TEXT NOT NULL,
                purpose      TEXT,
                phone_number TEXT,
                status       TEXT NOT NULL DEFAULT 'active'
                             CHECK (status IN ('active','disabled')),
                definition   JSONB NOT NULL DEFAULT '{}'::jsonb,
                created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
                updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
                UNIQUE (tenant_id, slug)
            )
            """,
            """
            CREATE UNIQUE INDEX IF NOT EXISTS uq_agents_tenant_phone
                ON agents (tenant_id, phone_number) WHERE phone_number IS NOT NULL
            """,
            """
            CREATE INDEX IF NOT EXISTS idx_agents_phone
                ON agents (phone_number) WHERE phone_number IS NOT NULL
            """,
            """
            CREATE INDEX IF NOT EXISTS idx_agents_tenant_status
                ON agents (tenant_id, status)
            """,
            """
            CREATE TABLE IF NOT EXISTS capture_schemas (
                id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
                tenant_id  UUID NOT NULL,
                agent_id   UUID NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
                name       TEXT NOT NULL,
                schema     JSONB NOT NULL DEFAULT '{"fields": []}'::jsonb,
                active     BOOLEAN NOT NULL DEFAULT true,
                created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
                updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
            )
            """,
            """
            CREATE INDEX IF NOT EXISTS idx_capture_schemas_agent
                ON capture_schemas (tenant_id, agent_id)
            """,
            """
            CREATE TABLE IF NOT EXISTS capture_records (
                id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
                tenant_id            UUID NOT NULL,
                capture_schema_id    UUID NOT NULL REFERENCES capture_schemas (id) ON DELETE CASCADE,
                agent_id             UUID NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
                conversation_id      UUID NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
                data                 JSONB NOT NULL DEFAULT '{}'::jsonb,
                extraction_confidence DOUBLE PRECISION,
                created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
            )
            """,
            """
            CREATE INDEX IF NOT EXISTS idx_capture_records_agent
                ON capture_records (tenant_id, agent_id, created_at DESC)
            """,
            """
            CREATE INDEX IF NOT EXISTS idx_capture_records_conversation
                ON capture_records (tenant_id, conversation_id)
            """,
            # tenant_id -> slug projection (write-through from identity
            # resolutions, see app/tenants.py). identity-service has no
            # by-id lookup, so this local mapping is what lets the INTERNAL
            # /v1/agents/resolve payload carry tenant_slug. Deliberately
            # NOT RLS-scoped: resolve runs cross-tenant by design.
            """
            CREATE TABLE IF NOT EXISTS tenant_slugs (
                tenant_id  UUID PRIMARY KEY,
                slug       TEXT NOT NULL UNIQUE,
                updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
            )
            """,
            # -------- Row Level Security (SQL-001; verbatim policy shape from
            # infra/postgres/init-scripts/07-agents-capture-schema.sql) ------
            # tenant_slugs stays policy-free: /v1/agents/resolve is
            # cross-tenant by design (see the comment above its CREATE).
            """
            ALTER TABLE agents ENABLE ROW LEVEL SECURITY
            """,
            """
            ALTER TABLE agents FORCE ROW LEVEL SECURITY
            """,
            """
            DO $$
            BEGIN
                IF NOT EXISTS (SELECT FROM pg_policies
                               WHERE schemaname = 'public' AND tablename = 'agents'
                                 AND policyname = 'tenant_isolation') THEN
                    CREATE POLICY tenant_isolation ON agents
                        USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
                END IF;
            END
            $$
            """,
            """
            ALTER TABLE capture_schemas ENABLE ROW LEVEL SECURITY
            """,
            """
            ALTER TABLE capture_schemas FORCE ROW LEVEL SECURITY
            """,
            """
            DO $$
            BEGIN
                IF NOT EXISTS (SELECT FROM pg_policies
                               WHERE schemaname = 'public' AND tablename = 'capture_schemas'
                                 AND policyname = 'tenant_isolation') THEN
                    CREATE POLICY tenant_isolation ON capture_schemas
                        USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
                END IF;
            END
            $$
            """,
            """
            ALTER TABLE capture_records ENABLE ROW LEVEL SECURITY
            """,
            """
            ALTER TABLE capture_records FORCE ROW LEVEL SECURITY
            """,
            """
            DO $$
            BEGIN
                IF NOT EXISTS (SELECT FROM pg_policies
                               WHERE schemaname = 'public' AND tablename = 'capture_records'
                                 AND policyname = 'tenant_isolation') THEN
                    CREATE POLICY tenant_isolation ON capture_records
                        USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
                END IF;
            END
            $$
            """,
            # Grants to the conversation service role (created by
            # 05-app-roles.sql on fresh installs). Guarded so the bootstrap
            # also works when the role is absent (e.g. embedded test Postgres).
            """
            DO $$
            BEGIN
                IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_conversation') THEN
                    EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.agents TO app_conversation';
                    EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.capture_schemas TO app_conversation';
                    EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE public.capture_records TO app_conversation';
                END IF;
            END
            $$
            """,
        ]
        async with self._db._pool_acquire() as conn:
            for stmt in ddl:
                await conn.execute(stmt)
        log.info("agents/capture tables ensured")

    # ------------------------------------------------------------------
    # agents (SPEC-W38 F1)
    # ------------------------------------------------------------------

    _AGENT_COLS = (
        "id, tenant_id, name, slug, purpose, phone_number, status,"
        " definition, created_at, updated_at"
    )

    # Resolve-only select list: agents columns (aliased) + the tenant slug
    # projection (NULL when the tenant never resolved through a slug call).
    _RESOLVE_COLS = (
        ", ".join(
            f"a.{c.strip()}" for c in _AGENT_COLS.replace("\n", "").split(",")
        )
        + ", t.slug AS tenant_slug"
    )

    async def remember_tenant_slug(
        self, tenant_id: uuid.UUID | str, slug: str
    ) -> None:
        """Write-through tenant_id -> slug projection (called by the
        TenantResolver after every successful identity lookup). Best-effort:
        callers log and continue on failure.

        J-14 exemption: tenant_slugs is deliberately NOT RLS-scoped (the
        mapping is cross-tenant by design), so this runs outside the tenant
        tx; it writes ONLY the (tenant_id, slug) pair, never tenant data."""
        async with self._db._pool_acquire() as conn:
            await conn.execute(
                """
                INSERT INTO tenant_slugs (tenant_id, slug, updated_at)
                VALUES ($1, $2, now())
                ON CONFLICT (tenant_id)
                DO UPDATE SET slug = $2, updated_at = now()
                """,
                uuid.UUID(str(tenant_id)),
                slug,
            )

    async def create_agent(
        self,
        tenant_id: uuid.UUID,
        name: str,
        slug: str | None = None,
        purpose: str | None = None,
        phone_number: str | None = None,
        definition: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        async with self._db._tenant_tx(tenant_id) as conn:
            try:
                row = await conn.fetchrow(
                    f"""
                    INSERT INTO agents (tenant_id, name, slug, purpose,
                                        phone_number, definition)
                    VALUES ($1, $2, $3, $4, $5, $6::jsonb)
                    RETURNING {self._AGENT_COLS}
                    """,
                    tenant_id,
                    name,
                    slug or slugify(name),
                    purpose,
                    phone_number,
                    json.dumps(definition or {}),
                )
            except asyncpg.UniqueViolationError as exc:
                _raise_duplicate(exc)
        return _agent_dict(row)

    async def list_agents(
        self, tenant_id: uuid.UUID, limit: int = 50, offset: int = 0
    ) -> list[dict[str, Any]]:
        async with self._db._tenant_tx(tenant_id) as conn:
            rows = await conn.fetch(
                f"""
                SELECT {self._AGENT_COLS} FROM agents
                ORDER BY created_at DESC
                LIMIT $1 OFFSET $2
                """,
                limit,
                offset,
            )
        return [_agent_dict(r) for r in rows]

    async def get_agent(
        self, agent_id: uuid.UUID, tenant_id: uuid.UUID
    ) -> dict[str, Any]:
        async with self._db._tenant_tx(tenant_id) as conn:
            row = await conn.fetchrow(
                f"SELECT {self._AGENT_COLS} FROM agents WHERE id = $1",
                agent_id,
            )
        if row is None:
            raise NotFoundError(f"agent {agent_id} not found")
        return _agent_dict(row)

    async def update_agent(
        self,
        agent_id: uuid.UUID,
        tenant_id: uuid.UUID,
        *,
        name: str | None = None,
        slug: str | None = None,
        purpose: str | None = None,
        phone_number: str | None = None,
        status: str | None = None,
        definition: dict[str, Any] | None = None,
        clear_phone: bool = False,
    ) -> dict[str, Any]:
        """PATCH semantics: only provided fields change (clear_phone forces
        phone_number back to NULL). updated_at is bumped by the service —
        03-conversation-schema.sql uses no trigger, so none here either."""
        sets: list[str] = []
        args: list[Any] = [agent_id]
        i = 2
        for col, val in (
            ("name", name),
            ("slug", slug),
            ("purpose", purpose),
            ("status", status),
        ):
            if val is not None:
                sets.append(f"{col} = ${i}")
                args.append(val)
                i += 1
        if phone_number is not None or clear_phone:
            sets.append(f"phone_number = ${i}")
            args.append(phone_number)
            i += 1
        if definition is not None:
            sets.append(f"definition = ${i}::jsonb")
            args.append(json.dumps(definition))
            i += 1
        if not sets:
            return await self.get_agent(agent_id, tenant_id)
        sets.append("updated_at = now()")
        async with self._db._tenant_tx(tenant_id) as conn:
            try:
                row = await conn.fetchrow(
                    f"""
                    UPDATE agents SET {", ".join(sets)}
                    WHERE id = $1
                    RETURNING {self._AGENT_COLS}
                    """,
                    *args,
                )
            except asyncpg.UniqueViolationError as exc:
                _raise_duplicate(exc)
        if row is None:
            raise NotFoundError(f"agent {agent_id} not found")
        return _agent_dict(row)

    async def disable_agent(
        self, agent_id: uuid.UUID, tenant_id: uuid.UUID
    ) -> dict[str, Any]:
        """Soft delete per SPEC-W38 §2: status='disabled' (the row stays so
        capture_records/capture_schemas FKs and call history survive)."""
        return await self.update_agent(agent_id, tenant_id, status="disabled")

    async def resolve_agent_by_phone(
        self, phone: str, tenant_id: uuid.UUID | None = None
    ) -> dict[str, Any] | None:
        """Dialed-number -> agent lookup for /v1/agents/resolve (INTERNAL,
        voice-runtime only). The whole point of the endpoint is number ->
        tenant+agent, so it is deliberately NOT tenant-scoped (documented
        J-14 exemption): it runs outside the tenant tx and matches the
        idx_agents_phone partial index. A shared number across tenants
        resolves to the oldest active agent (deterministic; logged). The
        row also carries tenant_slug (LEFT JOIN tenant_slugs) so the voice
        runtime can bootstrap its TenantContext without a second lookup.

        NOTE: under the least-privilege app_conversation_login role this
        query sees ZERO agents rows unless app.tenant_id is set (FORCE RLS
        on agents) — the endpoint is deployed against the RLS-bypassing
        bootstrap DSN. That asymmetry is intentional: the general API pool
        can never enumerate other tenants' agents through this path."""
        async with self._db._pool_acquire() as conn:
            if tenant_id is not None:
                row = await conn.fetchrow(
                    f"""
                    SELECT {self._RESOLVE_COLS}
                    FROM agents a
                    LEFT JOIN tenant_slugs t ON t.tenant_id = a.tenant_id
                    WHERE a.phone_number = $1 AND a.status = 'active'
                      AND a.tenant_id = $2
                    ORDER BY a.created_at LIMIT 1
                    """,
                    phone,
                    tenant_id,
                )
            else:
                rows = await conn.fetch(
                    f"""
                    SELECT {self._RESOLVE_COLS}
                    FROM agents a
                    LEFT JOIN tenant_slugs t ON t.tenant_id = a.tenant_id
                    WHERE a.phone_number = $1 AND a.status = 'active'
                    ORDER BY a.created_at LIMIT 2
                    """,
                    phone,
                )
                if len(rows) > 1:
                    log.warning(
                        "phone number shared by multiple tenants; resolving to oldest agent",
                        phone=phone,
                    )
                row = rows[0] if rows else None
        return _agent_dict(row) if row is not None else None

    # ------------------------------------------------------------------
    # capture_schemas (SPEC-W38 F3)
    # ------------------------------------------------------------------

    _SCHEMA_COLS = (
        "id, tenant_id, agent_id, name, schema, active, created_at, updated_at"
    )

    async def create_capture_schema(
        self,
        tenant_id: uuid.UUID,
        agent_id: uuid.UUID,
        name: str,
        schema: dict[str, Any],
        active: bool = True,
    ) -> dict[str, Any]:
        async with self._db._tenant_tx(tenant_id) as conn:
            row = await conn.fetchrow(
                f"""
                INSERT INTO capture_schemas (tenant_id, agent_id, name, schema, active)
                VALUES ($1, $2, $3, $4::jsonb, $5)
                RETURNING {self._SCHEMA_COLS}
                """,
                tenant_id,
                agent_id,
                name,
                json.dumps(schema),
                active,
            )
        return _schema_dict(row)

    async def list_capture_schemas(
        self,
        tenant_id: uuid.UUID,
        agent_id: uuid.UUID | None = None,
        *,
        active_only: bool = False,
    ) -> list[dict[str, Any]]:
        async with self._db._tenant_tx(tenant_id) as conn:
            rows = await conn.fetch(
                f"""
                SELECT {self._SCHEMA_COLS} FROM capture_schemas
                WHERE ($1::uuid IS NULL OR agent_id = $1)
                  AND (NOT $2::bool OR active)
                ORDER BY created_at
                """,
                agent_id,
                active_only,
            )
        return [_schema_dict(r) for r in rows]

    async def update_capture_schema(
        self,
        schema_id: uuid.UUID,
        tenant_id: uuid.UUID,
        *,
        name: str | None = None,
        schema: dict[str, Any] | None = None,
        active: bool | None = None,
    ) -> dict[str, Any]:
        sets: list[str] = []
        args: list[Any] = [schema_id]
        i = 2
        if name is not None:
            sets.append(f"name = ${i}")
            args.append(name)
            i += 1
        if schema is not None:
            sets.append(f"schema = ${i}::jsonb")
            args.append(json.dumps(schema))
            i += 1
        if active is not None:
            sets.append(f"active = ${i}")
            args.append(active)
            i += 1
        if not sets:
            async with self._db._tenant_tx(tenant_id) as conn:
                row = await conn.fetchrow(
                    f"SELECT {self._SCHEMA_COLS} FROM capture_schemas WHERE id = $1",
                    schema_id,
                )
            if row is None:
                raise NotFoundError(f"capture schema {schema_id} not found")
            return _schema_dict(row)
        sets.append("updated_at = now()")
        async with self._db._tenant_tx(tenant_id) as conn:
            row = await conn.fetchrow(
                f"""
                UPDATE capture_schemas SET {", ".join(sets)}
                WHERE id = $1
                RETURNING {self._SCHEMA_COLS}
                """,
                *args,
            )
        if row is None:
            raise NotFoundError(f"capture schema {schema_id} not found")
        return _schema_dict(row)

    async def delete_capture_schema(
        self, schema_id: uuid.UUID, tenant_id: uuid.UUID
    ) -> None:
        async with self._db._tenant_tx(tenant_id) as conn:
            result = await conn.execute(
                "DELETE FROM capture_schemas WHERE id = $1", schema_id
            )
        if result == "DELETE 0":
            raise NotFoundError(f"capture schema {schema_id} not found")

    # ------------------------------------------------------------------
    # capture_records (SPEC-W38 F3)
    # ------------------------------------------------------------------

    _RECORD_COLS = (
        "id, tenant_id, capture_schema_id, agent_id, conversation_id,"
        " data, extraction_confidence, created_at"
    )

    async def insert_capture_record(
        self,
        tenant_id: uuid.UUID,
        capture_schema_id: uuid.UUID,
        agent_id: uuid.UUID,
        conversation_id: uuid.UUID,
        data: dict[str, Any],
        extraction_confidence: float | None = None,
    ) -> dict[str, Any]:
        async with self._db._tenant_tx(tenant_id) as conn:
            row = await conn.fetchrow(
                f"""
                INSERT INTO capture_records
                    (tenant_id, capture_schema_id, agent_id, conversation_id,
                     data, extraction_confidence)
                VALUES ($1, $2, $3, $4, $5::jsonb, $6)
                RETURNING {self._RECORD_COLS}
                """,
                tenant_id,
                capture_schema_id,
                agent_id,
                conversation_id,
                json.dumps(data),
                extraction_confidence,
            )
        return _record_dict(row)

    async def list_capture_records(
        self,
        tenant_id: uuid.UUID,
        agent_id: uuid.UUID | None = None,
        conversation_id: uuid.UUID | None = None,
        limit: int = 100,
    ) -> list[dict[str, Any]]:
        """Newest first, bounded at 100 rows by default (SPEC-W38 §2)."""
        async with self._db._tenant_tx(tenant_id) as conn:
            rows = await conn.fetch(
                f"""
                SELECT {self._RECORD_COLS} FROM capture_records
                WHERE ($1::uuid IS NULL OR agent_id = $1)
                  AND ($2::uuid IS NULL OR conversation_id = $2)
                ORDER BY created_at DESC
                LIMIT $3
                """,
                agent_id,
                conversation_id,
                limit,
            )
        return [_record_dict(r) for r in rows]


def _raise_duplicate(exc: asyncpg.UniqueViolationError) -> None:
    msg = str(exc)
    if "uq_agents_tenant_phone" in msg:
        raise DuplicatePhoneError(msg) from exc
    raise DuplicateSlugError(msg) from exc
