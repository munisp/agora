"""asyncpg persistence for the conversation DB (SPEC §7).

RLS note: init script 03-conversation-schema.sql enables FORCE ROW LEVEL
SECURITY with policy tenant_id = current_setting('app.tenant_id')::uuid, so
every transaction sets app.tenant_id via set_config(..., true) (LOCAL).
"""

from __future__ import annotations

import json
import uuid
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from typing import Any

import asyncpg

from .config import Config
from .logging import get_logger

log = get_logger(__name__)


class NotFoundError(Exception):
    pass


class Database:
    def __init__(self, cfg: Config) -> None:
        self._cfg = cfg
        self._pool: asyncpg.Pool | None = None

    async def connect(self) -> None:
        self._pool = await asyncpg.create_pool(
            self._cfg.database_dsn,
            min_size=self._cfg.pg_min_size,
            max_size=self._cfg.pg_max_size,
        )
        log.info("postgres pool created", database=self._cfg.pg_database)

    async def close(self) -> None:
        if self._pool is not None:
            await self._pool.close()
            self._pool = None

    async def ping(self) -> None:
        async with self._pool_acquire() as conn:
            await conn.fetchval("SELECT 1")

    async def ensure_intel_columns(self) -> None:
        """SPEC-W3 §4 innovation 3: idempotent ALTER for enrichment fields.

        Nullable columns — old rows stay NULL (lexicon/LLM enrichment only
        applies to new turns). Safe to run on every startup.
        """
        async with self._pool_acquire() as conn:
            await conn.execute(
                "ALTER TABLE turns ADD COLUMN IF NOT EXISTS sentiment DOUBLE PRECISION"
            )
            await conn.execute(
                "ALTER TABLE turns ADD COLUMN IF NOT EXISTS intent TEXT"
            )
            await conn.execute(
                "ALTER TABLE turns ADD COLUMN IF NOT EXISTS entities JSONB"
            )
        log.info("turn intel columns ensured")

    async def ensure_turn_idempotency(self) -> None:
        """SPEC-W3 §3: idempotent ALTER + unique partial index for the
        Idempotency-Key dedupe store on turns.

        The key is nullable (old rows and key-less appends stay NULL and are
        never deduplicated); the partial unique index on
        (conversation_id, idempotency_key) makes concurrent same-key appends
        safe — the loser gets a UniqueViolation and re-reads the winner's
        row. Safe to run on every startup.
        """
        async with self._pool_acquire() as conn:
            await conn.execute(
                "ALTER TABLE turns ADD COLUMN IF NOT EXISTS idempotency_key TEXT"
            )
            await conn.execute(
                """
                CREATE UNIQUE INDEX IF NOT EXISTS uq_turns_idempotency_key
                ON turns (conversation_id, idempotency_key)
                WHERE idempotency_key IS NOT NULL
                """
            )
        log.info("turn idempotency key ensured")

    async def ensure_contact_column(self) -> None:
        """SPEC-W3 §2 innovation 13 (GDPR): idempotent ALTER adding the
        nullable contact_phone column used by the ?contact= filter and the
        privacy erase consumer. Populated at conversation creation from the
        caller's site/session metadata when provided. Safe on every startup.
        """
        async with self._pool_acquire() as conn:
            await conn.execute(
                "ALTER TABLE conversations ADD COLUMN IF NOT EXISTS contact_phone TEXT"
            )
            await conn.execute(
                "CREATE INDEX IF NOT EXISTS idx_conversations_contact_phone "
                "ON conversations (tenant_id, contact_phone)"
            )
        log.info("conversation contact_phone column ensured")

    async def ensure_ussd_channel(self) -> None:
        """SPEC-W12 contract §2: widen the conversations.channel CHECK so
        "ussd" is a persisted channel value (ussd joins the channel enum).

        Idempotent re-create of the inline CHECK from
        03-conversation-schema.sql; NOT VALID keeps existing rows out of the
        scan while new inserts are still validated. Safe on every startup.
        """
        async with self._pool_acquire() as conn:
            await conn.execute(
                "ALTER TABLE conversations "
                "DROP CONSTRAINT IF EXISTS conversations_channel_check"
            )
            await conn.execute(
                "ALTER TABLE conversations "
                "ADD CONSTRAINT conversations_channel_check "
                "CHECK (channel IN ('voice','chat','phone','api','ussd')) NOT VALID"
            )
        log.info("conversation ussd channel check ensured")

    def _pool_acquire(self) -> Any:
        assert self._pool is not None, "Database.connect() not called"
        return self._pool.acquire()

    @asynccontextmanager
    async def _tenant_tx(self, tenant_id: uuid.UUID) -> AsyncIterator[asyncpg.Connection]:
        """Acquire a connection inside a transaction with app.tenant_id set."""
        async with self._pool_acquire() as conn:
            async with conn.transaction():
                await conn.execute(
                    "SELECT set_config('app.tenant_id', $1, true)", str(tenant_id)
                )
                yield conn

    # ------------------------------------------------------------------
    # conversations
    # ------------------------------------------------------------------

    async def create_conversation(
        self,
        tenant_id: uuid.UUID,
        site_slug: str,
        channel: str,
        contact_phone: str | None = None,
        conversation_id: uuid.UUID | None = None,
    ) -> asyncpg.Record:
        """Insert a conversation; optionally with a caller-chosen id.

        SPEC-W12: the USSD hook passes conversation_id=uuid5(tenant,
        sessionId) — ON CONFLICT DO NOTHING + re-read makes concurrent first
        callbacks of the same session safe (exactly one row wins).
        contact_phone is the GDPR contact marker (SPEC-W3 §2) and feeds the
        incident IDP callback_number.
        """
        async with self._tenant_tx(tenant_id) as conn:
            row = await conn.fetchrow(
                """
                INSERT INTO conversations (id, tenant_id, site_slug, channel,
                                         contact_phone)
                VALUES (COALESCE($5, gen_random_uuid()), $1, $2, $3, $4)
                ON CONFLICT (id) DO NOTHING
                RETURNING id, tenant_id, site_slug, channel, contact_phone,
                          started_at, ended_at
                """,
                tenant_id,
                site_slug,
                channel,
                contact_phone,
                conversation_id,
            )
            if row is None:  # lost the same-id race — read the winner
                row = await conn.fetchrow(
                    """
                    SELECT id, tenant_id, site_slug, channel, contact_phone,
                           started_at, ended_at
                    FROM conversations WHERE id = $1
                    """,
                    conversation_id,
                )
            return row

    async def list_conversations(
        self,
        tenant_id: uuid.UUID,
        limit: int = 50,
        offset: int = 0,
        contact: str | None = None,
    ) -> list[asyncpg.Record]:
        """List conversations, optionally filtered by the GDPR contact
        marker (SPEC-W3 §2 ?contact= filter; None = no filter)."""
        async with self._tenant_tx(tenant_id) as conn:
            return await conn.fetch(
                """
                SELECT id, tenant_id, site_slug, channel, contact_phone,
                       started_at, ended_at
                FROM conversations
                WHERE ($3::text IS NULL OR contact_phone = $3)
                ORDER BY started_at DESC
                LIMIT $1 OFFSET $2
                """,
                limit,
                offset,
                contact,
            )

    async def get_conversation(
        self, conversation_id: uuid.UUID, tenant_id: uuid.UUID
    ) -> asyncpg.Record:
        async with self._tenant_tx(tenant_id) as conn:
            row = await conn.fetchrow(
                """
                SELECT id, tenant_id, site_slug, channel, contact_phone,
                       started_at, ended_at
                FROM conversations WHERE id = $1
                """,
                conversation_id,
            )
            if row is None:
                raise NotFoundError(f"conversation {conversation_id} not found")
            return row

    # ------------------------------------------------------------------
    # GDPR erasure (SPEC-W3 §2 innovation 13)
    # ------------------------------------------------------------------

    async def erase_contact_data(
        self, tenant_id: uuid.UUID, phone: str | None, email: str | None
    ) -> tuple[int, int]:
        """Right-to-erasure tombstone: delete all turns of conversations
        whose contact_phone matches the given phone or e-mail, then clear the
        contact_phone marker itself (conversation shells are kept so booking
        history and analytics stay referentially intact).

        Returns (conversations_matched, turns_deleted).
        """
        refs = [r for r in (phone, email) if r]
        if not refs:
            return (0, 0)
        async with self._tenant_tx(tenant_id) as conn:
            convs = await conn.fetch(
                "SELECT id FROM conversations WHERE contact_phone = ANY($1::text[])",
                refs,
            )
            ids = [r["id"] for r in convs]
            if not ids:
                return (0, 0)
            deleted = await conn.fetchval(
                "WITH d AS (DELETE FROM turns WHERE conversation_id = ANY($1::uuid[]) "
                "RETURNING 1) SELECT count(*) FROM d",
                ids,
            )
            await conn.execute(
                "UPDATE conversations SET contact_phone = NULL WHERE id = ANY($1::uuid[])",
                ids,
            )
            return (len(ids), int(deleted or 0))

    # ------------------------------------------------------------------
    # Data retention (NDPA 2023 storage limitation — docs/compliance/ndpa.md)
    # ------------------------------------------------------------------

    async def list_tenant_ids(self) -> list[uuid.UUID]:
        """Distinct tenant ids present in the conversations table.

        RLS caveat: this enumeration runs WITHOUT app.tenant_id set, so it
        only sees rows when the connecting role bypasses RLS (the default
        opendesk superuser DSN). With an RLS-enforced role
        (app_conversation_login) it returns an empty list — run the
        retention sweep with the superuser DSN or a maintenance role.
        """
        async with self._pool_acquire() as conn:
            rows = await conn.fetch(
                "SELECT DISTINCT tenant_id FROM conversations"
            )
            return [r["tenant_id"] for r in rows]

    async def delete_turns_older_than(
        self, tenant_id: uuid.UUID, days: int, batch_size: int = 1000
    ) -> int:
        """Hard-delete turns older than ``days`` days for ONE tenant, in
        batches (bounded lock time per statement). Returns total deleted.

        Runs inside a tenant-scoped transaction (app.tenant_id set), so RLS
        keeps the sweep inside the tenant even under an enforced role. Turn
        age is measured on turns.ts against the DATABASE clock (now()), so
        no app-side clock skew can extend retention. GDPR-erased
        conversations already have no turns; this is orthogonal and only
        removes aged rows that erasure did not cover.
        """
        total = 0
        async with self._tenant_tx(tenant_id) as conn:
            while True:
                count = int(
                    await conn.fetchval(
                        """
                        WITH doomed AS (
                            SELECT t.id
                            FROM turns t
                            JOIN conversations c ON c.id = t.conversation_id
                            WHERE t.ts < now() - ($1::int * INTERVAL '1 day')
                            ORDER BY t.ts
                            LIMIT $2
                        ),
                        d AS (
                            DELETE FROM turns t USING doomed dm
                            WHERE t.id = dm.id
                            RETURNING 1
                        )
                        SELECT count(*) FROM d
                        """,
                        days,
                        batch_size,
                    )
                    or 0
                )
                total += count
                if count < batch_size:
                    return total

    # ------------------------------------------------------------------
    # turns
    # ------------------------------------------------------------------

    _TURN_COLS = (
        "id, conversation_id, seq, role, text, tool_calls,"
        " sentiment, intent, entities, idempotency_key, ts"
    )

    async def add_turn(
        self,
        conversation_id: uuid.UUID,
        tenant_id: uuid.UUID,
        role: str,
        text: str,
        tool_calls: list[dict[str, Any]] | None,
        sentiment: float | None = None,
        intent: str | None = None,
        entities: dict[str, Any] | None = None,
        idempotency_key: str | None = None,
    ) -> tuple[asyncpg.Record, bool]:
        """Append a turn with seq = max(seq)+1, atomically per conversation.

        Returns (row, created). The (conversation_id, seq) UNIQUE constraint
        plus the transaction makes concurrent appends safe.
        sentiment/intent/entities are the SPEC-W3 §4 call-intelligence
        enrichment (nullable).

        SPEC-W3 §3 idempotency: when idempotency_key is given, a replay
        (same conversation + key) returns the ORIGINAL turn with
        created=False instead of inserting a duplicate; the unique partial
        index uq_turns_idempotency_key decides concurrent same-key races.
        """
        async with self._tenant_tx(tenant_id) as conn:
            # serialize concurrent turn appends for this conversation
            await conn.execute(
                "SELECT pg_advisory_xact_lock(hashtext($1::text))", str(conversation_id)
            )
            if idempotency_key:
                existing = await conn.fetchrow(
                    f"SELECT {self._TURN_COLS} FROM turns"
                    " WHERE conversation_id = $1 AND idempotency_key = $2",
                    conversation_id,
                    idempotency_key,
                )
                if existing is not None:
                    return existing, False
            try:
                row = await conn.fetchrow(
                    """
                    INSERT INTO turns (conversation_id, seq, role, text, tool_calls,
                                       sentiment, intent, entities, idempotency_key)
                    SELECT $1,
                           COALESCE((SELECT MAX(seq) FROM turns WHERE conversation_id = $1), 0) + 1,
                           $2, $3, $4::jsonb, $5, $6, $7::jsonb, $8
                    RETURNING id, conversation_id, seq, role, text, tool_calls,
                              sentiment, intent, entities, idempotency_key, ts
                    """,
                    conversation_id,
                    role,
                    text,
                    json.dumps(tool_calls) if tool_calls is not None else None,
                    sentiment,
                    intent,
                    json.dumps(entities) if entities is not None else None,
                    idempotency_key,
                )
            except asyncpg.UniqueViolationError:
                if not idempotency_key:
                    raise
                # Lost the same-key race (the advisory lock serializes per
                # conversation, but the index is the authoritative guard) —
                # return the winner's row.
                row = await conn.fetchrow(
                    f"SELECT {self._TURN_COLS} FROM turns"
                    " WHERE conversation_id = $1 AND idempotency_key = $2",
                    conversation_id,
                    idempotency_key,
                )
                if row is None:
                    raise
                return row, False
            if row is None:  # INSERT ... SELECT with bad FK raises instead
                raise NotFoundError(f"conversation {conversation_id} not found")
            return row, True

    async def list_turns(
        self, conversation_id: uuid.UUID, tenant_id: uuid.UUID
    ) -> list[asyncpg.Record]:
        async with self._tenant_tx(tenant_id) as conn:
            return await conn.fetch(
                """
                SELECT id, conversation_id, seq, role, text, tool_calls, ts
                FROM turns WHERE conversation_id = $1 ORDER BY seq
                """,
                conversation_id,
            )

    async def sentiment_summary(
        self, conversation_id: uuid.UUID, tenant_id: uuid.UUID
    ) -> tuple[float | None, int]:
        """(avg sentiment, scored-turn count) for one conversation.

        Only turns with a non-NULL sentiment (written by app/intel.py) count;
        (None, 0) means "nothing to enrich" (STRATEGY §3, Wave 5 innovation 2).
        """
        async with self._tenant_tx(tenant_id) as conn:
            row = await conn.fetchrow(
                """
                SELECT AVG(sentiment) AS avg_sentiment,
                       COUNT(sentiment) AS scored_turns
                FROM turns
                WHERE conversation_id = $1 AND sentiment IS NOT NULL
                """,
                conversation_id,
            )
        avg = row["avg_sentiment"] if row else None
        count = int(row["scored_turns"]) if row else 0
        return (float(avg) if avg is not None else None), count

    async def conversation_exists(
        self, conversation_id: uuid.UUID, tenant_id: uuid.UUID
    ) -> bool:
        async with self._tenant_tx(tenant_id) as conn:
            return bool(
                await conn.fetchval(
                    "SELECT 1 FROM conversations WHERE id = $1", conversation_id
                )
            )

    async def conversation_meta(
        self, conversation_id: uuid.UUID, tenant_id: uuid.UUID
    ) -> dict[str, Any] | None:
        """Lightweight lookup used by the indexer for enrichment."""
        async with self._tenant_tx(tenant_id) as conn:
            row = await conn.fetchrow(
                "SELECT site_slug, channel FROM conversations WHERE id = $1",
                conversation_id,
            )
            return dict(row) if row else None
