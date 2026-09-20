"""SPEC-W45 UC helpdesk automation tests: escalation detection (phrase
lexicon + LLM intent names) and ticket emission (Dapr POST with
X-Internal-Token, tenant-bound, deduped per conversation). Offline: fake
Dapr client + fake agent store, no Postgres, no sidecar."""

from __future__ import annotations

import sys
import uuid

import pytest

sys.path.insert(0, ".")

from app import helpdesk  # noqa: E402
from app.dapr_client import DaprError  # noqa: E402

pytestmark = pytest.mark.asyncio

TENANT = uuid.uuid4()
CONV = uuid.uuid4()
TURN = uuid.uuid4()


class TestEscalationDetection:
    @pytest.mark.parametrize(
        "text",
        [
            "I want to speak to a human please",
            "Can I talk to an agent?",
            "This is ridiculous, get me a real person",
            "Let me speak to a manager!",
            "I want to lodge a complaint",
            "customer service now",
        ],
    )
    async def test_phrases_hit(self, text):
        hit, signal = helpdesk.is_escalation(text)
        assert hit, text
        assert signal.startswith("phrase:")

    @pytest.mark.parametrize(
        "text",
        [
            "thanks, that solved my problem",
            "what time do you open tomorrow?",
            "the agent showing on my dashboard is wrong",  # single benign word
            "I am a person who likes your service",
        ],
    )
    async def test_benign_misses(self, text):
        hit, _ = helpdesk.is_escalation(text)
        assert not hit, text

    async def test_llm_intent_hit(self):
        hit, signal = helpdesk.is_escalation("asdf", intent="human_escalation")
        assert hit
        assert signal == "intent:human_escalation"

    async def test_unknown_intent_miss(self):
        hit, _ = helpdesk.is_escalation("asdf", intent="book_appointment")
        assert not hit


class _FakeDapr:
    def __init__(self, err: Exception | None = None):
        self.calls: list[dict] = []
        self.err = err

    async def invoke_post(self, app_id, method, *, json_body=None, headers=None):
        if self.err is not None:
            raise self.err
        self.calls.append(
            {"app_id": app_id, "method": method, "json": json_body, "headers": headers}
        )
        return {"ticket": {"id": "t-1"}}


class _FakeAgents:
    def __init__(self, slug: str | None = "acme"):
        self.slug = slug

    async def tenant_slug_for(self, tenant_id):
        return self.slug


class _ns:
    def __init__(self, d):
        self.__dict__.update(d)


def _cfg(**over):
    base = dict(
        helpdesk_enabled=True,
        booking_app_id="booking",
        booking_internal_token="tok-123",
    )
    base.update(over)
    return _ns(base)


class TestTicketEmission:
    async def test_escalation_posts_ticket(self):
        dapr = _FakeDapr()
        auto = helpdesk.HelpdeskAutomation(_cfg(), dapr, _FakeAgents())
        scheduled = auto.maybe_escalate(
            tenant_id=TENANT, conversation_id=CONV, turn_id=TURN,
            text="I want to speak to a human", intent=None,
            channel="voice", contact_phone="+2348030000000",
        )
        assert scheduled
        # Let the background task run.
        import asyncio

        await asyncio.gather(*asyncio.all_tasks() - {asyncio.current_task()},
                             return_exceptions=True)
        assert len(dapr.calls) == 1
        call = dapr.calls[0]
        assert call["app_id"] == "booking"
        assert call["method"] == "v1/helpdesk/tickets"
        assert call["headers"]["X-Internal-Token"] == "tok-123"
        assert call["headers"]["X-Tenant-Slug"] == "acme"  # tenant-bound
        assert call["json"]["conversation_id"] == str(CONV)
        assert call["json"]["channel"] == "voice"
        assert call["json"]["priority"] == "high"

    async def test_dedupes_per_conversation(self):
        dapr = _FakeDapr()
        auto = helpdesk.HelpdeskAutomation(_cfg(), dapr, _FakeAgents())
        for _ in range(3):
            auto.maybe_escalate(
                tenant_id=TENANT, conversation_id=CONV, turn_id=TURN,
                text="talk to an agent", intent=None, channel="web",
                contact_phone=None,
            )
        import asyncio

        await asyncio.gather(*asyncio.all_tasks() - {asyncio.current_task()},
                             return_exceptions=True)
        assert len(dapr.calls) == 1, "one ticket per conversation"

    async def test_no_token_skips_loudly(self, caplog):
        dapr = _FakeDapr()
        auto = helpdesk.HelpdeskAutomation(_cfg(booking_internal_token=""), dapr, _FakeAgents())
        auto.maybe_escalate(
            tenant_id=TENANT, conversation_id=uuid.uuid4(), turn_id=TURN,
            text="speak to a human", intent=None, channel="web",
            contact_phone=None,
        )
        import asyncio

        await asyncio.gather(*asyncio.all_tasks() - {asyncio.current_task()},
                             return_exceptions=True)
        assert len(dapr.calls) == 0

    async def test_unknown_tenant_slug_skips(self):
        dapr = _FakeDapr()
        auto = helpdesk.HelpdeskAutomation(_cfg(), dapr, _FakeAgents(slug=None))
        auto.maybe_escalate(
            tenant_id=TENANT, conversation_id=uuid.uuid4(), turn_id=TURN,
            text="speak to a human", intent=None, channel="web",
            contact_phone=None,
        )
        import asyncio

        await asyncio.gather(*asyncio.all_tasks() - {asyncio.current_task()},
                             return_exceptions=True)
        assert len(dapr.calls) == 0

    async def test_dapr_failure_logged_never_raised(self):
        dapr = _FakeDapr(err=DaprError("boom", status_code=503))
        auto = helpdesk.HelpdeskAutomation(_cfg(), dapr, _FakeAgents())
        auto.maybe_escalate(
            tenant_id=TENANT, conversation_id=uuid.uuid4(), turn_id=TURN,
            text="speak to a human", intent=None, channel="web",
            contact_phone=None,
        )
        import asyncio

        results = await asyncio.gather(
            *asyncio.all_tasks() - {asyncio.current_task()}, return_exceptions=True
        )
        assert all(not isinstance(r, Exception) for r in results)

    async def test_disabled_config(self):
        dapr = _FakeDapr()
        auto = helpdesk.HelpdeskAutomation(_cfg(helpdesk_enabled=False), dapr, _FakeAgents())
        scheduled = auto.maybe_escalate(
            tenant_id=TENANT, conversation_id=CONV, turn_id=TURN,
            text="speak to a human", intent=None, channel="web",
            contact_phone=None,
        )
        assert not scheduled
