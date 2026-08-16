"""Self-improving knowledge base (SPEC-W3 §4, innovation 4).

When hybrid search can't answer a question (top RRF score below
``SUGGEST_THRESHOLD``, default 0.35) and the query looks like a question,
the service records a ``kb_suggestions`` row (deduped per tenant+question).
Staff review the queue via GET /v1/suggestions and either approve (the
question becomes a real ingested document) or reject it.

This module holds the pure logic + SQL so it is unit-testable without
OpenSearch/embeddings; the HTTP wiring lives in app/main.py.
"""

from __future__ import annotations

import re
import uuid
from typing import Any, Protocol

# Bootstrap DDL (idempotent — runs on service startup). RLS mirrors the
# documents/chunks pattern: FORCE RLS with the app.tenant_id GUC.
DDL = """
CREATE TABLE IF NOT EXISTS kb_suggestions (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL,
    question   TEXT NOT NULL,
    top_score  DOUBLE PRECISION,
    status     TEXT NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending','approved','rejected')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, question)
);
ALTER TABLE kb_suggestions ENABLE ROW LEVEL SECURITY;
ALTER TABLE kb_suggestions FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE tablename = 'kb_suggestions' AND policyname = 'tenant_isolation'
    ) THEN
        CREATE POLICY tenant_isolation ON kb_suggestions
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
    END IF;
END $$;
"""

QUESTION_WORDS = frozenset(
    "who what when where why how which whose whom is are was were do does "
    "did can could should will would may might shall".split()
)

_FIRST_WORD_RE = re.compile(r"[a-zA-Z]+")


def looks_like_question(query: str) -> bool:
    """True when the query ends with '?' or starts with a question word."""
    q = query.strip()
    if not q:
        return False
    if q.endswith("?"):
        return True
    m = _FIRST_WORD_RE.match(q)
    return bool(m and m.group(0).lower() in QUESTION_WORDS)


def should_suggest(top_score: float | None, query: str, threshold: float) -> bool:
    """Gate for recording a suggestion: weak retrieval + question-shaped query."""
    score = top_score if top_score is not None else 0.0
    return score < threshold and looks_like_question(query)


class Conn(Protocol):
    async def execute(self, query: str, *args: Any) -> str: ...
    async def fetchrow(self, query: str, *args: Any) -> Any: ...
    async def fetch(self, query: str, *args: Any) -> list: ...


async def record_suggestion(
    conn: Conn, tenant_id: uuid.UUID, question: str, top_score: float | None
) -> dict[str, Any] | None:
    """Insert a suggestion, deduped by (tenant_id, question).

    Returns the new row, or None when the question was already recorded.
    """
    row = await conn.fetchrow(
        """
        INSERT INTO kb_suggestions (tenant_id, question, top_score)
        VALUES ($1, $2, $3)
        ON CONFLICT (tenant_id, question) DO NOTHING
        RETURNING id, tenant_id, question, top_score, status, created_at
        """,
        tenant_id,
        question.strip(),
        top_score,
    )
    return dict(row) if row else None


async def list_suggestions(
    conn: Conn, tenant_id: uuid.UUID, status: str | None = None
) -> list[dict[str, Any]]:
    if status:
        rows = await conn.fetch(
            """
            SELECT id, tenant_id, question, top_score, status, created_at
            FROM kb_suggestions WHERE status = $1
            ORDER BY created_at DESC LIMIT 200
            """,
            status,
        )
    else:
        rows = await conn.fetch(
            """
            SELECT id, tenant_id, question, top_score, status, created_at
            FROM kb_suggestions ORDER BY created_at DESC LIMIT 200
            """
        )
    return [dict(r) for r in rows]


async def get_suggestion(conn: Conn, suggestion_id: uuid.UUID) -> dict[str, Any] | None:
    row = await conn.fetchrow(
        """
        SELECT id, tenant_id, question, top_score, status, created_at
        FROM kb_suggestions WHERE id = $1
        """,
        suggestion_id,
    )
    return dict(row) if row else None


async def set_status(conn: Conn, suggestion_id: uuid.UUID, status: str) -> bool:
    """Mark a suggestion approved/rejected. False when the row is gone."""
    result = await conn.execute(
        "UPDATE kb_suggestions SET status = $2 WHERE id = $1",
        suggestion_id,
        status,
    )
    return result != "UPDATE 0"
