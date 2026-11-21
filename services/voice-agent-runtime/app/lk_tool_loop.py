"""Lets a live call use more than one tool for one question (livekit-agents 0.10.x).

VoicePipelineAgent 0.10.2 runs the tools of the model's first reply, then asks the model for the answer and speaks
that answer's text — any tool calls in the answer are dropped. So a question that needs two lookups (the model first
calls get_business_info, then wants get_availability) ended in silence: no answer, nothing recorded.

ToolLoopLLM wraps the worker's LLM. On the answer round (the context ends with tool results) it runs the extra tool
calls itself, adds their results and asks again, up to `max_rounds` times, and streams only the final reply's text to
the pipeline. The first round is passed through untouched, so the pipeline's own tool handling (filler lines, events)
is unchanged. Listeners hear about the extra calls (call activity on screen, transcripts).
"""
from __future__ import annotations

import asyncio
from typing import Any, Callable

from livekit.agents import llm as lk_llm

from .logging import get_logger

log = get_logger("tool-loop")

_END = object()


def _is_answer_round(chat_ctx: Any) -> bool:
    msgs = getattr(chat_ctx, "messages", None) or []
    return bool(msgs) and getattr(msgs[-1], "role", None) == "tool"


class _ToolLoopStream(lk_llm.LLMStream):
    def __init__(self, owner: "ToolLoopLLM", *, chat_ctx: Any, fnc_ctx: Any, first_round: bool = False) -> None:
        super().__init__(chat_ctx=chat_ctx, fnc_ctx=fnc_ctx)
        self._owner = owner
        self._queue: asyncio.Queue[Any] = asyncio.Queue()
        self._runner = asyncio.create_task(self._run())

    async def _run(self) -> None:
        owner = self._owner
        ctx = self._chat_ctx.copy()
        try:
            for round_no in range(owner.max_rounds + 1):
                last = round_no == owner.max_rounds  # out of rounds: answer without tools
                stream = owner.inner.chat(chat_ctx=ctx, fnc_ctx=None if last else self._fnc_ctx)
                try:
                    async for chunk in stream:
                        await self._queue.put(chunk)  # the pipeline speaks only the text in these
                    calls = list(stream.function_calls or [])
                finally:
                    await stream.aclose()
                if not calls:
                    return
                owner.notify(calls)
                called = [c.execute() for c in calls]
                await asyncio.gather(*(c.task for c in called), return_exceptions=True)
                owner.notify_finished(called)
                done = [c for c in called if c.result is not None]
                if not done:
                    return
                ctx.messages.append(lk_llm.ChatMessage.create_tool_calls([c.call_info for c in done], text=""))
                ctx.messages.extend(lk_llm.ChatMessage.create_tool_from_called_function(c) for c in done)
                log.info("extra tool round", round=round_no + 1, tools=[c.call_info.function_info.name for c in done])
        except Exception as exc:  # noqa: BLE001 - end the reply rather than hang the call
            log.warning("tool loop failed", error=str(exc)[:300])
        finally:
            await self._queue.put(_END)

    async def __anext__(self) -> Any:
        item = await self._queue.get()
        if item is _END:
            raise StopAsyncIteration
        return item

    async def aclose(self) -> None:
        if not self._runner.done():
            self._runner.cancel()
            try:
                await self._runner
            except (asyncio.CancelledError, Exception):  # noqa: BLE001
                pass
        await super().aclose()


import re

# "I have booked your visit", "your appointment is confirmed for Tuesday", "you're all set for 9:00"
_BOOKED_CLAIM = re.compile(
    r"\b(i(?:'ve| have)? (?:now )?(?:booked|scheduled|reserved|confirmed)|i(?:'ve| have) (?:now |already )?(?:set up|arranged|fixed)|(?:is|has been|now) (?:booked|scheduled|confirmed)"
    r"|(?:booking|appointment|visit|call) (?:is |has been )?(?:confirmed|booked)|you(?:'re| are) (?:all )?(?:booked|set for)"
    # Nigerian Pidgin: "I don book am", "e don book", "I don confirm your appointment"
    r"|\b(?:i|we|e|e?m) (?:don|done) (?:book|fix|confirm|schedule)\w*"
    # "Your appointment … don set for Tuesday" (Pidgin), "your appointment is set/fixed", "you're all set": a
    # Pidgin call said this after the caller confirmed, with no booking made (2026-11-19).
    r"|\b(?:appointment|booking|visit|call|consultation|session)\b[^.!?]{0,90}?\b(?:don|is|has been|now) (?:set|fixed|ready)\b"
    r"|\ball set\b"
    # A promise to book now ("I go book it for you now", "I will book that", "let me book…") ends the turn without
    # booking: the model then waits for the caller. Treated like a claim: the booking is made in this same turn.
    r"|\b(?:i(?:'ll| will| go| dey| am going to)|let me|we(?:'ll| will| go)) (?:now |just |go ahead and )?(?:book|fix|schedule|reserve)\b)",
    re.I)
_BOOKING_WORDS = re.compile(r"appointment|visit|\bcall\b|consultation|clinic|session|slot|booking|\bbooked\b|scheduled|"
                            r"onboarding|registration", re.I)
_SENTENCE_END = re.compile(r"[.!?](\s|$)")


def booking_claim(text: str) -> bool:
    """Does this reply tell the caller an appointment is booked? ("I have confirmed your number" does not.)"""
    return bool(_BOOKED_CLAIM.search(text) and _BOOKING_WORDS.search(text))


def _booked_in(chat_ctx: Any) -> bool:
    """True when a book/reschedule tool already ran in this conversation (its result is in the history)."""
    for m in getattr(chat_ctx, "messages", None) or []:
        if getattr(m, "role", None) == "tool" and getattr(m, "name", "") in ("book_appointment", "reschedule_appointment"):
            return True
        for call in getattr(m, "tool_calls", None) or []:
            name = getattr(getattr(call, "function_info", None), "name", "") or getattr(call, "function_name", "")
            if name in ("book_appointment", "reschedule_appointment"):
                return True
    return False


class _ClaimGuardStream(lk_llm.LLMStream):
    """Passes a reply through sentence by sentence; a sentence that says the appointment is booked when no booking
    tool ran is not spoken: the model is told to call book_appointment instead, the call runs, and its answer is
    spoken. (gemma told a caller "I have booked your enrollment clinic…" without calling the tool: no appointment,
    no CRM record.)"""

    def __init__(self, owner: "ToolLoopLLM", *, chat_ctx: Any, fnc_ctx: Any, kwargs: dict) -> None:
        super().__init__(chat_ctx=chat_ctx, fnc_ctx=fnc_ctx)
        self._owner, self._kwargs = owner, kwargs
        self._queue: asyncio.Queue[Any] = asyncio.Queue()
        self._runner = asyncio.create_task(self._run())

    async def _run(self) -> None:
        owner = self._owner
        try:
            stream = owner.inner.chat(chat_ctx=self._chat_ctx, fnc_ctx=self._fnc_ctx, **self._kwargs)
            held: list[Any] = []
            text = ""
            claimed = False
            try:
                async for chunk in stream:
                    delta = "".join((getattr(c.delta, "content", None) or "") for c in getattr(chunk, "choices", []) or [])
                    held.append(chunk)
                    text += delta
                    if _SENTENCE_END.search(text) or len(text) > 220:
                        if booking_claim(text) and not _booked_in(self._chat_ctx):
                            claimed = True
                            break
                        for c in held:
                            await self._queue.put(c)
                        held, text = [], ""
                if not claimed:
                    if held and booking_claim(text) and not _booked_in(self._chat_ctx) and not stream.function_calls:
                        claimed = True
                    else:
                        for c in held:
                            await self._queue.put(c)
                        self._function_calls_info = list(stream.function_calls or [])
                        return
            finally:
                await stream.aclose()
            log.warning("unbacked booking claim held back; asking for book_appointment", said=text[:160])
            ctx = self._chat_ctx.copy()
            ctx.messages.append(lk_llm.ChatMessage.create(role="system", text=(
                "You were about to tell the caller the appointment is booked, but book_appointment has NOT been called, "
                "so nothing is booked. Call book_appointment now with the agreed offering, team member, start time, "
                "the caller's confirmed phone number and name. Only after it succeeds, tell the caller it is booked.")))
            inner = _ToolLoopStream(owner, chat_ctx=ctx, fnc_ctx=self._fnc_ctx, first_round=True)
            async for chunk in inner:
                await self._queue.put(chunk)
            await inner.aclose()
        except Exception as exc:  # noqa: BLE001 - end the reply rather than hang the call
            log.warning("claim guard failed", error=str(exc)[:300])
        finally:
            await self._queue.put(_END)

    async def __anext__(self) -> Any:
        item = await self._queue.get()
        if item is _END:
            raise StopAsyncIteration
        return item

    async def aclose(self) -> None:
        if not self._runner.done():
            self._runner.cancel()
            try:
                await self._runner
            except (asyncio.CancelledError, Exception):  # noqa: BLE001
                pass
        await super().aclose()


class ToolLoopLLM(lk_llm.LLM):
    def __init__(self, inner: Any, *, max_rounds: int = 3) -> None:
        super().__init__()
        self.inner = inner
        self.max_rounds = max_rounds
        self._on_tools: list[Callable[[list[Any]], None]] = []
        self._on_finished: list[Callable[[list[Any]], None]] = []

    def on_extra_tools(self, collected: Callable[[list[Any]], None] | None = None,
                       finished: Callable[[list[Any]], None] | None = None) -> None:
        if collected:
            self._on_tools.append(collected)
        if finished:
            self._on_finished.append(finished)

    def notify(self, calls: list[Any]) -> None:
        for fn in self._on_tools:
            try:
                fn(calls)
            except Exception as exc:  # noqa: BLE001
                log.debug("tool listener failed", error=str(exc)[:200])

    def notify_finished(self, called: list[Any]) -> None:
        for fn in self._on_finished:
            try:
                fn(called)
            except Exception as exc:  # noqa: BLE001
                log.debug("tool listener failed", error=str(exc)[:200])

    def chat(self, *, chat_ctx: Any, fnc_ctx: Any = None, **kwargs: Any) -> Any:
        if fnc_ctx is not None and self.max_rounds > 0 and _is_answer_round(chat_ctx):
            return _ToolLoopStream(self, chat_ctx=chat_ctx, fnc_ctx=fnc_ctx)
        if fnc_ctx is not None and self.max_rounds > 0:
            return _ClaimGuardStream(self, chat_ctx=chat_ctx, fnc_ctx=fnc_ctx, kwargs=kwargs)
        return self.inner.chat(chat_ctx=chat_ctx, fnc_ctx=fnc_ctx, **kwargs)

    def __getattr__(self, name: str) -> Any:  # anything else (model, capabilities, ...) comes from the real LLM
        if name == "inner":
            raise AttributeError(name)
        return getattr(self.inner, name)
