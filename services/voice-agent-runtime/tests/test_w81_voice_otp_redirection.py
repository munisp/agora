"""SPEC-W81 A1 / G4 redirection proof: the OTP verification tools are
registered and ai_callable on the LiveKit voice path.

This file deliberately imports ONLY symbols that already exist on the base
tree, so the tests FAIL on base (wrappers absent from ReceptionistFunctions,
tools missing from SLOW_TOOLS) and PASS on the candidate — a clean
redirection signal rather than a collection error.

A1: request_verification_code / verify_caller_code are exposed as
ReceptionistFunctions ai_callable wrappers delegating to the existing
ToolLayer methods (the voice system prompt already references them).
"""

from __future__ import annotations

import inspect
import json
import re

from app.async_tools import SLOW_TOOLS

try:  # pragma: no cover - import shim (real SDK present in the worker image)
    import app.livekit_worker as lw
except ModuleNotFoundError:  # pragma: no cover - import shim
    import test_live_tts_chain  # noqa: F401 - installs the livekit stubs

    import app.livekit_worker as lw

OTP_TOOLS = ("request_verification_code", "verify_caller_code")


def test_otp_tools_registered_and_ai_callable():
    """G4 core proof: both OTP tools are ReceptionistFunctions methods,
    async, and decorated with @lk_llm.ai_callable."""
    fns = lw.ReceptionistFunctions
    src = inspect.getsource(fns)
    for name in OTP_TOOLS:
        method = getattr(fns, name, None)
        assert method is not None, f"{name} missing from ReceptionistFunctions"
        assert inspect.iscoroutinefunction(method), f"{name} must be async"
        pattern = r"@lk_llm\.ai_callable\(.*?\)\s*\n\s*async def\s+" + name + r"\("
        assert re.search(pattern, src, re.S), f"{name} is not ai_callable"


def test_otp_tools_are_slow_tools():
    """Both OTP tools do portal HTTP round-trips -> SLOW_TOOLS membership."""
    for name in OTP_TOOLS:
        assert name in SLOW_TOOLS


async def test_wrappers_delegate_to_tool_layer():
    """The voice-path wrappers call the SAME ToolLayer methods and return
    the JSON-serialized result to the model."""

    class _RecordingTools:
        def __init__(self) -> None:
            self.calls: list[tuple] = []

        async def request_verification_code(self, phone: str):
            self.calls.append(("request_verification_code", phone))
            return {"status": "code_sent"}

        async def verify_caller_code(self, phone: str, code: str):
            self.calls.append(("verify_caller_code", phone, code))
            return {"status": "verified"}

    tools = _RecordingTools()
    fns = lw.ReceptionistFunctions(tools)  # type: ignore[arg-type]
    out1 = json.loads(await fns.request_verification_code(phone="+2348035550101"))
    out2 = json.loads(
        await fns.verify_caller_code(phone="+2348035550101", code="123456")
    )
    assert out1 == {"status": "code_sent"}
    assert out2 == {"status": "verified"}
    assert tools.calls == [
        ("request_verification_code", "+2348035550101"),
        ("verify_caller_code", "+2348035550101", "123456"),
    ]
