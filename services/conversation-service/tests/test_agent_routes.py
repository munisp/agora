"""Agents registry + capture REST tests (SPEC-W38 F1/F3, gate G2):

CRUD for /v1/agents, /v1/capture-schemas, /v1/capture-records and the
internal /v1/agents/resolve — plus tenant isolation: the fake store mirrors
the RLS policy (rows are only visible inside their tenant's transaction),
so "tenant A cannot see tenant B's agent" is asserted at the API level with
two tenants (no Postgres required — mirrors the offline approach of
test_ussd.py/test_quality.py).
"""

from __future__ import annotations

import contextlib
import sys
import uuid
from datetime import UTC, datetime

import asyncpg
import pytest

sys.path.insert(0, ".")

from app.agent_db import (  # noqa: E402
    DuplicatePhoneError,
    DuplicateSlugError,
    slugify,
)
from app.config import Config  # noqa: E402
from app.db import NotFoundError  # noqa: E402
from app.logging import get_logger  # noqa: E402
from app.tenants import TenantInfo, TenantNotFoundError  # noqa: E402

pytestmark = pytest.mark.asyncio

TENANT_A = uuid.uuid4()
TENANT_B = uuid.uuid4()
TENANT_A_SLUG = "acme"
PHONE_A = "+2348012345678"


# ------------------------------------------------------- fake tenant resolver
class FakeTenantResolver:
    """Slug -> TenantInfo map with the app.tenants.TenantResolver interface."""

    def __init__(self, mapping: dict[str, uuid.UUID]):
        self._mapping = dict(mapping)

    async def by_slug(self, slug: str) -> TenantInfo:
        tenant_id = self._mapping.get(slug)
        if tenant_id is None:
            raise TenantNotFoundError(f"tenant {slug!r} not found")
        return TenantInfo(id=str(tenant_id), slug=slug)


# ------------------------------------------------------------- fake store
class FakeAgentStore:
    """In-memory AgentStore with RLS-equivalent tenant filtering."""

    def __init__(self):
        self.agents: dict[uuid.UUID, dict] = {}
        self.schemas: dict[uuid.UUID, dict] = {}
        self.records: dict[uuid.UUID, dict] = {}
        self.tenant_slugs: dict[uuid.UUID, str] = {}

    async def remember_tenant_slug(self, tenant_id, slug):
        self.tenant_slugs[uuid.UUID(str(tenant_id))] = slug

    # ---- agents
    async def create_agent(self, tenant_id, name, slug=None, purpose=None,
                           phone_number=None, definition=None):
        slug = slug or slugify(name)
        for a in self.agents.values():
            if a["tenant_id"] != tenant_id:
                continue
            if phone_number and a["phone_number"] == phone_number:
                raise DuplicatePhoneError("uq_agents_tenant_phone")
            if a["slug"] == slug:
                raise DuplicateSlugError("agents_tenant_id_slug_key")
        rec = dict(id=uuid.uuid4(), tenant_id=tenant_id, name=name, slug=slug,
                   purpose=purpose, phone_number=phone_number, status="active",
                   definition=definition or {},
                   created_at=datetime.now(UTC), updated_at=datetime.now(UTC))
        self.agents[rec["id"]] = rec
        return rec

    async def list_agents(self, tenant_id, limit=50, offset=0):
        rows = [a for a in self.agents.values() if a["tenant_id"] == tenant_id]
        rows.sort(key=lambda a: a["created_at"], reverse=True)
        return rows[offset:offset + limit]

    async def get_agent(self, agent_id, tenant_id):
        rec = self.agents.get(agent_id)
        if rec is None or rec["tenant_id"] != tenant_id:  # RLS hides cross-tenant
            raise NotFoundError(f"agent {agent_id} not found")
        return rec

    async def update_agent(self, agent_id, tenant_id, *, name=None, slug=None,
                           purpose=None, phone_number=None, status=None,
                           definition=None, clear_phone=False):
        rec = await self.get_agent(agent_id, tenant_id)
        if phone_number and any(
            a["tenant_id"] == tenant_id and a["phone_number"] == phone_number
            and a["id"] != agent_id for a in self.agents.values()
        ):
            raise DuplicatePhoneError("uq_agents_tenant_phone")
        if name is not None:
            rec["name"] = name
        if slug is not None:
            rec["slug"] = slug
        if purpose is not None:
            rec["purpose"] = purpose
        if phone_number is not None or clear_phone:
            rec["phone_number"] = phone_number
        if status is not None:
            rec["status"] = status
        if definition is not None:
            rec["definition"] = definition
        rec["updated_at"] = datetime.now(UTC)
        return rec

    async def disable_agent(self, agent_id, tenant_id):
        return await self.update_agent(agent_id, tenant_id, status="disabled")

    async def resolve_agent_by_phone(self, phone, tenant_id=None):
        rows = [a for a in self.agents.values()
                if a["phone_number"] == phone and a["status"] == "active"
                and (tenant_id is None or a["tenant_id"] == tenant_id)]
        rows.sort(key=lambda a: a["created_at"])
        if not rows:
            return None
        # LEFT JOIN tenant_slugs: the key is always present, NULL when the
        # tenant never resolved through a slug call.
        return {**rows[0],
                "tenant_slug": self.tenant_slugs.get(rows[0]["tenant_id"])}

    # ---- capture_schemas
    async def create_capture_schema(self, tenant_id, agent_id, name, schema,
                                    active=True):
        agent = self.agents.get(agent_id)
        if agent is None or agent["tenant_id"] != tenant_id:
            raise asyncpg.ForeignKeyViolationError("fk agents")
        rec = dict(id=uuid.uuid4(), tenant_id=tenant_id, agent_id=agent_id,
                   name=name, schema=schema, active=active,
                   created_at=datetime.now(UTC), updated_at=datetime.now(UTC))
        self.schemas[rec["id"]] = rec
        return rec

    async def list_capture_schemas(self, tenant_id, agent_id=None, *,
                                   active_only=False):
        rows = [s for s in self.schemas.values()
                if s["tenant_id"] == tenant_id
                and (agent_id is None or s["agent_id"] == agent_id)
                and (not active_only or s["active"])]
        rows.sort(key=lambda s: s["created_at"])
        return rows

    async def update_capture_schema(self, schema_id, tenant_id, *, name=None,
                                    schema=None, active=None):
        rec = self.schemas.get(schema_id)
        if rec is None or rec["tenant_id"] != tenant_id:
            raise NotFoundError(f"capture schema {schema_id} not found")
        if name is not None:
            rec["name"] = name
        if schema is not None:
            rec["schema"] = schema
        if active is not None:
            rec["active"] = active
        rec["updated_at"] = datetime.now(UTC)
        return rec

    async def delete_capture_schema(self, schema_id, tenant_id):
        rec = self.schemas.get(schema_id)
        if rec is None or rec["tenant_id"] != tenant_id:
            raise NotFoundError(f"capture schema {schema_id} not found")
        del self.schemas[schema_id]

    # ---- capture_records
    async def insert_capture_record(self, tenant_id, capture_schema_id,
                                    agent_id, conversation_id, data,
                                    extraction_confidence=None):
        rec = dict(id=uuid.uuid4(), tenant_id=tenant_id,
                   capture_schema_id=capture_schema_id, agent_id=agent_id,
                   conversation_id=conversation_id, data=data,
                   extraction_confidence=extraction_confidence,
                   created_at=datetime.now(UTC))
        self.records[rec["id"]] = rec
        return rec

    async def list_capture_records(self, tenant_id, agent_id=None,
                                   conversation_id=None, limit=100):
        rows = [r for r in self.records.values()
                if r["tenant_id"] == tenant_id
                and (agent_id is None or r["agent_id"] == agent_id)
                and (conversation_id is None
                     or r["conversation_id"] == conversation_id)]
        rows.sort(key=lambda r: r["created_at"], reverse=True)
        return rows[:limit]


def _app():
    from fastapi import FastAPI

    from app.agent_routes import router

    @contextlib.asynccontextmanager
    async def lifespan(app):
        app.state.cfg = Config()
        app.state.agent_store = FakeAgentStore()
        app.state.tenant_resolver = FakeTenantResolver({TENANT_A_SLUG: TENANT_A})
        app.state.log = get_logger("agent-routes-test")
        yield

    app = FastAPI(lifespan=lifespan)
    app.include_router(router)
    return app


@pytest.fixture()
def client():
    from fastapi.testclient import TestClient

    with TestClient(_app()) as c:
        yield c


def _create_agent(client, tenant=TENANT_A, **over):
    body = {"name": "Front Desk", "purpose": "answer calls",
            "phone_number": PHONE_A,
            "definition": {"persona": "warm receptionist"}}
    body.update(over)
    return client.post("/v1/agents", json=body,
                       headers={"X-Tenant-ID": str(tenant)})


# --------------------------------------------------------------- agent CRUD
def test_create_agent_201(client):
    r = _create_agent(client)
    assert r.status_code == 201, r.text
    agent = r.json()
    assert agent["name"] == "Front Desk"
    assert agent["slug"] == "front-desk"  # default slug from name
    assert agent["status"] == "active"
    assert agent["phone_number"] == PHONE_A
    assert agent["definition"] == {"persona": "warm receptionist"}
    assert agent["tenant_id"] == str(TENANT_A)


def test_get_and_list_agents(client):
    agent = _create_agent(client).json()
    r = client.get(f"/v1/agents/{agent['id']}",
                   headers={"X-Tenant-ID": str(TENANT_A)})
    assert r.status_code == 200
    assert r.json()["id"] == agent["id"]
    r = client.get("/v1/agents", headers={"X-Tenant-ID": str(TENANT_A)})
    assert [a["id"] for a in r.json()["agents"]] == [agent["id"]]


def test_patch_agent(client):
    agent = _create_agent(client).json()
    r = client.patch(f"/v1/agents/{agent['id']}",
                     json={"purpose": "triage", "status": "disabled"},
                     headers={"X-Tenant-ID": str(TENANT_A)})
    assert r.status_code == 200, r.text
    patched = r.json()
    assert patched["purpose"] == "triage"
    assert patched["status"] == "disabled"
    assert patched["name"] == "Front Desk"  # untouched fields survive


def test_patch_agent_clear_phone(client):
    agent = _create_agent(client).json()
    r = client.patch(f"/v1/agents/{agent['id']}", json={"phone_number": None},
                     headers={"X-Tenant-ID": str(TENANT_A)})
    assert r.status_code == 200
    assert r.json()["phone_number"] is None


def test_delete_agent_is_soft(client):
    agent = _create_agent(client).json()
    r = client.delete(f"/v1/agents/{agent['id']}",
                      headers={"X-Tenant-ID": str(TENANT_A)})
    assert r.status_code == 200
    assert r.json()["status"] == "disabled"
    # row still readable (soft delete, not removed)
    r = client.get(f"/v1/agents/{agent['id']}",
                   headers={"X-Tenant-ID": str(TENANT_A)})
    assert r.status_code == 200
    assert r.json()["status"] == "disabled"


def test_duplicate_phone_conflicts(client):
    assert _create_agent(client).status_code == 201
    r = _create_agent(client, name="Other Agent")
    assert r.status_code == 409


def test_bad_phone_rejected(client):
    r = _create_agent(client, phone_number="08012345678")  # not E.164
    assert r.status_code == 422


def test_tenant_scope_required(client):
    r = client.get("/v1/agents")
    assert r.status_code == 400


# ---------------------------------------------------- ?tenant= uuid OR slug
def test_tenant_uuid_query_param_unchanged(client):
    """Back-compat: ?tenant=<uuid> keeps working (no resolver involved)."""
    agent = _create_agent(client).json()
    r = client.get("/v1/agents", params={"tenant": str(TENANT_A)})
    assert r.status_code == 200, r.text
    assert [a["id"] for a in r.json()["agents"]] == [agent["id"]]


def test_tenant_slug_query_param_resolves(client):
    """admin-web passes the org slug (?tenant=acme): it must resolve to the
    tenant UUID instead of 422ing on uuid_parsing."""
    r = _create_agent(client)  # created via X-Tenant-ID header (TENANT_A)
    assert r.status_code == 201
    agent = r.json()
    r = client.get("/v1/agents", params={"tenant": TENANT_A_SLUG})
    assert r.status_code == 200, r.text
    assert [a["id"] for a in r.json()["agents"]] == [agent["id"]]
    # and the write path (admin-web POST /api/agents?tenant=<slug>)
    r = client.post("/v1/agents",
                    params={"tenant": TENANT_A_SLUG},
                    json={"name": "Slug Agent"})
    assert r.status_code == 201, r.text
    assert r.json()["tenant_id"] == str(TENANT_A)


def test_tenant_slug_unknown_404(client):
    r = client.get("/v1/agents", params={"tenant": "ghost"})
    assert r.status_code == 404


# --------------------------------------------------------- tenant isolation
def test_tenant_b_cannot_see_tenant_a_agent(client):
    agent = _create_agent(client, tenant=TENANT_A).json()
    # get / patch / delete as tenant B all 404 (RLS hides the row)
    for method in ("get", "patch", "delete"):
        r = getattr(client, method)(
            f"/v1/agents/{agent['id']}",
            **({"json": {"name": "x"}} if method == "patch" else {}),
            headers={"X-Tenant-ID": str(TENANT_B)},
        )
        assert r.status_code == 404, f"{method}: {r.status_code} {r.text}"
    # tenant B list is empty
    r = client.get("/v1/agents", headers={"X-Tenant-ID": str(TENANT_B)})
    assert r.json()["agents"] == []
    # tenant B cannot attach a capture schema to tenant A's agent
    r = client.post("/v1/capture-schemas",
                    json={"agent_id": agent["id"], "name": "leads",
                          "schema": {"fields": []}},
                    headers={"X-Tenant-ID": str(TENANT_B)})
    assert r.status_code == 404
    # same phone number is free for tenant B (unique PER TENANT)
    r = _create_agent(client, tenant=TENANT_B)
    assert r.status_code == 201


def test_tenant_b_cannot_see_tenant_a_schemas_or_records(client):
    agent = _create_agent(client, tenant=TENANT_A).json()
    schema = client.post(
        "/v1/capture-schemas",
        json={"agent_id": agent["id"], "name": "leads",
              "schema": {"fields": [{"key": "name", "type": "string",
                                     "label": "Caller name", "required": True}]}},
        headers={"X-Tenant-ID": str(TENANT_A)},
    ).json()
    assert client.get("/v1/capture-schemas",
                      headers={"X-Tenant-ID": str(TENANT_B)}
                      ).json()["capture_schemas"] == []
    for method in ("patch", "delete"):
        r = getattr(client, method)(
            f"/v1/capture-schemas/{schema['id']}",
            **({"json": {"active": False}} if method == "patch" else {}),
            headers={"X-Tenant-ID": str(TENANT_B)},
        )
        assert r.status_code == 404
    assert client.get("/v1/capture-records",
                      headers={"X-Tenant-ID": str(TENANT_B)}
                      ).json()["capture_records"] == []


# ------------------------------------------------------------------ resolve
async def test_resolve_returns_agent_and_definition(client):
    agent = _create_agent(client).json()
    await client.app.state.agent_store.remember_tenant_slug(TENANT_A, TENANT_A_SLUG)
    r = client.get("/v1/agents/resolve", params={"phone": PHONE_A})
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["agent"]["id"] == agent["id"]
    assert body["agent"]["tenant_slug"] == TENANT_A_SLUG
    assert body["definition"] == {"persona": "warm receptionist"}


def test_resolve_tenant_slug_null_without_projection(client):
    """LEFT JOIN semantics: tenant_slug key is present (null) when the
    tenant never resolved through a slug call (no projection row)."""
    agent = _create_agent(client).json()
    r = client.get("/v1/agents/resolve", params={"phone": PHONE_A})
    assert r.status_code == 200, r.text
    body = r.json()
    assert body["agent"]["id"] == agent["id"]
    assert "tenant_slug" in body["agent"]
    assert body["agent"]["tenant_slug"] is None


def test_resolve_404_unknown_or_disabled(client):
    r = client.get("/v1/agents/resolve", params={"phone": "+2340000000"})
    assert r.status_code == 404
    agent = _create_agent(client).json()
    client.delete(f"/v1/agents/{agent['id']}",
                  headers={"X-Tenant-ID": str(TENANT_A)})
    # disabled agents no longer resolve
    r = client.get("/v1/agents/resolve", params={"phone": PHONE_A})
    assert r.status_code == 404


def test_resolve_tenant_narrowing(client):
    _create_agent(client, tenant=TENANT_A)
    r = client.get("/v1/agents/resolve", params={"phone": PHONE_A, "tenant": str(TENANT_B)})
    assert r.status_code == 404
    r = client.get("/v1/agents/resolve", params={"phone": PHONE_A, "tenant": str(TENANT_A)})
    assert r.status_code == 200


# ------------------------------------------------------------ capture CRUD
def test_capture_schema_crud(client):
    agent = _create_agent(client).json()
    r = client.post("/v1/capture-schemas",
                    json={"agent_id": agent["id"], "name": "lead capture",
                          "schema": {"fields": [
                              {"key": "caller_name", "type": "string",
                               "label": "Name", "required": True},
                              {"key": "budget", "type": "number",
                               "label": "Budget"}]}},
                    headers={"X-Tenant-ID": str(TENANT_A)})
    assert r.status_code == 201, r.text
    schema = r.json()
    assert schema["active"] is True
    assert schema["agent_id"] == agent["id"]

    r = client.get(f"/v1/capture-schemas?agent_id={agent['id']}",
                   headers={"X-Tenant-ID": str(TENANT_A)})
    assert [s["id"] for s in r.json()["capture_schemas"]] == [schema["id"]]

    r = client.patch(f"/v1/capture-schemas/{schema['id']}",
                     json={"active": False},
                     headers={"X-Tenant-ID": str(TENANT_A)})
    assert r.status_code == 200
    assert r.json()["active"] is False
    assert r.json()["name"] == "lead capture"  # untouched

    r = client.delete(f"/v1/capture-schemas/{schema['id']}",
                      headers={"X-Tenant-ID": str(TENANT_A)})
    assert r.status_code == 204
    assert client.get("/v1/capture-schemas",
                      headers={"X-Tenant-ID": str(TENANT_A)}
                      ).json()["capture_schemas"] == []


async def test_capture_records_list_filters(client):
    agent = _create_agent(client).json()
    schema = client.post("/v1/capture-schemas",
                         json={"agent_id": agent["id"], "name": "s",
                               "schema": {"fields": []}},
                         headers={"X-Tenant-ID": str(TENANT_A)}).json()
    store = client.app.state.agent_store
    conv1, conv2 = uuid.uuid4(), uuid.uuid4()
    for conv in (conv1, conv2):
        await store.insert_capture_record(
            TENANT_A, uuid.UUID(schema["id"]), uuid.UUID(agent["id"]),
            conv, {"caller_name": "Ada"}, extraction_confidence=0.9)

    r = client.get("/v1/capture-records",
                   headers={"X-Tenant-ID": str(TENANT_A)})
    assert len(r.json()["capture_records"]) == 2
    rec = r.json()["capture_records"][0]
    assert rec["data"] == {"caller_name": "Ada"}
    assert rec["extraction_confidence"] == 0.9

    r = client.get(f"/v1/capture-records?conversation_id={conv1}",
                   headers={"X-Tenant-ID": str(TENANT_A)})
    rows = r.json()["capture_records"]
    assert len(rows) == 1
    assert rows[0]["conversation_id"] == str(conv1)

    r = client.get(f"/v1/capture-records?agent_id={uuid.uuid4()}",
                   headers={"X-Tenant-ID": str(TENANT_A)})
    assert r.json()["capture_records"] == []


# ---------------------------------------------------------------- slug util
def test_slugify():
    assert slugify("Front Desk!") == "front-desk"
    assert slugify("  ACME — Salon  ") == "acme-salon"
    assert slugify("!!!") == "agent"
