"""Upload-job persistence for async document ingest (SPEC-W111).

Idempotent bootstrap DDL + a tiny store. ALL access runs through
Database.tenant_conn (RLS + explicit tenant predicate): a cross-tenant job
id is a 404, never a 403 oracle. The sync upload path never touches this
table; a bootstrap failure must not break sync ingest (lifespan logs and
continues, kb_suggestions precedent).
"""

from __future__ import annotations

import uuid
from dataclasses import dataclass
from datetime import datetime

DDL = """
CREATE TABLE IF NOT EXISTS upload_jobs (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    filename    TEXT NOT NULL,
    title       TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending','running','done','failed')),
    document_id UUID,
    chars       INTEGER,
    error       TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE upload_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE upload_jobs FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE tablename = 'upload_jobs' AND policyname = 'tenant_isolation'
    ) THEN
        CREATE POLICY tenant_isolation ON upload_jobs
            USING (tenant_id = current_setting('app.tenant_id')::uuid);
    END IF;
    -- SPEC-W112: maintenance-only bypass for the stale-running reaper.
    -- The reaper connects with app.upload_reaper='on' AND a placeholder
    -- app.tenant_id so the tenant_isolation expression cannot error; this
    -- policy widens visibility for that connection only. It is an UPDATE
    -- policy without WITH CHECK, so the reaper can flip stale rows to
    -- failed but cannot INSERT rows into arbitrary tenants.
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE tablename = 'upload_jobs' AND policyname = 'upload_job_reaper'
    ) THEN
        CREATE POLICY upload_job_reaper ON upload_jobs
            FOR UPDATE
            USING (current_setting('app.upload_reaper', true) = 'on');
    END IF;
END $$;
"""

# SPEC-W112: honest failure text for jobs interrupted mid-run (service
# restart/crash). The caller is told to resubmit; nothing is silently lost.
REAPED_ERROR = (
    "interrupted: service restarted while job was running; resubmit upload"
)


@dataclass(frozen=True)
class UploadJob:
    id: str
    tenant_id: str
    filename: str
    title: str
    status: str
    document_id: str | None
    chars: int | None
    error: str | None
    created_at: datetime
    updated_at: datetime


def _row_to_job(row) -> UploadJob:
    return UploadJob(
        id=str(row["id"]),
        tenant_id=str(row["tenant_id"]),
        filename=row["filename"],
        title=row["title"],
        status=row["status"],
        document_id=str(row["document_id"]) if row["document_id"] else None,
        chars=row["chars"],
        error=row["error"],
        created_at=row["created_at"],
        updated_at=row["updated_at"],
    )


class UploadJobsStore:
    def __init__(self, db):
        self._db = db

    async def create(self, tenant_id: str, filename: str, title: str) -> str:
        async with self._db.tenant_conn(tenant_id) as conn:
            row = await conn.fetchrow(
                "INSERT INTO upload_jobs (tenant_id, filename, title)"
                " VALUES ($1, $2, $3) RETURNING id",
                uuid.UUID(tenant_id), filename, title,
            )
        return str(row["id"])

    async def _set(self, tenant_id: str, job_id: str, status: str, **fields) -> None:
        sets = ["status = $3", "updated_at = now()"]
        args: list = [uuid.UUID(tenant_id), uuid.UUID(job_id), status]
        for i, (col, val) in enumerate(fields.items(), start=4):
            sets.append(f"{col} = ${i}")
            args.append(val)
        async with self._db.tenant_conn(tenant_id) as conn:
            await conn.execute(
                f"UPDATE upload_jobs SET {', '.join(sets)}"
                " WHERE id = $2 AND tenant_id = $1",
                *args,
            )

    async def set_running(self, tenant_id: str, job_id: str) -> None:
        await self._set(tenant_id, job_id, "running")

    async def set_done(self, tenant_id: str, job_id: str, document_id: str,
                       chars: int) -> None:
        await self._set(tenant_id, job_id, "done",
                        document_id=uuid.UUID(document_id), chars=chars)

    async def set_failed(self, tenant_id: str, job_id: str, error: str) -> None:
        await self._set(tenant_id, job_id, "failed", error=error[:1000])

    async def get(self, tenant_id: str, job_id: str) -> UploadJob | None:
        async with self._db.tenant_conn(tenant_id) as conn:
            row = await conn.fetchrow(
                "SELECT * FROM upload_jobs WHERE id = $1 AND tenant_id = $2",
                uuid.UUID(job_id), uuid.UUID(tenant_id),
            )
        return _row_to_job(row) if row else None

    async def reap_stale(self, stale_after_s: float) -> int:
        """SPEC-W112: fail stale `running` jobs honestly.

        A process crash/restart strands jobs in `running` forever — the
        background task is gone but the row never settles, and the tenant
        polls a job that will never finish. This sweep marks rows whose
        updated_at is older than stale_after_s as failed with REAPED_ERROR
        so the tenant gets an honest terminal state and can resubmit.

        Only stale RUNNING rows are touched: pending jobs may simply not
        have started yet, and done/failed rows are terminal. Runs on the
        maintenance connection (app.upload_reaper='on') — see the
        upload_job_reaper policy in DDL. Returns the number of reaped rows.
        """
        async with self._db.reaper_conn() as conn:
            rows = await conn.fetch(
                "UPDATE upload_jobs"
                " SET status = 'failed', error = $1, updated_at = now()"
                " WHERE status = 'running'"
                "   AND updated_at < now() - make_interval(secs => $2)"
                " RETURNING id",
                REAPED_ERROR, stale_after_s,
            )
        return len(rows)
