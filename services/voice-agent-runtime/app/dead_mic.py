"""SPEC-W73 A1: dead-mic detection (per-session asyncio watchdog).

A caller whose microphone is broken/muted hears the greeting and then dead
air: the VAD never fires, no turn ever commits, and the session sits open
until the caller gives up. DeadMicMonitor watches for user speech activity
and intervenes:

- no user speech within ``first_prompt_s`` of the monitor start (the
  greeting has been queued by then) -> a short "check your microphone"
  re-prompt is spoken via ``agent.say`` (``voice_dead_mic_prompts_total``);
- still no speech within ``second_prompt_s`` of the re-prompt -> a brief
  polite closer is spoken and, when hangup is enabled, the session is ended
  through the room-disconnect path (``voice_dead_mic_hangups_total``);
- ANY user speech activity resets the watchdog to zero
  (``voice_dead_mic_resets_total``) and the cycle restarts.

User-speech signal: the LiveKit ``VoicePipelineAgent`` events
``user_started_speaking`` (and ``user_speech_committed``, belt-and-braces),
both verified against the livekit-agents 0.10.2 wheel
(``livekit/agents/pipeline/pipeline_agent.py`` ``EventTypes`` Literal,
emitted by ``VoicePipelineAgent._on_vad_started`` /
``AgentPlayout`` commit paths). Session end / escalation / emergency
transfer cancel the monitor: no orphan tasks, no prompts after escalation,
no interference with the existing dead-air/apology paths.

The spoken lines are deterministic per-tenant CONSTANTS (same text every
session), so they land in the TTS LRU once and every re-prompt is a cache
hit.
"""

from __future__ import annotations

import asyncio
import inspect
from typing import Any, Awaitable, Callable

from . import metrics
from .logging import get_logger

log = get_logger("dead-mic")

#: Spoken when no user speech is observed within the first window.
DEAD_MIC_PROMPT = "I can't seem to hear you — can you check your microphone?"

#: Spoken when silence continues past the second window, right before the
#: hangup (or as the final word when hangup is disabled).
DEAD_MIC_CLOSER = (
    "I still can't hear you, so I'll end this call for now. "
    "Please call us back when your microphone is working. Goodbye."
)


class DeadMicMonitor:
    """Asyncio watchdog for one voice session.

    Parameters are plain values (not the Settings object) so the monitor is
    offline-testable with a fake agent. ``on_hangup`` is the session-end
    callback (the worker passes ``ctx.room.disconnect`` — verified on the
    livekit 0.17.x ``rtc.Room``); ``is_escalated`` is an optional predicate
    (the worker passes an escalation-room check) that suppresses every
    further utterance once a human handoff is in flight.
    """

    def __init__(
        self,
        *,
        agent: Any,
        first_prompt_s: float = 12.0,
        second_prompt_s: float = 10.0,
        hangup_enabled: bool = True,
        prompt_text: str = DEAD_MIC_PROMPT,
        closer_text: str = DEAD_MIC_CLOSER,
        on_hangup: Callable[[], Awaitable[None]] | None = None,
        is_escalated: Callable[[], bool] | None = None,
        registry: Any | None = None,
    ) -> None:
        self._agent = agent
        self._first_prompt_s = max(0.0, float(first_prompt_s))
        self._second_prompt_s = max(0.0, float(second_prompt_s))
        self._hangup_enabled = hangup_enabled
        self._prompt_text = prompt_text
        self._closer_text = closer_text
        self._on_hangup = on_hangup
        self._is_escalated = is_escalated
        self._registry = registry or metrics.get_registry()
        self._speech: asyncio.Event | None = None
        self._task: asyncio.Task | None = None

    # ------------------------------------------------------------ lifecycle
    @property
    def task(self) -> asyncio.Task | None:
        """The watchdog task (test/introspection seam)."""
        return self._task

    def start(self) -> bool:
        """Spawn the watchdog task. Idempotent; False without a running loop."""
        if self._task is not None:
            return True
        try:
            self._speech = asyncio.Event()
            self._task = asyncio.create_task(self._run(), name="dead-mic-monitor")
        except RuntimeError as exc:  # no running event loop
            log.warning("dead-mic monitor not started", error=str(exc)[:200])
            self._speech = None
            self._task = None
            return False
        return True

    def cancel(self) -> None:
        """Cancel the watchdog (session end / escalation / transfer).

        Sync and idempotent: safe from the room ``disconnected`` callback,
        which is a sync event handler."""
        task, self._task = self._task, None
        if task is not None and not task.done():
            task.cancel()

    async def aclose(self) -> None:
        """Cancel the watchdog and await its termination (no orphan task)."""
        task, self._task = self._task, None
        if task is None:
            return
        if not task.done():
            task.cancel()
        try:
            await task
        except asyncio.CancelledError:
            pass

    # ------------------------------------------------------------- signals
    def note_user_speech(self, *_args: Any, **_kwargs: Any) -> None:
        """User-speech event callback (``user_started_speaking`` /
        ``user_speech_committed``). ANY activity resets the watchdog."""
        if self._speech is not None:
            self._speech.set()

    # -------------------------------------------------------------- internals
    def _suppressed(self) -> bool:
        """True when a handoff/escalation is in flight — stay silent."""
        if self._is_escalated is None:
            return False
        try:
            return bool(self._is_escalated())
        except Exception:  # noqa: BLE001 - predicate failure must not break
            return False

    async def _wait_for_speech(self, timeout_s: float) -> bool:
        """Wait up to ``timeout_s`` for user speech; consume and report the
        reset when it arrives."""
        assert self._speech is not None
        try:
            await asyncio.wait_for(self._speech.wait(), timeout_s)
        except asyncio.TimeoutError:
            return False
        self._speech.clear()
        self._registry.dead_mic_resets.inc()
        return True

    async def _say(self, text: str, *, stage: str) -> None:
        if self._suppressed():
            return
        say = getattr(self._agent, "say", None)
        if say is None:
            return
        try:
            result = say(text, allow_interruptions=True)
            if inspect.isawaitable(result):
                await result
        except Exception as exc:  # noqa: BLE001 - never break the call
            log.warning("dead-mic utterance failed", stage=stage, error=str(exc)[:200])
            return
        self._registry.dead_mic_prompts.inc(stage=stage)

    async def _hangup(self) -> None:
        self._registry.dead_mic_hangups.inc()
        if self._on_hangup is None:
            return
        try:
            result = self._on_hangup()
            if inspect.isawaitable(result):
                await result
        except Exception as exc:  # noqa: BLE001 - hangup failure only logs
            log.warning("dead-mic hangup failed", error=str(exc)[:200])

    async def _run(self) -> None:
        try:
            while True:
                if await self._wait_for_speech(self._first_prompt_s):
                    continue  # speech reset the watchdog to zero
                await self._say(self._prompt_text, stage="first")
                if self._suppressed():
                    return  # handoff in flight: no closer, no hangup
                if await self._wait_for_speech(self._second_prompt_s):
                    continue
                await self._say(self._closer_text, stage="closer")
                if self._suppressed():
                    return
                if self._hangup_enabled:
                    await self._hangup()
                # With hangup disabled the closer is the last word: looping
                # would re-prompt a caller we already said goodbye to.
                return
        except asyncio.CancelledError:
            raise
        except Exception as exc:  # noqa: BLE001 - watchdog dies quietly
            log.warning("dead-mic monitor stopped on error", error=str(exc)[:200])
