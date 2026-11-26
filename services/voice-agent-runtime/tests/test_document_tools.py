"""2026-11-26: the receptionist knows about caller documents -- send_document_upload_link points to the paperclip on a
web chat, a photo on WhatsApp, and WhatsApps a signed upload link on a call; check_shared_documents answers kinds and
check results (never details), from the chat session or, on a call, the control plane's signed lookup; the system
prompt lists what was shared."""

from __future__ import annotations

import dataclasses

import pytest

from app.config import Settings
from app.documents import document_lookup_sig, verify_document_link
from app.prompts import build_system_prompt, split_prompt
from app.session_state import SessionState
from app.tenant_context import TenantContext
from app.tools import TOOL_NAMES, ToolLayer
from conftest import FakeDapr

SECRET = "link-secret"


def _ctx() -> TenantContext:
    return TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme", display_name="Acme Pensions")


def _settings(**over) -> Settings:
    base = dict(document_link_secret=SECRET, document_link_base_url="https://agora.example/d/", document_link_ttl_s=3600)
    base.update(over)
    return dataclasses.replace(Settings(), **base)


def _layer(session: SessionState, settings: Settings | None = None) -> ToolLayer:
    return ToolLayer(dapr=FakeDapr(), settings=settings or _settings(), ctx=_ctx(), session=session)  # type: ignore[arg-type]


class FakeHttp:
    """Stands in for httpx.AsyncClient: records requests, answers what the test sets."""

    calls: list[tuple[str, str, dict]] = []
    get_json: dict = {"documents": []}

    def __init__(self, *a, **kw):
        pass

    async def __aenter__(self):
        return self

    async def __aexit__(self, *a):
        return False

    async def post(self, url, json=None, **kw):
        FakeHttp.calls.append(("POST", url, json or {}))
        return _Resp(200, {})

    async def get(self, url, headers=None, **kw):
        FakeHttp.calls.append(("GET", url, headers or {}))
        return _Resp(200, FakeHttp.get_json)


class _Resp:
    def __init__(self, code, body):
        self.status_code, self._body, self.text = code, body, ""

    def json(self):
        return self._body


@pytest.fixture(autouse=True)
def fake_http(monkeypatch):
    import httpx

    FakeHttp.calls = []
    FakeHttp.get_json = {"documents": []}
    monkeypatch.setattr(httpx, "AsyncClient", FakeHttp)


def test_tools_are_registered():
    assert "send_document_upload_link" in TOOL_NAMES and "check_shared_documents" in TOOL_NAMES


async def test_web_chat_points_to_the_paperclip():
    s = SessionState(conversation_id="c1", site_slug="demo")
    s.text_chat, s.channel = True, "web"
    out = await _layer(s).send_document_upload_link(document_type="pay_stub")
    assert out["status"] == "use_paperclip" and "paperclip" in out["message"] and "pay stub" in out["message"]
    assert FakeHttp.calls == []


async def test_whatsapp_chat_asks_for_a_photo():
    s = SessionState(conversation_id="c1", site_slug="demo")
    s.text_chat, s.channel = True, "whatsapp"
    out = await _layer(s).send_document_upload_link(document_type="receipt")
    assert out["status"] == "use_whatsapp" and "photo of the receipt" in out["message"]


async def test_call_without_a_whatsapp_number_says_so():
    s = SessionState(conversation_id="c1", site_slug="demo")
    out = await _layer(s).send_document_upload_link(document_type="pay_stub")
    assert out["status"] == "error" and "website" in out["message"]
    assert FakeHttp.calls == []


async def test_call_sends_a_signed_link_for_this_conversation():
    s = SessionState(conversation_id="conv-42", site_slug="demo")
    s.whatsapp_to = "+2348065550199"
    out = await _layer(s).send_document_upload_link(document_type="pay_stub")
    assert out["status"] == "sent"
    method, url, body = FakeHttp.calls[0]
    assert method == "POST" and url.endswith("/v1/whatsapp/send") and body["to"] == "+2348065550199"
    assert "Acme Pensions" in body["message"] and "60 minutes" in body["message"]
    link = body["message"].split("https://agora.example/d/")[1].split(" ")[0]
    token, _, query = link.partition("?")
    assert query == "kind=pay_stub"
    payload = verify_document_link(secret=SECRET, token=token)
    assert payload["sid"] == "conv-42" and payload["t"] == "t-uuid"


async def test_link_not_offered_when_unconfigured():
    s = SessionState(conversation_id="c1", site_slug="demo")
    s.whatsapp_to = "+2348065550199"
    out = await _layer(s, _settings(document_link_base_url="")).send_document_upload_link()
    assert out["status"] == "error" and "not set up" in out["message"]


async def test_check_on_a_chat_reads_the_session():
    s = SessionState(conversation_id="c1", site_slug="demo")
    s.text_chat = True
    assert (await _layer(s).check_shared_documents())["status"] == "none"
    s.shared_documents.append({"kind": "pay_stub", "status": "done", "validation_verdict": "validated",
                               "document_id": "secret-id"})
    out = await _layer(s).check_shared_documents()
    assert out["documents"] == ["a pay stub: received and passed the consistency checks"]
    assert "secret-id" not in str(out)
    assert FakeHttp.calls == []


async def test_check_on_a_call_asks_the_control_plane_signed():
    s = SessionState(conversation_id="conv-9", site_slug="demo")
    FakeHttp.get_json = {"documents": [{"kind": "receipt", "status": "processing"}]}
    out = await _layer(s).check_shared_documents()
    method, url, headers = FakeHttp.calls[0]
    assert method == "GET" and url.endswith("/voice/internal/documents/conv-9")
    assert headers["X-Document-Sig"] == document_lookup_sig(SECRET, "conv-9")
    assert out["documents"] == ["a receipt: received, still being checked (about two minutes)"]


def test_prompt_lists_shared_documents_in_the_tail_and_the_rule_in_the_body():
    text = build_system_prompt(_ctx(), conversation_id="c1",
                               shared_documents=[{"kind": "pay_stub", "status": "done", "validation_verdict": "needs_review"}])
    body, tail = split_prompt(text)
    assert "send_document_upload_link" in body and "check_shared_documents" in body
    assert "DOCUMENTS THEY SHARED IN THIS CONVERSATION" in tail
    assert "a pay stub: received; a team member will review it" in tail
    assert "DOCUMENTS THEY SHARED" not in build_system_prompt(_ctx(), conversation_id="c1")


def test_chat_prompt_says_how_to_share_a_document_on_each_channel():
    from app.chat import chat_documents_line

    assert "paperclip" in chat_documents_line("web") and "Never offer a link" in chat_documents_line("web")
    assert "paperclip" in chat_documents_line(None)
    assert "photo" in chat_documents_line("whatsapp") and "paperclip" not in chat_documents_line("whatsapp")
