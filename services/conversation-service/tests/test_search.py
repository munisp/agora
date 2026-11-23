"""SPEC-W117: OpenSearch advanced search — pure query builder, param
validation, endpoint happy path (fake OS client), fail-closed 503."""

from __future__ import annotations

import sys
import uuid
from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

sys.path.insert(0, ".")

from app.routes import _require_tenant  # noqa: E402
from app.search import ALLOWED_ROLES, build_search_query, router  # noqa: E402

TENANT = uuid.uuid4()


# --- pure builder ------------------------------------------------------------
def test_tenant_filter_always_first():
    body = build_search_query(tenant_id=str(TENANT), limit=25, offset=0)
    filt = body["query"]["bool"]["filter"]
    assert filt[0] == {"term": {"tenant_id": str(TENANT)}}
    assert body["query"]["bool"]["must"] == [{"match_all": {}}]
    assert body["size"] == 25 and body["from"] == 0


def test_all_filters_and_q():
    body = build_search_query(
        tenant_id=str(TENANT), q="pay stub", channel="whatsapp",
        site_slug="acme", role="user", conversation_id=str(uuid.uuid4()),
        from_ts="2026-11-01T00:00:00Z", to_ts="2026-11-24T00:00:00Z",
        limit=10, offset=20,
    )
    b = body["query"]["bool"]
    kinds = [list(f)[0] for f in b["filter"]]
    assert kinds == ["term", "term", "term", "term", "term", "range"]
    assert b["must"][0]["match"]["text"]["query"] == "pay stub"
    assert b["must"][0]["match"]["text"]["operator"] == "and"
    rng = b["filter"][-1]["range"]["ts"]
    assert rng["gte"] == "2026-11-01T00:00:00Z"
    assert rng["lte"] == "2026-11-24T00:00:00Z"


def test_blank_q_is_match_all():
    body = build_search_query(tenant_id=str(TENANT), q="   ", limit=5, offset=0)
    assert body["query"]["bool"]["must"] == [{"match_all": {}}]


# --- endpoint ---------------------------------------------------------------
class FakeOS:
    def __init__(self, payload=None, error=None):
        self.payload = payload or {"hits": {"total": {"value": 0}, "hits": []}}
        self.error = error
        self.calls = []

    async def search(self, *, index, body):
        self.calls.append({"index": index, "body": body})
        if self.error:
            raise self.error
        return self.payload


def _app(os_client):
    app = FastAPI()
    app.include_router(router)
    app.state.search_os = os_client
    app.state.cfg = SimpleNamespace(conversations_index="conversations")
    app.dependency_overrides[_require_tenant] = lambda: TENANT
    return app


def _hit(conv=None):
    return {"_source": {
        "conversation_id": conv or str(uuid.uuid4()),
        "site_slug": "acme", "channel": "web", "role": "user",
        "text": "I need a pay stub", "ts": "2026-11-23T10:00:00Z",
    }}


def test_search_happy_path_shape():
    os_client = FakeOS({"hits": {"total": {"value": 1}, "hits": [_hit("c1")]}})
    client = TestClient(_app(os_client))
    resp = client.get("/v1/conversations/search", params={"q": "pay stub"})
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["total"] == 1
    assert body["hits"][0]["conversation_id"] == "c1"
    assert body["hits"][0]["text"] == "I need a pay stub"
    # tenant isolation reached the cluster query
    sent = os_client.calls[0]["body"]
    assert {"term": {"tenant_id": str(TENANT)}} in sent["query"]["bool"]["filter"]
    assert os_client.calls[0]["index"] == "conversations"


def test_search_validation_errors():
    client = TestClient(_app(FakeOS()))
    assert client.get("/v1/conversations/search",
                      params={"from_ts": "not-a-date"}).status_code == 400
    assert client.get("/v1/conversations/search",
                      params={"from_ts": "2026-11-24", "to_ts": "2026-11-01"}
                      ).status_code == 400
    assert client.get("/v1/conversations/search",
                      params={"role": "bogus"}).status_code == 400
    assert client.get("/v1/conversations/search",
                      params={"conversation_id": "nope"}).status_code == 400
    assert client.get("/v1/conversations/search",
                      params={"limit": 100, "offset": 10000}).status_code == 400


def test_search_limit_clamped_and_role_list():
    os_client = FakeOS()
    client = TestClient(_app(os_client))
    resp = client.get("/v1/conversations/search", params={"limit": 500})
    assert resp.status_code == 200
    assert os_client.calls[0]["body"]["size"] == 100
    assert "user" in ALLOWED_ROLES and "agent" in ALLOWED_ROLES


def test_search_os_down_is_loud_503():
    client = TestClient(_app(FakeOS(error=RuntimeError("cluster down"))))
    resp = client.get("/v1/conversations/search")
    assert resp.status_code == 503
    assert "search_unavailable" in resp.text


def test_search_unconfigured_is_503():
    client = TestClient(_app(None))
    resp = client.get("/v1/conversations/search")
    assert resp.status_code == 503


def test_search_filters_reach_cluster():
    os_client = FakeOS()
    client = TestClient(_app(os_client))
    conv = str(uuid.uuid4())
    client.get("/v1/conversations/search", params={
        "channel": "whatsapp", "site_slug": "acme", "role": "user",
        "conversation_id": conv, "from_ts": "2026-11-01", "to_ts": "2026-11-24",
        "offset": 5, "limit": 10,
    })
    body = os_client.calls[0]["body"]
    filt = body["query"]["bool"]["filter"]
    assert {"term": {"channel": "whatsapp"}} in filt
    assert {"term": {"site_slug": "acme"}} in filt
    assert {"term": {"role": "user"}} in filt
    assert {"term": {"conversation_id": conv}} in filt
    assert body["from"] == 5 and body["size"] == 10


def test_search_total_int_form_tolerated():
    os_client = FakeOS({"hits": {"total": 3, "hits": [_hit() for _ in range(3)]}})
    client = TestClient(_app(os_client))
    assert client.get("/v1/conversations/search").json()["total"] == 3
