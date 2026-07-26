"""Text-in/text-out agent path (POST /voice/chat): runs the same tool layer
as the voice pipeline through an in-process LLM tool-calling loop, without
audio stages."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .async_tools import AsyncToolRunner, ToolAckPolicy
from .config import Settings
from .dapr_client import DaprClient
from .escalation import LiveKitEscalation
from .intent_router import find_agent, route_intent
from .logging import get_logger
from .pipeline.llm import LLMInterface, run_tool_loop, run_tool_loop_stream
from .plugin_tools import build_plugin_tools
from .prompts import build_system_prompt
from .session_state import SessionState, SessionStore
from .tenant_context import fetch_tenant_context
from .tools import UI_ACTION_TOOL_NAMES, ToolLayer

log = get_logger("chat")

# SPEC-W9 Part B: prompt addendum for the agent-driven UI action tools.
# prompts.py is shared ownership this wave, so the addendum is injected
# here (chat path only), and only when the UI action tools are actually
# registered in the turn's tool layer.
UI_ACTION_PROMPT_ADDENDUM = (
    "When it would help the visitor on the website, you can offer to show "
    "them a page, highlight an element such as the booking form, or "
    "pre-select a service in the booking form using the navigate_to_page, "
    "highlight_element and prefill_booking tools."
)


@dataclass
class ChatHistory:
    """Bounded per-conversation message history (dev-grade, in-memory)."""

    max_messages: int = 40
    messages: dict[str, list[dict[str, Any]]] = field(default_factory=dict)

    def get(self, conversation_id: str) -> list[dict[str, Any]]:
        return self.messages.setdefault(conversation_id, [])

    def append(self, conversation_id: str, *msgs: dict[str, Any]) -> None:
        history = self.get(conversation_id)
        history.extend(msgs)
        if len(history) > self.max_messages:
            del history[: len(history) - self.max_messages]


class ChatService:
    def __init__(
        self,
        *,
        settings: Settings,
        dapr: DaprClient,
        llm: LLMInterface,
        sessions: SessionStore,
        escalation: LiveKitEscalation | None = None,
    ) -> None:
        self._settings = settings
        self._dapr = dapr
        self._llm = llm
        self._sessions = sessions
        self._histories = ChatHistory()
        self._escalation = escalation or LiveKitEscalation(settings)
        # VOICE-SCALING §5: hard per-tool timeout on every Dapr tool call.
        self._tool_runner = AsyncToolRunner(
            timeout_s=settings.tool_timeout_s,
            ack_grace_ms=settings.tool_ack_grace_ms,
        )

    async def _prepare_turn(
        self,
        *,
        site_slug: str,
        message: str,
        conversation_id: str | None,
        persona_override: str | None = None,
    ) -> tuple[SessionState, ToolLayer, list[dict[str, Any]]]:
        """Shared turn setup for the buffered and SSE streaming chat paths
        (SPEC-W3 §3): session, tenant context, multi-agent routing, tool
        layer and the re-rendered system prompt + user message in history.
        """
        session = self._sessions.get_or_create(conversation_id, site_slug)
        ctx = await fetch_tenant_context(self._dapr, self._settings, site_slug)

        # Wave 5 #8 A/B prompt testing: swap the tenant persona for the
        # eval candidate. Hard-gated by EVAL_PERSONA_OVERRIDE (default off).
        if persona_override and self._settings.eval_persona_override:
            ctx.agent_persona = persona_override

        # Multi-agent crews (SPEC-W3 §4, innovation 6): score the message
        # against the pack agents' intent keywords. A match swaps the active
        # specialist; no match keeps the current one for continuity, and a
        # session with no active specialist falls back to the base persona.
        routed = route_intent(message, ctx.agents)
        if routed is not None and routed.get("id") != session.active_agent:
            log.info(
                "specialist agent activated",
                conversation_id=session.conversation_id,
                agent=routed.get("id"),
            )
            session.active_agent = str(routed.get("id"))
        active_agent = find_agent(ctx.agents, session.active_agent)

        plugin_tools = build_plugin_tools(
            ctx.custom_tools,
            allowed_hosts_raw=self._settings.plugin_allowed_hosts,
            context={
                "site_slug": ctx.site_slug,
                "tenant_slug": ctx.tenant_slug,
                "tenant_id": ctx.tenant_id,
            },
            # SPEC-W9 Part C: pack-level mcpServers ride the tenant context
            # into build_mcp_tools (env MCP_SERVERS alone worked before).
            tenant_ctx=ctx,
        )
        # SPEC-W9 Part B: fresh per-turn collector for validated UI actions
        # (navigate / highlight / prefill_booking). Read back after the tool
        # loop and attached to the outgoing payload.
        ui_action_sink: list[dict[str, Any]] = []
        tool_layer = ToolLayer(
            dapr=self._dapr,
            settings=self._settings,
            ctx=ctx,
            session=session,
            escalation=self._escalation,
            plugin_tools=plugin_tools,
            ui_action_sink=ui_action_sink,
        )

        history = self._histories.get(session.conversation_id)
        # The system prompt is re-rendered every turn so a specialist-agent
        # swap (or fallback to the base persona) takes effect immediately.
        system_prompt = build_system_prompt(
            ctx,
            conversation_id=session.conversation_id,
            active_agent=active_agent,
            extra_tool_names=[t.name for t in plugin_tools],
            # Wave 5 #3: honor a previously detected caller language (voice
            # path sets it via whisper; None keeps the tenant default).
            language=session.active_language,
        )
        # SPEC-W9 Part B: tell the agent it may drive the visitor's UI — but
        # only when the UI action tools are registered for this turn.
        registered = {t["function"]["name"] for t in tool_layer.schemas()}
        if any(name in registered for name in UI_ACTION_TOOL_NAMES):
            system_prompt = f"{system_prompt}\n\n{UI_ACTION_PROMPT_ADDENDUM}"
        if history and history[0].get("role") == "system":
            history[0] = {"role": "system", "content": system_prompt}
        else:
            history.insert(0, {"role": "system", "content": system_prompt})
        history.append({"role": "user", "content": message})
        return session, tool_layer, history

    async def _post_copilot(self, session: SessionState, reply: str) -> bool:
        """Whisper-copilot (SPEC-W3 §4, innovation 1): after an escalation
        the agent stays on and posts each reply as a suggested draft to the
        escalation room data channel (best-effort when LiveKit is down)."""
        if (
            self._settings.copilot_mode
            and session.escalation_room
            and reply.strip()
        ):
            return await self._escalation.post_suggestion(
                session.escalation_room,
                {
                    "type": "copilot_suggestion",
                    "conversation_id": session.conversation_id,
                    "text": reply,
                    "agent": session.active_agent or "base",
                },
            )
        return False

    async def handle_message(
        self,
        *,
        site_slug: str,
        message: str,
        conversation_id: str | None,
        persona_override: str | None = None,
        channel: str = "web",
    ) -> dict[str, Any]:
        session, tool_layer, history = await self._prepare_turn(
            site_slug=site_slug,
            message=message,
            conversation_id=conversation_id,
            persona_override=persona_override,
        )
        # SPEC-W6 Part A: omnichannel inbound — remember which channel the
        # message arrived on (session metadata + turn logging only).
        session.channel = channel

        reply, trace = await run_tool_loop(
            self._llm,
            messages=history,
            tools=tool_layer.schemas(),
            dispatch=self._tool_runner.wrap_dispatch(tool_layer.dispatch),
        )
        history.append({"role": "assistant", "content": reply})

        copilot_posted = await self._post_copilot(session, reply)

        log.info(
            "chat turn complete",
            conversation_id=session.conversation_id,
            channel=session.channel,
            tool_calls=len(trace),
            active_agent=session.active_agent,
            copilot=copilot_posted,
        )
        return {
            "conversation_id": session.conversation_id,
            "reply": reply,
            "tool_calls": trace,
            "phone_confirmed": session.confirmed_phone is not None,
            "active_agent": session.active_agent,
            "escalated": session.escalation_room is not None,
            # SPEC-W9 Part B (additive): validated UI actions the LLM invoked
            # this turn; the web widget executes them client-side.
            "ui_actions": list(tool_layer.collected_ui_actions),
        }

    async def handle_message_stream(
        self,
        *,
        site_slug: str,
        message: str,
        conversation_id: str | None,
        persona_override: str | None = None,
    ):
        """SSE streaming chat (SPEC-W3 §3): async generator of event dicts.

        Yields {"delta": "..."} for every LLM content chunk and
        {"tool_call": {...}} when a tool is executed — the SAME tool layer
        as the buffered path. The terminal event is
        {"done": true, "conversation_id": ...}. History and whisper-copilot
        bookkeeping happen after the final answer, exactly as in
        handle_message.
        """
        session, tool_layer, history = await self._prepare_turn(
            site_slug=site_slug,
            message=message,
            conversation_id=conversation_id,
            persona_override=persona_override,
        )

        # VOICE-SCALING §5 async tools (chat path): an immediate {"ack": ...}
        # SSE event precedes every slow tool execution; the tool call itself
        # runs under the hard timeout via the runner.
        ack_policy = ToolAckPolicy.from_context(tool_layer.tenant_context)

        reply = ""
        trace: list[dict[str, Any]] = []
        async for event in run_tool_loop_stream(
            self._llm,
            messages=history,
            tools=tool_layer.schemas(),
            dispatch=self._tool_runner.wrap_dispatch(tool_layer.dispatch),
            ack_for_tool=ack_policy.ack_for,
        ):
            kind = event["type"]
            if kind == "delta":
                yield {"delta": event["text"]}
            elif kind == "ack":
                yield {"ack": event["text"], "tool": event["tool"]}
            elif kind == "tool":
                yield {
                    "tool_call": {
                        "tool": event["tool"],
                        "arguments": event["arguments"],
                    }
                }
            elif kind == "final":
                reply = event["text"]
                trace = event["trace"]

        history.append({"role": "assistant", "content": reply})
        copilot_posted = await self._post_copilot(session, reply)
        log.info(
            "chat stream turn complete",
            conversation_id=session.conversation_id,
            tool_calls=len(trace),
            active_agent=session.active_agent,
            copilot=copilot_posted,
            ui_actions=len(tool_layer.collected_ui_actions),
        )
        # SPEC-W9 Part B: validated UI actions ride the stream as
        # `data: {"ui_action": {...}}` frames ahead of the terminal done frame.
        for action in tool_layer.collected_ui_actions:
            yield {"ui_action": action}
        yield {"done": True, "conversation_id": session.conversation_id}

