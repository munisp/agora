"""SPEC-W74 A2 tests: FallbackTTS.stream_pcm depth-1 prefetch (ported from
the PiperTTS W71-P4 pattern) + first-chunk observability
(``voice_tts_first_chunk_seconds``).

Offline: a fake in-memory provider with controllable per-chunk asyncio
delays; the full stack exercises the real chain (synthesize ->
synthesize_pcm -> _wav_to_pcm -> LRU) exactly as production does.
"""

from __future__ import annotations

import asyncio
import gc
import io
import re
import time
import wave

import pytest

from app import metrics
from app.tts_providers.chain import FallbackTTS


def _wav(payload: bytes, rate: int = 22050) -> bytes:
    """RIFF wav whose decoded 16-bit mono PCM is exactly `payload` (payload
    length must be even)."""
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)
        wf.setframerate(rate)
        wf.writeframes(payload)
    return buf.getvalue()


class FakeProvider:
    """TTSProvider with per-chunk controllable asyncio delays + failures.

    synthesize() returns a wav whose PCM payload is the chunk text encoded,
    so the chain's synthesize_pcm yields text.encode() per chunk."""

    name = "fake"
    default_voice = "fv"

    def __init__(
        self,
        delays: dict[str, float] | None = None,
        fail_on: set[str] | None = None,
        hang_on: set[str] | None = None,
    ) -> None:
        self.delays = dict(delays or {})
        self.fail_on = set(fail_on or set())
        self.hang_on = set(hang_on or set())
        self.intervals: dict[str, tuple[float, float]] = {}
        self.entered: dict[str, asyncio.Event] = {}
        self.released = asyncio.Event()
        self.calls: list[str] = []

    async def synthesize(self, text: str, voice: str, language: str) -> bytes:
        self.calls.append(text)
        start = time.monotonic()
        if text in self.hang_on:
            ev = self.entered.setdefault(text, asyncio.Event())
            ev.set()
            await self.released.wait()  # hangs unless the prefetch is cancelled
        else:
            await asyncio.sleep(self.delays.get(text, 0.01))
        self.intervals[text] = (start, time.monotonic())
        if text in self.fail_on:
            raise RuntimeError(f"boom:{text}")
        return _wav(text.encode())

    async def list_voices(self):
        return []

    async def available(self) -> bool:
        return True

    async def aclose(self) -> None:
        pass


def _chain(provider: FakeProvider, **kw) -> FallbackTTS:
    return FallbackTTS({"fake": provider}, ["fake"], sample_rate=22050, **kw)


def _first_chunk_count() -> int:
    m = re.search(r"^voice_tts_first_chunk_seconds_count (\d+)", metrics.render(), re.M)
    return int(m.group(1)) if m else 0


def _no_leaked_tasks() -> list[asyncio.Task]:
    return [
        t
        for t in asyncio.all_tasks()
        if t is not asyncio.current_task() and not t.done()
    ]


# ---------------------------------------------------------------------------
# (a) overlap proof — 3 chunks at delay D complete well under 3xD
# ---------------------------------------------------------------------------
async def test_a_overlap_proof_wall_time_and_instrumentation():
    # Uneven chunk times (W71-P4 precedent): with uniform delays chunk N+1
    # and chunk N can finish together, hiding the overlap. Delays are sized
    # so the expected wall (~0.15s) has wide margin under the bound.
    delays = {"One.": 0.12, "Two.": 0.15, "Three.": 0.03}
    sequential = sum(delays.values())  # 0.30s on the pre-W74 path
    provider = FakeProvider(delays=delays)
    chain = _chain(provider)
    t0 = time.monotonic()
    chunks = [p async for p in chain.stream_pcm("One. Two. Three.")]
    wall = time.monotonic() - t0
    assert chunks == [b"One.", b"Two.", b"Three."]
    # Depth-1 prefetch must overlap: chunk N+1's synthesis starts before
    # chunk N's finishes (instrumentation proof, not just timing)...
    assert provider.intervals["Two."][0] < provider.intervals["One."][1]
    assert provider.intervals["Three."][0] < provider.intervals["Two."][1]
    # ...and the wall time stays far below the sequential sum.
    assert wall < 0.75 * sequential, f"wall {wall:.3f}s not < {0.75 * sequential:.3f}s"
    await chain.aclose()


# ---------------------------------------------------------------------------
# (b) order preservation under out-of-order completion
# ---------------------------------------------------------------------------
async def test_b_order_preserved_under_out_of_order_completion():
    # Chunk 2 finishes BEFORE chunk 1; yields must stay in original order.
    provider = FakeProvider(delays={"One.": 0.08, "Two.": 0.02, "Three.": 0.01})
    chain = _chain(provider)
    chunks = [p async for p in chain.stream_pcm("One. Two. Three.")]
    assert chunks == [b"One.", b"Two.", b"Three."]
    # Completion order really was out of order (non-vacuity).
    assert provider.intervals["Two."][1] < provider.intervals["One."][1]
    # Full-utterance cache entry assembled in ORIGINAL chunk order.
    cached = chain._cache.get(("|", "One. Two. Three.", "pcm"))
    assert cached == b"One.Two.Three."
    # Per-chunk chain LRU entries also populated (W46-P4 semantics unchanged).
    for chunk in ("One.", "Two.", "Three."):
        assert chain._cache.get(("|", chunk, "pcm")) == chunk.encode()
    await chain.aclose()


# ---------------------------------------------------------------------------
# (c) error at chunk i surfaces at iteration i
# ---------------------------------------------------------------------------
async def test_c_error_surfaces_at_own_iteration():
    provider = FakeProvider(fail_on={"Two."})
    chain = _chain(provider)
    got: list[bytes] = []
    with pytest.raises(RuntimeError, match="boom:Two\\."):
        async for pcm in chain.stream_pcm("One. Two. Three."):
            got.append(pcm)
    # Exactly like the sequential path: chunk 1 delivered, chunk 2's failure
    # raises at its own index, chunk 3 never yielded.
    assert got == [b"One."]
    await asyncio.sleep(0.02)
    assert _no_leaked_tasks() == []
    await chain.aclose()


# ---------------------------------------------------------------------------
# (d) consumer close after first yield: prefetch cancelled / retrieved
# ---------------------------------------------------------------------------
async def test_d_close_cancels_pending_prefetch_no_leaks():
    provider = FakeProvider(hang_on={"Two."})
    chain = _chain(provider)
    agen = chain.stream_pcm("One. Two. Three.")
    assert await agen.__anext__() == b"One."
    # The depth-1 prefetch of "Two." is in flight while we hold chunk 1.
    await asyncio.wait_for(provider.entered["Two."].wait(), 1)
    await agen.aclose()  # early exit: generator close
    provider.released.set()  # would hang forever if not cancelled
    await asyncio.sleep(0.05)
    assert _no_leaked_tasks() == []
    await chain.aclose()


async def test_d_close_retrieves_completed_prefetch_exception():
    """Completed-but-unconsumed prefetch error is retrieved on close — no
    'exception was never retrieved' asyncio noise."""
    loop = asyncio.get_running_loop()
    contexts: list[dict] = []
    old_handler = loop.get_exception_handler()
    loop.set_exception_handler(lambda l, ctx: contexts.append(ctx))
    try:
        prefetch_done = asyncio.Event()

        class _ObservedProvider(FakeProvider):
            """FakeProvider + deterministic completion signal (SPEC-W75 A2).

            The event fires in the SAME loop step in which the prefetch
            coroutine finishes: its ``finally`` runs while the exception is
            still propagating, and the task records its outcome before the
            loop processes the waiter's wakeup — so when ``wait()`` returns,
            the prefetch task is guaranteed done WITH its error, unconsumed.
            """

            async def synthesize(self, text: str, voice: str, language: str) -> bytes:
                try:
                    return await super().synthesize(text, voice, language)
                finally:
                    if text == "Two.":
                        prefetch_done.set()

        provider = _ObservedProvider(delays={"Two.": 0.0}, fail_on={"Two."})
        chain = _chain(provider)
        agen = chain.stream_pcm("One. Two. Three.")
        assert await agen.__anext__() == b"One."
        # Deterministic proof the failing prefetch completed UNCONSUMED
        # before the close — replaces the blind 0.05s sleep that flaked
        # 1/22 under host load (V-INT-74). Bounded so a regression fails
        # fast instead of hanging.
        await asyncio.wait_for(prefetch_done.wait(), 5)
        assert "Two." in provider.intervals  # the error path really ran
        await agen.aclose()
        # Drain loop turns + gc so any unretrieved-exception callback fires
        # NOW (fixed loop yields, not a wall-clock guess).
        for _ in range(10):
            await asyncio.sleep(0)
            gc.collect()
        assert contexts == [], f"asyncio reported: {contexts!r}"
        assert _no_leaked_tasks() == []
        await chain.aclose()
    finally:
        loop.set_exception_handler(old_handler)


# ---------------------------------------------------------------------------
# (e) full-utterance cache hit path unchanged + first-chunk metric
# ---------------------------------------------------------------------------
async def test_e_full_utterance_cache_hit_unchanged_with_metric():
    provider = FakeProvider(delays={"One.": 0.01, "Two.": 0.01, "Three.": 0.01})
    chain = _chain(provider)
    first = [p async for p in chain.stream_pcm("One. Two. Three.")]
    assert first == [b"One.", b"Two.", b"Three."]
    calls_after_first = list(provider.calls)
    before = _first_chunk_count()
    # Second stream: full-utterance cache hit ships the WHOLE buffer in one
    # yield and touches no provider.
    second = [p async for p in chain.stream_pcm("One. Two. Three.")]
    assert second == [b"One.Two.Three."]
    assert provider.calls == calls_after_first
    assert _first_chunk_count() == before + 1  # metric on the cache path too
    await chain.aclose()


# ---------------------------------------------------------------------------
# (f) single-chunk path unchanged (+ metric)
# ---------------------------------------------------------------------------
async def test_f_single_chunk_path_unchanged():
    provider = FakeProvider(delays={"Hi": 0.01})
    chain = _chain(provider)
    before = _first_chunk_count()
    chunks = [p async for p in chain.stream_pcm("Hi")]
    assert chunks == [b"Hi"]
    assert provider.calls == ["Hi"]
    # Single-chunk path also observes the first-chunk metric.
    assert _first_chunk_count() == before + 1
    # Empty text stays a no-op.
    assert [p async for p in chain.stream_pcm("   ")] == []
    await chain.aclose()


# ---------------------------------------------------------------------------
# (g) first-chunk metric observed on the multi-chunk path
# ---------------------------------------------------------------------------
async def test_g_metric_observed_on_multi_chunk_path():
    provider = FakeProvider(delays={"One.": 0.02, "Two.": 0.02})
    chain = _chain(provider)
    before = _first_chunk_count()
    chunks = [p async for p in chain.stream_pcm("One. Two.")]
    assert chunks == [b"One.", b"Two."]
    # Exactly ONE observation per stream call even with 2 yields.
    assert _first_chunk_count() == before + 1
    await chain.aclose()


# ---------------------------------------------------------------------------
# Empty-chunk skipping preserved under prefetch (sequential parity)
# ---------------------------------------------------------------------------
async def test_empty_chunk_skipping_preserved():
    class _EmptyMiddle(FakeProvider):
        async def synthesize(self, text: str, voice: str, language: str) -> bytes:
            await asyncio.sleep(0)
            self.calls.append(text)
            if text == "Two.":
                return _wav(b"")  # zero-frame wav -> empty PCM -> skipped
            return _wav(text.encode())

    provider = _EmptyMiddle()
    chain = _chain(provider)
    chunks = [p async for p in chain.stream_pcm("One. Two. Three.")]
    assert chunks == [b"One.", b"Three."]
    cached = chain._cache.get(("|", "One. Two. Three.", "pcm"))
    assert cached == b"One.Three."
    await chain.aclose()
