"""Async tools with filler ack + hard timeout (VOICE-SCALING §5, P0).

Slow Dapr-invoked tools (availability/booking/knowledge lookups) used to
leave the caller in silence while the downstream call completed. This module
implements the livekit-agents 0.10.x-pragmatic equivalent of 1.6's
`ctx.update(...)` / `ctx.with_filler(...)`:

- **Voice path** (`AsyncToolRunner.run` with a `speaker`): the per-industry
  ack line is scheduled as a TTS task BEFORE the tool call is awaited; if the
  tool answers within the grace window (default 400 ms) the ack is cancelled
  and never spoken.
- **Chat path** (`run_tool_loop_stream(ack_for_tool=...)` in pipeline/llm.py):
  SSE already streams, so an immediate {"ack": "..."} event is emitted before
  tool execution instead.
- **Every path**: a hard timeout (env TOOL_TIMEOUT_SECONDS, default 4s) on
  every Dapr tool call; a timeout (or failure) resolves to a spoken-apology
  payload instead of dead air — TimeoutError never escapes into the pipeline.
- **SPEC-W81 A1**: the OTP verification tools (request_verification_code /
  verify_caller_code) do a booking-portal HTTP round-trip whose client
  timeout is HTTP_TIMEOUT_S (15s default) — longer than the 4s global tool
  timeout. They are covered by a PER-TOOL timeout override
  (AsyncToolRunner timeout_overrides, wired from
  VOICE_VERIFICATION_TOOL_TIMEOUT_S, default 20s) so the global default is
  untouched and a slow-but-working portal still answers with a real result.
- **SPEC-W86**: read_keypad_entry waits on the caller's keypad (DTMF) for
  up to 60s — it joins SLOW_TOOLS with its own per-tool timeout override
  (KEYPAD_TOOL_TIMEOUT_S, default 65s) wired in the LiveKit worker.
"""

from __future__ import annotations

import asyncio
import time
from typing import Any, Awaitable, Callable

from . import metrics
from .logging import get_logger

log = get_logger("async-tools")

# SPEC-W81 A1: the caller-verification (OTP) tools, exposed on the LiveKit
# voice path too. They run through the slow-tool machinery (filler ack +
# hard timeout) because they do a portal HTTP round-trip.
VERIFICATION_TOOLS = frozenset({"request_verification_code", "verify_caller_code"})

# Tools whose Dapr round-trip can be felt by the caller. Everything else
# (get_business_info from the cached tenant context, request_human) answers
# fast enough to skip the filler.
SLOW_TOOLS = frozenset(
    {
        "get_availability",
        "book_appointment",
        "reschedule_appointment",
        "cancel_appointment",
        "lookup_appointment",
        "knowledge_search",
        # SPEC-W86: read_keypad_entry blocks up to 60s waiting for the
        # caller to type on the keypad — slow by definition.
        "read_keypad_entry",
    }
    | VERIFICATION_TOOLS
)

DEFAULT_ACK = "Let me check that for you…"
APOLOGY_MESSAGE = (
    "I'm having trouble reaching our booking system, "
    "please try again in a moment."
)

# Per-industry defaults (industry pack id -> persona-flavoured filler). Packs
# can override per tool or globally via tenant terminology (see ToolAckPolicy).
_INDUSTRY_ACKS = {
    "salon": "Let me take a look at the book for you…",
    "clinic": "One moment while I check our scheduling system…",
    "consultancy": "Let me pull that up for you…",
    "support-desk": "Let me look into that for you…",
}


class ToolAckPolicy:
    """Resolves the filler ack line per tool.

    Resolution order:
    1. tenant/pack terminology override: ``terminology["tool_ack"]`` — either
       a ``{tool: line}`` mapping or a single string applied to all slow tools.
    2. per-industry default (pack persona flavour).
    3. DEFAULT_ACK fallback.
    """

    def __init__(
        self,
        *,
        industry: str = "",
        terminology: dict[str, Any] | None = None,
        slow_tools: frozenset[str] = SLOW_TOOLS,
    ) -> None:
        self._industry = industry
        override = (terminology or {}).get("tool_ack")
        self._override_map: dict[str, str] = {}
        self._override_all: str | None = None
        if isinstance(override, dict):
            self._override_map = {
                str(k): str(v) for k, v in override.items() if v
            }
        elif isinstance(override, str) and override.strip():
            self._override_all = override.strip()
        self._slow_tools = slow_tools

    @classmethod
    def from_context(cls, ctx) -> "ToolAckPolicy":
        return cls(
            industry=getattr(ctx, "industry", "") or "",
            terminology=getattr(ctx, "terminology", None) or {},
        )

    def ack_for(self, tool: str) -> str | None:
        """Filler line for a slow tool, or None when the tool is fast."""
        if tool not in self._slow_tools:
            return None
        if tool in self._override_map:
            return self._override_map[tool]
        if self._override_all is not None:
            return self._override_all
        return _INDUSTRY_ACKS.get(self._industry, DEFAULT_ACK)


def apology_payload(status: str = "timeout") -> dict[str, Any]:
    """Tool result that makes the agent SPEAK the apology instead of going
    silent (returned to the model as the tool output)."""
    return {"status": status, "message": APOLOGY_MESSAGE, "spoken": APOLOGY_MESSAGE}


class AsyncToolRunner:
    """Timeout + filler + metrics wrapper shared by the chat and voice paths.

    SPEC-W81 A1: ``timeout_overrides`` maps individual tool names to a
    longer hard timeout (the OTP verification tools whose portal round-trip
    can outlast the global default). The global ``timeout_s`` is unchanged
    for every tool not listed — overrides are strictly opt-in, never a
    silent global change.
    """

    def __init__(
        self,
        *,
        timeout_s: float = 4.0,
        ack_grace_ms: int = 400,
        timeout_overrides: dict[str, float] | None = None,
    ) -> None:
        self._timeout_s = timeout_s
        self._grace_s = ack_grace_ms / 1000.0
        self._timeout_overrides = dict(timeout_overrides or {})

    def timeout_for(self, tool: str) -> float:
        """Effective hard timeout for ``tool`` (per-tool override, else global)."""
        return self._timeout_overrides.get(tool, self._timeout_s)

    @staticmethod
    def _result_label(result: Any) -> str:
        if isinstance(result, dict) and result.get("status") == "error":
            return "error"
        return "ok"

    async def run(
        self,
        tool: str,
        call: Callable[[], Awaitable[Any]],
        *,
        ack_policy: ToolAckPolicy | None = None,
        speaker: Callable[[str], Awaitable[Any]] | None = None,
    ) -> Any:
        """Execute a tool call with filler ack, hard timeout and metrics.

        Never raises: timeouts and failures resolve to `apology_payload`.
        """
        registry = metrics.get_registry()
        ack_line = ack_policy.ack_for(tool) if ack_policy is not None else None

        ack_task: asyncio.Task | None = None
        ack_started = False
        if ack_line and speaker is not None:

            async def _delayed_ack() -> None:
                nonlocal ack_started
                await asyncio.sleep(self._grace_s)
                ack_started = True
                try:
                    await speaker(ack_line)
                except Exception as exc:  # noqa: BLE001 - filler is best-effort
                    log.warning("ack filler failed", tool=tool, error=str(exc)[:200])

            ack_task = asyncio.create_task(_delayed_ack())

        timeout_s = self.timeout_for(tool)
        started = time.perf_counter()
        try:
            result = await asyncio.wait_for(call(), timeout=timeout_s)
            registry.tool_calls.inc(tool=tool, result=self._result_label(result))
            return result
        except TimeoutError:
            registry.tool_calls.inc(tool=tool, result="timeout")
            log.warning(
                "tool call timed out",
                tool=tool,
                timeout_s=timeout_s,
            )
            return apology_payload("timeout")
        except Exception as exc:  # noqa: BLE001 - never raise into the pipeline
            registry.tool_calls.inc(tool=tool, result="error")
            log.warning("tool call failed", tool=tool, error=str(exc)[:200])
            return apology_payload("error")
        finally:
            elapsed = time.perf_counter() - started
            if ack_task is not None:
                if ack_started:
                    log.info("tool ack spoken", tool=tool, elapsed_ms=int(elapsed * 1000))
                else:
                    # Fast path: the tool answered inside the grace window —
                    # cancel the filler before any audio is scheduled.
                    ack_task.cancel()

    def wrap_dispatch(self, dispatch) -> Callable[[str, dict[str, Any]], Awaitable[Any]]:
        """Chat-path adapter: timeout + metrics around ToolLayer.dispatch.

        The ack itself is delivered as an SSE event by run_tool_loop_stream
        (ack_for_tool), so no speaker is involved here.
        """

        async def _wrapped(name: str, arguments: dict[str, Any]) -> Any:
            return await self.run(name, lambda: dispatch(name, arguments))

        return _wrapped
