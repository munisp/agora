"""Capture primitive unit tests (SPEC-W38 F3, gate G4):

SessionEnded -> agent resolve -> active schemas -> turns -> LLM extraction
-> capture_records + CaptureExtracted. Offline: fake DB/store/publisher +
httpx.MockTransport LLM (same approach as test_intel.py/test_quality.py).
Degrade contract: LLM failure/timeout/garbage -> no record, no event, no
crash, offset still committed (_process returns True).
"""

from __future__ import annotations

import json
import sys
import uuid
from datetime import UTC, datetime

import httpx
import pytest

sys.path.insert(0, ".")

from app.capture import (  # noqa: E402
    EVENT_TYPE_CAPTURE_EXTRACTED,
    CaptureExtractor,
    build_capture_event,
    build_extraction_prompt,
    llm_extract_fields,
    parse_extraction,
    schema_fields,
)
from app.config import Config  # noqa: E402
from app.db import NotFoundError  # noqa: E402

pytestmark = pytest.mark.asyncio

TENANT = uuid.uuid4()
AGENT = uuid.uuid4()
CONV = uuid.uuid4()
SCHEMA_ID = uuid.uuid4()

FIELDS = [
    {"key": "caller_name", "type": "string", "label": "Caller name",
     "required": True},
    {"key": "party_size", "type": "number", "label": "Party size"},
    {"key": "confirmed", "type": "boolean", "label": "Confirmed"},
    {"key": "day", "type": "enum", "label": "Day",
     "options": ["Friday", "Saturday"]},
]


# ------------------------------------------------------------------- fakes
class FakeDB:
    def __init__(self, turns):
        self._turns = turns

    async def list_turns(self, conversation_id, tenant_id):
        return self._turns.get(conversation_id, [])


class FakeStore:
    def __init__(self, schemas):
        self._schemas = schemas
        self.records: list[dict] = []

    async def get_agent(self, agent_id, tenant_id):
        if agent_id != AGENT or tenant_id != TENANT:
            raise NotFoundError(f"agent {agent_id} not found")
        return {"id": agent_id, "tenant_id": tenant_id, "status": "active"}

    async def list_capture_schemas(self, tenant_id, agent_id, *,
                                   active_only=False):
        return [s for s in self._schemas if not active_only or s["active"]]

    async def insert_capture_record(self, tenant_id, capture_schema_id,
                                    agent_id, conversation_id, data,
                                    extraction_confidence=None):
        rec = dict(id=uuid.uuid4(), tenant_id=tenant_id,
                   capture_schema_id=capture_schema_id, agent_id=agent_id,
                   conversation_id=conversation_id, data=data,
                   extraction_confidence=extraction_confidence,
                   created_at=datetime.now(UTC))
        self.records.append(rec)
        return rec


class FakePublisher:
    def __init__(self):
        self.published: list[tuple[str, dict]] = []

    async def publish_event(self, topic, event):
        self.published.append((topic, event))


TURNS = [
    {"role": "user", "text": "Hi, this is Ada. Table for 4 on Friday please."},
    {"role": "agent", "text": "Booked for four on Friday. Confirmed."},
]


def make_schema(*, active: bool = True, fields=None) -> dict:
    return {"id": SCHEMA_ID, "tenant_id": TENANT, "agent_id": AGENT,
            "name": "reservation", "active": active,
            "schema": {"fields": FIELDS if fields is None else fields}}


def session_ended(*, agent_id=AGENT, tenant_id=TENANT,
                  conversation_id=CONV) -> bytes:
    data = {"conversationId": str(conversation_id), "channel": "voice",
            "siteSlug": "acme-salon"}
    if agent_id is not None:
        data["agentId"] = str(agent_id)
    return json.dumps({
        "specversion": "1.0",
        "id": str(uuid.uuid4()),
        "source": "voice-agent-runtime",
        "type": "com.opendesk.conversation.SessionEnded",
        "subject": "acme-salon",
        "time": "2026-03-01T10:00:00+00:00",
        "tenantid": str(tenant_id),
        "data": data,
    }).encode()


def llm_ok(request: httpx.Request) -> httpx.Response:
    assert request.headers.get("authorization") == "Bearer ollama"
    body = json.loads(request.content)
    assert body["response_format"] == {"type": "json_object"}
    assert "caller_name" in body["messages"][0]["content"]
    return httpx.Response(200, json={"choices": [{"message": {"content": json.dumps({
        "data": {"caller_name": "Ada", "party_size": "4", "confirmed": "yes",
                 "day": "Friday", "hallucinated_key": "dropped"},
        "confidence": 0.87,
    })}}]})


def make_extractor(*, schemas=None, turns=None, handler=llm_ok):
    cfg = Config(capture_topic="opendesk.conversation.captures",
                 intel_llm=True, intel_llm_timeout_s=3.0)
    store = FakeStore(schemas if schemas is not None else [make_schema()])
    publisher = FakePublisher()
    client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    extractor = CaptureExtractor(
        cfg, FakeDB(turns if turns is not None else {CONV: TURNS}),
        store, publisher, llm_client=client)
    return extractor, store, publisher


# --------------------------------------------------------------- happy path
async def test_extracts_record_and_publishes_event():
    extractor, store, publisher = make_extractor()
    assert await extractor._process(session_ended()) is True

    assert len(store.records) == 1
    rec = store.records[0]
    # declared keys only, coerced to declared types; hallucination dropped
    assert rec["data"] == {"caller_name": "Ada", "party_size": 4.0,
                           "confirmed": True, "day": "Friday"}
    assert rec["extraction_confidence"] == pytest.approx(0.87)
    assert rec["capture_schema_id"] == SCHEMA_ID

    assert len(publisher.published) == 1
    topic, evt = publisher.published[0]
    assert topic == "opendesk.conversation.captures"
    assert evt["type"] == EVENT_TYPE_CAPTURE_EXTRACTED
    assert evt["tenantid"] == str(TENANT)
    assert evt["subject"] == "acme-salon"
    d = evt["data"]
    assert d["record_id"] == str(rec["id"])
    assert d["tenant_id"] == str(TENANT)
    assert d["agent_id"] == str(AGENT)
    assert d["conversation_id"] == str(CONV)
    assert d["schema_id"] == str(SCHEMA_ID)
    assert d["data"]["caller_name"] == "Ada"


async def test_confidence_absent_when_model_omits_it():
    def handler(request):
        return httpx.Response(200, json={"choices": [{"message": {"content":
            '{"data": {"caller_name": "Ada"}}'}}]})

    extractor, store, _ = make_extractor(handler=handler)
    assert await extractor._process(session_ended()) is True
    assert store.records[0]["extraction_confidence"] is None


# ------------------------------------------------------------ degrade paths
async def test_llm_timeout_no_record_no_crash():
    def handler(request):
        raise httpx.TimeoutException("timed out")

    extractor, store, publisher = make_extractor(handler=handler)
    # offset committed (True), no record, no event, no exception
    assert await extractor._process(session_ended()) is True
    assert store.records == []
    assert publisher.published == []


async def test_llm_http_error_no_record():
    def handler(request):
        return httpx.Response(500, text="boom")

    extractor, store, publisher = make_extractor(handler=handler)
    assert await extractor._process(session_ended()) is True
    assert store.records == []
    assert publisher.published == []


async def test_llm_garbage_json_no_record():
    def handler(request):
        return httpx.Response(200, json={"choices": [{"message": {"content":
            "I cannot help with that."}}]})

    extractor, store, publisher = make_extractor(handler=handler)
    assert await extractor._process(session_ended()) is True
    assert store.records == []
    assert publisher.published == []


# ---------------------------------------------------------------- skip gates
async def test_skips_when_no_agent_id_on_session():
    extractor, store, publisher = make_extractor()
    assert await extractor._process(session_ended(agent_id=None)) is True
    assert store.records == [] and publisher.published == []


async def test_skips_when_agent_not_in_tenant():
    extractor, store, publisher = make_extractor()
    assert await extractor._process(session_ended(tenant_id=uuid.uuid4())) is True
    assert store.records == [] and publisher.published == []


async def test_skips_when_no_active_schema():
    extractor, store, publisher = make_extractor(
        schemas=[make_schema(active=False)])
    assert await extractor._process(session_ended()) is True
    assert store.records == [] and publisher.published == []


async def test_skips_when_schema_has_no_fields():
    extractor, store, publisher = make_extractor(
        schemas=[make_schema(fields=[])])
    assert await extractor._process(session_ended()) is True
    assert store.records == [] and publisher.published == []


async def test_skips_when_no_turns():
    extractor, store, publisher = make_extractor(turns={})
    assert await extractor._process(session_ended()) is True
    assert store.records == [] and publisher.published == []


async def test_skips_other_event_types_and_poison():
    extractor, store, publisher = make_extractor()
    env = json.loads(session_ended())
    env["type"] = "com.opendesk.conversation.SessionStarted"
    assert await extractor._process(json.dumps(env).encode()) is True
    assert await extractor._process(b"{not json") is True
    env2 = json.loads(session_ended())
    env2["data"]["conversationId"] = "not-a-uuid"
    assert await extractor._process(json.dumps(env2).encode()) is True
    assert store.records == [] and publisher.published == []


# ------------------------------------------------------------- pure helpers
def test_parse_extraction_tolerates_bare_object_and_fences():
    out = parse_extraction(
        '```json\n{"caller_name": "Ada", "party_size": 4}\n```', FIELDS)
    assert out == {"data": {"caller_name": "Ada", "party_size": 4},
                   "confidence": None}
    assert parse_extraction("no json here", FIELDS) is None


def test_parse_extraction_clamps_confidence():
    out = parse_extraction('{"data": {}, "confidence": 7}', FIELDS)
    assert out["confidence"] == 1.0


def test_schema_fields_drops_junk():
    assert schema_fields({"fields": [{"key": "a"}, {"nokey": 1}, "x", {}]}) == [
        {"key": "a"}]
    assert schema_fields({}) == []


def test_build_extraction_prompt_lists_fields_and_options():
    prompt = build_extraction_prompt(FIELDS)
    assert '"caller_name" (string)' in prompt
    assert "one of: Friday, Saturday" in prompt
    assert "[required]" in prompt


def test_build_capture_event_contract():
    env = json.loads(session_ended())
    record = {"id": uuid.uuid4(), "tenant_id": TENANT, "agent_id": AGENT,
              "conversation_id": CONV, "capture_schema_id": SCHEMA_ID,
              "data": {"caller_name": "Ada"}}
    evt = build_capture_event(record, env)
    assert evt["specversion"] == "1.0"
    assert evt["type"] == EVENT_TYPE_CAPTURE_EXTRACTED
    assert set(evt["data"]) == {"record_id", "tenant_id", "agent_id",
                                "conversation_id", "schema_id", "data"}
    # original record dict not mutated
    assert "record_id" not in record


async def test_llm_extract_fields_direct():
    out = await llm_extract_fields(
        "user: Ada here", FIELDS, Config(intel_llm=True),
        client=httpx.AsyncClient(transport=httpx.MockTransport(llm_ok)))
    assert out["data"]["caller_name"] == "Ada"
    assert out["confidence"] == pytest.approx(0.87)
