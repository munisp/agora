import asyncio
import json

from app.call_activity import TOPIC, activity_message, announce_activity, tool_label


class _Info:
    def __init__(self, name):
        self.function_info = type("F", (), {"name": name})()


class _Called:
    def __init__(self, name):
        self.call_info = _Info(name)


class _Agent:
    def __init__(self):
        self.handlers = {}

    def on(self, event, fn):
        self.handlers.setdefault(event, []).append(fn)

    def emit(self, event, *args):
        for fn in self.handlers.get(event, []):
            fn(*args)


class _Participant:
    def __init__(self):
        self.sent = []

    async def publish_data(self, payload, *, reliable=True, topic=""):
        self.sent.append((topic, json.loads(payload)))


def test_labels():
    assert tool_label("get_availability") == "Checking availability"
    assert tool_label("something_new") == "Looking that up"
    assert json.loads(activity_message("listening")) == {"state": "listening"}


def test_tool_call_is_announced_as_working_then_listening():
    async def run():
        agent, room = _Agent(), type("R", (), {"local_participant": _Participant()})()
        announce_activity(agent, room)
        agent.emit("agent_started_speaking")
        agent.emit("function_calls_collected", [_Called("get_availability")])
        agent.emit("function_calls_finished", [])
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        return room.local_participant.sent

    sent = asyncio.run(run())
    assert sent == [
        (TOPIC, {"state": "speaking"}),
        (TOPIC, {"state": "working", "tool": "get_availability", "label": "Checking availability"}),
        (TOPIC, {"state": "listening"}),
    ]
