"""ChatEndTracker: quiet text conversations end with ONE SessionEnded carrying a call-shaped quality block (2026-11-24)."""
import asyncio
from types import SimpleNamespace

from app.chat_end import SESSION_ENDED, ChatEndTracker


class _Dapr:
    def __init__(self):
        self.sent = []

    async def publish_best_effort(self, pubsub, topic, event, kind=None):
        self.sent.append((topic, event))


class _Clock:
    def __init__(self):
        self.t = 1000.0

    def __call__(self):
        return self.t


def _session(**kw):
    base = dict(conversation_id="conv-1", channel="whatsapp", verified_phone="+2348035550001", confirmed_phone=None,
                claimed_phone=None, caller_name="Ada Obi", caller_email=None, active_language="pcm",
                escalation_room=None, resolved_agent_id=None, site_slug="acme", stored_id_lookup=lambda: "stored-9")
    base.update(kw)
    return SimpleNamespace(**base)


_CTX = SimpleNamespace(tenant_id="11111111-1111-1111-1111-111111111111", tenant_slug="acme", site_slug="acme")
_SETTINGS = SimpleNamespace(dapr_pubsub="pubsub-kafka", conversation_events_topic="opendesk.conversation.events")


def _tracker():
    dapr, clock = _Dapr(), _Clock()
    return ChatEndTracker(dapr=dapr, settings=_SETTINGS, idle_s=600, clock=clock), dapr, clock


def test_quiet_whatsapp_chat_ends_once_with_quality():
    tr, dapr, clock = _tracker()
    tr.touch(_session(), _CTX, [{"tool": "get_availability"}])
    clock.t += 30
    tr.touch(_session(), _CTX, [{"tool": "book_appointment"}])
    clock.t += 599
    assert asyncio.run(tr.sweep_once()) == 0  # not quiet long enough yet
    clock.t += 2
    assert asyncio.run(tr.sweep_once()) == 1
    assert asyncio.run(tr.sweep_once()) == 0  # ended once
    topic, event = dapr.sent[0]
    assert topic == "opendesk.conversation.events" and event["type"] == SESSION_ENDED
    data = event["data"]
    assert data["channel"] == "whatsapp" and data["storedConversationId"] == "stored-9"
    q = data["quality"]
    assert q["channel"] == "whatsapp" and q["direction"] == "inbound"
    assert q["confirmed_phone"] == "+2348035550001" and q["contact_name"] == "Ada Obi"
    assert q["turn_count"] == 2 and q["tool_calls"] == {"get_availability": 1, "book_appointment": 1}
    assert q["language"] == "pcm"


def test_web_chat_uses_the_form_details_and_channel_chat():
    tr, dapr, clock = _tracker()
    tr.touch(_session(channel="web", verified_phone=None, claimed_phone="08035550002", caller_email="a@b.co"), _CTX, [])
    clock.t += 601
    asyncio.run(tr.sweep_once())
    q = dapr.sent[0][1]["data"]["quality"]
    assert q["channel"] == "chat" and q["confirmed_phone"] == "08035550002" and q["contact_email"] == "a@b.co"


def test_no_business_no_event_and_bad_session_never_raises():
    tr, dapr, clock = _tracker()
    tr.touch(_session(), None, [])
    tr.touch(object(), _CTX, None)  # missing attributes: ignored, never raises
    clock.t += 601
    asyncio.run(tr.sweep_once())
    assert dapr.sent == []


def test_whatsapp_messages_with_fresh_sessions_are_one_conversation():
    tr, dapr, clock = _tracker()
    tr.touch(_session(conversation_id="sess-a"), _CTX, [])
    clock.t += 60
    tr.touch(_session(conversation_id="sess-b"), _CTX, [])  # new runtime session, same stored conversation
    clock.t += 601
    assert asyncio.run(tr.sweep_once()) == 1
    assert dapr.sent[0][1]["data"]["quality"]["turn_count"] == 2
