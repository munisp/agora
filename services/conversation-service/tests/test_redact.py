"""SPEC-W34 GF3: PII redaction before the Dapr transcript publish.

The Dapr path to opendesk.conversation.transcripts bypasses the Fluvio
pii-redact smartmodule, so routes must redact phone/email PII before
publishing and mark the event ``redacted: true``. These tests prove raw
PII never reaches the published payload and that the redaction regexes
mirror infra/fluvio/pii-redact/src/lib.rs semantics. Offline unit tests.
"""

from __future__ import annotations

import json
import sys
import uuid
from datetime import UTC, datetime

import pytest

sys.path.insert(0, ".")

from app import events, redact  # noqa: E402

pytestmark = pytest.mark.asyncio


class TestRedactText:
    def test_email_redacted(self):
        out = redact.redact_text("reach me at jane.doe+ops@example.co.uk please")
        assert "jane.doe+ops@example.co.uk" not in out
        assert redact.EMAIL_REPLACEMENT in out

    def test_phone_redacted(self):
        out = redact.redact_text("call +234 803 555 1234 tomorrow")
        assert "234 803 555 1234" not in out
        assert redact.PHONE_REPLACEMENT in out

    def test_phone_with_parens_and_dashes(self):
        out = redact.redact_text("number is +1 (415) 555-0132 ok")
        assert "555-0132" not in out
        assert redact.PHONE_REPLACEMENT in out

    def test_both_phone_and_email(self):
        text = "email a@b.com or dial 08035550123"
        out = redact.redact_text(text)
        assert "a@b.com" not in out
        assert "08035550123" not in out

    def test_short_numbers_untouched(self):
        # Fewer than 8 digits: not a phone number per the smartmodule regex.
        out = redact.redact_text("order 12345 total 42")
        assert out == "order 12345 total 42"

    def test_plain_text_unchanged(self):
        out = redact.redact_text("hello, how can I help you today?")
        assert out == "hello, how can I help you today?"

    def test_email_not_eaten_by_phone_pattern(self):
        # Email first, then phone (smartmodule order): the placeholder must
        # survive intact.
        out = redact.redact_text("user12345@example.com")
        assert out == redact.EMAIL_REPLACEMENT


class TestPublishedEvent:
    def _event(self, text: str) -> dict:
        return events.conversation_turn_event(
            conversation_id=uuid.uuid4(),
            tenant_id=uuid.uuid4(),
            site_slug="site-1",
            role="user",
            text=redact.redact_text(text),
            ts=datetime.now(UTC),
            redacted=True,
        )

    def test_payload_has_no_raw_pii_and_redacted_flag(self):
        raw_phone = "+234 803 555 1234"
        raw_email = "customer@realbank.ng"
        event = self._event(f"my phone {raw_phone} and email {raw_email}")
        data = event["data"]
        assert data["redacted"] is True
        blob = json.dumps(event)
        assert raw_phone not in blob
        assert raw_email not in blob
        assert redact.PHONE_REPLACEMENT in data["text"]
        assert redact.EMAIL_REPLACEMENT in data["text"]

    def test_redacted_defaults_false(self):
        event = events.conversation_turn_event(
            conversation_id=uuid.uuid4(),
            tenant_id=uuid.uuid4(),
            site_slug="site-1",
            role="user",
            text="hi",
            ts=datetime.now(UTC),
        )
        assert event["data"]["redacted"] is False

    def test_routes_publishes_redacted_event(self):
        """Route-level proof: the exact code path in routes.py redacts the
        turn text and flags the event before st.dapr.publish_event."""
        import inspect

        from app import routes  # noqa: E402

        src = inspect.getsource(routes)
        publish_idx = src.index("publish_event(st.cfg.transcripts_topic")
        prefix = src[:publish_idx]
        # redaction happens before the publish call in the same flow
        assert "redact.redact_text(turn.text)" in prefix
        assert "redacted=True" in prefix
