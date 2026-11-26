"""Transcripts for the business's Conversations page (app/transcripts.py)."""
import asyncio
import json

import httpx

from app.transcripts import TranscriptRecorder, conversation_channel, exchange_turns, tool_turn_text

TENANT = "55ec7166-8dfc-4508-a4b7-f703f8e1312f"


def _client(log):
    def handler(request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content) if request.content else None
        log.append((request.method, request.url.path, request.headers.get("x-tenant-slugs"), body))
        if request.url.path == "/v1/conversations":
            return httpx.Response(201, json={"id": "conv-1"})
        return httpx.Response(201, json={"turn": {}})
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


async def test_one_conversation_per_session_turns_in_order():
    log = []
    rec = TranscriptRecorder("http://conversation:7007", client=_client(log))
    trace = [{"tool": "book_appointment", "arguments": {"offering": "Claim Assessment Visit"}, "result": {"ok": True}}]
    await rec.add_turns("chat:s1", exchange_turns("I had an accident", "Is anyone hurt?", trace),
                        tenant_id=TENANT, site_slug="harbour", channel="web", contact_phone="+2348035550101")
    await rec.add_turns("chat:s1", exchange_turns("No", "Good. Your policy number?", []),
                        tenant_id=TENANT, site_slug="harbour", channel="web")
    creates = [e for e in log if e[1] == "/v1/conversations"]
    assert len(creates) == 1
    assert creates[0][2] == TENANT and creates[0][3]["channel"] == "chat" and creates[0][3]["contact_phone"] == "+2348035550101"
    turns = [(e[3]["role"], e[3]["text"]) for e in log if e[1].endswith("/turns")]
    assert turns == [("user", "I had an accident"), ("tool", "book_appointment(offering: Claim Assessment Visit)"),
                     ("agent", "Is anyone hurt?"), ("user", "No"), ("agent", "Good. Your policy number?")]
    assert all(e[2] == TENANT for e in log)  # every call names the tenant


async def test_record_is_fire_and_forget_and_survives_an_outage():
    def down(request):
        return httpx.Response(503)
    rec = TranscriptRecorder("http://conversation:7007", client=httpx.AsyncClient(transport=httpx.MockTransport(down)))
    rec.record("chat:s2", exchange_turns("hi", "hello", []), tenant_id=TENANT, site_slug="harbour", channel="web")
    await asyncio.sleep(0.05)  # no exception reaches the caller


def test_helpers():
    assert conversation_channel("web") == "chat" and conversation_channel("phone") == "phone" and conversation_channel("voice") == "voice"
    assert tool_turn_text({"tool": "get_availability", "arguments": '{"date": "2026-11-17"}'}) == "get_availability(date: 2026-11-17)"
    assert TranscriptRecorder("").enabled is False
