"""Declarative agent definition (SPEC-W38 F2).

The conversation-service agents registry stores a per-agent ``definition``
JSONB document (all keys optional). The voice runtime merges it over the
bootstrapped TenantContext, with the merge order:

    env defaults < industry pack < agent definition

i.e. a definition key, when set, wins over whatever the industry pack (or
the static env defaults) established on the context. Unset keys leave the
context untouched, so pack/env behaviour is byte-identical for agents
without a definition.

The model is deliberately tolerant: unknown keys are ignored (the registry
schema may grow), and ``from_payload`` returns None for empty/non-dict
payloads so callers can treat "no definition" and "empty definition"
identically.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from pydantic import BaseModel, Field

from .logging import get_logger

if TYPE_CHECKING:  # avoid a runtime import cycle with tenant_context
    from .tenant_context import TenantContext

log = get_logger("agent-definition")


class VoiceSpec(BaseModel):
    """Definition ``voice`` block. ``provider`` mirrors the TTS provider
    chain names (piper|mms|xtts|azure|spitch); empty = runtime default."""

    provider: str = ""
    voice_id: str = ""
    language: str = ""


class OpsRules(BaseModel):
    """Definition ``ops_rules`` block (call guardrails for the runtime)."""

    max_call_seconds: int | None = None
    escalation_phone: str = ""


class AgentDefinition(BaseModel):
    """``agents.definition`` JSONB (SPEC-W38 §1 F2). All keys optional."""

    persona: str = ""  # replaces pack agentPersona when set
    voice: VoiceSpec = Field(default_factory=VoiceSpec)
    instructions: str = ""  # appended system-prompt block
    context_budget_tokens: int | None = None  # knowledge budget (~4 chars/token)
    tool_allowlist: list[str] = Field(default_factory=list)
    knowledge_packs: list[str] = Field(default_factory=list)
    ops_rules: OpsRules = Field(default_factory=OpsRules)

    @classmethod
    def from_payload(cls, payload: Any) -> "AgentDefinition | None":
        """Parse a registry definition payload. Returns None for empty or
        non-dict input; invalid shapes raise no error but log and coerce
        what pydantic accepts (validation errors propagate to the caller's
        fail-open handling)."""
        if not isinstance(payload, dict) or not payload:
            return None
        try:
            return cls.model_validate(payload)
        except Exception as exc:  # noqa: BLE001 - fail open, legacy path still works
            log.warning("invalid agent definition payload; ignoring", error=str(exc)[:200])
            return None


def merge_definition(
    ctx: "TenantContext", definition: AgentDefinition | None
) -> "TenantContext":
    """Apply ``definition`` over the bootstrapped tenant context.

    Merge order (SPEC-W38 §1 F2): env defaults < industry pack < agent
    definition. The definition is attached as ``ctx.agent_definition`` so
    the prompt builder (instructions block, knowledge budget) and the tool
    layer (tool_allowlist) consume it directly; fields that shadow existing
    context state (persona) are copied over. A None definition is a no-op.
    """
    if definition is None:
        return ctx
    ctx.agent_definition = definition
    if definition.persona.strip():
        # Definition persona replaces the industry-pack persona (the pack
        # persona was applied during fetch_tenant_context, env provides none).
        ctx.agent_persona = definition.persona.strip()
    log.info(
        "agent definition merged",
        tenant=ctx.tenant_slug,
        persona=bool(definition.persona.strip()),
        instructions=bool(definition.instructions.strip()),
        tool_allowlist=len(definition.tool_allowlist),
        budget=definition.context_budget_tokens,
    )
    return ctx
