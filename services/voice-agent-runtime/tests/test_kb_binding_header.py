"""SPEC-W108 Part A: the knowledge grounding leg must carry X-Tenant-Slugs.

knowledge-service binds every request to the gateway-style X-Tenant-Slugs
header (SPEC-W43 C1) and answers 401 without it. The voice runtime is an
internal Dapr caller and self-passes the header; before W108 the leg sent
none, so grounding was silently dead (best-effort try/except swallowed the
401) for voice AND for the WhatsApp/Telegram bridges that ride /voice/chat.
"""

from __future__ import annotations

import sys

sys.path.insert(0, ".")

import pytest

from app.tenant_context import _fetch_tenant_context_uncached

pytestmark = pytest.mark.asyncio


class _RecordingDapr:
    """Answers the three bootstrap invokes; records every call."""

    def __init__(self):
        self.calls = []

    async def invoke_get(self, app_id, method, *, params=None, headers=None):
        self.calls.append(
            {"app_id": app_id, "method": method, "params": params, "headers": headers}
        )
        if app_id == "booking":
            return {
                "site": {
                    "tenant_slug": "acme",
                    "tenant_id": "11111111-1111-1111-1111-111111111111",
                    "display_name": "Acme Salon",
                },
                "tenant": {"slug": "acme"},
                "offerings": [],
                "team_members": [],
            }
        if app_id == "identity":
            return {}  # enrichment leg: empty payload is fine
        if app_id == "knowledge":
            return {"snippets": ["We open 9am-5pm."], "prompt_block": "..."}
        raise AssertionError(f"unexpected invoke {app_id}/{method}")


class _Settings:
    booking_app_id = "booking"
    identity_app_id = "identity"
    knowledge_app_id = "knowledge"
    identity_internal_token = ""
    knowledge_query = "opening hours services pricing"
    knowledge_snippet_count = 3


async def test_knowledge_leg_carries_tenant_binding_header():
    dapr = _RecordingDapr()
    ctx = await _fetch_tenant_context_uncached(dapr, _Settings(), "acme")
    kb_calls = [c for c in dapr.calls if c["app_id"] == "knowledge"]
    assert len(kb_calls) == 1
    call = kb_calls[0]
    assert call["method"] == "v1/context"
    assert call["params"] == {"tenant": "acme", "q": "opening hours services pricing"}
    # THE W108 contract: the tenant-binding header is present and matches.
    assert call["headers"] == {"X-Tenant-Slugs": "acme"}
    # and grounding actually landed on the context
    assert ctx.knowledge_snippets == ["We open 9am-5pm."]


async def test_knowledge_leg_still_soft_fails():
    class _DownDapr(_RecordingDapr):
        async def invoke_get(self, app_id, method, *, params=None, headers=None):
            if app_id == "knowledge":
                raise RuntimeError("401 unauthorized")
            return await super().invoke_get(app_id, method, params=params, headers=headers)

    ctx = await _fetch_tenant_context_uncached(_DownDapr(), _Settings(), "acme")
    assert ctx.knowledge_snippets == []  # enrichment-only: never fatal
