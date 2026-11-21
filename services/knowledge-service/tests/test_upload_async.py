"""Async document ingest tests (SPEC-W111).

wait=false on POST /v1/documents/upload submits a docling async task and
returns 202 + job id; a background job polls, ingests via the UNCHANGED
_ingest_document, and marks the upload_jobs row done/failed. The sync path
(wait=true default) is byte-identical (covered by tests/test_upload.py).
"""

from __future__ import annotations

import base64
import json
import sys
import time
import uuid
from contextlib import asynccontextmanager
from datetime import UTC, datetime

sys.path.insert(0, ".")

import httpx
import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app import main as main_module
from app.config import settings
from app.docintel import DocIntelClient, DocIntelRejected, DocIntelUnavailable
from app.upload_jobs import UploadJobsStore

TENANT_A = "11111111-1111-1111-1111-111111111111"
TENANT_B = "22222222-2222-2222-2222-222222222222"

_REAL_CLIENT = httpx.AsyncClient


def _patch_transport(monkeypatch, handler):
    transport = httpx.MockTransport(handler)
    monkeypatch.setattr(
        httpx, "AsyncClient", lambda **kw: _REAL_CLIENT(transport=transport, **kw)
    )


# ------------------------------------------- DocIntelClient async contract
async def test_submit_poll_result_contract(monkeypatch):
    seen = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append((request.method, str(request.url)))
        if request.url.path.endswith("/convert/source/async"):
            return httpx.Response(200, json={"task_id": "task-1",
                                             "task_status": "pending"})
        if "/status/poll/" in request.url.path:
            return httpx.Response(200, json={"task_status": "success"})
        if "/result/" in request.url.path:
            return httpx.Response(200, json={
                "document": {"md_content": "# Big scan", "text_content": ""},
                "status": "success", "processing_time": 42.0,
            })
        return httpx.Response(404)

    _patch_transport(monkeypatch, handler)
    client = DocIntelClient("http://doc-intel:5001")
    task_id = await client.submit("scan.pdf", b"%PDF-big")
    assert task_id == "task-1"
    assert await client.poll(task_id) == "success"
    result = await client.result(task_id)
    assert result.text == "# Big scan"
    assert result.processing_time_s == 42.0
    assert seen[0][1] == "http://doc-intel:5001/v1/convert/source/async"
    assert seen[1][1] == "http://doc-intel:5001/v1/status/poll/task-1"
    assert seen[2][1] == "http://doc-intel:5001/v1/result/task-1"


async def test_submit_without_task_id_rejected(monkeypatch):
    _patch_transport(monkeypatch, lambda req: httpx.Response(200, json={}))
    client = DocIntelClient("http://doc-intel:5001")
    with pytest.raises(DocIntelRejected, match="task_id"):
        await client.submit("a.pdf", b"x")


async def test_poll_unavailable_raises(monkeypatch):
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("down", request=request)

    _patch_transport(monkeypatch, handler)
    client = DocIntelClient("http://doc-intel:5001")
    with pytest.raises(DocIntelUnavailable):
        await client.poll("task-1")


# ------------------------------------------------------- route + job store
class FakeJobsConn:
    """In-memory upload_jobs rows + recording for the ingest INSERTs."""

    def __init__(self, db):
        self._db = db
        self.execute_calls = []

    async def fetchrow(self, query, *args):
        if query.startswith("INSERT INTO upload_jobs"):
            job_id = uuid.uuid4()
            now = datetime.now(UTC)
            self._db.jobs[str(job_id)] = {
                "id": job_id, "tenant_id": args[0], "filename": args[1],
                "title": args[2], "status": "pending", "document_id": None,
                "chars": None, "error": None,
                "created_at": now, "updated_at": now,
            }
            return {"id": job_id}
        if query.startswith("SELECT * FROM upload_jobs"):
            row = self._db.jobs.get(str(args[0]))
            if row is None or str(row["tenant_id"]) != str(args[1]):
                return None
            return row
        raise AssertionError(f"unexpected fetchrow: {query}")

    async def execute(self, query, *args):
        self.execute_calls.append((query, args))
        if query.startswith("UPDATE upload_jobs"):
            row = self._db.jobs.get(str(args[1]))
            assert row is not None
            row["status"] = args[2]
            if len(args) > 3:  # set_done/set_failed carry extra fields
                for val in args[3:]:
                    if isinstance(val, uuid.UUID):
                        row["document_id"] = val
                    elif isinstance(val, int):
                        row["chars"] = val
                    else:
                        row["error"] = val
            row["updated_at"] = datetime.now(UTC)


class FakeDB:
    def __init__(self):
        self.jobs = {}
        self.tenant_calls = []
        self._conn = FakeJobsConn(self)

    @asynccontextmanager
    async def tenant_conn(self, tenant_id):
        self.tenant_calls.append(tenant_id)
        yield self._conn


class FakeEmbedder:
    async def embed(self, chunks):
        return [[0.0] * 384 for _ in chunks]


class FakeSearch:
    async def bulk_index_chunks(self, os_docs):
        return len(os_docs)


class FakeDocIntelAsync:
    """Scripts the async task lifecycle: poll answers in sequence."""

    def __init__(self, poll_sequence, result_text="big scan markdown"):
        self._polls = list(poll_sequence)
        self._result_text = result_text
        self.submitted = []

    async def submit(self, filename, data):
        self._polls = self._polls or ["success"]
        self.submitted.append(filename)
        return "task-1"

    async def poll(self, task_id):
        outcome = self._polls.pop(0) if self._polls else "success"
        if isinstance(outcome, Exception):
            raise outcome
        return outcome

    async def result(self, task_id):
        if isinstance(self._result_text, Exception):
            raise self._result_text
        from app.docintel import Extraction

        return Extraction(text=self._result_text, processing_time_s=5.0)


class FakeDeps:
    def __init__(self, db, docintel):
        self.db = db
        self.embedder = FakeEmbedder()
        self.search = FakeSearch()
        self.docintel = docintel
        self.upload_jobs = UploadJobsStore(db)
        self.bg_tasks = set()
        self.dapr = None


def _client(docintel):
    db = FakeDB()
    deps = FakeDeps(db, docintel)
    app = FastAPI()
    app.post("/v1/documents/upload", status_code=201)(main_module.upload_document)
    app.get("/v1/documents/upload/{job_id}")(main_module.get_upload_job)
    app.state.deps = deps
    return app, db, deps


def _payload(**over):
    body = {
        "tenant": TENANT_A,
        "filename": "annual-report.pdf",
        "content_base64": base64.b64encode(b"%PDF-big-scan").decode(),
        "wait": False,
    }
    body.update(over)
    return body


def _wait_job(db, job_id, timeout=5.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        row = db.jobs.get(job_id)
        if row and row["status"] in ("done", "failed"):
            return row
        time.sleep(0.05)
    return db.jobs[job_id]


def test_async_upload_happy_path_ingests_and_marks_done(monkeypatch):
    monkeypatch.setattr(settings, "doc_intel_poll_s", 0.01)
    app, db, deps = _client(FakeDocIntelAsync(["pending", "started", "success"]))
    with TestClient(app) as client:
        resp = client.post("/v1/documents/upload", json=_payload(),
                           headers={"X-Tenant-Slugs": TENANT_A})
        assert resp.status_code == 202, resp.text
        job_id = resp.json()["job_id"]
        assert resp.json()["status"] == "pending"

        row = _wait_job(db, job_id)
        assert row["status"] == "done"
        assert row["chars"] == len("big scan markdown")
        assert row["document_id"] is not None
        # ingest went through the shared pipeline under the tenant context
        doc_insert = [c for c in db._conn.execute_calls
                      if "INSERT INTO documents" in c[0]]
        assert doc_insert and doc_insert[0][1][3] == "big scan markdown"
        assert doc_insert[0][1][4] == "upload:annual-report.pdf"

        # status endpoint reports the finished job
        r = client.get(f"/v1/documents/upload/{job_id}",
                       params={"tenant": TENANT_A},
                       headers={"X-Tenant-Slugs": TENANT_A})
        assert r.status_code == 200, r.text
        assert r.json()["status"] == "done"
        assert r.json()["document_id"] == str(row["document_id"])
    assert deps.bg_tasks == set()


def test_async_upload_failure_marks_job_failed(monkeypatch):
    monkeypatch.setattr(settings, "doc_intel_poll_s", 0.01)
    app, db, _ = _client(FakeDocIntelAsync(["started", "failure"]))
    with TestClient(app) as client:
        resp = client.post("/v1/documents/upload", json=_payload(),
                           headers={"X-Tenant-Slugs": TENANT_A})
        assert resp.status_code == 202, resp.text
        job_id = resp.json()["job_id"]
        row = _wait_job(db, job_id)
        assert row["status"] == "failed"
        assert "failure" in (row["error"] or "")


def test_async_upload_submit_outage_marks_job_failed(monkeypatch):
    monkeypatch.setattr(settings, "doc_intel_poll_s", 0.01)

    class DownSubmit(FakeDocIntelAsync):
        async def submit(self, filename, data):
            raise DocIntelUnavailable("doc-intel unreachable")

    app, db, _ = _client(DownSubmit([]))
    with TestClient(app) as client:
        resp = client.post("/v1/documents/upload", json=_payload(),
                           headers={"X-Tenant-Slugs": TENANT_A})
        assert resp.status_code == 202, resp.text
        row = _wait_job(db, resp.json()["job_id"])
        assert row["status"] == "failed"
        assert "unreachable" in (row["error"] or "")


def test_job_status_is_tenant_scoped(monkeypatch):
    monkeypatch.setattr(settings, "doc_intel_poll_s", 0.01)
    app, db, _ = _client(FakeDocIntelAsync(["success"]))
    with TestClient(app) as client:
        resp = client.post("/v1/documents/upload", json=_payload(),
                           headers={"X-Tenant-Slugs": TENANT_A})
        assert resp.status_code == 202, resp.text
        job_id = resp.json()["job_id"]
        _wait_job(db, job_id)
        # another tenant bound to TENANT_B gets a 404, never the job (no oracle)
        r = client.get(f"/v1/documents/upload/{job_id}",
                       params={"tenant": TENANT_B},
                       headers={"X-Tenant-Slugs": TENANT_B})
        assert r.status_code == 404, r.text
        # malformed uuid also 404
        r = client.get("/v1/documents/upload/not-a-uuid",
                       params={"tenant": TENANT_A},
                       headers={"X-Tenant-Slugs": TENANT_A})
        assert r.status_code == 404, r.text


def test_sync_path_default_unchanged_by_w111(monkeypatch):
    """wait=true (default) never touches the jobs table or async API."""
    from app.docintel import Extraction

    class SyncOnly:
        def __init__(self):
            self.convert_calls = 0

        async def convert(self, filename, data):
            self.convert_calls += 1
            return Extraction(text="sync body text", processing_time_s=1.0)

        async def submit(self, filename, data):
            raise AssertionError("async API must not be used when wait=true")

    docintel = SyncOnly()
    app, db, _ = _client(docintel)
    with TestClient(app) as client:
        resp = client.post("/v1/documents/upload", json=_payload(wait=True),
                           headers={"X-Tenant-Slugs": TENANT_A})
        assert resp.status_code == 201, resp.text
        assert resp.json()["extraction"]["engine"] == "docling"
        assert docintel.convert_calls == 1
        assert db.jobs == {}
