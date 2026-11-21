"""POST /v1/documents/upload endpoint tests (SPEC-W107).

House idiom (test_documents_list.py precedent): bare FastAPI app with the
route mounted directly, stubbed app.state.deps. FakeDocIntel stands in for
the sidecar client at the deps seam — production wires the real
DocIntelClient in the service lifespan (no mock on any production path).
"""

from __future__ import annotations

import base64
import sys
import uuid
from contextlib import asynccontextmanager

sys.path.insert(0, ".")

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app import main as main_module
from app.config import settings
from app.docintel import (
    DocIntelEmpty,
    DocIntelRejected,
    DocIntelUnavailable,
    Extraction,
)

TENANT_A = "11111111-1111-1111-1111-111111111111"
TENANT_B = "22222222-2222-2222-2222-222222222222"

BODY = "Refund policy: customers may request a refund within 30 days. " * 30


class FakeConn:
    def __init__(self):
        self.execute_calls = []

    async def execute(self, query, *args):
        self.execute_calls.append((query, args))


class FakeDB:
    def __init__(self, conn):
        self._conn = conn
        self.tenant_calls = []

    @asynccontextmanager
    async def tenant_conn(self, tenant_id):
        self.tenant_calls.append(tenant_id)
        yield self._conn


class FakeEmbedder:
    def __init__(self):
        self.calls = []

    async def embed(self, chunks):
        self.calls.append(list(chunks))
        return [[0.0] * 384 for _ in chunks]


class FakeSearch:
    def __init__(self):
        self.indexed = []

    async def bulk_index_chunks(self, os_docs):
        self.indexed.append(list(os_docs))
        return len(os_docs)


class FakeDocIntel:
    def __init__(self, outcome):
        self.outcome = outcome
        self.calls = []

    async def convert(self, filename, data):
        self.calls.append((filename, data))
        if isinstance(self.outcome, Exception):
            raise self.outcome
        return self.outcome


class FakeDeps:
    def __init__(self, db, embedder, search, docintel):
        self.db = db
        self.embedder = embedder
        self.search = search
        self.docintel = docintel
        self.dapr = None  # UUID tenants skip the identity lookup


def _client(docintel_outcome=None):
    if docintel_outcome is None:
        docintel_outcome = Extraction(text=BODY, processing_time_s=2.5)
    conn = FakeConn()
    deps = FakeDeps(
        FakeDB(conn), FakeEmbedder(), FakeSearch(), FakeDocIntel(docintel_outcome)
    )
    app = FastAPI()
    app.post("/v1/documents/upload", status_code=201)(main_module.upload_document)
    app.state.deps = deps
    return TestClient(app), conn, deps


def _payload(**over):
    body = {
        "tenant": TENANT_A,
        "filename": "refund-policy.pdf",
        "content_base64": base64.b64encode(b"%PDF-fake-bytes").decode(),
    }
    body.update(over)
    return body


def _post(client, **over):
    return client.post(
        "/v1/documents/upload",
        json=_payload(**over),
        headers={"X-Tenant-Slugs": TENANT_A},
    )


def test_upload_happy_path_ingests_extracted_markdown():
    client, conn, deps = _client()
    resp = _post(client)
    assert resp.status_code == 201, resp.text
    body = resp.json()
    assert body["tenant_id"] == TENANT_A
    assert body["title"] == "refund-policy"  # filename stem default
    assert body["chunk_count"] > 0
    assert body["indexed"] == body["chunk_count"]
    assert body["extraction"] == {
        "engine": "docling",
        "processing_time_s": 2.5,
        "chars": len(BODY),
    }
    # Extraction fed the shared ingest pipeline verbatim.
    assert deps.docintel.calls == [("refund-policy.pdf", b"%PDF-fake-bytes")]
    assert deps.db.tenant_calls == [TENANT_A]
    doc_insert = conn.execute_calls[0]
    assert "INSERT INTO documents" in doc_insert[0]
    assert doc_insert[1][3] == BODY  # body arg is the extracted markdown
    assert doc_insert[1][4] == "upload:refund-policy.pdf"
    assert deps.embedder.calls and deps.search.indexed


def test_upload_title_override_honored():
    client, conn, _ = _client()
    resp = _post(client, title="Refunds v2")
    assert resp.status_code == 201, resp.text
    assert resp.json()["title"] == "Refunds v2"


@pytest.mark.parametrize("filename", ["evil.exe", "noextension", "archive.zip"])
def test_upload_unsupported_extension_415(filename):
    client, conn, deps = _client()
    resp = _post(client, filename=filename)
    assert resp.status_code == 415, resp.text
    assert "pdf" in resp.json()["detail"]  # allowlist surfaced
    assert conn.execute_calls == []
    assert deps.docintel.calls == []  # rejected before the sidecar


def test_upload_invalid_base64_400():
    client, conn, deps = _client()
    resp = _post(client, content_base64="!!!not-base64!!!")
    assert resp.status_code == 400, resp.text
    assert conn.execute_calls == []
    assert deps.docintel.calls == []


def test_upload_oversize_decoded_413(monkeypatch):
    monkeypatch.setattr(settings, "doc_intel_max_bytes", 8)
    client, conn, deps = _client()
    resp = _post(client)  # 15 decoded bytes > 8
    assert resp.status_code == 413, resp.text
    assert conn.execute_calls == []
    assert deps.docintel.calls == []


def test_upload_oversize_extracted_text_413(monkeypatch):
    monkeypatch.setattr(settings, "doc_intel_max_chars", 10)
    client, conn, deps = _client()
    resp = _post(client)
    assert resp.status_code == 413, resp.text
    assert conn.execute_calls == []  # nothing persisted


def test_upload_empty_extraction_422_names_ocr():
    client, conn, deps = _client(DocIntelEmpty("doc-intel extracted no text. OCR..."))
    resp = _post(client)
    assert resp.status_code == 422, resp.text
    assert "OCR" in resp.json()["detail"]
    assert conn.execute_calls == []
    assert deps.db.tenant_calls == []  # tenant bound, but no insert attempted


def test_upload_conversion_rejected_422():
    client, conn, _ = _client(DocIntelRejected("doc-intel conversion failed: []"))
    resp = _post(client)
    assert resp.status_code == 422, resp.text
    assert conn.execute_calls == []


def test_upload_sidecar_down_503():
    client, conn, _ = _client(DocIntelUnavailable("doc-intel unreachable"))
    resp = _post(client)
    assert resp.status_code == 503, resp.text
    assert conn.execute_calls == []


def test_upload_tenant_binding_enforced():
    client, conn, deps = _client()
    resp = client.post(
        "/v1/documents/upload",
        json=_payload(),
        headers={"X-Tenant-Slugs": TENANT_B},  # caller bound to another tenant
    )
    assert resp.status_code == 403, resp.text
    assert deps.db.tenant_calls == []
    assert deps.docintel.calls == []  # rejected before touching the sidecar
