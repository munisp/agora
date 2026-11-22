"""Upload-job stale-running reaper tests (SPEC-W112).

A crash/restart strands wait=false jobs in `running` forever — the
background task is gone but the row never settles and the tenant polls a
job that will never finish. The reaper (startup sweep + interval loop)
marks stale running rows failed with an honest resubmit message. Pending
rows (not started yet) and terminal rows (done/failed) are NEVER touched.
The sweep runs on the maintenance reaper_conn (app.upload_reaper='on'
activates the upload_job_reaper RLS policy; FORCE RLS stays on for every
request connection).

REDIRECTION PROOF (SPEC-W112 R3): if the UPDATE loses its
`status = 'running'` predicate, test_reaper_sql_predicates_running_only
and test_terminal_rows_untouched FAIL.
"""

from __future__ import annotations

import asyncio
import sys
import uuid
from contextlib import asynccontextmanager
from datetime import UTC, datetime, timedelta

sys.path.insert(0, ".")

import pytest

from app import main as main_module
from app.upload_jobs import DDL, REAPED_ERROR, UploadJobsStore

pytestmark = pytest.mark.asyncio

TENANT_A = "11111111-1111-1111-1111-111111111111"
TENANT_B = "22222222-2222-2222-2222-222222222222"


class FakeReaperConn:
    """Emulates the reaper UPDATE against in-memory rows, faithfully
    applying the WHERE predicates from the emitted SQL."""

    def __init__(self, db):
        self._db = db
        self.queries = []

    async def fetch(self, query, *args):
        self.queries.append((query, args))
        error_text, stale_s = args
        now = datetime.now(UTC)
        cutoff = now - timedelta(seconds=stale_s)
        running_only = "status = 'running'" in query
        reaped = []
        for row in self._db.jobs.values():
            if running_only and row["status"] != "running":
                continue  # emulate the SQL predicate
            if row["updated_at"] >= cutoff:
                continue  # not stale
            row["status"] = "failed"
            row["error"] = error_text
            row["updated_at"] = now
            reaped.append({"id": row["id"]})
        return reaped


class FakeDB:
    def __init__(self):
        self.jobs = {}
        self.reaper_conn_used = 0
        self.tenant_conn_used = 0
        self._reaper = FakeReaperConn(self)

    @asynccontextmanager
    async def reaper_conn(self):
        self.reaper_conn_used += 1
        yield self._reaper

    @asynccontextmanager
    async def tenant_conn(self, tenant_id):
        self.tenant_conn_used += 1
        yield self._reaper  # not used by reaper paths


def _job(status, *, age_s=0.0, tenant=TENANT_A):
    now = datetime.now(UTC)
    job_id = uuid.uuid4()
    return str(job_id), {
        "id": job_id, "tenant_id": uuid.UUID(tenant), "filename": "f.pdf",
        "title": "f", "status": status, "document_id": None, "chars": None,
        "error": None, "created_at": now - timedelta(seconds=age_s),
        "updated_at": now - timedelta(seconds=age_s),
    }


def _store(*jobs):
    db = FakeDB()
    for job_id, row in jobs:
        db.jobs[job_id] = row
    return UploadJobsStore(db), db


# ------------------------------------------------- DDL
def test_ddl_includes_reaper_policy():
    assert "upload_job_reaper" in DDL
    assert "app.upload_reaper" in DDL
    # FORCE RLS unchanged — request connections stay tenant-bound
    assert "FORCE ROW LEVEL SECURITY" in DDL


# ------------------------------------------------- sweep semantics
async def test_stale_running_job_marked_failed_honestly():
    store, db = _store(_job("running", age_s=2000))
    reaped = await store.reap_stale(960.0)
    assert reaped == 1
    row = next(iter(db.jobs.values()))
    assert row["status"] == "failed"
    assert row["error"] == REAPED_ERROR
    assert "resubmit" in row["error"]


async def test_fresh_running_job_untouched():
    store, db = _store(_job("running", age_s=5))
    reaped = await store.reap_stale(960.0)
    assert reaped == 0
    assert next(iter(db.jobs.values()))["status"] == "running"


async def test_pending_job_never_reaped():
    # pending means the background task may not have started yet; even a
    # very old pending row is not the reaper's business
    store, db = _store(_job("pending", age_s=99999))
    reaped = await store.reap_stale(960.0)
    assert reaped == 0
    assert next(iter(db.jobs.values()))["status"] == "pending"


async def test_terminal_rows_untouched():
    store, db = _store(
        _job("done", age_s=99999),
        _job("failed", age_s=99999),
    )
    reaped = await store.reap_stale(960.0)
    assert reaped == 0
    statuses = sorted(r["status"] for r in db.jobs.values())
    assert statuses == ["done", "failed"]


async def test_reaper_sql_predicates_running_only():
    store, db = _store(_job("running", age_s=2000))
    await store.reap_stale(960.0)
    query, _ = db._reaper.queries[0]
    assert "WHERE status = 'running'" in query
    assert "updated_at < now()" in query


async def test_reaper_uses_maintenance_conn_not_tenant_conn():
    store, db = _store(_job("running", age_s=2000))
    await store.reap_stale(960.0)
    assert db.reaper_conn_used == 1
    assert db.tenant_conn_used == 0


async def test_reaper_spans_tenants():
    store, db = _store(
        _job("running", age_s=2000, tenant=TENANT_A),
        _job("running", age_s=2000, tenant=TENANT_B),
    )
    reaped = await store.reap_stale(960.0)
    assert reaped == 2
    assert all(r["status"] == "failed" for r in db.jobs.values())


# ------------------------------------------------- loop behavior
async def test_reaper_loop_survives_sweep_failure(monkeypatch):
    class _ExplodingStore:
        def __init__(self):
            self.calls = 0

        async def reap_stale(self, stale_after_s):
            self.calls += 1
            raise RuntimeError("db down")

    monkeypatch.setattr(main_module.settings, "doc_intel_reaper_interval_s", 0.01)
    store = _ExplodingStore()
    deps = type("D", (), {"upload_jobs": store})()
    task = asyncio.create_task(main_module._reaper_loop(deps))
    await asyncio.sleep(0.05)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert store.calls >= 2  # kept sweeping after failures


async def test_reaper_loop_sweeps_immediately_then_intervals(monkeypatch):
    class _Counting:
        def __init__(self):
            self.calls = 0

        async def reap_stale(self, stale_after_s):
            self.calls += 1
            return 0

    monkeypatch.setattr(main_module.settings, "doc_intel_reaper_interval_s", 0.05)
    store = _Counting()
    deps = type("D", (), {"upload_jobs": store})()
    task = asyncio.create_task(main_module._reaper_loop(deps))
    await asyncio.sleep(0.01)
    assert store.calls == 1  # startup sweep before first sleep
    await asyncio.sleep(0.12)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert store.calls >= 3


async def test_reaper_config_defaults():
    from app.config import Settings

    s = Settings()
    assert s.doc_intel_reaper_enabled is True
    assert s.doc_intel_reaper_interval_s == 60.0
    assert s.doc_intel_reaper_stale_s == 960.0
    # stale threshold exceeds the job timeout: a legitimately running job
    # (deadline 900s) is never reaped mid-flight
    assert s.doc_intel_reaper_stale_s > s.doc_intel_job_timeout_s
