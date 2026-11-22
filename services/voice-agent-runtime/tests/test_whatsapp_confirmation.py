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
