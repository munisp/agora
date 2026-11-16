"""What the receptionist is doing right now, told to the caller's screen during a voice call.

The browser shows a person on camera: talking while the receptionist speaks, typing while it uses a tool (checking
availability, looking up a booking), listening otherwise. Speaking can be read from the audio, but tool use is silent,
so the worker publishes small JSON messages on the room's data channel, topic "agora.activity":

    {"state": "thinking"}       the caller finished speaking; the receptionist is working out the answer
    {"state": "working", "tool": "get_availability", "label": "Checking availability"}
    {"state": "speaking"} / {"state": "listening"}

Best effort: a failed publish never affects the call.
"""
from __future__ import annotations

import asyncio
import json
from typing import Any

from .logging import get_logger

log = get_logger("call_activity")

TOPIC = "agora.activity"

_LABELS = {
    "get_availability": "Checking availability",
    "book_appointment": "Making the booking",
    "reschedule_appointment": "Moving the booking",
    "cancel_appointment": "Cancelling the booking",
    "lookup_appointment": "Looking up the booking",
    "get_business_info": "Looking that up",
    "request_verification_code": "Sending a code",
    "verify_caller_code": "Checking the code",
    "request_human": "Fetching a colleague",
    "capture_location": "Noting the location",
    "prefill_booking": "Filling in the form",
    "navigate_to_page": "Opening the page",
    "highlight_element": "Pointing it out",
}


def tool_label(name: str | None) -> str:
    return _LABELS.get(name or "", "Looking that up")


def activity_message(state: str, tool: str | None = None) -> bytes:
    body: dict[str, Any] = {"state": state}
    if tool is not None:
        body["tool"] = tool
        body["label"] = tool_label(tool)
    return json.dumps(body).encode()


def _first_tool_name(called: Any) -> str | None:
    for fn in called or []:
        info = getattr(fn, "call_info", fn)
        name = getattr(getattr(info, "function_info", None), "name", None) or getattr(info, "name", None)
        if name:
            return str(name)
    return None


def announce_activity(agent: Any, room: Any) -> None:
    """Hook the agent's events (livekit-agents 0.10) and publish each state change to the room."""

    def _send(payload: bytes) -> None:
        async def _run() -> None:
            try:
                await room.local_participant.publish_data(payload, reliable=True, topic=TOPIC)
            except Exception as exc:  # noqa: BLE001 - the screen's animation is cosmetic
                log.warning("activity not published", error=str(exc)[:200])

        try:
            asyncio.get_running_loop().create_task(_run())
        except RuntimeError:
            return

    def _collected(called, *_a, **_k) -> None:
        _send(activity_message("working", _first_tool_name(called) or "tool"))

    try:
        # The caller stopped talking (VAD): she starts working on it. user_speech_committed alone is too late — 0.10
        # commits a tool-less question only when the reply starts playing. Talking again puts her back to listening.
        agent.on("user_stopped_speaking", lambda *_a, **_k: _send(activity_message("thinking")))
        agent.on("user_started_speaking", lambda *_a, **_k: _send(activity_message("listening")))
        agent.on("function_calls_collected", _collected)
        # Back to thinking: the model now reads the tool's answer before replying.
        agent.on("function_calls_finished", lambda *_a, **_k: _send(activity_message("thinking")))
        agent.on("agent_started_speaking", lambda *_a, **_k: _send(activity_message("speaking")))
        agent.on("agent_stopped_speaking", lambda *_a, **_k: _send(activity_message("listening")))
        llm = getattr(agent, "_llm", None)
        if hasattr(llm, "on_extra_tools"):  # app/lk_tool_loop.py: lookups made while answering
            llm.on_extra_tools(collected=_collected, finished=lambda *_a, **_k: _send(activity_message("thinking")))
    except Exception as exc:  # noqa: BLE001
        log.warning("call activity hooks unavailable", error=str(exc)[:200])


def announce_language(agent: Any, room: Any) -> None:
    """Tell the caller's screen when the call switches language: {"state": "language", "language": "yo"}."""
    listeners = getattr(agent, "agora_language_listeners", None)
    if listeners is None:
        return

    def _publish(code: str) -> None:
        payload = json.dumps({"state": "language", "language": code}).encode()

        async def _run() -> None:
            try:
                await room.local_participant.publish_data(payload, reliable=True, topic=TOPIC)
            except Exception as exc:  # noqa: BLE001
                log.warning("language not published", error=str(exc)[:200])

        try:
            asyncio.get_running_loop().create_task(_run())
        except RuntimeError:
            return

    listeners.append(_publish)
