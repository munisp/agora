"""Webhook receiver tests (SPEC-W118 §4): token/HMAC guards, ignore
matrix, happy-path append with the exact idempotency-key contract."""

from __future__ import annotations

import hashlib
import hmac
import json
import uuid

import pytest
from fastapi.testclient import TestClient

pytestmark = pytest.mark.asyncio

from conftest import (  # noqa: E402
    BRIDGE_TOKEN,
    CONV_ID,
    TENANT_A,
    FakeChatwoot,
    FakeConvClient,
    FakeStore,
    make_mapping,
)

from app.config import Settings  # noqa: E402
from app.main import create_app  # noqa: E402

CW_CONV_ID = 4242
WH_URL = f"/v1/chatwoot/webhook/{TENANT_A}/whsec-test-token"


def _app(store, conv, *, hmac_secret=None):
    store.tenants[TENANT_A] = make_mapping(hmac_secret=hmac_secret)
    cfg = Settings(
        sync_enabled=False, chatwoot_bridge_internal_token=BRIDGE_TOKEN
    )
    return create_app(
        cfg, store=store, chatwoot_client=FakeChatwoot(), conv_client=conv
    )


def _payload(**overrides):
    p = {
        "event": "message_type",
        "id": 9001,
        "content": "We can help with that — agent Jane",
        "message_type": "outgoing",
        "content_type": "text",
        "private": False,
        "source_id": None,
        "sender": {"id": 3, "name": "Jane", "email": "jane@acme.test"},
        "conversation": {"id": CW_CONV_ID, "inbox_id": 12, "status": "open"},
        "account": {"id": 7, "name": "Acme"},
    }
    p.update(overrides)
    return p


def _linked_store():
    store = FakeStore()
    store.links[(TENANT_A, CONV_ID)] = {
        "tenant_id": TENANT_A,
        "conversation_id": CONV_ID,
        "chatwoot_conversation_id": CW_CONV_ID,
        "chatwoot_contact_id": 1001,
        "contact_source_id": "src-1001",
    }
    return store


def test_unknown_token_is_404_no_oracle():
    store, conv = _linked_store(), FakeConvClient()
    with TestClient(_app(store, conv)) as c:
        r = c.post(f"/v1/chatwoot/webhook/{TENANT_A}/wrong-token",
                   json=_payload())
        assert r.status_code == 404
        r = c.post(f"/v1/chatwoot/webhook/{uuid.uuid4()}/whsec-test-token",
                   json=_payload())
        assert r.status_code == 404
    assert conv.appended == []


def test_hmac_enforced_when_configured():
    store, conv = _linked_store(), FakeConvClient()
    with TestClient(_app(store, conv, hmac_secret="shh") ) as c:
        r = c.post(WH_URL, content=json.dumps(_payload()))
        assert r.status_code == 401
        body = json.dumps(_payload()).encode()
        sig = hmac.new(b"shh", body, hashlib.sha256).hexdigest()
        r = c.post(WH_URL, content=body,
                   headers={"X-Chatwoot-Signature": sig})
        assert r.status_code == 200


def test_agent_reply_appended_with_exact_idempotency_key():
    store, conv = _linked_store(), FakeConvClient()
    with TestClient(_app(store, conv)) as c:
        r = c.post(WH_URL, json=_payload())
        assert r.status_code == 200
        assert r.json() == {"status": "appended"}
    assert len(conv.appended) == 1
    call = conv.appended[0]
    assert call["conversation_id"] == CONV_ID
    assert call["tenant_id"] == TENANT_A
    assert call["text"] == "We can help with that — agent Jane"
    assert call["idempotency_key"] == f"chatwoot-7-{CW_CONV_ID}-9001"


def test_duplicate_delivery_is_idempotent():
    store, conv = _linked_store(), FakeConvClient()
    with TestClient(_app(store, conv)) as c:
        assert c.post(WH_URL, json=_payload()).json()["status"] == "appended"
        assert c.post(WH_URL, json=_payload()).json()["status"] == "duplicate"


def test_ignore_matrix():
    store, conv = _linked_store(), FakeConvClient()
    with TestClient(_app(store, conv)) as c:
        cases = [
            (_payload(event="conversation_created"), "ignored_event"),
            (_payload(private=True), "ignored_private"),
            (_payload(content_type="input_select"), "ignored_content_type"),
            (_payload(message_type="incoming"), "ignored_direction"),
            (_payload(content=""), "ignored_empty"),
            (_payload(conversation={"id": 999999}), "ignored_unlinked"),
        ]
        for payload, expected in cases:
            r = c.post(WH_URL, json=payload)
            assert r.status_code == 200, payload
            assert r.json() == {"status": expected}, payload
    assert conv.appended == []


def test_echo_of_bridge_posted_message_is_dropped():
    store, conv = _linked_store(), FakeConvClient()
    store.sent.add((TENANT_A, "od-evt-1"))  # bridge posted this earlier
    with TestClient(_app(store, conv)) as c:
        r = c.post(WH_URL, json=_payload(source_id="od-evt-1"))
        assert r.json() == {"status": "ignored_echo"}
    assert conv.appended == []


def test_conversation_service_outage_is_503_so_chatwoot_retries():
    store, conv = _linked_store(), FakeConvClient()
    conv.fail_append = True
    with TestClient(_app(store, conv)) as c:
        r = c.post(WH_URL, json=_payload())
        assert r.status_code == 503


def test_healthz():
    store, conv = FakeStore(), FakeConvClient()
    with TestClient(_app(store, conv)) as c:
        r = c.get("/healthz")
        assert r.status_code == 200
        assert r.json() == {"status": "ok"}
