"""send_whatsapp_confirmation (2026-11-22): the receptionist WhatsApps the confirmed appointment via the
messaging-gateway; the text is composed by the tool, never by the model."""
from __future__ import annotations

import json

import httpx
import pytest

from app.config import Settings
from app.session_state import SessionState
from app.tenant_context import TenantContext
from app.tools import ToolLayer

from conftest import FakeDapr


def _layer(session: SessionState) -> ToolLayer:
    return ToolLayer(
        dapr=FakeDapr(),  # type: ignore[arg-type]
        settings=Settings(booking_url="http://booking:7002", phone_hash_salt="s", messaging_gateway_url="http://mg:7011"),
        ctx=TenantContext(site_slug="leadway-pensure", tenant_id="t", tenant_slug="leadway-pensure",
                          display_name="Leadway Pensure PFA"),
        session=session,
    )


@pytest.fixture()
def gateway(monkeypatch):
    sent: list[dict] = []
    status = {"code": 200}
    real = httpx.AsyncClient

    def handler(request: httpx.Request) -> httpx.Response:
        sent.append({"url": str(request.url), **json.loads(request.content)})
        return httpx.Response(status["code"], json={"ok": status["code"] < 400})

    monkeypatch.setattr(httpx, "AsyncClient", lambda **kw: real(transport=httpx.MockTransport(handler), **kw))
    return sent, status


async def test_sends_composed_text_to_the_demo_whatsapp_number(gateway):
    sent, _ = gateway
    s = SessionState(conversation_id="c1", site_slug="leadway-pensure")
    s.caller_name, s.confirmed_phone, s.whatsapp_to = "Ngozi Okafor", "+2348065553108", "+2347052256260"
    out = await _layer(s).send_whatsapp_confirmation(offering="RSA Registration Call", team_member="Aisha Bello",
                                                     when="Tuesday 24 November at 9:00 am")
    assert out["status"] == "sent"
    assert sent[0]["url"] == "http://mg:7011/v1/whatsapp/send" and sent[0]["to"] == "+2347052256260"
    assert sent[0]["message"].startswith("Hello Ngozi, this is Leadway Pensure PFA.")
    assert "Your RSA Registration Call with Aisha Bello on Tuesday 24 November at 9:00 am is confirmed." in sent[0]["message"]


async def test_never_falls_back_to_the_contacts_number(gateway):
    sent, _ = gateway
    s = SessionState(conversation_id="c1", site_slug="leadway-pensure")
    s.confirmed_phone = "+2348065553108"  # a demo contact's number: may be a real stranger's
    out = await _layer(s).send_whatsapp_confirmation(offering="Consultation", when="Thursday at 10 am")
    assert out["status"] == "error" and sent == []


async def test_no_number_or_gateway_error_never_raises(gateway):
    sent, status = gateway
    s = SessionState(conversation_id="c1", site_slug="leadway-pensure")
    assert (await _layer(s).send_whatsapp_confirmation(offering="X", when="Y"))["status"] == "error" and not sent
    s.whatsapp_to = "+2347052256260"
    status["code"] = 503
    out = await _layer(s).send_whatsapp_confirmation(offering="X", when="Y")
    assert out["status"] == "error" and "do not mention it" in out["message"]


# ---- POST /voice-admin/whatsapp: the dashboard's separate "WhatsApp reminder" button -----------------------------
from app.control_plane import create_app  # noqa: E402


def _staff_client(monkeypatch, gateway_status=200):
    sent: list[dict] = []
    real = httpx.AsyncClient

    def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "GET" and "/public/sites/" in str(request.url):
            return httpx.Response(200, json={"business_name": "Leadway Pensure PFA"})
        if str(request.url).endswith("/v1/whatsapp/send"):
            sent.append(json.loads(request.content))
            return httpx.Response(gateway_status, json={})
        return httpx.Response(404)

    app = create_app(Settings(booking_url="http://booking:7002", messaging_gateway_url="http://mg:7011",
                              voice_admin_internal_token="t"))
    monkeypatch.setattr(httpx, "AsyncClient", lambda **kw: real(transport=httpx.MockTransport(handler), **kw)
                        if "transport" not in kw else real(**kw))
    client = real(transport=httpx.ASGITransport(app=app), base_url="http://test")
    return client, sent


BODY = {"site_slug": "leadway-pensure", "to": "0705 225 6260", "contact_name": "Ngozi Okafor",
        "offering": "RSA Registration Call", "team_member": "Aisha Bello", "when": "Tuesday 24 November at 9:00 am"}
OWNER = {"X-User-Roles": "owner", "X-Tenant-Slugs": "leadway-pensure"}


async def test_owner_sends_a_reminder_to_the_chosen_number(monkeypatch):
    client, sent = _staff_client(monkeypatch)
    async with client as c:
        r = await c.post("/voice-admin/whatsapp", json=BODY, headers=OWNER)
    assert r.status_code == 200 and r.json()["to"] == "+2347052256260"
    assert sent[0]["to"] == "+2347052256260"
    assert sent[0]["message"].startswith("Hello Ngozi, this is Leadway Pensure PFA.")
    assert "⏰ Reminder: your RSA Registration Call with Aisha Bello is on Tuesday 24 November at 9:00 am." in sent[0]["message"]


async def test_other_business_is_refused_and_nothing_sent(monkeypatch):
    client, sent = _staff_client(monkeypatch)
    async with client as c:
        r = await c.post("/voice-admin/whatsapp", json=BODY, headers={"X-User-Roles": "owner", "X-Tenant-Slugs": "other"})
    assert r.status_code == 401 and sent == []


async def test_whatsapp_off_is_a_clear_error(monkeypatch):
    client, sent = _staff_client(monkeypatch, gateway_status=503)
    async with client as c:
        r = await c.post("/voice-admin/whatsapp", json=BODY, headers=OWNER)
    assert r.status_code == 502 and "not switched on" in r.json()["detail"]
