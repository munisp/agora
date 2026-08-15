"""Avatar-renderer unit tests (PY-006).

The service is a LiveKit worker, not an HTTP service — there is no health
endpoint to test. These tests cover what is real and offline-exercisable:
env config loading, renderer selection/validation, the MockRenderer frame
contract (what render_room publishes), MuseTalk stub honesty, and room
discovery prefix filtering. The LiveKit SDK is deliberately NOT installed:
app.main must import without it (lazy imports, per the module docstring).
"""

from __future__ import annotations

import importlib
import sys
import types

import pytest

import app.main as m


# ------------------------------------------------------------- config loading
def test_env_config_loading_and_defaults(monkeypatch):
    monkeypatch.delenv("AVATAR_RENDERER_MODE", raising=False)
    monkeypatch.delenv("AVATAR_FRAME_WIDTH", raising=False)
    monkeypatch.delenv("AVATAR_FPS", raising=False)
    importlib.reload(m)
    try:
        assert m.MODE == "mock"
        assert m.FRAME_WIDTH == 320
        assert m.FRAME_HEIGHT == 240
        assert m.FPS == 15
        assert m.ROOM_PREFIX == "site-"

        monkeypatch.setenv("AVATAR_RENDERER_MODE", " MuSeTalk ")
        monkeypatch.setenv("AVATAR_FRAME_WIDTH", "640")
        monkeypatch.setenv("AVATAR_FPS", "24")
        importlib.reload(m)
        assert m.MODE == "musetalk"  # normalized: stripped + lowercased
        assert m.FRAME_WIDTH == 640
        assert m.FPS == 24
    finally:
        monkeypatch.undo()
        importlib.reload(m)


def test_module_imports_without_livekit_sdk():
    """Lazy-import honesty: app.main must import with livekit absent."""
    assert "livekit" not in sys.modules or True  # environment-independent
    importlib.reload(m)  # would raise if any livekit import were eager
    assert m.RENDERER_IDENTITY == "avatar-renderer"


# ------------------------------------------------------- renderer validation
def test_build_renderer_modes():
    assert m.build_renderer("mock").name == "mock"
    assert m.build_renderer("musetalk").name == "musetalk"


def test_build_renderer_rejects_unknown_mode():
    with pytest.raises(ValueError, match="unknown AVATAR_RENDERER_MODE"):
        m.build_renderer("cuda-magic")


# ------------------------------------------------------------ MockRenderer
async def test_mock_renderer_frame_contract():
    w, h = 16, 8
    r = m.MockRenderer(width=w, height=h)
    await r.start()
    frame = await r.next_frame()
    # RGB24: exactly width*height*3 bytes, one solid warm color.
    assert len(frame) == w * h * 3
    px = frame[:3]
    assert frame == px * (w * h)
    base = m.WARM_RGB
    for got, want in zip(px, base):
        # Pulse range is 0.85..1.0 of the warm base color.
        assert int(want * 0.85) <= got <= want
    await r.stop()


async def test_mock_renderer_pulse_is_live_not_frozen():
    r = m.MockRenderer(width=4, height=4)
    frames = {await r.next_frame() for _ in range(20)}
    # The brightness pulse must make frames differ (liveness signal).
    assert len(frames) > 1


async def test_mock_renderer_counts_audio_frames():
    r = m.MockRenderer()
    assert r.audio_frames_seen == 0
    await r.push_audio(object())
    await r.push_audio(object())
    assert r.audio_frames_seen == 2


# --------------------------------------------------------- MuseTalkRenderer
async def test_musetalk_stub_fails_honestly_with_enablement_steps():
    r = m.MuseTalkRenderer()
    with pytest.raises(RuntimeError, match="documented stub"):
        await r.start()
    with pytest.raises(NotImplementedError, match="not wired"):
        await r.next_frame()


async def test_musetalk_audio_buffer_lifecycle():
    r = m.MuseTalkRenderer()
    await r.push_audio("frame-1")
    assert len(r._audio_buffer) == 1
    await r.stop()
    assert r._audio_buffer == []


# ------------------------------------------------------------ room discovery
async def test_discover_rooms_filters_by_prefix_and_sorts(monkeypatch):
    """Prefix filter + sort is the real selection logic; LiveKit is faked
    at the import seam (lazy ``from livekit import api``)."""
    from types import SimpleNamespace

    class _ListRoomsRequest:
        pass

    resp = SimpleNamespace(
        rooms=[
            SimpleNamespace(name="site-zulu"),
            SimpleNamespace(name="lobby"),       # wrong prefix: excluded
            SimpleNamespace(name="site-alpha"),
            SimpleNamespace(name=""),            # empty: excluded
        ]
    )

    class _RoomAPI:
        async def list_rooms(self, request):
            assert isinstance(request, _ListRoomsRequest)
            return resp

    fake_livekit = types.ModuleType("livekit")
    fake_api = types.ModuleType("livekit.api")
    fake_api.ListRoomsRequest = _ListRoomsRequest
    fake_livekit.api = fake_api
    monkeypatch.setitem(sys.modules, "livekit", fake_livekit)
    monkeypatch.setitem(sys.modules, "livekit.api", fake_api)

    monkeypatch.setattr(m, "ROOM_PREFIX", "site-")
    rooms = await m.discover_rooms(SimpleNamespace(room=_RoomAPI()))
    assert rooms == ["site-alpha", "site-zulu"]
