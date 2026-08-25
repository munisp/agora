"""USSD session→conversation mapping + inbound hook tests (SPEC-W12):

- deterministic uuid5(tenant, sessionId) conversation key
- cumulative "1*2*3" input parsed to the LAST selection
- low-literacy menu mode: 1/2 select, 0 back, 00 main menu, invalid retry
- request/reply invoke contract (reply text + continue flag for CON/END)
- route wiring: get-or-create conversation (channel="ussd"), user+agent
  turns, idempotent callback replays, incident classifier unchanged
  (ussd→web in the IDP, agent replies never classified)
"""

from __future__ import annotations

import contextlib
import sys
import time
import uuid
from datetime import UTC, datetime

import pytest

sys.path.insert(0, ".")

from app import incidents, ussd  # noqa: E402
from app.config import Config  # noqa: E402
from app.db import NotFoundError  # noqa: E402

pytestmark = pytest.mark.asyncio

TENANT = uuid.uuid4()
SESSION = "ATUid_9f3k2session"
PHONE = "+2348012345678"

MENU = [
    ussd.UssdMenuItem(key="1", label="Book appointment", action="book"),
    ussd.UssdMenuItem(key="2", label="Check booking status", action="status"),
]


# ------------------------------------------------------------- pure logic
def test_session_conversation_id_deterministic_uuid5():
    a = ussd.session_conversation_id(TENANT, SESSION)
    b = ussd.session_conversation_id(TENANT, SESSION)
    assert a == b
    assert a.version == 5
    # matches the contract formula uuid5(tenant, sessionId) exactly
    assert a == uuid.uuid5(uuid.NAMESPACE_URL, f"opendesk:ussd:{TENANT}:{SESSION}")


def test_session_conversation_id_varies_per_tenant_and_session():
    base = ussd.session_conversation_id(TENANT, SESSION)
    assert ussd.session_conversation_id(uuid.uuid4(), SESSION) != base
    assert ussd.session_conversation_id(TENANT, "other-session") != base


def test_parse_last_selection():
    assert ussd.parse_last_selection("") == ""
    assert ussd.parse_last_selection("1") == "1"
    assert ussd.parse_last_selection("1*2*3") == "3"
    assert ussd.parse_last_selection("1**2") == "2"  # empty segment skipped
    assert ussd.parse_last_selection("1*2*") == "2"  # trailing star
    assert ussd.parse_last_selection("hello world") == "hello world"


def test_selection_path():
    assert ussd.selection_path("") == []
    assert ussd.selection_path("1*2*3") == ["1", "2", "3"]


def test_render_menu_numbered_with_nav_footer():
    text = ussd.render_menu(MENU)
    assert "1. Book appointment" in text
    assert "2. Check booking status" in text
    assert "0. Back" in text
    assert "00. Main menu" in text


def test_menu_reply_first_request_shows_menu_and_continues():
    reply, cont, item = ussd.menu_reply(MENU, "")
    assert cont is True
    assert item is None
    assert "1. Book appointment" in reply


def test_menu_reply_select_item_is_terminal():
    reply, cont, item = ussd.menu_reply(MENU, "1")
    assert cont is False  # gateway renders END
    assert item is not None and item.action == "book"
    assert "Book appointment" in reply


def test_menu_reply_cumulative_input_uses_last_selection():
    # user drilled 1 then changed their mind to 2 — LAST selection wins
    reply, cont, item = ussd.menu_reply(MENU, "1*2")
    assert cont is False
    assert item is not None and item.key == "2"
    assert "Check booking status" in reply


def test_menu_reply_back_and_main_menu_rerender():
    for sel in ("0", "00", "1*0", "2*00"):
        reply, cont, item = ussd.menu_reply(MENU, sel)
        assert cont is True, sel
        assert item is None, sel
        assert "1. Book appointment" in reply, sel


def test_menu_reply_invalid_selection_reprompts():
    reply, cont, item = ussd.menu_reply(MENU, "9")
    assert cont is True
    assert item is None
    assert reply.startswith("Invalid selection")
    assert "1. Book appointment" in reply


def _req(**over):
    body = dict(
        tenant_id=TENANT,
        site_slug="acme",
        session_id=SESSION,
        service_code="*384*123#",
        phone_number=PHONE,
        text="",
        menu=None,
    )
    body.update(over)
    return ussd.UssdTurnRequest(**body)


def test_user_turn_text_session_start_marker():
    assert ussd.user_turn_text(_req()) == "(ussd session started via *384*123#)"
    assert ussd.user_turn_text(_req(service_code="")) == "(ussd session started)"


def test_user_turn_text_menu_selection_uses_label():
    text = ussd.user_turn_text(_req(menu=MENU, text="1*2"))
    assert text == "Check booking status (ussd menu option 2)"


def test_user_turn_text_navigation_and_invalid():
    assert ussd.user_turn_text(_req(menu=MENU, text="0")) == "(ussd: back to main menu)"
    assert ussd.user_turn_text(_req(menu=MENU, text="00")) == "(ussd: back to main menu)"
    assert ussd.user_turn_text(_req(menu=MENU, text="9")) == "ussd invalid selection: 9"


def test_user_turn_text_text_mode_raw_last_selection():
    assert ussd.user_turn_text(_req(text="hello")) == "hello"
    assert ussd.user_turn_text(_req(text="1*my house is on fire")) == "my house is on fire"


def test_build_reply_text_mode_ack():
    reply, cont, item = ussd.build_reply(_req(text="hello"), "ACK-TEXT")
    assert (reply, cont, item) == ("ACK-TEXT", True, None)


def test_idempotency_key_stable_per_callback_step():
    k1 = ussd.idempotency_key(_req(text="1"))
    assert k1 == ussd.idempotency_key(_req(text="1"))
    assert k1 != ussd.idempotency_key(_req(text="1*2"))
    assert k1 != ussd.idempotency_key(_req())  # first request distinct


def test_response_payload_contract():
    body = _req(menu=MENU, text="1")
    reply, cont, item = ussd.menu_reply(MENU, "1")
    payload = ussd.response_payload(
        conversation_id=ussd.session_conversation_id(TENANT, SESSION),
        reply=reply,
        continue_session=cont,
        body=body,
        selected=item,
    )
    assert payload["continue"] is False
    assert payload["mode"] == "menu"
    assert payload["selection"] == "1"
    assert payload["action"] == "book"
    assert payload["reply"] == reply
    uuid.UUID(payload["conversation_id"])


def test_config_env(monkeypatch):
    monkeypatch.setenv("USSD_ENABLED", "false")
    monkeypatch.setenv("USSD_TEXT_MODE_REPLY", "custom ack")
    from app.config import load

    cfg = load()
    assert cfg.ussd_enabled is False
    assert cfg.ussd_text_mode_reply == "custom ack"
    cfg = Config()
    assert cfg.ussd_enabled is True
    assert cfg.ussd_text_mode_reply


def test_incidents_channel_map_ussd_is_web_like():
    # SPEC-W12 contract §2: classifier treats ussd as web-like.
    assert incidents.map_channel("ussd") == "web"


# ------------------------------------------------------------- route wiring
class _FakeConn:
    """In-memory incident_counters stand-in (same as test_incidents)."""

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
        self.convs = {}
        self.turns = {}
        self.conn = _FakeConn()
        self.outbox = {}
        self.emitted = {}

    @contextlib.asynccontextmanager
    async def _tenant_tx(self, tenant_id):
        yield self.conn

    async def create_conversation(self, tenant_id, site_slug, channel,
                                  contact_phone=None, conversation_id=None):
        cid = conversation_id or uuid.uuid4()
        if cid in self.convs:  # ON CONFLICT (id) DO NOTHING → re-read winner
            return self.convs[cid]
        rec = dict(id=cid, tenant_id=tenant_id, site_slug=site_slug, channel=channel,
                   contact_phone=contact_phone,
                   started_at=datetime.now(UTC), ended_at=None)
        self.convs[cid] = rec
        return rec

    async def get_conversation(self, cid, tenant_id):
        if cid not in self.convs:
            raise NotFoundError(f"conversation {cid} not found")
        return self.convs[cid]

    async def add_turn(self, cid, tenant_id, role, text, tool_calls,
                       sentiment=None, intent=None, entities=None,
                       idempotency_key=None, outbox=None):
        existing = self.turns.get(cid, [])
        if idempotency_key:
            for t in existing:
                if t["idempotency_key"] == idempotency_key:
                    return t, False, None
        rec = dict(id=uuid.uuid4(), conversation_id=cid, seq=len(existing) + 1,
                   role=role, text=text, tool_calls=tool_calls,
                   sentiment=sentiment, intent=intent, entities=entities,
                   idempotency_key=idempotency_key, ts=datetime.now(UTC))
        existing.append(rec)
        self.turns[cid] = existing
        outbox_id = None
        if outbox is not None:
            topic, builder = outbox
            outbox_id = uuid.uuid4()
            self.outbox[outbox_id] = {"payload": builder(rec), "topic": topic,
                                      "sent": False}
        return rec, True, outbox_id

    async def outbox_mark_sent(self, outbox_id, tenant_id):
        self.outbox[outbox_id]["sent"] = True

    # --- SPEC-W43 Y-03 durable incident gate stand-in (same shape as
    # tests/test_incidents.py _FakeDB) ---
    async def incident_emit_record(self, tenant_id, dedupe_key, build):
        key = (str(tenant_id), dedupe_key)
        row = self.emitted.get(key)
        if row is not None:
            if row["published"]:
                return None, "duplicate"
            return row["payload"], "retry"
        payload = build(f"INC-2026-{len(self.emitted) + 1:06d}")
        self.emitted[key] = {"payload": payload, "published": False}
        return payload, "created"

    async def incident_mark_published(self, tenant_id, dedupe_key):
        self.emitted[(str(tenant_id), dedupe_key)]["published"] = True

    async def incident_unsent(self, limit=100):
        return [
            (uuid.UUID(t), k, r["payload"])
            for (t, k), r in self.emitted.items()
            if not r["published"]
        ][:limit]


class _FakeSink:
    async def publish(self, rec):
        return None


class _FakeDapr:
    def __init__(self):
        self.published: list[tuple[str, dict]] = []

    async def publish_event(self, topic, event):
        self.published.append((topic, event))


def _wiring_app(cfg: Config | None = None):
    from fastapi import FastAPI

    from app.logging import get_logger
    from app.routes import router

    @contextlib.asynccontextmanager
    async def lifespan(app):
        app.state.cfg = cfg or Config()
        app.state.db = _FakeDB()
        app.state.sink = _FakeSink()
        app.state.intel_sink = _FakeSink()
        app.state.dapr = _FakeDapr()
        app.state.log = get_logger("ussd-wiring-test")
        yield

    app = FastAPI(lifespan=lifespan)
    app.include_router(router)
    return app


def _post(client, **over):
    body = {
        "tenant_id": str(TENANT),
        "site_slug": "acme",
        "session_id": SESSION,
        "service_code": "*384*123#",
        "phone_number": PHONE,
        "text": "",
        "menu": None,
    }
    body.update(over)
    return client.post("/v1/ussd/turns", json=body)


@pytest.fixture(autouse=True)
def _clean_dedupe():
    incidents._reset_dedupe()
    yield
    incidents._reset_dedupe()


def test_ussd_menu_session_happy_path():
    from fastapi.testclient import TestClient

    app = _wiring_app()
    with TestClient(app) as c:
        # 1) first callback (empty text) → menu, session continues (CON)
        r = _post(c, menu=[m.model_dump() for m in MENU])
        assert r.status_code == 200, r.text
        body = r.json()
        conv_id = ussd.session_conversation_id(TENANT, SESSION)
        assert body["conversation_id"] == str(conv_id)
        assert body["continue"] is True
        assert body["mode"] == "menu"
        assert "1. Book appointment" in body["reply"]

        # 2) selection "1" → confirmation, session ends (END)
        r = _post(c, text="1", menu=[m.model_dump() for m in MENU])
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["conversation_id"] == str(conv_id)  # same conversation
        assert body["continue"] is False
        assert "Book appointment" in body["reply"]
        assert body["action"] == "book"

        # conversation + turns persisted with the ussd channel
        db = app.state.db
        conv = db.convs[conv_id]
        assert conv["channel"] == "ussd"
        assert conv["contact_phone"] == PHONE
        turns = db.turns[conv_id]
        assert [t["role"] for t in turns] == ["user", "agent", "user", "agent"]
        assert turns[0]["text"] == "(ussd session started via *384*123#)"
        assert turns[2]["text"] == "Book appointment (ussd menu option 1)"
        assert "1. Book appointment" in turns[1]["text"]  # menu render
        assert "Book appointment" in turns[3]["text"]  # confirmation


def test_ussd_callback_replay_is_idempotent():
    from fastapi.testclient import TestClient

    app = _wiring_app()
    with TestClient(app) as c:
        r1 = _post(c, text="1", menu=[m.model_dump() for m in MENU])
        assert r1.status_code == 200, r1.text
        # AT retries the SAME callback (same sessionId + cumulative text)
        r2 = _post(c, text="1", menu=[m.model_dump() for m in MENU])
        assert r2.status_code == 200, r2.text
        assert r2.json()["reply"] == r1.json()["reply"]
        conv_id = ussd.session_conversation_id(TENANT, SESSION)
        # still exactly one user + one agent turn
        assert len(app.state.db.turns[conv_id]) == 2


def test_ussd_text_mode_passthrough():
    from fastapi.testclient import TestClient

    app = _wiring_app()
    with TestClient(app) as c:
        r = _post(c, text="I want to book for tomorrow")
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["continue"] is True
        assert body["mode"] == "text"
        assert body["reply"] == Config().ussd_text_mode_reply
        conv_id = ussd.session_conversation_id(TENANT, SESSION)
        turns = app.state.db.turns[conv_id]
        assert turns[0]["text"] == "I want to book for tomorrow"
        assert turns[0]["role"] == "user"


def _wait_for_idps(dapr, n=1, timeout=5.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        idps = [e for t, e in dapr.published
                if e.get("type") == "com.opendesk.incidents.IDPCreated"]
        if len(idps) >= n:
            return idps
        time.sleep(0.05)
    return [e for t, e in dapr.published
            if e.get("type") == "com.opendesk.incidents.IDPCreated"]


def test_ussd_user_turn_passes_incident_classifier_unchanged():
    from fastapi.testclient import TestClient

    app = _wiring_app()
    with TestClient(app) as c:
        # text mode: last selection carries an emergency (PCM lexicon hit)
        r = _post(c, text="1*thief dey my compound")
        assert r.status_code == 200, r.text
        idps = _wait_for_idps(app.state.dapr)
        assert len(idps) == 1
        idp = idps[0]["data"]
        assert idp["channel"] == "web"  # ussd treated web-like (contract §2)
        assert idp["severity"] == "high"
        assert idp["incident_type"] == "crime"
        assert idp["callback_number"] == PHONE
        assert idp["conversation_id"] == str(
            ussd.session_conversation_id(TENANT, SESSION)
        )


def test_ussd_agent_reply_turn_never_classified():
    from fastapi.testclient import TestClient

    emergency_menu = [
        {"key": "1", "label": "Report emergency", "action": "sos"},
        {"key": "2", "label": "Opening hours", "action": "info"},
    ]
    app = _wiring_app()
    with TestClient(app) as c:
        # user picks "Report emergency": the USER turn classifies (desired —
        # classifier unchanged), the agent confirmation contains the same
        # lexicon hit but must NOT emit a second IDP.
        r = _post(c, text="1", menu=emergency_menu)
        assert r.status_code == 200, r.text
        assert "Report emergency" in r.json()["reply"]
        idps = _wait_for_idps(app.state.dapr)
        time.sleep(0.2)
        assert len(idps) == 1  # exactly one: the user turn only
        assert idps[0]["data"]["channel"] == "web"


def test_ussd_disabled_returns_503():
    from fastapi.testclient import TestClient

    app = _wiring_app(Config(ussd_enabled=False))
    with TestClient(app) as c:
        r = _post(c)
        assert r.status_code == 503


def test_ussd_invalid_tenant_rejected_422():
    from fastapi.testclient import TestClient

    app = _wiring_app()
    with TestClient(app) as c:
        r = c.post("/v1/ussd/turns", json={
            "tenant_id": "not-a-uuid",
            "site_slug": "acme",
            "session_id": SESSION,
        })
        assert r.status_code == 422
