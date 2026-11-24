"""Search index kept in step with Postgres deletes (2026-11-24).
Offline: fake OpenSearch client / fake db, no Postgres, no Kafka."""

from __future__ import annotations

import asyncio
import json
import sys
import uuid
from datetime import UTC, datetime

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

sys.path.insert(0, ".")

from app import internal_routes  # noqa: E402
from app.indexer import TranscriptIndexer  # noqa: E402
from app.privacy import PrivacyEraseConsumer  # noqa: E402
from app.search_index import SearchIndex  # noqa: E402
from app.tenant_lifecycle import TenantLifecycleConsumer  # noqa: E402

pytestmark = pytest.mark.asyncio

T1, T2 = str(uuid.uuid4()), str(uuid.uuid4())
C_LIVE, C_GONE, C_GONE2 = str(uuid.uuid4()), str(uuid.uuid4()), str(uuid.uuid4())


class _Cfg:
    opensearch_addr = "http://opensearch:9200"
    conversations_index = "conversations"
    internal_token = "secret"
    indexer_prune_seconds = 0
    identity_events_topic = "opendesk.identity.events"
    tenant_events_group = "g"
    kafka_brokers = ["kafka:9092"]


class _FakeOS:
    """delete_by_query + composite-agg search; pages = list of bucket lists."""

    def __init__(self, pages=None, fail=False):
        self.deletes: list[dict] = []
        self.pages = pages or [[]]
        self.fail = fail
        self.searches = 0

    async def delete_by_query(self, index, body, params=None):
        if self.fail:
            raise ConnectionError("opensearch down")
        self.deletes.append({"index": index, "query": body["query"], "params": params})
        return {"deleted": 3}

    async def search(self, index, body):
        if self.fail:
            raise ConnectionError("opensearch down")
        page = self.pages[self.searches]
        self.searches += 1
        agg: dict = {"buckets": page}
        if self.searches < len(self.pages):
            agg["after_key"] = page[-1]["key"]
        return {"aggregations": {"pairs": agg}}


def _index(os_client) -> SearchIndex:
    si = SearchIndex(_Cfg())  # type: ignore[arg-type]
    si._client = os_client
    return si


def _bucket(t, c):
    return {"key": {"t": t, "c": c}, "doc_count": 2}


class TestSearchIndex:
    async def test_delete_conversations_scoped_to_tenant(self):
        fake = _FakeOS()
        n = await _index(fake).delete_conversations(uuid.UUID(T1), [C_GONE])
        assert n == 3
        q = fake.deletes[0]["query"]["bool"]["filter"]
        assert {"term": {"tenant_id": T1}} in q
        assert {"terms": {"conversation_id": [C_GONE]}} in q
        assert fake.deletes[0]["params"]["refresh"] == "true"

    async def test_delete_empty_is_noop(self):
        fake = _FakeOS()
        assert await _index(fake).delete_conversations(uuid.UUID(T1), []) == 0
        assert fake.deletes == []

    async def test_failure_returns_none(self):
        assert await _index(_FakeOS(fail=True)).delete_tenant(uuid.UUID(T1)) is None

    async def test_retention_range(self):
        fake = _FakeOS()
        await _index(fake).delete_older_than(uuid.UUID(T1), 180)
        assert {"range": {"ts": {"lt": "now-180d"}}} in fake.deletes[0]["query"]["bool"]["filter"]

    async def test_prune_deletes_only_missing_conversations(self):
        fake = _FakeOS(pages=[[_bucket(T1, C_LIVE), _bucket(T1, C_GONE)], [_bucket(T2, C_GONE2)]])

        class _DB:
            async def existing_conversation_ids(self, tenant_id, ids):
                return {C_LIVE} & set(ids)

        result = await _index(fake).prune_orphans(_DB())
        assert fake.searches == 2  # followed after_key
        assert result == {T1: 3, T2: 3}
        deleted = {
            (f[0]["term"]["tenant_id"], tuple(f[1]["terms"]["conversation_id"]))
            for f in (d["query"]["bool"]["filter"] for d in fake.deletes)
        }
        assert deleted == {(T1, (C_GONE,)), (T2, (C_GONE2,))}

    async def test_prune_unreachable_returns_none(self):
        class _DB:
            async def existing_conversation_ids(self, tenant_id, ids):
                raise AssertionError("not reached")

        assert await _index(_FakeOS(fail=True)).prune_orphans(_DB()) is None


def _turn_event(conversation_id=C_LIVE):
    return {
        "id": "e1",
        "tenantid": T1,
        "data": {"conversationId": conversation_id, "tenantId": T1, "role": "user",
                 "text": "hello", "ts": datetime(2026, 11, 24, 10, 0, tzinfo=UTC).isoformat()},
    }


class TestIndexerSkipsDeleted:
    def _indexer(self, meta=None, raises=False):
        class _DB:
            async def turn_index_meta(self, cid, tid, ts, role):
                if raises:
                    raise RuntimeError("db down")
                assert ts.tzinfo is not None and role == "user"
                return meta

        return TranscriptIndexer(_Cfg(), _DB())  # type: ignore[arg-type]

    async def test_live_turn_indexed_with_enrichment(self):
        ix = self._indexer({"site_slug": "acme", "channel": "voice", "turn_exists": True})
        doc = await ix._to_doc(_turn_event())
        assert doc["site_slug"] == "acme" and doc["channel"] == "voice"

    async def test_deleted_conversation_skipped(self):
        assert await self._indexer(None)._to_doc(_turn_event()) is None

    async def test_deleted_turn_skipped(self):
        ix = self._indexer({"site_slug": "acme", "channel": "web", "turn_exists": False})
        assert await ix._to_doc(_turn_event()) is None

    async def test_lookup_failure_still_indexes(self):
        doc = await self._indexer(raises=True)._to_doc(_turn_event())
        assert doc is not None and "site_slug" not in doc


class _FakeDB:
    def __init__(self, ids=None):
        self.ids = ids if ids is not None else [uuid.UUID(C_GONE)]

    async def erase_contact_conversations(self, tenant_id, phone, email):
        return (self.ids, 4)

    async def purge_tenant_data(self, tenant_id):
        return (1, 4)


def _gdpr_app(search_index):
    app = FastAPI()
    app.state.cfg = _Cfg()
    app.state.db = _FakeDB()
    app.state.search_index = search_index
    app.include_router(internal_routes.router)
    return TestClient(app)


class TestDeletePathsMirrorToSearch:
    async def test_gdpr_endpoint_deletes_from_index(self):
        fake = _FakeOS()
        resp = _gdpr_app(_index(fake)).post(
            "/internal/gdpr/erase", json={"tenant_id": T1, "phone": "+2348030000000"},
            headers={"X-Internal-Token": "secret"})
        assert resp.status_code == 200
        assert resp.json()["conversations_matched"] == 1
        assert fake.deletes[0]["query"]["bool"]["filter"][1] == {"terms": {"conversation_id": [C_GONE]}}

    async def test_gdpr_endpoint_503_when_index_fails(self):
        resp = _gdpr_app(_index(_FakeOS(fail=True))).post(
            "/internal/gdpr/erase", json={"tenant_id": T1, "phone": "+2348030000000"},
            headers={"X-Internal-Token": "secret"})
        assert resp.status_code == 503

    async def test_prune_endpoint(self):
        class _DB(_FakeDB):
            async def existing_conversation_ids(self, tenant_id, ids):
                return set()

        app = FastAPI()
        app.state.cfg = _Cfg()
        app.state.db = _DB()
        app.state.search_index = _index(_FakeOS(pages=[[_bucket(T1, C_GONE)]]))
        app.include_router(internal_routes.router)
        resp = TestClient(app).post("/internal/search/prune", headers={"X-Internal-Token": "secret"})
        assert resp.status_code == 200
        assert resp.json() == {"pruned_docs": {T1: 3}}

    async def test_privacy_consumer_retries_when_index_fails(self, monkeypatch):
        async def _no_sleep(_s):
            return None

        monkeypatch.setattr(asyncio, "sleep", _no_sleep)
        env = {"type": "PrivacyEraseRequested", "data": {"tenant_id": T1, "phone": "+234"}}
        ok_consumer = PrivacyEraseConsumer(_Cfg(), _FakeDB(), _index(_FakeOS()))  # type: ignore[arg-type]
        assert await ok_consumer._process(json.dumps(env).encode()) is True
        bad = PrivacyEraseConsumer(_Cfg(), _FakeDB(), _index(_FakeOS(fail=True)))  # type: ignore[arg-type]
        assert await bad._process(json.dumps(env).encode()) is False

    async def test_tenant_purge_deletes_tenant_docs(self, monkeypatch):
        async def _no_sleep(_s):
            return None

        monkeypatch.setattr(asyncio, "sleep", _no_sleep)
        fake = _FakeOS()
        env = {"id": "e", "type": "TenantDeleted", "data": {"tenant_id": T1}}
        c = TenantLifecycleConsumer(_Cfg(), _FakeDB(), _index(fake))  # type: ignore[arg-type]
        assert await c._process(json.dumps(env).encode()) is True
        assert fake.deletes[0]["query"] == {"term": {"tenant_id": T1}}
        bad = TenantLifecycleConsumer(_Cfg(), _FakeDB(), _index(_FakeOS(fail=True)))  # type: ignore[arg-type]
        assert await bad._process(json.dumps(env).encode()) is False
