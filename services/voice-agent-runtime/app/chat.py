"""Text-in/text-out agent path (POST /voice/chat): runs the same tool layer
as the voice pipeline through an in-process LLM tool-calling loop, without
audio stages."""

from __future__ import annotations

import re

from dataclasses import dataclass, field
from typing import Any

from . import metrics
from . import visitor as visitor_mod
from .async_tools import AsyncToolRunner, ToolAckPolicy
from .codeswitch import instruction as codeswitch_instruction
from .config import Settings
from .dapr_client import DaprClient
from .escalation import LiveKitEscalation
from .intent_router import find_agent, route_intent
from .logging import get_logger
from .transcripts import TranscriptRecorder, exchange_turns
from .pipeline.llm import LLMInterface, run_tool_loop, run_tool_loop_stream
from .plugin_tools import build_plugin_tools
from .prompts import build_system_prompt, is_tail_message, split_prompt
from .session_state import (
    SessionState,
    SessionStore,
    store_get,
    store_get_or_create,
    store_load_history,
    store_save_history,
    store_save_session,
)
from .tenant_context import fetch_tenant_context, fetch_turn_knowledge
from .tools import UI_ACTION_TOOL_NAMES, ToolLayer
from .translation import (
    TranslatorInterface,
    build_translator,
    process_inbound_text,
    process_outbound_text,
)

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


# 2026-11-23: gemma sometimes ends a tool turn by echoing its chat-template tokens ("<tool_call|><|tool_response>")
# instead of a sentence. They are stripped; when nothing human is left, the last tool's own message is the reply.
_TEMPLATE_TOKEN = re.compile(r"<\|[^<>|\s]*\|?>|<[a-z_]+\|>")


def clean_reply(reply: str, trace: list[dict[str, Any]] | None) -> str:
    cleaned = _TEMPLATE_TOKEN.sub("", reply or "").strip()
    if cleaned:
        return cleaned
    for entry in reversed(trace or []):
        result = entry.get("result")
        if isinstance(result, dict) and isinstance(result.get("message"), str) and result["message"].strip():
            return result["message"].strip()
    return cleaned


TURN_KNOWLEDGE_MARK = "\n\n[Business knowledge that may help answer this message — not written by the customer]"


def _strip_turn_knowledge(history: list[dict[str, Any]]) -> None:
    """Put the latest user message back to what the customer wrote (drop this turn's knowledge block)."""
    for entry in reversed(history):
        content = entry.get("content")
        if entry.get("role") == "user" and isinstance(content, str) and TURN_KNOWLEDGE_MARK in content:
            entry["content"] = content.split(TURN_KNOWLEDGE_MARK, 1)[0]
            return


CHAT_TEXT_ADDENDUM = (
    "TYPED CHAT\n"
    "- This is a typed chat on the website, not a phone call: the customer reads your replies. Write phone numbers, "
    "emails, dates and amounts normally (for example 0803 555 1212, kemi.ojo@gmail.com, Tuesday 24 November at "
    "9:00 am) — never spell digits out as words and never say \"dot\" or \"at\" for an email. When confirming "
    "details, list them once in that written form. Never mention a call, the line, hearing or speaking."
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
        transcripts: TranscriptRecorder | None = None,
        translator: TranslatorInterface | None = None,
        chat_end: Any = None,
    ) -> None:
        self._settings = settings
        # 2026-11-24: app.chat_end.ChatEndTracker — ends quiet text conversations so they reach the CRM.
        self._chat_end = chat_end
        self._dapr = dapr
        self._llm = llm
        self._sessions = sessions
        # SPEC-W82 A1: translation layer (mock-default; fail-open — a turn
        # never blocks or fails on translation).
        self._translator = (
            translator if translator is not None else build_translator(settings)
        )
        self._histories = ChatHistory()
        self._escalation = escalation or LiveKitEscalation(settings)
        # Transcripts for the business's Conversations page (fire-and-forget; None = not recorded).
        self._transcripts = transcripts
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
        session_secret: str | None = None,
        persona_override: str | None = None,
        visitor: Any = None,
        channel: str = "web",
    ) -> tuple[SessionState, ToolLayer, list[dict[str, Any]]]:
        """Shared turn setup for the buffered and SSE streaming chat paths
        (SPEC-W3 §3): session, tenant context, multi-agent routing, tool
        layer and the re-rendered system prompt + user message in history.

        K15(b): resume requires the session_secret issued at creation; a
        conversation_id alone NEVER recovers confirmed/verified phone,
        history or escalation state (SessionStore starts fresh instead).
        """
        session = await store_get_or_create(
            self._sessions, conversation_id, site_slug, session_secret=session_secret
        )
        session.text_chat = True
        if channel in ("whatsapp", "telegram", "email") and conversation_id:
            # The messaging gateway records these chats and sends its stored conversation id as conversation_id.
            session.stored_id_lookup = lambda: conversation_id
        elif self._transcripts is not None:
            key = f"chat:{session.conversation_id}"
            session.stored_id_lookup = lambda: self._transcripts.stored_id(key)
        # 2026-11-21: the public page's pre-chat form (name/phone/email, self-reported; app/visitor.py).
        visitor_mod.apply(session, visitor)
        ctx = await fetch_tenant_context(self._dapr, self._settings, site_slug)

        # Wave 5 #8 A/B prompt testing: swap the tenant persona for the
        # eval candidate. Hard-gated by EVAL_PERSONA_OVERRIDE (default off).
        if persona_override and self._settings.eval_persona_override:
            ctx.agent_persona = persona_override

        # SPEC-W82 A2 (inbound translation): detect the caller's language
        # BEFORE route_intent; pure pcm/yo/ig/ha turns are translated to
        # English for routing/LLM. The detection and the pre-translation
        # text are stashed on the per-turn context (detected_language /
        # original_text), and the session's active_language is SET — the
        # chat path previously never set it, so downstream locale/TTS logic
        # (locale instruction, outbound translation) now works for text
        # users. Code-switched pcm/en turns (A4.1) are NOT force-translated
        # wholesale; they pass through with a code-switch annotation. All of
        # this is fail-open and gated by VOICE_TRANSLATE_ENABLED (a "0"
        # flag makes the block a strict no-op).
        inbound = await process_inbound_text(
            message,
            self._translator,
            enabled=self._settings.voice_translate_enabled,
            path="chat",
        )
        if inbound.detected_language:
            ctx.detected_language = inbound.detected_language
            ctx.original_text = inbound.original_text
            if inbound.detected_language != "en":
                session.active_language = inbound.detected_language
            if inbound.code_switch:
                ctx.code_switch = True
                ctx.code_switch_segments = list(inbound.segments)
        message = inbound.text

        # SPEC-W109: per-turn knowledge grounding. The query is the
        # POST-translation message (the KB embeddings are English-centric).
        # Turn snippets lead, bootstrap snippets follow; ctx is a per-turn
        # deepcopy so the merge never leaks across turns. Best-effort: an
        # empty/failed fetch keeps the bootstrap snippets exactly.
        turn_knowledge: list[str] = []
        if self._settings.knowledge_turn_queries:
            turn_snippets = await fetch_turn_knowledge(
                self._dapr,
                self._settings,
                ctx.tenant_slug,
                message,
                timeout_s=self._settings.knowledge_turn_timeout_s,
                channel=channel,
            )
            if turn_snippets:
                # 2026-11-23 latency: this turn's snippets ride the prompt's per-conversation tail
                # (it rides the user message for this request); the KNOWLEDGE block keeps the bootstrap snippets so the
                # prompt body stays identical across turns and the GPU server can reuse it.
                turn_knowledge = [s for s in turn_snippets if s not in ctx.knowledge_snippets][
                    : self._settings.knowledge_snippet_count
                ]

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

        # SPEC-W97 G8: hydrate the persisted history (dapr backend) BEFORE
        # the turn reads it; None = the backend keeps no record (memory:
        # ChatHistory owns it, hydration is a no-op).
        persisted = await store_load_history(
            self._sessions, session.conversation_id
        )
        if persisted is not None:
            self._histories.messages[session.conversation_id] = list(persisted)
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
            visitor=visitor_mod.from_session(session),
        )
        # 2026-11-23: a typed chat reads, it is not heard — write details normally.
        system_prompt = f"{system_prompt}\n{CHAT_TEXT_ADDENDUM}"
        # SPEC-W9 Part B: tell the agent it may drive the visitor's UI — but
        # only when the UI action tools are registered for this turn.
        registered = {t["function"]["name"] for t in tool_layer.schemas()}
        if any(name in registered for name in UI_ACTION_TOOL_NAMES):
            system_prompt = f"{system_prompt}\n\n{UI_ACTION_PROMPT_ADDENDUM}"
        # SPEC-W82 A4.1: a code-switched pcm/en turn keeps its mixed text;
        # the locale addendum tells the LLM to answer in the same register.
        if ctx.code_switch:
            system_prompt = f"{system_prompt}{codeswitch_instruction()}"
        body, tail = split_prompt(system_prompt)
        if history and history[0].get("role") == "system":
            history[0] = {"role": "system", "content": body}
        else:
            history.insert(0, {"role": "system", "content": body})
        # The per-conversation tail is its own system message right after the body (after the tool list in the
        # model's template), so the body + tools stay byte-identical and cached.
        if len(history) > 1 and history[1].get("role") == "system" and is_tail_message(str(history[1].get("content") or "")):
            history[1] = {"role": "system", "content": tail}
        elif tail:
            history.insert(1, {"role": "system", "content": tail})
        # 2026-11-23 latency: this turn's knowledge rides the user's message for THIS request only (restored to the
        # plain message after the reply, see _strip_turn_knowledge), so the system prompt never changes per turn.
        content = message
        if turn_knowledge:
            content = (f"{message}{TURN_KNOWLEDGE_MARK}\n"
                       + "\n".join(f"- {s}" for s in turn_knowledge))
        history.append({"role": "user", "content": content})
        return session, tool_layer, history

    async def _persist_turn(self, session: SessionState) -> None:
        """SPEC-W97 G8: turn-boundary persistence (session + bounded
        history). Fail-open — a state-store outage degrades resume, never
        breaks a turn; outcomes ride voice_session_store_ops_total."""
        try:
            await store_save_session(self._sessions, session)
            await store_save_history(
                self._sessions,
                session.conversation_id,
                self._histories.get(session.conversation_id),
            )
        except Exception as exc:  # noqa: BLE001 - persistence never breaks a turn
            log.warning(
                "session persist failed",
                conversation_id=session.conversation_id,
                error=str(exc)[:200],
            )

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

    def _record(self, session: SessionState, tool_layer: Any, message: str, reply: str, trace: list[dict[str, Any]]) -> None:
        if self._chat_end is not None:  # every channel, before the early returns below
            self._chat_end.touch(session, getattr(tool_layer, "tenant_context", None), trace)
        if self._transcripts is None:
            return
        # WhatsApp/Telegram: the messaging gateway's bridge records the conversation (keyed by the customer's number);
        # recording here as well listed every message twice, in two conversations.
        if (getattr(session, "channel", None) or "web") in ("whatsapp", "telegram", "email"):
            return
        ctx = getattr(tool_layer, "tenant_context", None)
        tenant_id = str(getattr(ctx, "tenant_id", "") or "")
        self._transcripts.record(
            f"chat:{session.conversation_id}", exchange_turns(message, reply, trace),
            tenant_id=tenant_id, site_slug=session.site_slug, channel=getattr(session, "channel", None) or "web",
            contact_phone=session.verified_phone or session.confirmed_phone or session.claimed_phone,
            contact_name=getattr(session, "caller_name", None),
        )

    def _stored_key(self, session: SessionState) -> str:
        return f"chat:{session.conversation_id}"

    async def _human_reply(self, session: SessionState, tool_layer: Any, message: str) -> dict[str, Any] | None:
        """The turn's response while staff own the conversation (web chat only; WhatsApp/Telegram are gated in the
        messaging gateway, which records those conversations). None = the receptionist answers as usual."""
        if self._transcripts is None or (getattr(session, "channel", None) or "web") in ("whatsapp", "telegram", "email"):
            return None
        stored = self._transcripts.stored_id(self._stored_key(session))
        ctx = getattr(tool_layer, "tenant_context", None)
        tenant_id = str(getattr(ctx, "tenant_id", "") or "")
        if not stored or not tenant_id or not await self._transcripts.handoff_active(stored, tenant_id):
            return None
        self._transcripts.record(
            self._stored_key(session), [{"role": "user", "text": message}],
            tenant_id=tenant_id, site_slug=session.site_slug, channel=getattr(session, "channel", None) or "web",
            contact_phone=session.verified_phone or session.confirmed_phone or session.claimed_phone,
            contact_name=getattr(session, "caller_name", None),
        )
        await self._persist_turn(session)
        log.info("chat turn left for staff (human handoff active)", conversation_id=session.conversation_id)
        return {
            "conversation_id": session.conversation_id,
            "session_secret": session.session_secret,
            "reply": "",
            "tool_calls": [],
            "phone_confirmed": session.confirmed_phone is not None,
            "phone_verified": session.verified_phone is not None,
            "active_agent": session.active_agent,
            "escalated": True,
            "human": True,
            "ui_actions": [],
            "request_contact": False,
        }

    async def staff_messages(self, conversation_id: str, session_secret: str | None, after: int = 0) -> dict[str, Any]:
        """2026-11-23: replies a person sent from the human inbox to this web chat (the widget polls this).
        Requires the session's resume secret, like resuming the chat itself."""
        session = await store_get(self._sessions, conversation_id)
        if session is None or not session.check_secret(session_secret) or self._transcripts is None:
            return {"messages": [], "human": False}
        stored = self._transcripts.stored_id(self._stored_key(session))
        if not stored:
            return {"messages": [], "human": False}
        ctx = await fetch_tenant_context(self._dapr, self._settings, session.site_slug)
        conv = await self._transcripts.conversation(stored, str(ctx.tenant_id))
        if not conv:
            return {"messages": [], "human": False}
        handoff = (conv.get("call_details") or {}).get("handoff") or {}
        msgs = [{"seq": t["seq"], "text": t["text"], "ts": t.get("ts"), "agent": handoff.get("agent") or "Staff"}
                for t in conv.get("turns") or [] if t.get("role") == "staff" and int(t.get("seq") or 0) > after]
        return {"messages": msgs, "human": bool(handoff.get("active"))}

    async def handle_message(
        self,
        *,
        site_slug: str,
        message: str,
        conversation_id: str | None,
        session_secret: str | None = None,
        persona_override: str | None = None,
        channel: str = "web",
        pin_verified_identity: str | None = None,
        visitor: Any = None,
    ) -> dict[str, Any]:
        session, tool_layer, history = await self._prepare_turn(
            site_slug=site_slug,
            message=message,
            conversation_id=conversation_id,
            session_secret=session_secret,
            persona_override=persona_override,
            visitor=visitor,
            channel=channel,
        )
        # SPEC-W6 Part A: omnichannel inbound — remember which channel the
        # message arrived on (session metadata + turn logging only).
        session.channel = channel
        # SPEC-W45 K15(c): channel-verified identity pinning (e.g. WhatsApp
        # wa_id). The HTTP layer (control_plane) passes this ONLY when the
        # request was authorized with the internal token AND the channel's
        # provider supplies a verified identity — a public web caller can
        # never set it. Pinned identity satisfies the OTP gate.
        if pin_verified_identity and channel == "email":
            # 2026-11-24: the email channel's verified identity is the sender's address (the messaging gateway
            # received the mail from it), not a phone: it is the customer's e-mail, never a verified phone.
            session.caller_email = pin_verified_identity
            session.visitor_form = True  # the prompt then lists the known address and does not ask for it again
        elif pin_verified_identity:
            pinned = session.mark_verified(pin_verified_identity)
            session.claimed_phone = session.claimed_phone or pinned
            # SPEC-W81 A3: a channel-pinned identity IS a completed step-up.
            metrics.stepup_verified("channel")
            log.info(
                "channel-verified identity pinned",
                conversation_id=session.conversation_id,
                channel=channel,
            )

        # 2026-11-23: a person took this chat over in the human inbox — record the customer's message (the inbox
        # bridge shows it to the staff member) and let the receptionist stay quiet.
        human = await self._human_reply(session, tool_layer, message)
        if human is not None:
            return human

        reply, trace = await run_tool_loop(
            self._llm,
            messages=history,
            tools=tool_layer.schemas(),
            dispatch=self._tool_runner.wrap_dispatch(tool_layer.dispatch),
        )
        reply = clean_reply(reply, trace)
        _strip_turn_knowledge(history)
        history.append({"role": "assistant", "content": reply})
        # SPEC-W82 A3 (outbound translation): the caller-visible reply is
        # translated en -> active_language; the HISTORY keeps the English
        # reply (internal reasoning stays English). Fail-open English, and
        # skipped when the reply is already substantially non-English.
        outbound_reply = await process_outbound_text(
            reply,
            session.active_language,
            self._translator,
            enabled=self._settings.voice_translate_enabled,
        )

        copilot_posted = await self._post_copilot(session, reply)
        self._record(session, tool_layer, message, reply, trace)

        log.info(
            "chat turn complete",
            conversation_id=session.conversation_id,
            channel=session.channel,
            tool_calls=len(trace),
            active_agent=session.active_agent,
            copilot=copilot_posted,
        )
        # SPEC-W85: ask Telegram callers to share their contact when a
        # step-up gate refused this turn and the session is not verified.
        # The gateway answers with a one-time request_contact keyboard;
        # the resulting self-shared phone is provider-verified (same trust
        # class as wa_id) and bound chat_id -> phone for subsequent pins.
        # verification_unavailable is included: where the portal OTP is
        # unconfigured, the contact share is the ONLY verification path.
        request_contact = (
            channel == "telegram"
            and session.verified_phone is None
            and any(
                isinstance(entry.get("result"), dict)
                and (
                    entry["result"].get("status") == "verification_required"
                    # verification_unavailable rides status="error" with the
                    # reason in `error` (W81 honest-degrade shape).
                    or entry["result"].get("error") == "verification_unavailable"
                )
                for entry in trace
            )
        )
        # SPEC-W97 G8: persist session + history at the turn boundary
        # (fail-open; memory backend = no-op).
        await self._persist_turn(session)
        return {
            "conversation_id": session.conversation_id,
            # K15(b): the resume credential — the client MUST present it
            # together with conversation_id to continue this session.
            "session_secret": session.session_secret,
            "reply": outbound_reply,
            "tool_calls": trace,
            "phone_confirmed": session.confirmed_phone is not None,
            "phone_verified": session.verified_phone is not None,
            "active_agent": session.active_agent,
            "escalated": session.escalation_room is not None,
            # SPEC-W9 Part B (additive): validated UI actions the LLM invoked
            # this turn; the web widget executes them client-side.
            "ui_actions": list(tool_layer.collected_ui_actions),
            # SPEC-W85 (additive): telegram contact-share prompt request.
            "request_contact": request_contact,
        }

    async def handle_message_stream(
        self,
        *,
        site_slug: str,
        message: str,
        conversation_id: str | None,
        session_secret: str | None = None,
        persona_override: str | None = None,
        visitor: Any = None,
    ):
        """SSE streaming chat (SPEC-W3 §3): async generator of event dicts.

        Yields {"delta": "..."} for every LLM content chunk and
        {"tool_call": {...}} when a tool is executed — the SAME tool layer
        as the buffered path. The terminal event is
        {"done": true, "conversation_id": ..., "session_secret": ...}
        (K15(b): both are required to resume). History and whisper-copilot
        bookkeeping happen after the final answer, exactly as in
        handle_message.
        """
        session, tool_layer, history = await self._prepare_turn(
            site_slug=site_slug,
            message=message,
            conversation_id=conversation_id,
            session_secret=session_secret,
            persona_override=persona_override,
            visitor=visitor,
        )
        # 2026-11-23: staff own this chat (human inbox): record the message, stay quiet, tell the widget.
        human = await self._human_reply(session, tool_layer, message)
        if human is not None:
            yield {"done": True, "human": True, "conversation_id": session.conversation_id,
                   "session_secret": session.session_secret}
            return

        # VOICE-SCALING §5 async tools (chat path): an immediate {"ack": ...}
        # SSE event precedes every slow tool execution; the tool call itself
        # runs under the hard timeout via the runner.
        try:
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

            streamed = reply
            reply = clean_reply(reply, trace)
            _strip_turn_knowledge(history)
            history.append({"role": "assistant", "content": reply})
            # SPEC-W82 A3 (outbound translation, SSE path): the deltas already
            # streamed in English, so the translated reply rides the terminal
            # done frame as an additive "reply" key (only present when it
            # differs from the streamed text). Fail-open English.
            outbound_reply = await process_outbound_text(
                reply,
                session.active_language,
                self._translator,
                enabled=self._settings.voice_translate_enabled,
            )
            copilot_posted = await self._post_copilot(session, reply)
            self._record(session, tool_layer, message, reply, trace)
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
            done_frame: dict[str, Any] = {
                "done": True,
                "conversation_id": session.conversation_id,
                "session_secret": session.session_secret,
                # 2026-11-23: the widget starts fetching staff replies (human inbox) once a chat is escalated.
                "escalated": session.escalation_room is not None,
            }
            if outbound_reply != streamed:  # translated, or cleaned of template tokens
                # SPEC-W82 A3 (additive): translated reply for non-English
                # sessions (the deltas above streamed the English original).
                done_frame["reply"] = outbound_reply
            yield done_frame
        finally:
            # SPEC-W97 G8: persist session + history at the turn
            # boundary — even on a mid-stream client disconnect
            # (GeneratorExit). Fail-open; memory backend = no-op.
            await self._persist_turn(session)
