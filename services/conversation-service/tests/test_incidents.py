"""Emergency-intent classifier + IDP emission tests (SPEC-W11 Part A):

- lexicon scoring EN + PCM, severity tiers, incident_type, hazard extraction
- threshold gating (INCIDENT_MIN_SCORE)
- build_idp canonical shape vs docs/schemas/incident-data-packet.json
- reference-number format/increment (fake counter store)
- emission dedupe per conversation_id+turn_id + CloudEvent contract
- route wiring: emergency user turn -> non-blocking IDP publish
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import sys
import time
import uuid
from datetime import UTC, datetime
from pathlib import Path

import pytest

sys.path.insert(0, ".")

from app.config import Config  # noqa: E402
from app import incidents  # noqa: E402

pytestmark = pytest.mark.asyncio

SCHEMA_PATH = (
    Path(__file__).resolve().parents[3] / "docs" / "schemas" / "incident-data-packet.json"
)

TENANT = uuid.uuid4()
CONV = uuid.uuid4()


# ------------------------------------------------------------------ fakes
class _FakeConn:
    """In-memory incident_counters stand-in (upsert semantics)."""

    def __init__(self):
        self.counters: dict[tuple[str, int], int] = {}

    async def fetchval(self, sql, tenant_id, year):
        key = (str(tenant_id), year)
        self.counters[key] = self.counters.get(key, 0) + 1
        return self.counters[key]

    async def execute(self, *args):
        return None


class _FakeDB:
    def __init__(self):
        self.conn = _FakeConn()

    @contextlib.asynccontextmanager
    async def _tenant_tx(self, tenant_id):
        yield self.conn


class _FakeDapr:
    def __init__(self):
        self.published: list[tuple[str, dict]] = []

    async def publish_event(self, topic, event):
        self.published.append((topic, event))


@pytest.fixture(autouse=True)
def _clean_dedupe():
    incidents._reset_dedupe()
    yield
    incidents._reset_dedupe()


def _emit_kwargs(db, dapr, **over):
    kw = dict(
        cfg=Config(),
        db=db,
        dapr=dapr,
        tenant_id=TENANT,
        conversation_id=CONV,
        turn_id=uuid.uuid4(),
        text="there's been an accident",
        channel="voice",
        site_slug="acme",
        contact_phone="+2348012345678",
        captured_at=datetime(2026, 2, 3, 10, 30, tzinfo=UTC),
    )
    kw.update(over)
    return kw


# ------------------------------------------------------------- classifier
def test_classify_pcm_thief_dey():
    r = incidents.classify_emergency("thief dey my compound")
    assert r["score"] >= 0.75
    assert r["severity"] == "high"
    assert r["incident_type"] == "crime"


def test_classify_en_accident():
    r = incidents.classify_emergency("there's been an accident on the expressway")
    assert r["score"] == 0.75
    assert r["severity"] == "high"
    assert r["incident_type"] == "crash"
    assert "traffic" in r["hazards"]


def test_classify_pcm_fire_critical():
    r = incidents.classify_emergency("fire dey burn my house o")
    assert r["score"] == 1.0
    assert r["severity"] == "critical"
    assert r["incident_type"] == "fire"
    assert "fire" in r["hazards"]


def test_classify_critical_dying_kidnap():
    for text in ("he is dying please", "they want to kidnap my brother"):
        r = incidents.classify_emergency(text)
        assert r["severity"] == "critical", text
        assert r["score"] == 1.0, text


def test_classify_armed_robbery_weapons_hazard():
    r = incidents.classify_emergency("armed robbery at my shop, they have guns")
    assert r["severity"] == "critical"
    assert r["incident_type"] == "crime"
    assert "weapons" in r["hazards"]


def test_classify_medium_below_default_threshold():
    # single medium signal: detected but below the 0.6 default threshold
    r = incidents.classify_emergency("help me")
    assert r["score"] == 0.5
    assert r["severity"] == "medium"
    hit, _ = incidents.is_emergency("help me", Config().incident_min_score)
    assert hit is False


def test_classify_normal_chat_negative():
    for text in (
        "I would like to book a haircut on Tuesday at ten",
        "good afternoon, what time do you open tomorrow?",
        "thank you so much, the service was wonderful",
        "can you help me with my booking reference?",  # 0.5 < 0.6: no emit
    ):
        hit, r = incidents.is_emergency(text, Config().incident_min_score)
        assert hit is False, text
        assert r["score"] < 0.6, text


def test_classify_no_signal_is_zero():
    r = incidents.classify_emergency("I want to confirm my appointment")
    assert r["score"] == 0.0
    assert r["severity"] is None
    assert r["hazards"] == []


def test_hazard_extraction_gas_and_injuries():
    hazards = incidents.extract_hazards("gas leak in the kitchen and somebody is bleeding")
    assert "gas" in hazards
    assert "injuries" in hazards


def test_threshold_gating_env_override():
    cfg = Config(incident_min_score=0.5)
    hit, _ = incidents.is_emergency("help me", cfg.incident_min_score)
    assert hit is True
    cfg = Config(incident_min_score=0.9)
    hit, _ = incidents.is_emergency("emergency", cfg.incident_min_score)
    assert hit is False  # 0.75 < 0.9


def test_config_env(monkeypatch):
    monkeypatch.setenv("INCIDENT_MIN_SCORE", "0.8")
    monkeypatch.setenv("INCIDENTS_TOPIC", "custom.incidents")
    monkeypatch.setenv("INCIDENT_ENABLED", "false")
    from app.config import load

    cfg = load()
    assert cfg.incident_min_score == 0.8
    assert cfg.incidents_topic == "custom.incidents"
    assert cfg.incident_enabled is False


# ---------------------------------------------------- reference numbering
async def test_reference_number_format_and_increment():
    db = _FakeDB()
    now = datetime(2026, 6, 1, tzinfo=UTC)
    r1 = await incidents.next_reference_number(db, TENANT, now=now)
    r2 = await incidents.next_reference_number(db, TENANT, now=now)
    assert r1 == "INC-2026-000001"
    assert r2 == "INC-2026-000002"
    other = await incidents.next_reference_number(db, uuid.uuid4(), now=now)
    assert other == "INC-2026-000001"  # per-tenant sequence


# ------------------------------------------------------------ IDP shape
async def test_emit_builds_canonical_idp_valid_against_schema():
    jsonschema = pytest.importorskip("jsonschema")
    db, dapr = _FakeDB(), _FakeDapr()
    idp = await incidents.emit_for_turn(**_emit_kwargs(db, dapr))
    assert idp is not None
    schema = json.loads(SCHEMA_PATH.read_text())
    jsonschema.Draft202012Validator.check_schema(schema)
    jsonschema.validate(idp, schema)


async def test_emit_idp_field_values():
    db, dapr = _FakeDB(), _FakeDapr()
    turn_id = uuid.uuid4()
    idp = await incidents.emit_for_turn(**_emit_kwargs(db, dapr, turn_id=turn_id))
    assert idp["schema_version"] == "1.0"
    assert idp["tenant_id"] == str(TENANT)
    assert idp["conversation_id"] == str(CONV)
    assert idp["incident_id"] == str(incidents.incident_uuid(CONV, turn_id))
    assert idp["captured_at"] == "2026-02-03T10:30:00+00:00"
    assert idp["channel"] == "voice"
    assert idp["location"] is None
    assert idp["callback_number"] == "+2348012345678"
    assert idp["incident_type"] == "crash"
    assert idp["severity"] == "high"
    assert idp["people_involved"] == 0
    assert idp["hazards"] == ["traffic"]
    assert idp["narrative_summary"] == "there's been an accident"
    assert idp["reference_number"] == "INC-2026-000001"
    assert idp["contact_id"] is None


async def test_build_idp_structural_contract():
    """Dependency-free structural check (always runs, jsonschema or not)."""
    cls = incidents.classify_emergency("blood everywhere, she is dying")
    idp = incidents.build_idp(
        tenant_id=TENANT,
        conversation_id=CONV,
        turn_id=uuid.uuid4(),
        text="blood everywhere, she is dying",
        channel="chat",
        classification=cls,
        reference_number="INC-2026-000042",
        captured_at=datetime(2026, 1, 1, tzinfo=UTC),
    )
    assert set(idp.keys()) == {
        "incident_id", "schema_version", "tenant_id", "captured_at", "channel",
        "location", "callback_number", "incident_type", "severity",
        "people_involved", "hazards", "narrative_summary", "reference_number",
        "contact_id", "conversation_id",
    }
    assert idp["channel"] == "web"  # chat -> web mapping
    assert idp["severity"] == "critical"
    assert idp["incident_type"] == "medical"
    assert set(idp["hazards"]) <= {"weapons", "injuries", "fire", "gas", "traffic"}
    uuid.UUID(idp["incident_id"])  # parses


def test_narrative_summary_truncated_to_500():
    cls = incidents.classify_emergency("emergency")
    long_text = "emergency " + "x" * 900
    idp = incidents.build_idp(
        tenant_id=TENANT,
        conversation_id=CONV,
        turn_id=uuid.uuid4(),
        text=long_text,
        channel="voice",
        classification=cls,
        reference_number="INC-2026-000001",
        captured_at=datetime.now(UTC),
    )
    assert len(idp["narrative_summary"]) == 500


# ------------------------------------------------------------- emission
async def test_emit_publishes_cloudevent_to_incidents_topic():
    db, dapr = _FakeDB(), _FakeDapr()
    idp = await incidents.emit_for_turn(**_emit_kwargs(db, dapr))
    assert len(dapr.published) == 1
    topic, event = dapr.published[0]
    assert topic == "opendesk.incidents"
    assert event["specversion"] == "1.0"
    assert event["type"] == "com.opendesk.incidents.IDPCreated"
    assert event["source"] == "//conversation-service"
    assert event["id"] == idp["incident_id"]  # CloudEvent id = incident_id (dedupe)
    assert event["tenantid"] == str(TENANT)
    assert event["subject"] == "acme"
    assert event["data"] == idp


async def test_emit_below_threshold_publishes_nothing():
    db, dapr = _FakeDB(), _FakeDapr()
    out = await incidents.emit_for_turn(**_emit_kwargs(db, dapr, text="hi, I want to book"))
    assert out is None
    assert dapr.published == []


async def test_emit_dedupe_per_conversation_and_turn():
    db, dapr = _FakeDB(), _FakeDapr()
    turn_id = uuid.uuid4()
    first = await incidents.emit_for_turn(**_emit_kwargs(db, dapr, turn_id=turn_id))
    second = await incidents.emit_for_turn(**_emit_kwargs(db, dapr, turn_id=turn_id))
    assert first is not None
    assert second is None
    assert len(dapr.published) == 1
    # a different turn in the same conversation DOES emit
    third = await incidents.emit_for_turn(**_emit_kwargs(db, dapr, turn_id=uuid.uuid4()))
    assert third is not None
    assert len(dapr.published) == 2
    # deterministic incident ids differ per turn
    assert first["incident_id"] != third["incident_id"]


async def test_emit_failure_logged_never_raised(caplog):
    class _BoomDapr:
        async def publish_event(self, topic, event):
            raise RuntimeError("broker down")

    db = _FakeDB()
    out = await incidents.emit_for_turn(**_emit_kwargs(db, _BoomDapr()))
    assert out is None  # failure swallowed
    # failed key is NOT latched: a retry of the same turn may emit later
    db2, dapr2 = _FakeDB(), _FakeDapr()
    retry = await incidents.emit_for_turn(**_emit_kwargs(db2, dapr2))
    assert retry is not None


# --------------------------------------------------------- route wiring
def _wiring_app():
    from fastapi import FastAPI

    from app.logging import get_logger
    from app.routes import router

    class FakeDB(_FakeDB):
        def __init__(self):
            super().__init__()
            self.convs = {}
            self.turns = {}

        async def create_conversation(self, tenant_id, site_slug, channel, contact_phone=None):
            cid = uuid.uuid4()
            rec = dict(id=cid, tenant_id=tenant_id, site_slug=site_slug, channel=channel,
                       contact_phone=contact_phone,
                       started_at=datetime.now(UTC), ended_at=None)
            self.convs[cid] = rec
            return rec

        async def get_conversation(self, cid, tenant_id):
            return self.convs[cid]

        async def add_turn(self, cid, tenant_id, role, text, tool_calls,
                           sentiment=None, intent=None, entities=None,
                           idempotency_key=None):
            seq = len(self.turns.get(cid, [])) + 1
            rec = dict(id=uuid.uuid4(), conversation_id=cid, seq=seq, role=role,
                       text=text, tool_calls=tool_calls, sentiment=sentiment,
                       intent=intent, entities=entities,
                       idempotency_key=idempotency_key, ts=datetime.now(UTC))
            self.turns.setdefault(cid, []).append(rec)
            return rec, True

    class FakeSink:
        async def publish(self, rec):
            return None

    @contextlib.asynccontextmanager
    async def lifespan(app):
        app.state.cfg = Config()
        app.state.db = FakeDB()
        app.state.sink = FakeSink()
        app.state.intel_sink = FakeSink()
        app.state.dapr = _FakeDapr()
        app.state.log = get_logger("wiring-test")
        yield

    app = FastAPI(lifespan=lifespan)
    app.include_router(router)
    return app


def test_route_wiring_emits_idp_on_emergency_turn():
    from fastapi.testclient import TestClient

    app = _wiring_app()
    with TestClient(app) as c:
        r = c.post("/v1/conversations", json={
            "tenant_id": str(TENANT), "site_slug": "acme", "channel": "voice",
            "contact_phone": "+2348099999999"})
        assert r.status_code == 201, r.text
        cid = r.json()["id"]

        # normal turn: no incident event
        r = c.post(f"/v1/conversations/{cid}/turns?tenant={TENANT}",
                   json={"role": "user", "text": "I want to book for tomorrow"})
        assert r.status_code == 201, r.text

        # emergency user turn: IDP emitted (background task — poll briefly)
        r = c.post(f"/v1/conversations/{cid}/turns?tenant={TENANT}",
                   json={"role": "user", "text": "thief dey my compound"})
        assert r.status_code == 201, r.text

        dapr = app.state.dapr
        deadline = time.time() + 5
        incident_events = []
        while time.time() < deadline:
            incident_events = [e for t, e in dapr.published
                               if e.get("type") == "com.opendesk.incidents.IDPCreated"]
            if incident_events:
                break
            time.sleep(0.05)
        assert len(incident_events) == 1
        idp = incident_events[0]["data"]
        assert idp["severity"] == "high"
        assert idp["incident_type"] == "crime"
        assert idp["callback_number"] == "+2348099999999"
        assert idp["reference_number"].startswith("INC-")
        # agent-role turns never classify as caller emergencies
        r = c.post(f"/v1/conversations/{cid}/turns?tenant={TENANT}",
                   json={"role": "agent", "text": "emergency services are on the way"})
        assert r.status_code == 201, r.text
        time.sleep(0.2)
        assert len([e for t, e in dapr.published
                    if e.get("type") == "com.opendesk.incidents.IDPCreated"]) == 1
