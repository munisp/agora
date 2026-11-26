"""SPEC-W73 A1 tests: dead-mic detection (app/dead_mic.py + worker wiring).

Redirection proofs (a)-(e) from SPEC-W73 A4, all offline-deterministic with
a fake agent/session/room — no LiveKit server, no models, no network:

(a) silence past FIRST_PROMPT_S -> exactly one re-prompt spoken;
(b) user speech before the deadline -> no prompt, reset counter incremented;
(c) continued silence -> closer + hangup path invoked + hangup metric;
(d) VOICE_DEAD_MIC_ENABLED=0 -> complete no-op (no monitor, no listeners,
    no task);
(e) session end cancels the pending monitor — no orphan task (asyncio task
    introspection);
plus: escalation suppresses every dead-mic utterance, env parsing, and the
metric exposition.
"""

from __future__ import annotations

import asyncio

import pytest

from app import metrics
from app.config import Settings, load_settings
from app.dead_mic import DEAD_MIC_CLOSER, DEAD_MIC_PROMPT, DeadMicMonitor

try:  # pragma: no cover - import shim (real SDK present in the worker image)
    import app.livekit_worker as lw
except ModuleNotFoundError:  # pragma: no cover - import shim
    import test_live_tts_chain  # noqa: F401 - installs the livekit stubs

    import app.livekit_worker as lw


@pytest.fixture(autouse=True)
def _clean_registry():
    metrics.reset_registry()
    yield
    metrics.reset_registry()


class FakeAgent:
    """Minimal VoicePipelineAgent stand-in: records say() calls and
    registered event listeners (the real emitter supports multiple handlers
    per event; the monitor registers on user_started_speaking AND
    user_speech_committed — both verified 0.10.2 EventTypes)."""

    def __init__(self) -> None:
        self.spoken: list[str] = []
        self.listeners: dict[str, list] = {}

    async def say(self, text, allow_interruptions=False):  # noqa: ARG002
        self.spoken.append(text)

    def on(self, event, cb=None):
        self.listeners.setdefault(event, []).append(cb)


class FakeRoom:
    def __init__(self) -> None:
        self.disconnected = False

    async def disconnect(self) -> None:
        self.disconnected = True


class FakeSession:
    escalation_room = None


def _rendered() -> str:
    return metrics.render()


# ---------------------------------------------------------------------------
# (a) silence past FIRST_PROMPT_S -> exactly one re-prompt spoken
# ---------------------------------------------------------------------------
async def test_a_silence_past_first_window_speaks_exactly_one_prompt():
    agent = FakeAgent()
    monitor = DeadMicMonitor(
        agent=agent,
        first_prompt_s=0.05,
        second_prompt_s=60.0,  # park in the second window for the assertion
        hangup_enabled=True,
    )
    assert monitor.start()
    await asyncio.sleep(0.2)
    assert agent.spoken == [DEAD_MIC_PROMPT]  # exactly one prompt
    assert 'voice_dead_mic_prompts_total{stage="first"} 1' in _rendered()
    # no closer / no hangup samples (HELP/TYPE header lines always render)
    assert 'voice_dead_mic_prompts_total{stage="closer"}' not in _rendered()
    assert "\nvoice_dead_mic_hangups_total " not in "\n" + _rendered()
    await monitor.aclose()


# ---------------------------------------------------------------------------
# (b) user speech before the deadline -> no prompt, reset counter incremented
# ---------------------------------------------------------------------------
async def test_b_user_speech_resets_monitor_no_prompt():
    agent = FakeAgent()
    monitor = DeadMicMonitor(
        agent=agent, first_prompt_s=0.1, second_prompt_s=0.1, hangup_enabled=True
    )
    monitor.start()
    await asyncio.sleep(0.03)
    monitor.note_user_speech()  # user_started_speaking callback shape
    await asyncio.sleep(0.06)  # inside the restarted first window
    assert agent.spoken == []
    assert "voice_dead_mic_resets_total 1" in _rendered()
    # Speech again inside the restarted window: second reset, still silent.
    monitor.note_user_speech()
    await asyncio.sleep(0.05)
    assert agent.spoken == []
    assert "voice_dead_mic_resets_total 2" in _rendered()
    await monitor.aclose()


# ---------------------------------------------------------------------------
# (c) continued silence -> closer + hangup path invoked + hangup metric
# ---------------------------------------------------------------------------
async def test_c_continued_silence_closes_and_hangs_up():
    agent = FakeAgent()
    room = FakeRoom()
    monitor = DeadMicMonitor(
        agent=agent,
        first_prompt_s=0.03,
        second_prompt_s=0.03,
        hangup_enabled=True,
        on_hangup=room.disconnect,
    )
    monitor.start()
    task = monitor.task
    await asyncio.wait_for(asyncio.shield(task), timeout=2.0)
    assert agent.spoken == [DEAD_MIC_PROMPT, DEAD_MIC_CLOSER]
    assert room.disconnected  # hangup path invoked (room.disconnect)
    rendered = _rendered()
    assert 'voice_dead_mic_prompts_total{stage="first"} 1' in rendered
    assert 'voice_dead_mic_prompts_total{stage="closer"} 1' in rendered
    assert "voice_dead_mic_hangups_total 1" in rendered


async def test_c_hangup_disabled_speaks_closer_but_ends_no_session():
    agent = FakeAgent()
    room = FakeRoom()
    monitor = DeadMicMonitor(
        agent=agent,
        first_prompt_s=0.03,
        second_prompt_s=0.03,
        hangup_enabled=False,
        on_hangup=room.disconnect,
    )
    monitor.start()
    await asyncio.wait_for(asyncio.shield(monitor.task), timeout=2.0)
    assert agent.spoken == [DEAD_MIC_PROMPT, DEAD_MIC_CLOSER]
    assert not room.disconnected
    assert "voice_dead_mic_hangups_total 1" not in _rendered()


# ---------------------------------------------------------------------------
# (d) ENABLED=0 -> complete no-op
# ---------------------------------------------------------------------------
async def test_d_disabled_is_complete_noop():
    agent = FakeAgent()
    room = FakeRoom()
    session = FakeSession()
    settings = Settings(voice_dead_mic_enabled=False)
    before = set(asyncio.all_tasks())
    monitor = lw.start_dead_mic_monitor(settings, agent, session, room)
    assert monitor is None
    assert agent.listeners == {}  # no event listeners registered
    assert agent.spoken == []
    assert not room.disconnected
    assert set(asyncio.all_tasks()) == before  # no watchdog task spawned


async def test_d_enabled_wires_verified_events_and_starts():
    agent = FakeAgent()
    room = FakeRoom()
    session = FakeSession()
    settings = Settings(
        voice_dead_mic_enabled=True,
        voice_dead_mic_first_prompt_s=60.0,
        voice_dead_mic_second_prompt_s=60.0,
    )
    monitor = lw.start_dead_mic_monitor(settings, agent, session, room)
    assert monitor is not None
    # Wheel-verified 0.10.2 event names, both registered.
    assert agent.listeners["user_started_speaking"] == [monitor.note_user_speech]
    assert agent.listeners["user_speech_committed"] == [monitor.note_user_speech]
    assert monitor.task is not None and not monitor.task.done()
    # An emitted user_started_speaking reaches the monitor.
    agent.listeners["user_started_speaking"][0]()
    await monitor.aclose()


# ---------------------------------------------------------------------------
# (e) session end cancels the pending monitor (no orphan task)
# ---------------------------------------------------------------------------
async def test_e_session_end_cancels_monitor_no_orphan_task():
    agent = FakeAgent()
    monitor = DeadMicMonitor(agent=agent, first_prompt_s=60.0)
    monitor.start()
    task = monitor.task
    assert task in asyncio.all_tasks()
    monitor.cancel()  # the room `disconnected` path (sync callback)
    for _ in range(20):
        if task.done():
            break
        await asyncio.sleep(0.01)
    assert task.done() and task.cancelled()
    assert task not in asyncio.all_tasks()
    assert agent.spoken == []  # no prompt after session end


async def test_e_aclose_awaits_termination():
    agent = FakeAgent()
    monitor = DeadMicMonitor(agent=agent, first_prompt_s=60.0)
    monitor.start()
    task = monitor.task
    await monitor.aclose()
    assert task.done()
    assert task not in asyncio.all_tasks()
    assert monitor.task is None


# ---------------------------------------------------------------------------
# Escalation / handoff: no prompts after escalation
# ---------------------------------------------------------------------------
async def test_escalation_suppresses_prompts():
    agent = FakeAgent()
    session = FakeSession()
    monitor = DeadMicMonitor(
        agent=agent,
        first_prompt_s=0.03,
        second_prompt_s=0.03,
        hangup_enabled=True,
        on_hangup=FakeRoom().disconnect,
        is_escalated=lambda: session.escalation_room is not None,
    )
    monitor.start()
    session.escalation_room = "esc-123"  # warm handoff in flight
    await asyncio.sleep(0.15)
    assert agent.spoken == []
    assert 'voice_dead_mic_prompts_total{stage=' not in _rendered()
    await monitor.aclose()


async def test_speech_during_second_window_resets_instead_of_hangup():
    agent = FakeAgent()
    room = FakeRoom()
    monitor = DeadMicMonitor(
        agent=agent,
        first_prompt_s=0.05,
        second_prompt_s=0.3,
        hangup_enabled=True,
        on_hangup=room.disconnect,
    )
    monitor.start()
    await asyncio.sleep(0.12)  # first prompt fired at ~0.05
    assert agent.spoken == [DEAD_MIC_PROMPT]
    monitor.note_user_speech()  # caller's mic wakes up mid-second-window
    await asyncio.sleep(0.03)  # inside the restarted first window (< 0.05)
    assert agent.spoken == [DEAD_MIC_PROMPT]  # no closer
    assert not room.disconnected
    assert "voice_dead_mic_resets_total 1" in _rendered()
    await monitor.aclose()


# ---------------------------------------------------------------------------
# Env parsing (A3)
# ---------------------------------------------------------------------------
def test_env_parsing(monkeypatch):
    for key in (
        "VOICE_DEAD_MIC_ENABLED",
        "VOICE_DEAD_MIC_FIRST_PROMPT_S",
        "VOICE_DEAD_MIC_SECOND_PROMPT_S",
        "VOICE_DEAD_MIC_HANGUP_ENABLED",
    ):
        monkeypatch.delenv(key, raising=False)
    defaults = load_settings()
    assert defaults.voice_dead_mic_enabled is True
    assert defaults.voice_dead_mic_first_prompt_s == 12.0
    assert defaults.voice_dead_mic_second_prompt_s == 10.0
    assert defaults.voice_dead_mic_hangup_enabled is True

    monkeypatch.setenv("VOICE_DEAD_MIC_ENABLED", "0")
    monkeypatch.setenv("VOICE_DEAD_MIC_FIRST_PROMPT_S", "8.5")
    monkeypatch.setenv("VOICE_DEAD_MIC_SECOND_PROMPT_S", "6")
    monkeypatch.setenv("VOICE_DEAD_MIC_HANGUP_ENABLED", "false")
    tuned = load_settings()
    assert tuned.voice_dead_mic_enabled is False
    assert tuned.voice_dead_mic_first_prompt_s == 8.5
    assert tuned.voice_dead_mic_second_prompt_s == 6.0
    assert tuned.voice_dead_mic_hangup_enabled is False

    # A typo'd float must not crash boot (W70 A1 posture).
    monkeypatch.setenv("VOICE_DEAD_MIC_FIRST_PROMPT_S", "soon-ish")
    assert load_settings().voice_dead_mic_first_prompt_s == 12.0


async def test_receptionist_activity_also_resets_the_watchdog():
    """Silence only counts after the receptionist has finished (replies and lookups take longer than the window)."""
    agent = FakeAgent()
    settings = Settings(voice_dead_mic_enabled=True, voice_dead_mic_first_prompt_s=60.0, voice_dead_mic_second_prompt_s=60.0)
    monitor = lw.start_dead_mic_monitor(settings, agent, FakeSession(), FakeRoom())
    for event in ("agent_started_speaking", "agent_stopped_speaking", "function_calls_collected", "function_calls_finished"):
        assert agent.listeners[event] == [monitor.note_user_speech]
    await monitor.aclose()
