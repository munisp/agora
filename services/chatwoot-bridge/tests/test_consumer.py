"""Outbound consumer logic tests (SPEC-W118 §4): process_event outcome
matrix against in-memory fakes — no Kafka, no network."""

from __future__ import annotations

import pytest

pytestmark = pytest.mark.asyncio

from conftest import (  # noqa: E402
    CONV_ID,
    TENANT_A,
    FakeChatwoot,
    FakeConvClient,
    FakeStore,
    make_event,
    make_mapping,
)

from app.config import Settings  # noqa: E402
from app.consumer import BridgeConsumer  # noqa: E402


def _consumer(store, chatwoot, conv):
    return BridgeConsumer(Settings(sync_enabled=False), store, chatwoot, conv)


def _mapped_store(**mapping_overrides):
    store = FakeStore()
    store.tenants[TENANT_A] = make_mapping(**mapping_overrides)
    return store


async def test_origin_chatwoot_is_skipped_loop_breaker():
    store, cw, conv = _mapped_store(), FakeChatwoot(), FakeConvClient()
    out = await _consumer(store, cw, conv).process_event(
        make_event(role="agent", origin="chatwoot")
    )
    assert out == "skipped_origin"
    assert cw.messages == []


async def test_system_and_tool_roles_skipped():
    store, cw, conv = _mapped_store(), FakeChatwoot(), FakeConvClient()
    c = _consumer(store, cw, conv)
    assert await c.process_event(make_event(role="system")) == "skipped_role"
    assert await c.process_event(make_event(role="tool")) == "skipped_role"
    assert cw.messages == []


async def test_malformed_events_skipped():
    store, cw, conv = _mapped_store(), FakeChatwoot(), FakeConvClient()
    c = _consumer(store, cw, conv)
    assert await c.process_event({"data": {}}) == "skipped_malformed"
    assert await c.process_event(make_event(text="")) == "skipped_malformed"
    assert await c.process_event("not-a-dict") == "skipped_malformed"


async def test_unmapped_tenant_skipped_not_error():
    store, cw, conv = FakeStore(), FakeChatwoot(), FakeConvClient()
    assert await _consumer(store, cw, conv).process_event(
        make_event()
    ) == "skipped_unmapped"


async def test_disabled_mapping_skipped():
    store, cw, conv = _mapped_store(enabled=False), FakeChatwoot(), FakeConvClient()
    assert await _consumer(store, cw, conv).process_event(
        make_event()
    ) == "skipped_unmapped"


async def test_user_turn_posts_incoming_and_creates_link():
    store, cw, conv = _mapped_store(), FakeChatwoot(), FakeConvClient()
    out = await _consumer(store, cw, conv).process_event(
        make_event(event_id="evt-42")
    )
    assert out == "posted"
    # contact + conversation created from the conversation-service row
    assert cw.contacts[0]["identifier"] == f"od-{CONV_ID}"
    assert cw.contacts[0]["phone_number"] == "+2348012345678"
    assert cw.conversations[0]["opendesk_conversation_id"] == CONV_ID
    # message is incoming with the deterministic source_id
    msg = cw.messages[0]
    assert msg["message_type"] == "incoming"
    assert msg["content"] == "hello from customer"
    assert msg["source_id"] == "od-evt-42"
    # link + sent registry persisted
    assert await store.get_link(TENANT_A, CONV_ID) is not None
    assert await store.sent_lookup(TENANT_A, "od-evt-42")


async def test_agent_turn_posts_outgoing():
    store, cw, conv = _mapped_store(), FakeChatwoot(), FakeConvClient()
    out = await _consumer(store, cw, conv).process_event(
        make_event(role="agent", text="agent reply", event_id="evt-7")
    )
    assert out == "posted"
    assert cw.messages[0]["message_type"] == "outgoing"


async def test_second_turn_reuses_link_no_new_contact():
    store, cw, conv = _mapped_store(), FakeChatwoot(), FakeConvClient()
    c = _consumer(store, cw, conv)
    await c.process_event(make_event(event_id="evt-1"))
    await c.process_event(make_event(event_id="evt-2", text="again"))
    assert len(cw.contacts) == 1
    assert len(cw.conversations) == 1
    assert len(cw.messages) == 2


async def test_redelivery_of_same_event_is_skipped_duplicate():
    store, cw, conv = _mapped_store(), FakeChatwoot(), FakeConvClient()
    c = _consumer(store, cw, conv)
    assert await c.process_event(make_event(event_id="evt-9")) == "posted"
    assert await c.process_event(make_event(event_id="evt-9")) == "skipped_duplicate"
    assert len(cw.messages) == 1


async def test_missing_conversation_skips():
    store, cw = _mapped_store(), FakeChatwoot()
    conv = FakeConvClient(conversation=None)
    assert await _consumer(store, cw, conv).process_event(
        make_event()
    ) == "skipped_no_conversation"
    assert cw.contacts == []


async def test_chatwoot_outage_reports_error_and_records_nothing():
    store, cw, conv = _mapped_store(), FakeChatwoot(), FakeConvClient()
    cw.fail_next = True  # fails the contact create (first call)
    out = await _consumer(store, cw, conv).process_event(make_event())
    assert out == "error_link"
    assert not await store.sent_lookup(TENANT_A, "od-evt-0001")


async def test_post_failure_after_link_keeps_link_reports_error():
    store, cw, conv = _mapped_store(), FakeChatwoot(), FakeConvClient()
    c = _consumer(store, cw, conv)
    await c.process_event(make_event(event_id="evt-1"))  # establishes link
    cw.fail_next = True
    out = await c.process_event(make_event(event_id="evt-2", text="second"))
    assert out == "error_post"
    assert not await store.sent_lookup(TENANT_A, "od-evt-2")
