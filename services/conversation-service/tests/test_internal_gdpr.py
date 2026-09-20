"""SPEC-W45 K19 internal GDPR routes + K9 tenant-lifecycle consumer tests.
Offline: fake db / FastAPI TestClient, no Postgres, no Kafka."""

from __future__ import annotations

import json
import sys
import uuid

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

sys.path.insert(0, ".")

from app import internal_routes  # noqa: E402
from app.tenant_lifecycle import TenantLifecycleConsumer  # noqa: E402

pytestmark = pytest.mark.asyncio

TENANT = uuid.uuid4()


class _FakeDB:
    def __init__(self):
        self.export_calls: list[tuple] = []
        self.erase_calls: list[tuple] = []
        self.purge_calls: list[uuid.UUID] = []

    async def export_contact_conversations(self, tenant_id, contact):
        self.export_calls.append((tenant_id, contact))
        return [
            {
                "id": str(uuid.uuid4()),
                "tenant_id": str(tenant_id),
                "site_slug": "acme",
                "channel": "web",
                "contact_phone": contact,
                "started_at": "2026-01-01T10:00:00+00:00",
                "ended_at": None,
                "turns": [
                    {"id": str(uuid.uuid4()), "seq": 1, "role": "user",
                     "text": "hello", "ts": "2026-01-01T10:00:01+00:00"}
                ],
            }
        ]

    async def erase_contact_data(self, tenant_id, phone, email):
        self.erase_calls.append((tenant_id, phone, email))
        return (2, 7)

    async def purge_tenant_data(self, tenant_id):
        self.purge_calls.append(tenant_id)
        return (5, 42)


class _Cfg:
    def __init__(self, token: str):
        self.internal_token = token
        self.identity_events_topic = "opendesk.identity.events"
        self.tenant_events_group = "conversation-tenant-lifecycle"
        self.kafka_brokers = ["kafka:9092"]


def _app(token: str) -> tuple[FastAPI, _FakeDB]:
    app = FastAPI()
    db = _FakeDB()
    app.state.cfg = _Cfg(token)
    app.state.db = db
    app.include_router(internal_routes.router)
    return app, db


class TestInternalGdprExport:
    async def test_export_ok(self):
        app, db = _app("secret")
        client = TestClient(app)
        resp = client.get(
            f"/internal/gdpr/conversations?tenant_id={TENANT}&contact=%2B2348030000000",
            headers={"X-Internal-Token": "secret"},
        )
        assert resp.status_code == 200
        body = resp.json()
        assert body["tenant_id"] == str(TENANT)
        assert body["conversations"][0]["turns"][0]["text"] == "hello"
        assert db.export_calls == [(TENANT, "+2348030000000")]

    async def test_export_unset_token_503(self):
        app, _ = _app("")
        client = TestClient(app)
        resp = client.get(
            f"/internal/gdpr/conversations?tenant_id={TENANT}&contact=x",
            headers={"X-Internal-Token": "anything"},
        )
        assert resp.status_code == 503  # fail closed

    async def test_export_wrong_token_401(self):
        app, _ = _app("secret")
        client = TestClient(app)
        resp = client.get(
            f"/internal/gdpr/conversations?tenant_id={TENANT}&contact=x",
            headers={"X-Internal-Token": "nope"},
        )
        assert resp.status_code == 401

    async def test_export_missing_token_401(self):
        app, _ = _app("secret")
        client = TestClient(app)
        resp = client.get(f"/internal/gdpr/conversations?tenant_id={TENANT}&contact=x")
        assert resp.status_code == 401


class TestInternalGdprErase:
    async def test_erase_ok(self):
        app, db = _app("secret")
        client = TestClient(app)
        resp = client.post(
            "/internal/gdpr/erase",
            json={"tenant_id": str(TENANT), "phone": "+2348030000000"},
            headers={"X-Internal-Token": "secret"},
        )
        assert resp.status_code == 200
        body = resp.json()
        assert body["status"] == "erased"
        assert body["conversations_matched"] == 2
        assert body["turns_deleted"] == 7
        assert db.erase_calls == [(TENANT, "+2348030000000", None)]

    async def test_erase_requires_contact(self):
        app, _ = _app("secret")
        client = TestClient(app)
        resp = client.post(
            "/internal/gdpr/erase",
            json={"tenant_id": str(TENANT)},
            headers={"X-Internal-Token": "secret"},
        )
        assert resp.status_code == 400

    async def test_erase_wrong_token_401(self):
        app, _ = _app("secret")
        client = TestClient(app)
        resp = client.post(
            "/internal/gdpr/erase",
            json={"tenant_id": str(TENANT), "email": "a@b.c"},
            headers={"X-Internal-Token": "nope"},
        )
        assert resp.status_code == 401


class TestTenantLifecycleConsumer:
    def _consumer(self, db) -> TenantLifecycleConsumer:
        return TenantLifecycleConsumer(_Cfg("x"), db)  # type: ignore[arg-type]

    async def test_tenant_deleted_purges(self):
        db = _FakeDB()
        c = self._consumer(db)
        env = {
            "specversion": "1.0",
            "id": "evt-1",
            "type": "com.opendesk.identity.TenantDeleted",
            "subject": "acme",
            "tenantid": str(TENANT),
            "data": {
                "tenant_slug": "acme",
                "tenant_id": str(TENANT),
                "deleted_at": "2026-01-01T00:00:00Z",
                "actor": "platform-admin",
            },
        }
        ok = await c._process(json.dumps(env).encode())
        assert ok is True
        assert db.purge_calls == [TENANT]

    async def test_other_types_acked(self):
        db = _FakeDB()
        c = self._consumer(db)
        env = {"id": "e2", "type": "com.opendesk.identity.MemberInvited", "data": {}}
        ok = await c._process(json.dumps(env).encode())
        assert ok is True
        assert db.purge_calls == []

    async def test_malformed_acked(self):
        db = _FakeDB()
        c = self._consumer(db)
        assert await c._process(b"{not json") is True
        assert db.purge_calls == []

    async def test_bad_tenant_id_acked(self):
        db = _FakeDB()
        c = self._consumer(db)
        env = {"id": "e3", "type": "TenantDeleted", "data": {"tenant_id": "not-a-uuid"}}
        assert await c._process(json.dumps(env).encode()) is True
        assert db.purge_calls == []

    async def test_purge_failure_not_committed(self):
        class _FailDB(_FakeDB):
            async def purge_tenant_data(self, tenant_id):
                raise RuntimeError("db down")

        c = self._consumer(_FailDB())
        env = {"id": "e4", "type": "TenantDeleted", "data": {"tenant_id": str(TENANT)}}
        ok = await c._process(json.dumps(env).encode())
        assert ok is False  # offset NOT committed — redelivery
