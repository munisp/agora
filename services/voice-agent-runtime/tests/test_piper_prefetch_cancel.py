"""SPEC-W75 A3 tests: PiperTTS.stream_pcm prefetch cancel-race coverage.

Mirrors the FallbackTTS W74 cancel-race coverage for the piper stage: the
early-exit path must never leak a pending prefetch task, and a prefetch
coroutine that raises while its cancellation is in flight must have that
exception retrieved so asyncio never logs "Task exception was never
retrieved" (SPEC-W74 A2 pattern, ported to PiperTTS by SPEC-W75 A1).

Offline: ``PiperTTS.synthesize_pcm`` is overridden per-instance with a fake
synth (same pattern as tests/test_w71_perf.py), so no piper binary or HTTP
sidecar is needed.
"""

from __future__ import annotations

import asyncio
import gc
import time

from app.pipeline.tts import PiperTTS


def _piper_with_fake_synth(synth) -> PiperTTS:
    piper = PiperTTS(mode="http")
    piper.synthesize_pcm = synth  # instance-level override
    return piper


def _no_leaked_tasks() -> list[asyncio.Task]:
    return [
        t
        for t in asyncio.all_tasks()
        if t is not asyncio.current_task() and not t.done()
    ]


class _HandlerCapture:
    """Install a loop exception-handler capture; restore the old one on exit."""

    def __init__(self) -> None:
        self.contexts: list[dict] = []

    def __enter__(self) -> "_HandlerCapture":
        self._loop = asyncio.get_running_loop()
        self._old = self._loop.get_exception_handler()
        self._loop.set_exception_handler(lambda l, ctx: self.contexts.append(ctx))
        return self

    def __exit__(self, *exc) -> bool:
        self._loop.set_exception_handler(self._old)
        return False


async def _drain(turns: int = 10) -> None:
    """Fixed loop turns + gc so pending-task finalization and any
    unretrieved-exception callbacks fire NOW (no wall-clock guesses)."""
    for _ in range(turns):
        await asyncio.sleep(0)
        gc.collect()


# ---------------------------------------------------------------------------
# (a) early close with a PENDING prefetch: cancelled, no leaks, no reports
# ---------------------------------------------------------------------------
async def test_a_close_cancels_pending_prefetch_no_leaks_no_reports():
    started = asyncio.Event()
    release = asyncio.Event()
    cancelled: list[str] = []

    async def synth(text: str) -> bytes:
        if text == "Two.":
            started.set()
            try:
                await release.wait()  # hangs unless the prefetch is cancelled
            except asyncio.CancelledError:
                cancelled.append(text)
                raise
        return text.encode()

    with _HandlerCapture() as cap:
        piper = _piper_with_fake_synth(synth)
        agen = piper.stream_pcm("One. Two. Three.")
        assert await agen.__anext__() == b"One."
        # Depth-1 prefetch of "Two." is in flight while we hold chunk 1.
        await asyncio.wait_for(started.wait(), 5)
        await agen.aclose()  # early exit: generator close
        release.set()  # would hang forever if not cancelled
        await _drain()
        assert cancelled == ["Two."]  # CancelledError really delivered
        assert cap.contexts == [], f"asyncio reported: {cap.contexts!r}"
        assert _no_leaked_tasks() == []
        await piper.aclose()


# ---------------------------------------------------------------------------
# (b) prefetch raising AROUND cancellation: retrieved, zero handler reports
# ---------------------------------------------------------------------------
async def test_b_prefetch_error_racing_cancel_is_retrieved():
    """cancel() is only a request: a prefetch coroutine that converts the
    delivered CancelledError into its own failure finishes NOT cancelled —
    the early-exit path must still retrieve that exception via its done
    callback (the W74 cancel-race corner, now covered for PiperTTS)."""
    started = asyncio.Event()

    async def synth(text: str) -> bytes:
        if text == "Two.":
            started.set()
            try:
                await asyncio.Event().wait()  # pending until cancelled
            except asyncio.CancelledError:
                # Provider-side failure during teardown (e.g. transport
                # close raising): the task ends with THIS, not cancelled.
                raise RuntimeError("boom:Two.") from None
        return text.encode()

    with _HandlerCapture() as cap:
        piper = _piper_with_fake_synth(synth)
        agen = piper.stream_pcm("One. Two. Three.")
        assert await agen.__anext__() == b"One."
        await asyncio.wait_for(started.wait(), 5)  # prefetch still pending
        await agen.aclose()  # cancels the pending prefetch
        del agen  # drop the last frame reference to the prefetch task
        await _drain()
        assert cap.contexts == [], f"asyncio reported: {cap.contexts!r}"
        assert _no_leaked_tasks() == []
        await piper.aclose()


# ---------------------------------------------------------------------------
# (c) 200-iteration cancel/raise race stress (bounded wall-clock)
# ---------------------------------------------------------------------------
async def test_c_cancel_race_stress_200_iterations():
    t0 = time.monotonic()
    with _HandlerCapture() as cap:
        for i in range(200):
            started = asyncio.Event()

            async def synth(text: str) -> bytes:
                if text == "Two.":
                    started.set()
                    try:
                        await asyncio.Event().wait()
                    except asyncio.CancelledError:
                        raise RuntimeError(f"boom:{i}") from None
                return text.encode()

            piper = _piper_with_fake_synth(synth)
            agen = piper.stream_pcm("One. Two. Three.")
            assert await agen.__anext__() == b"One."
            await asyncio.wait_for(started.wait(), 5)
            await agen.aclose()
            await asyncio.sleep(0)  # let the cancelled prefetch settle
        await _drain()
        assert cap.contexts == [], f"asyncio reported: {cap.contexts!r}"
        assert _no_leaked_tasks() == []
    wall = time.monotonic() - t0
    assert wall < 30, f"200-iteration stress took {wall:.1f}s (>30s bound)"


# ---------------------------------------------------------------------------
# (d) normal streaming path unaffected: ordered chunks, complete cache entry
# ---------------------------------------------------------------------------
async def test_d_normal_streaming_path_unaffected():
    async def synth(text: str) -> bytes:
        await asyncio.sleep(0.005)
        return text.encode()

    piper = _piper_with_fake_synth(synth)
    chunks = [p async for p in piper.stream_pcm("One. Two. Three.")]
    assert chunks == [b"One.", b"Two.", b"Three."]
    # Full-utterance cache entry assembled complete, in ORIGINAL order.
    cached = piper._cache.get((piper.voice, "One. Two. Three.", "pcm"))
    assert cached == b"One.Two.Three."
    # Repeat is served as a single whole-buffer cache hit.
    assert [p async for p in piper.stream_pcm("One. Two. Three.")] == [
        b"One.Two.Three."
    ]
    await piper.aclose()
