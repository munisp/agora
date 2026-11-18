"""ToolLoopLLM: a second lookup in the answer round is run, and its result answered (it used to be dropped)."""
import asyncio
import json

import pytest

lk_llm = pytest.importorskip("livekit.agents.llm")

from app.lk_tool_loop import ToolLoopLLM  # noqa: E402


def _fnc_ctx(ran):
    ctx = lk_llm.FunctionContext()

    @ctx.ai_callable(description="Open slots")
    async def get_availability() -> str:
        ran.append("get_availability")
        return json.dumps({"slots": ["09:00", "10:00"]})

    return ctx


class _Stream(lk_llm.LLMStream):
    def __init__(self, chunks, calls, *, chat_ctx, fnc_ctx):
        super().__init__(chat_ctx=chat_ctx, fnc_ctx=fnc_ctx)
        self._chunks = list(chunks)
        self._function_calls_info = list(calls)

    async def __anext__(self):
        if not self._chunks:
            raise StopAsyncIteration
        return self._chunks.pop(0)


class _ScriptedLLM:
    """Each chat() call plays the next (chunks, tool names) step."""

    def __init__(self, steps):
        self.steps = list(steps)
        self.contexts = []
        self.tool_flags = []

    def chat(self, *, chat_ctx, fnc_ctx=None, **_kw):
        chunks, names = self.steps.pop(0)
        self.contexts.append(chat_ctx)
        self.tool_flags.append(fnc_ctx is not None)
        calls = [
            lk_llm.FunctionCallInfo(tool_call_id=f"call-{i}", function_info=fnc_ctx.ai_functions[n],
                                    raw_arguments="{}", arguments={})
            for i, n in enumerate(names)
        ]
        return _Stream(chunks, calls, chat_ctx=chat_ctx, fnc_ctx=fnc_ctx)


def _answer_round_ctx():
    ctx = lk_llm.ChatContext().append(role="user", text="Anything next Wednesday?")
    ctx.messages.append(lk_llm.ChatMessage(role="tool", content='{"name": "Harbour"}', tool_call_id="call-0"))
    return ctx


async def _collect(stream):
    out = []
    async for chunk in stream:
        out.append(chunk)
    await stream.aclose()
    return out


def test_second_lookup_is_run_and_answered():
    async def run():
        ran, seen = [], []
        fnc = _fnc_ctx(ran)
        inner = _ScriptedLLM([([], ["get_availability"]), (["We have 9 and 10."], [])])
        llm = ToolLoopLLM(inner)
        llm.on_extra_tools(collected=lambda calls: seen.append([c.function_info.name for c in calls]))
        out = await _collect(llm.chat(chat_ctx=_answer_round_ctx(), fnc_ctx=fnc))
        return ran, seen, out, inner

    ran, seen, out, inner = asyncio.run(run())
    assert ran == ["get_availability"]
    assert seen == [["get_availability"]]
    assert out == ["We have 9 and 10."]
    roles = [m.role for m in inner.contexts[1].messages]
    assert roles[-2:] == ["assistant", "tool"]  # the lookup and its result went back to the model


def _chunk(text):
    try:
        return lk_llm.ChatChunk(request_id="r", choices=[lk_llm.Choice(delta=lk_llm.ChoiceDelta(role="assistant", content=text))])
    except TypeError:  # older ChatChunk without request_id
        return lk_llm.ChatChunk(choices=[lk_llm.Choice(delta=lk_llm.ChoiceDelta(role="assistant", content=text))])


def _text(chunks):
    return "".join(c if isinstance(c, str) else "".join((ch.delta.content or "") for ch in c.choices) for c in chunks)


def test_first_round_reply_passes_through_unchanged():
    async def run():
        inner = _ScriptedLLM([([_chunk("Hello, how can "), _chunk("I help you today?")], [])])
        ctx = lk_llm.ChatContext().append(role="user", text="hello")
        return await _collect(ToolLoopLLM(inner).chat(chat_ctx=ctx, fnc_ctx=_fnc_ctx([])))

    assert _text(asyncio.run(run())) == "Hello, how can I help you today?"


def test_unbacked_booking_claim_runs_book_appointment_first():
    """gemma said "I have booked your clinic…" without calling the tool: the claim is held back, the model is told
    to call book_appointment, the booking runs, and only the answer after it is spoken."""
    async def run():
        ran = []
        fnc = _fnc_ctx(ran)

        @fnc.ai_callable(description="Book")
        async def book_appointment() -> str:
            ran.append("book_appointment")
            return json.dumps({"status": "accepted"})

        inner = _ScriptedLLM([
            ([_chunk("I have booked your enrollment clinic with Aisha Bello for Tuesday at 9. "), _chunk("Anything else?")], []),
            ([], ["book_appointment"]),
            ([_chunk("Your clinic with Aisha Bello is booked for Tuesday at 9.")], []),
        ])
        ctx = lk_llm.ChatContext().append(role="user", text="Nine o'clock is fine.")
        out = await _collect(ToolLoopLLM(inner).chat(chat_ctx=ctx, fnc_ctx=fnc))
        return ran, out, inner

    ran, out, inner = asyncio.run(run())
    assert ran == ["book_appointment"]
    spoken = _text(out)
    assert spoken == "Your clinic with Aisha Bello is booked for Tuesday at 9."  # the unbacked claim was never spoken
    assert any("book_appointment has NOT been called" in (m.content if isinstance(m.content, str) else " ".join(map(str, m.content)))
               for m in inner.contexts[1].messages if m.role == "system")


def test_rounds_are_capped_and_last_round_has_no_tools():
    async def run():
        ran = []
        fnc = _fnc_ctx(ran)
        inner = _ScriptedLLM([([], ["get_availability"]), ([], ["get_availability"]), (["Done."], [])])
        out = await _collect(ToolLoopLLM(inner, max_rounds=2).chat(chat_ctx=_answer_round_ctx(), fnc_ctx=fnc))
        return ran, out, inner

    ran, out, inner = asyncio.run(run())
    assert ran == ["get_availability", "get_availability"]
    assert out == ["Done."]
    assert inner.tool_flags == [True, True, False]
