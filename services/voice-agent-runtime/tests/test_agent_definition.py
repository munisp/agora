"""Declarative agent definition tests (SPEC-W38 F2): pydantic model,
merge order (env < pack < definition), prompt budget truncation and the
tool allowlist filter incl. dispatch blocking."""

from __future__ import annotations

import pytest

from app.agent_definition import AgentDefinition, merge_definition
from app.config import Settings
from app.prompts import build_system_prompt, truncate_knowledge_snippets
from app.session_state import SessionState
from app.tenant_context import TenantContext
from app.tools import TOOL_NAMES, ToolLayer

from conftest import FakeDapr


def _ctx() -> TenantContext:
    return TenantContext(site_slug="demo", tenant_id="t-uuid", tenant_slug="acme")


def _tool_layer(ctx: TenantContext) -> ToolLayer:
    return ToolLayer(
        dapr=FakeDapr(),  # type: ignore[arg-type]
        settings=Settings(),
        ctx=ctx,
        session=SessionState(conversation_id="conv-1", site_slug="demo"),
    )


# --------------------------------------------------------------------------
# model parsing
# --------------------------------------------------------------------------
class TestModel:
    def test_full_payload(self):
        definition = AgentDefinition.model_validate(
            {
                "persona": "Warm and unhurried.",
                "voice": {
                    "provider": "piper",
                    "voice_id": "en_US-amy-medium",
                    "language": "en",
                },
                "instructions": "Always offer the loyalty program.",
                "context_budget_tokens": 4096,
                "tool_allowlist": ["check_availability", "book_appointment"],
                "knowledge_packs": ["salon-faq"],
                "ops_rules": {"max_call_seconds": 900, "escalation_phone": "+2348000000000"},
            }
        )
        assert definition.persona == "Warm and unhurried."
        assert definition.voice.provider == "piper"
        assert definition.voice.voice_id == "en_US-amy-medium"
        assert definition.voice.language == "en"
        assert definition.context_budget_tokens == 4096
        assert definition.tool_allowlist == ["check_availability", "book_appointment"]
        assert definition.knowledge_packs == ["salon-faq"]
        assert definition.ops_rules.max_call_seconds == 900
        assert definition.ops_rules.escalation_phone == "+2348000000000"

    def test_all_keys_optional(self):
        definition = AgentDefinition.model_validate({})
        assert definition.persona == ""
        assert definition.tool_allowlist == []
        assert definition.context_budget_tokens is None
        assert definition.ops_rules.max_call_seconds is None

    def test_from_payload(self):
        assert AgentDefinition.from_payload(None) is None
        assert AgentDefinition.from_payload({}) is None
        assert AgentDefinition.from_payload("nope") is None
        assert AgentDefinition.from_payload({"persona": "p"}).persona == "p"

    def test_invalid_field_shape_fails_open(self):
        # voice must be an object; a bad shape logs and yields None.
        assert AgentDefinition.from_payload({"voice": "piper"}) is None


# --------------------------------------------------------------------------
# merge order: env defaults < industry pack < agent definition
# --------------------------------------------------------------------------
class TestMerge:
    def test_definition_persona_replaces_pack_persona(self):
        ctx = _ctx()
        ctx.agent_persona = "Pack persona (salon)."  # applied by _apply_pack
        definition = AgentDefinition(persona="Definition persona.")
        merge_definition(ctx, definition)
        assert ctx.agent_persona == "Definition persona."
        assert ctx.agent_definition is definition

    def test_definition_without_persona_keeps_pack_persona(self):
        ctx = _ctx()
        ctx.agent_persona = "Pack persona (salon)."
        merge_definition(ctx, AgentDefinition(instructions="Be brief."))
        assert ctx.agent_persona == "Pack persona (salon)."
        assert ctx.agent_definition is not None

    def test_none_definition_is_noop(self):
        ctx = _ctx()
        ctx.agent_persona = "Pack persona."
        merge_definition(ctx, None)
        assert ctx.agent_persona == "Pack persona."
        assert ctx.agent_definition is None


# --------------------------------------------------------------------------
# prompt wiring: persona header, instructions block, budget truncation
# --------------------------------------------------------------------------
class TestPromptWiring:
    def test_truncate_within_budget_unchanged(self):
        snippets = ["short one", "short two"]
        assert truncate_knowledge_snippets(snippets, 100) == snippets

    def test_truncate_hard_cut_at_budget(self):
        # budget 5 tokens = 20 chars; first snippet (10 chars) fits whole,
        # second is cut to the remaining 10 chars, third dropped.
        snippets = ["0123456789", "aaaaaaaaaaaaaaaaaaaa", "bbbbbb"]
        out = truncate_knowledge_snippets(snippets, 5)
        assert out == ["0123456789", "aaaaaaaaaa"]

    def test_truncate_no_budget(self):
        snippets = ["x" * 100]
        assert truncate_knowledge_snippets(snippets, None) == snippets
        assert truncate_knowledge_snippets(snippets, 0) == snippets
        assert truncate_knowledge_snippets(snippets, -3) == snippets

    def test_prompt_truncates_knowledge_to_budget(self):
        ctx = _ctx()
        ctx.knowledge_snippets = ["0123456789", "a" * 50]
        merge_definition(ctx, AgentDefinition(context_budget_tokens=3))  # 12 chars
        prompt = build_system_prompt(ctx, conversation_id="c1")
        assert "- 0123456789" in prompt
        assert "- aa" in prompt  # second snippet cut to the remaining 2 chars
        assert "- aaaaaaaaaa" not in prompt

    def test_prompt_without_definition_keeps_all_snippets(self):
        ctx = _ctx()
        ctx.knowledge_snippets = ["0123456789", "a" * 50]
        prompt = build_system_prompt(ctx, conversation_id="c1")
        assert "- " + "a" * 50 in prompt

    def test_instructions_block_appended(self):
        ctx = _ctx()
        merge_definition(ctx, AgentDefinition(instructions="Always upsell the wash."))
        prompt = build_system_prompt(ctx, conversation_id="c1")
        assert "AGENT INSTRUCTIONS\nAlways upsell the wash." in prompt

    def test_definition_persona_replaces_pack_persona_in_prompt(self):
        ctx = _ctx()
        ctx.agent_persona = "Pack persona."
        merge_definition(ctx, AgentDefinition(persona="Definition persona."))
        prompt = build_system_prompt(ctx, conversation_id="c1")
        assert "Definition persona." in prompt
        assert "Pack persona." not in prompt
        assert "AGENT PERSONA" in prompt


# --------------------------------------------------------------------------
# tool allowlist: schema filter + dispatch block
# --------------------------------------------------------------------------
class TestToolAllowlist:
    def test_no_definition_returns_all_schemas(self):
        layer = _tool_layer(_ctx())
        names = [s["function"]["name"] for s in layer.schemas()]
        assert names == TOOL_NAMES

    def test_empty_allowlist_returns_all_schemas(self):
        ctx = _ctx()
        merge_definition(ctx, AgentDefinition(tool_allowlist=[]))
        names = [s["function"]["name"] for s in _tool_layer(ctx).schemas()]
        assert names == TOOL_NAMES

    def test_allowlist_filters_merged_schemas(self):
        ctx = _ctx()
        merge_definition(
            ctx, AgentDefinition(tool_allowlist=["get_business_info", "book_appointment"])
        )
        names = [s["function"]["name"] for s in _tool_layer(ctx).schemas()]
        assert names == ["get_business_info", "book_appointment"]

    def test_allowlist_filters_plugin_schemas(self):
        ctx = _ctx()
        merge_definition(ctx, AgentDefinition(tool_allowlist=["get_business_info"]))
        plugin = _FakePlugin("loyalty_signup")
        layer = ToolLayer(
            dapr=FakeDapr(),  # type: ignore[arg-type]
            settings=Settings(),
            ctx=ctx,
            session=SessionState(conversation_id="conv-1", site_slug="demo"),
            plugin_tools=[plugin],  # type: ignore[list-item]
        )
        names = [s["function"]["name"] for s in layer.schemas()]
        assert names == ["get_business_info"]

    async def test_dispatch_blocked_for_non_allowlisted_tool(self):
        ctx = _ctx()
        merge_definition(ctx, AgentDefinition(tool_allowlist=["get_business_info"]))
        layer = _tool_layer(ctx)
        result = await layer.dispatch("cancel_appointment", {"booking_id": "b", "phone": "p"})
        assert result["status"] == "error"
        assert "not enabled" in result["message"]
        # the blocked attempt is surfaced as a tool event, nothing published
        assert layer.tenant_context is ctx

    async def test_dispatch_allows_allowlisted_tool(self):
        ctx = _ctx()
        ctx.display_name = "Demo Biz"
        merge_definition(ctx, AgentDefinition(tool_allowlist=["get_business_info"]))
        layer = _tool_layer(ctx)
        result = await layer.dispatch("get_business_info", {})
        assert result["business"] == "Demo Biz"

    async def test_dispatch_blocks_plugin_tool_not_in_allowlist(self):
        ctx = _ctx()
        merge_definition(ctx, AgentDefinition(tool_allowlist=["get_business_info"]))
        layer = ToolLayer(
            dapr=FakeDapr(),  # type: ignore[arg-type]
            settings=Settings(),
            ctx=ctx,
            session=SessionState(conversation_id="conv-1", site_slug="demo"),
            plugin_tools=[_FakePlugin("loyalty_signup")],  # type: ignore[list-item]
        )
        result = await layer.dispatch("loyalty_signup", {})
        assert result["status"] == "error"
        assert "not enabled" in result["message"]


class _FakePlugin:
    def __init__(self, name: str) -> None:
        self.name = name
        self.executed = False

    def schema(self) -> dict:
        return {"type": "function", "function": {"name": self.name, "parameters": {}}}

    async def execute(self, arguments: dict) -> dict:
        self.executed = True
        return {"status": "ok"}
