"""Avatar renderer sidecar (SPEC-W9 Part A / A3): open lip-sync path.

LiveKit worker that watches for active ``site-{slug}`` rooms, joins them as
the ``avatar-renderer`` participant, subscribes to the voice agent's audio
track, and publishes a generated VIDEO track back into the room — which the
web client renders as the warm avatar tile (apps/admin-web).

Frame generation sits behind the ``Renderer`` protocol:

- ``MockRenderer`` (default, ``AVATAR_RENDERER_MODE=mock``): publishes a
  solid warm-color test pattern at 15fps with a slow brightness pulse. No
  GPU, no model weights — the full pipeline (discovery -> join -> publish ->
  browser tile) is e2e-testable on any machine.
- ``MuseTalkRenderer`` (``AVATAR_RENDERER_MODE=musetalk``): DOCUMENTED STUB.
  Buffers the agent's audio frames, runs MuseTalk lip-sync inference, and
  emits RGB frames. Requires a CUDA GPU + MuseTalk weights — see the
  Dockerfile enablement block and docs/avatar.md (GPU honesty section).

Env:
    LIVEKIT_URL / LIVEKIT_API_KEY / LIVEKIT_API_SECRET  (required)
    AVATAR_RENDERER_MODE=mock|musetalk                  (default mock)
    AVATAR_ROOM_PREFIX=site-                            (rooms to join)
    AVATAR_DISCOVERY_INTERVAL_S=5                       (room polling)
    AVATAR_FRAME_WIDTH / AVATAR_FRAME_HEIGHT / AVATAR_FPS

LiveKit imports are lazy so the module imports (and py_compiles) without the
SDK installed — unit tooling can exercise the renderers standalone.
"""

from __future__ import annotations

import asyncio
import logging
import math
import os
from typing import Any, Protocol

log = logging.getLogger("avatar-renderer")

LIVEKIT_URL = os.environ.get("LIVEKIT_URL", "ws://livekit:7880")
LIVEKIT_API_KEY = os.environ.get("LIVEKIT_API_KEY", "devkey")
LIVEKIT_API_SECRET = os.environ.get("LIVEKIT_API_SECRET", "secret")
MODE = os.environ.get("AVATAR_RENDERER_MODE", "mock").strip().lower()
ROOM_PREFIX = os.environ.get("AVATAR_ROOM_PREFIX", "site-")
DISCOVERY_INTERVAL_S = float(os.environ.get("AVATAR_DISCOVERY_INTERVAL_S", "5"))

FRAME_WIDTH = int(os.environ.get("AVATAR_FRAME_WIDTH", "320"))
FRAME_HEIGHT = int(os.environ.get("AVATAR_FRAME_HEIGHT", "240"))
FPS = int(os.environ.get("AVATAR_FPS", "15"))

# Warm terracotta test pattern (matches the web tile's warm styling).
WARM_RGB = (0xB4, 0x6A, 0x4A)

RENDERER_IDENTITY = "avatar-renderer"


class Renderer(Protocol):
    """Audio-driven frame generator.

    ``next_frame`` returns one frame of RGB24 pixel bytes
    (FRAME_WIDTH * FRAME_HEIGHT * 3). ``push_audio`` receives the agent's
    audio frames (rtc.AudioFrame in production; opaque here) for lip-sync.
    """

    name: str

    async def start(self) -> None: ...

    async def next_frame(self) -> bytes: ...

    async def push_audio(self, frame: Any) -> None: ...

    async def stop(self) -> None: ...


class MockRenderer:
    """Solid warm-color test pattern at 15fps with a slow brightness pulse
    so consumers can tell a live stream from a frozen frame — no GPU."""

    name = "mock"

    def __init__(self, width: int = FRAME_WIDTH, height: int = FRAME_HEIGHT):
        self._width = width
        self._height = height
        self._tick = 0
        self.audio_frames_seen = 0

    async def start(self) -> None:
        log.info("mock renderer started", size=f"{self._width}x{self._height}")

    async def next_frame(self) -> bytes:
        self._tick += 1
        # 4-second pulse at 15fps (0.85..1.0 brightness) — visible liveness.
        pulse = 0.85 + 0.15 * (0.5 + 0.5 * math.sin(self._tick / (FPS * 4) * 2 * math.pi))
        r, g, b = (min(255, int(c * pulse)) for c in WARM_RGB)
        return bytes((r, g, b)) * (self._width * self._height)

    async def push_audio(self, frame: Any) -> None:
        self.audio_frames_seen += 1  # counted for diagnostics; pattern is static

    async def stop(self) -> None:
        log.info("mock renderer stopped", audio_frames=self.audio_frames_seen)


class MuseTalkRenderer:
    """DOCUMENTED STUB — real lip-sync inference path (requires GPU).

    Intended pipeline (see docs/avatar.md + Dockerfile enablement block):

        1. ``push_audio`` appends rtc.AudioFrame samples to a ring buffer
           (melspectrogram windows, as MuseTalk's audio featurizer expects).
        2. ``next_frame`` runs MuseTalk inference (whisper feature ->
           UNet -> face latent -> VAE decode) against the prepared avatar
           latent for the configured reference video, yielding one RGB
           frame per call at FPS.
        3. Frames flow into rtc.VideoSource in ``render_room`` unchanged.

    Until torch + MuseTalk weights are wired in, instantiation succeeds (so
    config errors surface at ``start``) but ``start`` raises with the exact
    enablement steps.
    """

    name = "musetalk"

    def __init__(self, width: int = FRAME_WIDTH, height: int = FRAME_HEIGHT):
        self._width = width
        self._height = height
        self._audio_buffer: list[Any] = []  # step 1: ring buffer of audio frames

    async def start(self) -> None:
        raise RuntimeError(
            "MuseTalkRenderer is a documented stub. Enable it per the "
            "Dockerfile 'MuseTalk enablement' block (CUDA base image, "
            "MuseTalk clone + weights) and docs/avatar.md, then implement "
            "inference in next_frame (audio buffer -> MuseTalk -> RGB frames)."
        )

    async def next_frame(self) -> bytes:
        raise NotImplementedError(
            "MuseTalk inference not wired: buffer audio in push_audio, run "
            "MuseTalk here, return RGB24 bytes."
        )

    async def push_audio(self, frame: Any) -> None:
        self._audio_buffer.append(frame)

    async def stop(self) -> None:
        self._audio_buffer.clear()


def build_renderer(mode: str) -> Renderer:
    if mode == "mock":
        return MockRenderer()
    if mode == "musetalk":
        return MuseTalkRenderer()
    raise ValueError(f"unknown AVATAR_RENDERER_MODE {mode!r} (mock|musetalk)")


def _renderer_token(room_name: str) -> str:
    """Mint a join token for the renderer participant (lazy livekit import)."""
    from livekit import api as lk_api

    return (
        lk_api.AccessToken(LIVEKIT_API_KEY, LIVEKIT_API_SECRET)
        .with_identity(RENDERER_IDENTITY)
        .with_name("Avatar")
        .with_grants(lk_api.VideoGrants(room_join=True, room=room_name))
        .to_jwt()
    )


async def render_room(room_name: str, mode: str) -> None:
    """Join ``room_name``, subscribe to the agent's audio, publish video."""
    from livekit import rtc

    renderer = build_renderer(mode)
    room = rtc.Room()
    try:
        await renderer.start()
        await room.connect(LIVEKIT_URL, _renderer_token(room_name))
        log.info("renderer joined room", room=room_name, mode=mode)

        source = rtc.VideoSource(FRAME_WIDTH, FRAME_HEIGHT)
        track = rtc.LocalVideoTrack.create_video_track("avatar", source)
        options = rtc.TrackPublishOptions(source=rtc.TrackSource.SOURCE_CAMERA)
        await room.local_participant.publish_track(track, options)

        @room.on("track_subscribed")
        def _on_track(track, publication, participant):  # noqa: ANN001
            if track.kind != rtc.TrackKind.KIND_AUDIO:
                return

            async def _pump() -> None:
                stream = rtc.AudioStream(track)
                async for event in stream:
                    await renderer.push_audio(event.frame)

            asyncio.create_task(_pump())

        frame_period = 1.0 / FPS
        while True:
            started = asyncio.get_event_loop().time()
            data = await renderer.next_frame()
            frame = rtc.VideoFrame(
                FRAME_WIDTH, FRAME_HEIGHT, rtc.VideoBufferType.RGB24, data
            )
            source.capture_frame(frame)
            elapsed = asyncio.get_event_loop().time() - started
            await asyncio.sleep(max(0.0, frame_period - elapsed))
    finally:
        await renderer.stop()
        await room.disconnect()
        log.info("renderer left room", room=room_name)


async def discover_rooms(livekit_api: Any) -> list[str]:
    """Active room names matching our prefix (renderer joins those)."""
    from livekit import api as lk_api

    resp = await livekit_api.room.list_rooms(lk_api.ListRoomsRequest())
    return sorted(r.name for r in resp.rooms if r.name.startswith(ROOM_PREFIX))


async def main() -> None:
    from livekit import api as lk_api

    logging.basicConfig(
        level=os.environ.get("LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    log.info("avatar renderer starting", mode=MODE, prefix=ROOM_PREFIX, url=LIVEKIT_URL)

    livekit_api = lk_api.LiveKitAPI(LIVEKIT_URL, LIVEKIT_API_KEY, LIVEKIT_API_SECRET)
    sessions: dict[str, asyncio.Task] = {}
    try:
        while True:
            try:
                active = set(await discover_rooms(livekit_api))
            except Exception as exc:  # noqa: BLE001 - discovery is best-effort
                log.warning("room discovery failed", error=str(exc))
                await asyncio.sleep(DISCOVERY_INTERVAL_S)
                continue
            for room_name in active - set(sessions):
                log.info("joining room", room=room_name)
                sessions[room_name] = asyncio.create_task(render_room(room_name, MODE))
            for room_name in set(sessions) - active:
                sessions[room_name].cancel()
                del sessions[room_name]
            # Reap crashed sessions so a retry happens on the next sweep.
            for room_name, task in list(sessions.items()):
                if task.done():
                    exc = task.exception()
                    if exc is not None:
                        log.warning("render session ended", room=room_name, error=str(exc))
                    del sessions[room_name]
            await asyncio.sleep(DISCOVERY_INTERVAL_S)
    finally:
        for task in sessions.values():
            task.cancel()
        await livekit_api.aclose()


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:  # pragma: no cover
        pass

