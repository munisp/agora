"""Shared fixtures + fakes for ocr-service tests (SPEC-W53 Scope A).

OFFLINE contract: no network (local in-process stub servers only), no
model download. All external systems are fakes/stubs defined HERE —
production code has no mock paths other than the OCR_MOCK gate.
"""

from __future__ import annotations

import json
import struct
import sys
import threading
import zlib
from contextlib import asynccontextmanager
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

sys.path.insert(0, ".")

from app import main as main_module  # noqa: E402
from app.config import settings  # noqa: E402

TENANT_A = "11111111-2222-3333-4444-555555555555"
TENANT_B = "99999999-8888-7777-6666-555555555555"
INTERNAL_TOKEN = "test-internal-token"


# ---------------------------------------------------------------------------
# byte fixtures (built in pure python — no Pillow, no network)
# ---------------------------------------------------------------------------
def make_png(width: int = 16, height: int = 16, seed: int = 7) -> bytes:
    """Minimal valid non-interlaced 8-bit RGB PNG."""
    def chunk(ctype: bytes, payload: bytes) -> bytes:
        return (
            struct.pack(">I", len(payload))
            + ctype
            + payload
            + struct.pack(">I", zlib.crc32(ctype + payload) & 0xFFFFFFFF)
        )

    ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    rows = bytearray()
    for y in range(height):
        rows.append(0)  # filter type 0
        for x in range(width):
            rows += bytes(((x * seed + y) % 256, (y * seed) % 256, (x + y) % 256))
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", ihdr)
        + chunk(b"IDAT", zlib.compress(bytes(rows)))
        + chunk(b"IEND", b"")
    )


def make_jpeg() -> bytes:
    # magic + plausible JFIF-ish body (validation only sniffs magic).
    return b"\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00" + b"\x00" * 64


def make_webp() -> bytes:
    payload = b"WEBPVP8 " + b"\x00" * 32
    return b"RIFF" + struct.pack("<I", len(payload)) + payload


def make_pdf(pages: int = 2) -> bytes:
    """Fake-but-sniffable PDF with `pages` /Type /Page objects (mock mode
    never rasterizes; pdf_page_count counts the objects)."""
    objs = b"".join(
        b"%d 0 obj\n<< /Type /Page /Parent 0 0 R >>\nendobj\n" % (i + 1)
        for i in range(pages)
    )
    return b"%PDF-1.4\n" + objs + b"trailer\n<< >>\n%%EOF\n"


# ---------------------------------------------------------------------------
# fakes
# ---------------------------------------------------------------------------
class FakeConn:
    """asyncpg-shaped connection bound to one tenant; rows live on FakeDB."""

    def __init__(self, db: "FakeDB", tenant_id: str):
        self._db = db
        self._tenant = tenant_id

    # The routes call Database helper classmethods with this conn; the fakes
    # below shadow them via FakeDB overriding the helpers' conn protocol.


class FakeDB:
    """In-memory ocr_documents store enforcing tenant isolation."""

    def __init__(self):
        self.rows: dict[str, dict[str, Any]] = {}  # str(id) -> row
        self.transitions: list[tuple[str, str]] = []  # (id, new_status)

    @asynccontextmanager
    async def tenant_conn(self, tenant_id):
        yield _TenantScope(self, tenant_id)

    @asynccontextmanager
    async def sweep_conn(self):
        """SPEC-W58: cross-tenant scope for the retention sweeper."""
        yield _TenantScope(self, None)

    async def ping(self) -> bool:
        return True

    # Database helper API (classmethods in the real impl) ----------------
    async def insert_document(self, scope, doc: dict[str, Any]) -> None:
        assert doc["tenant_id"] == scope.tenant_id, "cross-tenant insert"
        self.rows[str(doc["id"])] = {
            **doc,
            "id": doc["id"],
            "status": "uploaded",
            "model_version": "",
            "extracted_text": None,
            "extracted_fields": None,
            "expires_at": doc.get("expires_at"),
            "error": None,
            "created_at": datetime.now(timezone.utc),
            "updated_at": datetime.now(timezone.utc),
        }
        self.transitions.append((str(doc["id"]), "uploaded"))

    async def mark_extracted(self, scope, document_id, **kw) -> None:
        row = self._owned(scope, document_id)
        row.update(kw)
        row["status"] = "extracted"
        row["error"] = None
        self.transitions.append((str(document_id), "extracted"))

    async def mark_failed(self, scope, document_id, *, error: str) -> None:
        row = self._owned(scope, document_id)
        row["status"] = "failed"
        row["error"] = error
        self.transitions.append((str(document_id), "failed"))

    async def get_document(self, scope, document_id):
        return self._owned(scope, document_id, missing_ok=True)

    # SPEC-W58 retention/erasure helper fakes ---------------------------------
    async def list_expired(self, scope, *, limit: int = 500):
        """Cross-tenant: scope.tenant_id is None (sweep_conn)."""
        assert scope.tenant_id is None, "list_expired must use sweep_conn"
        now = datetime.now(timezone.utc)
        rows = [
            r
            for r in self.rows.values()
            if r.get("expires_at") is not None
            and r["expires_at"] <= now
            and r["status"] != "erased"
        ]
        rows.sort(key=lambda r: r["expires_at"])
        return [
            {
                "id": r["id"],
                "tenant_id": r["tenant_id"],
                "storage_bucket": r["storage_bucket"],
                "storage_key": r["storage_key"],
            }
            for r in rows[:limit]
        ]

    async def delete_document(self, scope, document_id) -> None:
        assert scope.tenant_id is None, "delete_document must use sweep_conn"
        self.rows.pop(str(document_id), None)
        self.transitions.append((str(document_id), "deleted"))

    async def list_kyc_rows_matching_subject(self, scope, subject: str):
        out = []
        for r in self.rows.values():
            if r["tenant_id"] != scope.tenant_id:
                continue
            if not r["kind"].startswith("kyc_") or r["status"] == "erased":
                continue
            fields = r.get("extracted_fields")
            if isinstance(fields, str):
                fields = json.loads(fields)
            if isinstance(fields, dict) and any(
                isinstance(v, str) and v == subject for v in fields.values()
            ):
                out.append(
                    {
                        "id": r["id"],
                        "storage_bucket": r["storage_bucket"],
                        "storage_key": r["storage_key"],
                    }
                )
        return out

    async def mark_erased(self, scope, document_id) -> None:
        row = self._owned(scope, document_id)
        row["extracted_text"] = None
        row["extracted_fields"] = None
        row["status"] = "erased"
        row["error"] = None
        self.transitions.append((str(document_id), "erased"))

    async def list_tenant_documents(self, scope):
        return [
            {
                "id": r["id"],
                "storage_bucket": r["storage_bucket"],
                "storage_key": r["storage_key"],
            }
            for r in self.rows.values()
            if r["tenant_id"] == scope.tenant_id
        ]

    async def delete_tenant_documents(self, scope) -> int:
        doomed = [
            k for k, r in self.rows.items() if r["tenant_id"] == scope.tenant_id
        ]
        for k in doomed:
            self.transitions.append((k, "deleted"))
            del self.rows[k]
        return len(doomed)

    def _owned(self, scope, document_id, missing_ok=False):
        row = self.rows.get(str(document_id))
        if row is None or row["tenant_id"] != scope.tenant_id:
            if missing_ok:
                return None
            raise AssertionError("cross-tenant or missing row")
        return row


class _TenantScope:
    def __init__(self, db: FakeDB, tenant_id: str):
        self.db = db
        self.tenant_id = tenant_id


class FakeMinio:
    def __init__(self):
        self.objects: dict[tuple[str, str], bytes] = {}

    @property
    def bucket(self) -> str:
        return "documents"

    async def put(self, key: str, data: bytes, mime: str) -> None:
        self.objects[(self.bucket, key)] = data

    async def delete(self, key: str) -> None:
        # SPEC-W58: S3 DELETE on a missing key is a success (idempotent).
        self.objects.pop((self.bucket, key), None)


class FakeDapr:
    """Captures published messages: [(pubsub, topic, payload)]."""

    def __init__(self):
        self.published: list[tuple[str, str, Any]] = []

    async def publish(self, pubsub: str, topic: str, data: Any, **_kw) -> None:
        self.published.append((pubsub, topic, json.loads(json.dumps(data))))

    async def invoke(self, *a, **k):  # UUID tenants never reach this
        raise AssertionError("identity lookup not expected in tests")

    async def aclose(self) -> None:
        pass


class FakeDeps:
    def __init__(self, db=None, documents=None, dapr=None, ocr=None, objects=None):
        self.db = db or FakeDB()
        self.documents = documents or FakeMinio()
        self.dapr = dapr or FakeDapr()
        self.ocr = ocr  # only used when OCR_MOCK is false
        # SPEC-W58: object deleter for erasure consumers + the TTL sweeper;
        # the FakeMinio doubles as the deleter unless one is supplied.
        self.objects = objects or self.documents


@pytest.fixture
def deps():
    return FakeDeps()


@pytest.fixture
def client(deps, monkeypatch):
    """Bare FastAPI app with the real routes + fake deps (lifespan skipped)."""
    monkeypatch.setattr(settings, "ocr_mock", True)
    monkeypatch.setattr(settings, "internal_token", INTERNAL_TOKEN)
    monkeypatch.setattr(settings, "trust_direct_tenant", False)
    monkeypatch.setattr(settings, "ocr_max_bytes", 25 * 1024 * 1024)
    # Copy routes WITHOUT include_router — FastAPI 0.115 merges the
    # source router's lifespan (real DB/Dapr connect) on include_router;
    # appending routes directly keeps the test app lifespan-free.
    test_app = FastAPI()
    for route in main_module.app.router.routes:
        test_app.router.routes.append(route)
    test_app.state.deps = deps
    with TestClient(test_app) as c:
        yield c


# ---------------------------------------------------------------------------
# local stub HTTP server (llama-server / document_url fetches) — LOOPBACK ONLY
# ---------------------------------------------------------------------------
class StubServer:
    def __init__(self, handler):
        self._server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    @property
    def url(self) -> str:
        host, port = self._server.server_address
        return f"http://{host}:{port}"

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()


def make_chat_completion_stub(content: str) -> tuple[StubServer, list[dict]]:
    """Stub llama-server: captures POST /v1/chat/completions bodies."""
    captured: list[dict] = []

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            captured.append({"path": self.path, "json": json.loads(body)})
            payload = json.dumps(
                {
                    "id": "chatcmpl-stub",
                    "object": "chat.completion",
                    "choices": [
                        {"index": 0, "message": {"role": "assistant", "content": content}}
                    ],
                    "model": "minicpm-ocr",
                }
            ).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def log_message(self, *a):  # silence
            pass

    return StubServer(Handler), captured


def make_failing_stub(status: int = 500) -> StubServer:
    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802
            length = int(self.headers.get("Content-Length", "0"))
            self.rfile.read(length)
            self.send_response(status)
            self.send_header("Content-Length", "2")
            self.end_headers()
            self.wfile.write(b"{}")

        def log_message(self, *a):
            pass

    return StubServer(Handler)


def make_download_stub(data: bytes, filename_ok: bool = True) -> StubServer:
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):  # noqa: N802
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *a):
            pass

    return StubServer(Handler)
