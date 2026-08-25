"""Smoke test for conversation-service routes with in-memory fakes (no PG/Kafka)."""
import asyncio
import contextlib
import uuid
from datetime import UTC, datetime
from types import SimpleNamespace

from fastapi import FastAPI
from fastapi.testclient import TestClient

import sys
sys.path.insert(0, ".")

from app.routes import router  # noqa: E402
from app.logging import get_logger  # noqa: E402

TENANT = uuid.uuid4()


class FakeDB:
    def __init__(self):
        self.convs = {}
        self.turns = {}
        self.outbox = {}

    async def ping(self):
        return None

    async def create_conversation(self, tenant_id, site_slug, channel, contact_phone=None):
        cid = uuid.uuid4()
        rec = dict(id=cid, tenant_id=tenant_id, site_slug=site_slug, channel=channel,
                   contact_phone=contact_phone,
                   started_at=datetime.now(UTC), ended_at=None)
        self.convs[cid] = rec
        return rec

    async def list_conversations(self, tenant_id, limit, offset, contact=None):
        rows = [c for c in self.convs.values() if c["tenant_id"] == tenant_id]
        if contact:
            rows = [c for c in rows if c.get("contact_phone") == contact]
        return rows[offset:offset + limit]

    async def get_conversation(self, cid, tenant_id):
        from app.db import NotFoundError
        c = self.convs.get(cid)
        if not c or c["tenant_id"] != tenant_id:
            raise NotFoundError("nope")
        return c

    async def add_turn(self, cid, tenant_id, role, text, tool_calls,
                       sentiment=None, intent=None, entities=None,
                       idempotency_key=None, outbox=None):
        import asyncpg
        if cid not in self.convs:
            raise asyncpg.ForeignKeyViolationError("fk")
        if idempotency_key:
            for t in self.turns.get(cid, []):
                if t.get("idempotency_key") == idempotency_key:
                    return t, False, None
        seq = len(self.turns.get(cid, [])) + 1
        rec = dict(id=uuid.uuid4(), conversation_id=cid, seq=seq, role=role, text=text,
                   tool_calls=tool_calls, sentiment=sentiment, intent=intent,
                   entities=entities, idempotency_key=idempotency_key,
                   ts=datetime.now(UTC))
        self.turns.setdefault(cid, []).append(rec)
        outbox_id = None
        if outbox is not None:
            topic, builder = outbox
            outbox_id = uuid.uuid4()
            self.outbox[outbox_id] = {
                "id": outbox_id, "tenant_id": tenant_id, "topic": topic,
                "payload": builder(rec), "sent": False,
            }
        return rec, True, outbox_id

    async def outbox_mark_sent(self, outbox_id, tenant_id):
        self.outbox[outbox_id]["sent"] = True

    async def list_turns(self, cid, tenant_id):
        return self.turns.get(cid, [])


class FakeSink:
    def __init__(self):
        self.records = []

    async def publish(self, rec):
        self.records.append(rec)


class FakeDapr:
    def __init__(self):
        self.events = []

    async def publish_event(self, topic, event):
        self.events.append((topic, event))


@contextlib.asynccontextmanager
async def fake_lifespan(app):
    from app.config import Config
    # SPEC-W43 C1: this smoke suite exercises the standalone-dev path
    # (direct ?tenant=/X-Tenant-ID selection) — the gateway-bound
    # X-Tenant-Slugs matrix lives in tests/test_tenant_binding.py.
    app.state.cfg = Config(trust_direct_tenant=True)
    app.state.db = FakeDB()
    app.state.sink = FakeSink()
    app.state.intel_sink = FakeSink()
    app.state.dapr = FakeDapr()
    app.state.log = get_logger("smoke")
    yield


app = FastAPI(lifespan=fake_lifespan)
app.include_router(router)


def main():
    with TestClient(app) as c:
        # 401 without tenant scope (J-14: fail closed, never query with the
        # tenant GUC unset)
        r = c.get("/v1/conversations")
        assert r.status_code == 401, r.text

        r = c.post("/v1/conversations", json={
            "tenant_id": str(TENANT), "site_slug": "acme", "channel": "voice"})
        assert r.status_code == 201, r.text
        conv = r.json()
        cid = conv["id"]

        # turns: 401 without tenant, 201 with
        r = c.post(f"/v1/conversations/{cid}/turns", json={"role": "user", "text": "hi"})
        assert r.status_code == 401, r.text
        r = c.post(f"/v1/conversations/{cid}/turns?tenant={TENANT}",
                   json={"role": "user", "text": "hi", "tool_calls": [{"name": "get_business_info"}]})
        assert r.status_code == 201, r.text
        turn = r.json()["turn"]
        assert turn["seq"] == 1 and turn["tool_calls"][0]["name"] == "get_business_info"

        r = c.post(f"/v1/conversations/{cid}/turns?tenant={TENANT}",
                   headers={"X-Tenant-ID": str(TENANT)},
                   json={"role": "agent", "text": "hello!"})
        assert r.status_code == 201 and r.json()["turn"]["seq"] == 2

        # sink + dapr got both turns
        sink: FakeSink = app.state.sink
        dapr: FakeDapr = app.state.dapr
        assert len(sink.records) == 2, sink.records
        assert sink.records[0]["conversationId"] == cid
        assert set(sink.records[0].keys()) == {"conversationId", "tenantId", "role", "text", "ts"}
        assert len(dapr.events) == 2
        topic, evt = dapr.events[0]
        assert topic == "opendesk.conversation.transcripts"
        assert evt["specversion"] == "1.0" and evt["tenantid"] == str(TENANT)
        assert evt["subject"] == "acme"
        assert evt["data"]["role"] == "user"

        # call intelligence: enrichment stored on turns + published to the
        # enriched sink (SPEC-W3 §4, innovation 3)
        intel_sink: FakeSink = app.state.intel_sink
        assert len(intel_sink.records) == 2, intel_sink.records
        en = intel_sink.records[0]
        assert en["conversationId"] == cid and en["seq"] == 1
        assert set(en.keys()) == {"conversationId", "tenantId", "siteSlug", "seq",
                                  "role", "text", "sentiment", "sentimentLabel",
                                  "intent", "entities", "ts"}
        assert en["siteSlug"] == "acme"
        assert "sentiment" in turn and turn["intent"] is None  # INTEL_LLM off by default

        # SPEC-W3 §3: Idempotency-Key dedupe — replay returns the original
        # turn with 200 and does not re-publish any event.
        sink_events_before = len(sink.records)
        dapr_events_before = len(dapr.events)
        r = c.post(f"/v1/conversations/{cid}/turns?tenant={TENANT}",
                   headers={"Idempotency-Key": "turn-key-1"},
                   json={"role": "user", "text": "book me"})
        assert r.status_code == 201, r.text
        first_turn = r.json()["turn"]
        r = c.post(f"/v1/conversations/{cid}/turns?tenant={TENANT}",
                   headers={"Idempotency-Key": "turn-key-1"},
                   json={"role": "user", "text": "book me"})
        assert r.status_code == 200, r.text  # replay: existing turn, not 201
        replay_turn = r.json()["turn"]
        assert replay_turn["id"] == first_turn["id"]
        assert replay_turn["seq"] == first_turn["seq"]
        # exactly one new turn and one event round for the two requests
        assert len(app.state.db.turns[uuid.UUID(cid)]) == 3
        assert len(sink.records) == sink_events_before + 1
        assert len(dapr.events) == dapr_events_before + 1

        # 404 for unknown conversation turn append
        r = c.post(f"/v1/conversations/{uuid.uuid4()}/turns?tenant={TENANT}",
                   json={"role": "user", "text": "hi"})
        assert r.status_code == 404, r.text

        # list + detail
        r = c.get(f"/v1/conversations?tenant={TENANT}")
        assert r.status_code == 200 and len(r.json()["conversations"]) == 1
        r = c.get(f"/v1/conversations/{cid}", headers={"X-Tenant-ID": str(TENANT)})
        assert r.status_code == 200, r.text
        body = r.json()
        assert len(body["turns"]) == 3 and body["turns"][0]["role"] == "user"

        # detail for other tenant -> 404
        r = c.get(f"/v1/conversations/{cid}?tenant={uuid.uuid4()}")
        assert r.status_code == 404

        print("SMOKE_OK")


if __name__ == "__main__":
    main()
