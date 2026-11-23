"""Chatwoot API client tests (SPEC-W118 §4): exact request shapes over
httpx.MockTransport — no live Chatwoot."""

from __future__ import annotations

import json

import httpx
import pytest

pytestmark = pytest.mark.asyncio

from app.client import ChatwootClient, ChatwootError  # noqa: E402

TOKEN = "cw-token"
AID = 7


def _client(handler):
    return ChatwootClient(
        "http://chatwoot.test", transport=httpx.MockTransport(handler)
    )


def _recorded(handler):
    calls: list[httpx.Request] = []

    def h(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return handler(request)

    return h, calls


async def test_validate_inbox_true_when_listed():
    def h(request: httpx.Request) -> httpx.Response:
        assert request.url.path == "/api/v1/accounts/7/inboxes"
        assert request.headers["api_access_token"] == TOKEN
        return httpx.Response(200, json={"payload": [{"id": 12}, {"id": 13}]})

    h, _calls = _recorded(h)
    assert await _client(h).validate_inbox(TOKEN, AID, 12) is True
    assert await _client(h).validate_inbox(TOKEN, AID, 99) is False


async def test_create_contact_payload_and_source_id_parse():
    def h(request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content)
        assert request.url.path == "/api/v1/accounts/7/contacts"
        assert body == {
            "inbox_id": 12,
            "name": "OpenDesk whatsapp +2348012345678",
            "identifier": "od-conv-1",
            "phone_number": "+2348012345678",
        }
        return httpx.Response(200, json={
            "id": 55,
            "contact_inboxes": [{
                "source_id": "src-abc",
                "inbox": {"id": 12},
            }],
        })

    h, _calls = _recorded(h)
    contact_id, source_id = await _client(h).create_contact(
        TOKEN, AID, inbox_id=12,
        name="OpenDesk whatsapp +2348012345678",
        identifier="od-conv-1", phone_number="+2348012345678",
    )
    assert (contact_id, source_id) == (55, "src-abc")


async def test_create_contact_422_falls_back_to_search_and_bind():
    def h(request: httpx.Request) -> httpx.Response:
        if request.url.path.endswith("/contacts"):
            return httpx.Response(422, json={"message": "identifier taken"})
        if request.url.path.endswith("/contacts/search"):
            assert request.url.params["q"] == "od-conv-1"
            return httpx.Response(200, json={
                "payload": [{"id": 77, "identifier": "od-conv-1"}]
            })
        if request.url.path.endswith("/contact_inboxes"):
            assert json.loads(request.content) == {"inbox_id": 12}
            return httpx.Response(200, json={"source_id": "src-77"})
        raise AssertionError(request.url.path)

    h, _calls = _recorded(h)
    contact_id, source_id = await _client(h).create_contact(
        TOKEN, AID, inbox_id=12, name="n", identifier="od-conv-1"
    )
    assert (contact_id, source_id) == (77, "src-77")


async def test_create_conversation_and_message_shapes():
    def h(request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content)
        if request.url.path.endswith("/conversations"):
            assert body["source_id"] == "src-abc"
            assert body["inbox_id"] == 12
            assert body["status"] == "open"
            assert body["additional_attributes"] == {
                "opendesk_conversation_id": "conv-1"
            }
            return httpx.Response(200, json={"id": 888})
        if request.url.path.endswith("/messages"):
            assert body == {
                "content": "hello",
                "message_type": "incoming",
                "private": False,
                "source_id": "od-evt-1",
            }
            return httpx.Response(200, json={"id": 999})
        raise AssertionError(request.url.path)

    h, _calls = _recorded(h)
    c = _client(h)
    cw_id = await c.create_conversation(
        TOKEN, AID, source_id="src-abc", inbox_id=12,
        opendesk_conversation_id="conv-1",
    )
    assert cw_id == 888
    msg = await c.create_message(
        TOKEN, AID, 888, content="hello", message_type="incoming",
        source_id="od-evt-1",
    )
    assert msg["id"] == 999


async def test_webhook_create_and_list():
    def h(request: httpx.Request) -> httpx.Response:
        if request.method == "GET":
            return httpx.Response(200, json={"payload": [
                {"id": 5, "url": "http://bridge/hook"}]})
        body = json.loads(request.content)
        assert body["url"] == "http://bridge/hook2"
        assert body["subscriptions"] == ["message_type"]
        return httpx.Response(200, json={"id": 6})

    h, _calls = _recorded(h)
    c = _client(h)
    assert await c.list_webhooks(TOKEN, AID) == [
        {"id": 5, "url": "http://bridge/hook"}]
    assert await c.create_webhook(
        TOKEN, AID, url="http://bridge/hook2",
        subscriptions=["message_type"]) == 6


async def test_non_2xx_raises_chatwoot_error_with_status():
    def h(request: httpx.Request) -> httpx.Response:
        return httpx.Response(403, text="API access is not enabled")

    h, _calls = _recorded(h)
    with pytest.raises(ChatwootError) as ei:
        await _client(h).validate_inbox(TOKEN, AID, 12)
    assert ei.value.status == 403
    assert "not enabled" in ei.value.body
