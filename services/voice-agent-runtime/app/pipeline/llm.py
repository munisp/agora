"""LLM stage: interface + OpenAI-compatible implementation.

Targets any OpenAI-compatible endpoint (env LLM_BASE_URL): Ollama by default
(http://ollama:11434/v1), vLLM or a hosted provider by configuration. Used by
the text chat path; the LiveKit worker uses the equivalent
`livekit-plugins-openai` LLM node with the same settings.
"""

from __future__ import annotations

import json
import time
from typing import Any, Callable, Protocol

import httpx

import openai
from openai import AsyncOpenAI

from .. import metrics
from ..logging import get_logger

log = get_logger("llm")


class LLMInterface(Protocol):
    async def chat_with_tools(
        self,
        messages: list[dict[str, Any]],
        tools: list[dict[str, Any]],
    ) -> Any:
        """One chat-completion round with tool definitions.

        Returns the raw assistant message object (with optional `tool_calls`).
        """
        ...


def _record_usage(usage: Any) -> None:
    if usage is None:
        return
    registry = metrics.get_registry()
    prompt = getattr(usage, "prompt_tokens", None)
    completion = getattr(usage, "completion_tokens", None)
    if prompt:
        registry.llm_tokens.inc(prompt, kind="prompt")
    if completion:
        registry.llm_tokens.inc(completion, kind="completion")


class _BodyMergeTransport(httpx.AsyncBaseTransport):
    """Merges fixed fields into JSON chat-completions bodies (clients like livekit's plugin can't add them)."""

    def __init__(self, extra: dict[str, Any], inner: httpx.AsyncBaseTransport | None = None) -> None:
        self._extra = extra
        self._inner = inner or httpx.AsyncHTTPTransport()

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        if request.method == "POST" and request.url.path.endswith("/chat/completions"):
            try:
                body = json.loads(await request.aread())
            except ValueError:
                body = None
            if isinstance(body, dict):
                body = {**body, **{k: v for k, v in self._extra.items() if k not in body}}
                content = json.dumps(body).encode()
                headers = {k: v for k, v in request.headers.items() if k.lower() != "content-length"}
                request = httpx.Request(request.method, request.url, headers=headers, content=content,
                                        extensions=request.extensions)
        return await self._inner.handle_async_request(request)

    async def aclose(self) -> None:
        await self._inner.aclose()


def http_client_with_body(extra: dict[str, Any], timeout: Any = None) -> httpx.AsyncClient:
    kwargs: dict[str, Any] = {"transport": _BodyMergeTransport(extra)}
    if timeout is not None:
        kwargs["timeout"] = timeout
    return httpx.AsyncClient(**kwargs)


class OpenAICompatibleLLM:
    def __init__(
        self,
        base_url: str,
        model: str,
        api_key: str = "ollama",
        timeout_s: float | None = None,
        extra_body: dict[str, Any] | None = None,
        # SPEC-W71 P7: response-length cap (VOICE_LLM_MAX_TOKENS) applied on
        # OUR OWN client paths only (chat_with_tools / stream_with_tools) —
        # spoken replies are bounded, bounding LLM decode time per turn.
        # 0 disables (the kwarg is then omitted from the request). The
        # livekit-plugins-openai node is deliberately NOT touched (0.10.x
        # has no such param).
        max_tokens: int = 0,
    ) -> None:
        kwargs: dict[str, Any] = {"base_url": base_url, "api_key": api_key}
        if timeout_s is not None:
            kwargs["timeout"] = timeout_s
        if extra_body:
            kwargs["http_client"] = http_client_with_body(extra_body)
        self._client = AsyncOpenAI(**kwargs)
        self.model = model
        self._max_tokens = max_tokens

    def _create_kwargs(self, messages: list[dict[str, Any]], tools) -> dict[str, Any]:
        kwargs: dict[str, Any] = {
            "model": self.model,
            "messages": messages,
            "tools": tools or None,
            "tool_choice": "auto" if tools else None,
            "temperature": 0.3,
            # SPEC-W71 P7: None = omitted by the openai client (0 disables).
            "max_tokens": self._max_tokens or None,
        }
        return kwargs

    async def chat_with_tools(
        self,
        messages: list[dict[str, Any]],
        tools: list[dict[str, Any]],
    ) -> Any:
        timer = metrics.get_registry().llm_latency.time()
        try:
            with timer:
                resp = await self._client.chat.completions.create(
                    **self._create_kwargs(messages, tools)
                )
        finally:
            metrics.session_llm_latency(timer.elapsed)  # per-session quality
        _record_usage(getattr(resp, "usage", None))
        return resp.choices[0].message

    async def complete(self, messages: list[dict[str, Any]]) -> str:
        msg = await self.chat_with_tools(messages, tools=[])
        return msg.content or ""

    async def stream_with_tools(
        self,
        messages: list[dict[str, Any]],
        tools: list[dict[str, Any]],
    ) -> Any:
        """One STREAMING chat-completion round (SSE chat path, SPEC-W3 §3).

        Returns the OpenAI AsyncStream; each chunk's `choices[0].delta` may
        carry `content` text and/or incremental `tool_calls` fragments that
        must be assembled by index before execution.
        """
        timer = metrics.get_registry().llm_latency.time()
        try:
            with timer:
                stream = await self._client.chat.completions.create(
                    **self._create_kwargs(messages, tools),
                    stream=True,
                    stream_options={"include_usage": True},
                )
        finally:
            metrics.session_llm_latency(timer.elapsed)  # per-session quality
        return stream


# ---------------------------------------------------------------------------
# Fallback chain (VOICE-SCALING §3): primary -> secondary OpenAI-compatible
# endpoint with a circuit breaker. Retry-with-fallback = degraded latency,
# not dead calls.
# ---------------------------------------------------------------------------
def is_retryable_llm_error(exc: BaseException) -> bool:
    """Connection errors, timeouts, 429s and 5xxs justify a fallback hop."""
    if isinstance(exc, (openai.APIConnectionError, openai.APITimeoutError, TimeoutError)):
        return True
    if isinstance(exc, openai.APIStatusError):
        return exc.status_code == 429 or exc.status_code >= 500
    return False


class CircuitBreaker:
    """After `failure_threshold` consecutive primary failures, route calls to
    the fallback for `cooldown_s`, then allow one primary probe. A failed
    probe re-opens the circuit for another cooldown window."""

    def __init__(
        self,
        failure_threshold: int = 3,
        cooldown_s: float = 60.0,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self._threshold = max(1, failure_threshold)
        self._cooldown = cooldown_s
        self._clock = clock
        self._failures = 0
        self._opened_at: float | None = None

    @property
    def is_open(self) -> bool:
        return self._opened_at is not None and not self.primary_allowed()

    def primary_allowed(self) -> bool:
        if self._opened_at is None:
            return True
        return (self._clock() - self._opened_at) >= self._cooldown

    def record_success(self) -> None:
        self._failures = 0
        self._opened_at = None

    def record_failure(self) -> None:
        self._failures += 1
        if self._failures >= self._threshold:
            self._opened_at = self._clock()


class FallbackLLM:
    """LLMInterface wrapper: primary endpoint with circuit-broken fallback.

    Applies to every chat path entry point (buffered tool loop, streaming
    tool loop, `complete`). The LiveKit worker's LLM node is the
    livekit-plugins-openai class and cannot hot-swap endpoints mid-process;
    the fallback chain covers the chat/tool-loop paths (see README).
    """

    def __init__(
        self,
        primary: OpenAICompatibleLLM,
        fallback: OpenAICompatibleLLM | None,
        *,
        failure_threshold: int = 3,
        cooldown_s: float = 60.0,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self._primary = primary
        self._fallback = fallback
        self._breaker = CircuitBreaker(failure_threshold, cooldown_s, clock)

    @property
    def breaker(self) -> CircuitBreaker:
        return self._breaker

    async def _call_primary(self, method: str, *args) -> Any:
        try:
            result = await getattr(self._primary, method)(*args)
            self._breaker.record_success()
            return result
        except Exception as exc:
            if not is_retryable_llm_error(exc):
                raise
            self._breaker.record_failure()
            if self._fallback is None:
                raise
            log.warning(
                "primary LLM failed; using fallback",
                method=method,
                error=str(exc)[:200],
                circuit_open=self._breaker.is_open,
            )
            metrics.session_llm_fallback()  # per-session quality signal
            return await getattr(self._fallback, method)(*args)

    async def chat_with_tools(
        self,
        messages: list[dict[str, Any]],
        tools: list[dict[str, Any]],
    ) -> Any:
        if self._fallback is None or self._breaker.primary_allowed():
            return await self._call_primary("chat_with_tools", messages, tools)
        metrics.session_llm_fallback()  # circuit open: served by fallback
        return await self._fallback.chat_with_tools(messages, tools)

    async def complete(self, messages: list[dict[str, Any]]) -> str:
        msg = await self.chat_with_tools(messages, tools=[])
        return msg.content or ""

    async def stream_with_tools(
        self,
        messages: list[dict[str, Any]],
        tools: list[dict[str, Any]],
    ) -> Any:
        """Returns an async generator of chunks (fallback-safe).

        Falls back only when the primary fails BEFORE any chunk was yielded;
        a mid-stream failure re-raises to avoid duplicating spoken content.
        """
        return self._stream_gen(messages, tools)

    async def _stream_gen(self, messages, tools):
        if self._fallback is not None and not self._breaker.primary_allowed():
            metrics.session_llm_fallback()  # circuit open: served by fallback
            stream = await self._fallback.stream_with_tools(messages, tools)
            async for chunk in stream:
                yield chunk
            return
        yielded = False
        try:
            stream = await self._primary.stream_with_tools(messages, tools)
            async for chunk in stream:
                yielded = True
                yield chunk
            self._breaker.record_success()
            return
        except Exception as exc:
            if yielded or not is_retryable_llm_error(exc) or self._fallback is None:
                raise
            self._breaker.record_failure()
            log.warning(
                "primary LLM stream failed; using fallback", error=str(exc)[:200]
            )
            metrics.session_llm_fallback()  # per-session quality signal
        stream = await self._fallback.stream_with_tools(messages, tools)
        async for chunk in stream:
            yield chunk


def build_llm(settings) -> LLMInterface:
    """Factory from Settings: primary endpoint, plus the fallback chain when
    LLM_FALLBACK_BASE_URL is configured."""
    primary = OpenAICompatibleLLM(
        base_url=settings.llm_base_url,
        model=settings.llm_model,
        api_key=settings.llm_api_key,
        timeout_s=settings.llm_timeout_s,
        extra_body=getattr(settings, "llm_extra_body", None) or None,
        max_tokens=getattr(settings, "voice_llm_max_tokens", 300),
    )
    if not settings.llm_fallback_base_url:
        return primary
    fallback = OpenAICompatibleLLM(
        base_url=settings.llm_fallback_base_url,
        model=settings.llm_fallback_model or settings.llm_model,
        api_key=settings.llm_fallback_api_key or "none",
        timeout_s=settings.llm_timeout_s,
        max_tokens=getattr(settings, "voice_llm_max_tokens", 300),
    )
    return FallbackLLM(
        primary,
        fallback,
        failure_threshold=settings.llm_cb_failures,
        cooldown_s=settings.llm_cb_cooldown_s,
    )


async def run_tool_loop(
    llm: OpenAICompatibleLLM,
    *,
    messages: list[dict[str, Any]],
    tools: list[dict[str, Any]],
    dispatch,
    max_rounds: int = 6,
) -> tuple[str, list[dict[str, Any]]]:
    """Text-in/text-out agent loop with function calling.

    `dispatch(name, arguments)` is awaited for each tool call and must return
    a JSON-serialisable result. Returns (final_text, tool_trace).
    """
    trace: list[dict[str, Any]] = []
    for _round in range(max_rounds):
        msg = await llm.chat_with_tools(messages, tools)
        tool_calls = getattr(msg, "tool_calls", None) or []
        if not tool_calls:
            return msg.content or "", trace

        # Append the assistant message (with tool calls) then each tool result.
        messages.append(
            {
                "role": "assistant",
                "content": msg.content or "",
                "tool_calls": [
                    {
                        "id": tc.id,
                        "type": "function",
                        "function": {
                            "name": tc.function.name,
                            "arguments": tc.function.arguments,
                        },
                    }
                    for tc in tool_calls
                ],
            }
        )
        for tc in tool_calls:
            name = tc.function.name
            try:
                arguments = json.loads(tc.function.arguments or "{}")
            except json.JSONDecodeError:
                arguments = {}
            result = await dispatch(name, arguments)
            trace.append({"tool": name, "arguments": arguments, "result": result})
            messages.append(
                {
                    "role": "tool",
                    "tool_call_id": tc.id,
                    "content": json.dumps(result, ensure_ascii=False),
                }
            )
    log.warning("tool loop hit max rounds", rounds=max_rounds)
    return (
        "I'm sorry, I couldn't complete that request just now. "
        "Could you rephrase or try again?"
    ), trace


_MAX_ROUNDS_FALLBACK = (
    "I'm sorry, I couldn't complete that request just now. "
    "Could you rephrase or try again?"
)


async def run_tool_loop_stream(
    llm: OpenAICompatibleLLM,
    *,
    messages: list[dict[str, Any]],
    tools: list[dict[str, Any]],
    dispatch,
    max_rounds: int = 6,
    ack_for_tool=None,
):
    """Streaming variant of run_tool_loop (SPEC-W3 §3 SSE chat).

    Async generator yielding events through the SAME tool layer:
      - {"type": "delta", "text": ...} for every LLM content chunk,
      - {"type": "ack", "tool": name, "text": ...} immediately BEFORE a slow
        tool executes, when `ack_for_tool(name)` returns a filler line
        (VOICE-SCALING §5 async tools: no dead air on the SSE chat path),
      - {"type": "tool", "tool": name, "arguments": ..., "result": ...}
        when a streamed tool call completes and is executed,
      - {"type": "final", "text": full_reply, "trace": [...]} exactly once
        as the last event (callers use it for history bookkeeping; the
        terminal SSE `done` frame is emitted by the HTTP layer).

    Tool-call rounds append the assistant + tool messages to `messages`
    exactly like run_tool_loop, so history stays consistent between the
    streaming and non-streaming paths.
    """
    trace: list[dict[str, Any]] = []
    final_text = ""
    for _round in range(max_rounds):
        # SPEC-W71 P9: voice_llm_first_token_seconds — request start to the
        # first SSE chunk of this round's stream (observed once per round).
        request_started = time.perf_counter()
        stream = await llm.stream_with_tools(messages, tools)
        first_chunk_marked = False
        content_parts: list[str] = []
        # Assemble incremental tool-call fragments by their `index` slot.
        tc_acc: dict[int, dict[str, Any]] = {}
        async for chunk in stream:
            if not first_chunk_marked:
                first_chunk_marked = True
                metrics.get_registry().llm_first_token.observe(
                    time.perf_counter() - request_started
                )
            _record_usage(getattr(chunk, "usage", None))
            choices = getattr(chunk, "choices", None) or []
            if not choices:
                continue
            delta = choices[0].delta
            content = getattr(delta, "content", None)
            if content:
                content_parts.append(content)
                yield {"type": "delta", "text": content}
            for tc in getattr(delta, "tool_calls", None) or []:
                slot = tc_acc.setdefault(
                    tc.index, {"id": "", "name": "", "arguments": ""}
                )
                if tc.id:
                    slot["id"] = tc.id
                fn = getattr(tc, "function", None)
                if fn is not None:
                    if fn.name:
                        slot["name"] += fn.name
                    if fn.arguments:
                        slot["arguments"] += fn.arguments

        text = "".join(content_parts)
        if not tc_acc:
            final_text = text
            break

        tool_calls = [tc_acc[i] for i in sorted(tc_acc)]
        messages.append(
            {
                "role": "assistant",
                "content": text,
                "tool_calls": [
                    {
                        "id": tc["id"] or f"call_{i}",
                        "type": "function",
                        "function": {"name": tc["name"], "arguments": tc["arguments"]},
                    }
                    for i, tc in enumerate(tool_calls)
                ],
            }
        )
        for i, tc in enumerate(tool_calls):
            name = tc["name"]
            try:
                arguments = json.loads(tc["arguments"] or "{}")
            except json.JSONDecodeError:
                arguments = {}
            if ack_for_tool is not None:
                ack = ack_for_tool(name)
                if ack:
                    yield {"type": "ack", "tool": name, "text": ack}
            result = await dispatch(name, arguments)
            trace.append({"tool": name, "arguments": arguments, "result": result})
            yield {"type": "tool", "tool": name, "arguments": arguments, "result": result}
            messages.append(
                {
                    "role": "tool",
                    "tool_call_id": tc["id"] or f"call_{i}",
                    "content": json.dumps(result, ensure_ascii=False),
                }
            )
    else:
        log.warning("streaming tool loop hit max rounds", rounds=max_rounds)
        final_text = _MAX_ROUNDS_FALLBACK
        yield {"type": "delta", "text": final_text}

    yield {"type": "final", "text": final_text, "trace": trace}
