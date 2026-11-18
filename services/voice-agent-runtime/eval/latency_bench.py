"""Offline voice stage-latency bench (SPEC-W73 WS-B B1).

Measures STT / LLM / TTS stage durations and an end-to-end turn estimate
through the REAL pipeline seams with deterministic fakes injected at the
documented seams — the default mode opens zero sockets and downloads zero
models, so it runs anywhere (CI laptops included):

- STT: real ``app.pipeline.stt.FasterWhisperSTT`` with a deterministic stub
  model injected at the ``_model`` slot (the same seam the unit tests use).
  The full transcribe path runs (PCM -> float32 conversion, model call,
  metrics), only the whisper decode itself is stubbed.
- LLM: a stub ``LLMInterface`` driven through the real
  ``app.pipeline.llm.run_tool_loop``.
- TTS: real ``app.pipeline.tts.PiperTTS`` in http mode backed by an
  ``httpx.MockTransport`` serving a deterministic wav (cache disabled, so
  every iteration measures the full fetch+decode path).

``--live`` mode (optional, documented) points the same stages at the real
sidecars via env URLs (``WHISPER_MODEL`` / ``LLM_BASE_URL`` / ``LLM_MODEL``
/ ``PIPER_HTTP_URL`` / ``PIPER_VOICE``) and needs the running voice stack.

Output schema (pinned):

    {stt: {p50_ms, p95_ms, mean_ms, min_ms, max_ms, n}, llm: {...},
     tts: {...}, e2e: {...}, meta: {iterations, mode, ts}}

Exit code: 0 on success, non-zero on harness error.

Usage:
    python3 eval/latency_bench.py [--iterations 20] [--json bench.json] [--md bench.md] [--live]
"""

from __future__ import annotations

import argparse
import asyncio
import io
import json
import math
import statistics
import sys
import time
import wave
from pathlib import Path

# Make the app package importable when run as a script (same trick the test
# conftest uses).
_SERVICE_ROOT = Path(__file__).resolve().parents[1]
if str(_SERVICE_ROOT) not in sys.path:
    sys.path.insert(0, str(_SERVICE_ROOT))

SCHEMA_STAGES = ("stt", "llm", "tts", "e2e")

# Deterministic bench fixtures (offline mode).
_PCM_16K = b"\x00\x01" * 16000  # 1 s of deterministic 16 kHz s16le mono
_STT_TEXT = "i would like to book an appointment for tomorrow morning"
_LLM_REPLY = "Of course — I can help with that. What time works best for you?"
_TTS_TEXT = "Of course, I can help with that. What time works best for you?"


# ---------------------------------------------------------------------------
# Deterministic fakes (offline mode). They implement the same contracts as
# the real model/HTTP layers; only the expensive inference is replaced.
# ---------------------------------------------------------------------------
class _FakeSegment:
    """faster-whisper Segment-shaped stub (only ``.text`` is consumed)."""

    def __init__(self, text: str) -> None:
        self.text = text


class _FakeInfo:
    """faster-whisper TranscriptionInfo-shaped stub."""

    language = "en"


class DeterministicWhisper:
    """faster-whisper WhisperModel-shaped stub with a fixed transcript."""

    def __init__(self, text: str = _STT_TEXT) -> None:
        self._text = text
        self.calls = 0

    def transcribe(self, audio, **_kwargs):  # noqa: ANN001 - mirrors model API
        self.calls += 1
        return [_FakeSegment(self._text)], _FakeInfo()


class _StubMessage:
    """OpenAI assistant-message-shaped stub (content, no tool calls)."""

    def __init__(self, content: str) -> None:
        self.content = content
        self.tool_calls = None


class StubLLM:
    """LLMInterface stub: one deterministic assistant message per round."""

    def __init__(self, reply: str = _LLM_REPLY) -> None:
        self._reply = reply
        self.calls = 0

    async def chat_with_tools(self, messages, tools):  # noqa: ANN001
        self.calls += 1
        return _StubMessage(self._reply)


def _bench_wav(sample_rate: int = 22050, ms: int = 120) -> bytes:
    """Deterministic RIFF wav (silence) for the mocked piper HTTP path."""
    frames = b"\x00\x00" * int(sample_rate * ms / 1000)
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)
        wf.setframerate(sample_rate)
        wf.writeframes(frames)
    return buf.getvalue()


def build_offline_pipeline():
    """Real pipeline objects with deterministic fakes injected at the seams."""
    import httpx

    from app.pipeline.stt import FasterWhisperSTT
    from app.pipeline.tts import PiperTTS

    stt = FasterWhisperSTT(model_size="bench-stub")
    stt._model = DeterministicWhisper()  # test seam: skip model load/download

    wav = _bench_wav()
    transport = httpx.MockTransport(
        lambda _req: httpx.Response(
            200, content=wav, headers={"content-type": "audio/wav"}
        )
    )
    # cache_size=0: the LRU would otherwise serve iterations 2..N from memory
    # and the bench would measure cache hits, not synthesis.
    tts = PiperTTS(mode="http", http_url="http://bench.invalid", cache_size=0)
    tts._client = httpx.AsyncClient(transport=transport)

    llm = StubLLM()
    return stt, llm, tts


def build_live_pipeline():
    """--live mode: real sidecars via env URLs (needs the voice stack up)."""
    import os

    from app.pipeline.llm import OpenAICompatibleLLM
    from app.pipeline.stt import FasterWhisperSTT
    from app.pipeline.tts import PiperTTS

    stt = FasterWhisperSTT(model_size=os.environ.get("WHISPER_MODEL", "base"))
    llm = OpenAICompatibleLLM(
        base_url=os.environ.get("LLM_BASE_URL", "http://localhost:11434/v1"),
        model=os.environ.get("LLM_MODEL", "qwen3:8b"),
        api_key=os.environ.get("LLM_API_KEY", "ollama"),
    )
    tts = PiperTTS(
        mode="http",
        http_url=os.environ.get("PIPER_HTTP_URL", "http://localhost:5500"),
        voice=os.environ.get("PIPER_VOICE", "en_US-lessac-medium"),
        cache_size=0,
    )
    return stt, llm, tts


# ---------------------------------------------------------------------------
# Statistics
# ---------------------------------------------------------------------------
def percentile(samples: list[float], q: float) -> float:
    """Linear-interpolation percentile (q in [0, 100]) on `samples`.

    Matches the common "inclusive" convention: the percentile position is
    ``q/100 * (n-1)`` into the sorted samples, interpolated between the two
    neighbouring ranks. Empty input -> 0.0; a single sample -> that sample.
    """
    if not samples:
        return 0.0
    xs = sorted(float(s) for s in samples)
    if len(xs) == 1:
        return xs[0]
    rank = (q / 100.0) * (len(xs) - 1)
    lo = math.floor(rank)
    hi = math.ceil(rank)
    if lo == hi:
        return xs[lo]
    return xs[lo] + (xs[hi] - xs[lo]) * (rank - lo)


def summarize(samples_ms: list[float]) -> dict:
    """Pinned per-stage summary block."""
    if not samples_ms:
        return {
            "p50_ms": 0.0,
            "p95_ms": 0.0,
            "mean_ms": 0.0,
            "min_ms": 0.0,
            "max_ms": 0.0,
            "n": 0,
        }
    return {
        "p50_ms": round(percentile(samples_ms, 50), 3),
        "p95_ms": round(percentile(samples_ms, 95), 3),
        "mean_ms": round(statistics.fmean(samples_ms), 3),
        "min_ms": round(min(samples_ms), 3),
        "max_ms": round(max(samples_ms), 3),
        "n": len(samples_ms),
    }


# ---------------------------------------------------------------------------
# Bench loop
# ---------------------------------------------------------------------------
async def _dispatch_unreachable(name, arguments):  # noqa: ANN001
    raise AssertionError(f"bench stub LLM must never call tools (got {name})")


async def run_bench(iterations: int = 20, mode: str = "offline") -> dict:
    """Run `iterations` turns; return the pinned-schema report dict."""
    if iterations < 1:
        raise ValueError("iterations must be >= 1")
    if mode == "live":
        stt, llm, tts = build_live_pipeline()
    elif mode == "offline":
        stt, llm, tts = build_offline_pipeline()
    else:
        raise ValueError(f"unknown mode {mode!r} (expected 'offline' or 'live')")

    from app.pipeline.llm import run_tool_loop

    stt_ms: list[float] = []
    llm_ms: list[float] = []
    tts_ms: list[float] = []
    try:
        for _ in range(iterations):
            t0 = time.perf_counter()
            transcript = await stt.transcribe_pcm(_PCM_16K, sample_rate=16000)
            stt_ms.append((time.perf_counter() - t0) * 1000.0)

            messages = [{"role": "user", "content": transcript or _STT_TEXT}]
            t0 = time.perf_counter()
            reply, _trace = await run_tool_loop(
                llm, messages=messages, tools=[], dispatch=_dispatch_unreachable
            )
            llm_ms.append((time.perf_counter() - t0) * 1000.0)

            t0 = time.perf_counter()
            await tts.synthesize_pcm(reply or _TTS_TEXT)
            tts_ms.append((time.perf_counter() - t0) * 1000.0)
    finally:
        aclose = getattr(tts, "aclose", None)
        if aclose is not None:
            await aclose()

    e2e_ms = [s + l + t for s, l, t in zip(stt_ms, llm_ms, tts_ms)]
    return {
        "stt": summarize(stt_ms),
        "llm": summarize(llm_ms),
        "tts": summarize(tts_ms),
        "e2e": summarize(e2e_ms),
        "meta": {
            "iterations": iterations,
            "mode": mode,
            "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        },
    }


def render_markdown(report: dict) -> str:
    """Human-readable summary of a report dict (pinned schema)."""
    meta = report["meta"]
    lines = [
        "# Voice stage-latency bench (SPEC-W73)",
        "",
        f"Mode: `{meta['mode']}` | Iterations: {meta['iterations']} | "
        f"Generated: {meta['ts']}",
        "",
        "| Stage | p50 (ms) | p95 (ms) | mean (ms) | min (ms) | max (ms) | n |",
        "|---|---|---|---|---|---|---|",
    ]
    for stage in SCHEMA_STAGES:
        s = report[stage]
        lines.append(
            f"| {stage} | {s['p50_ms']} | {s['p95_ms']} | {s['mean_ms']} "
            f"| {s['min_ms']} | {s['max_ms']} | {s['n']} |"
        )
    lines += [
        "",
        "Offline mode measures the real pipeline code paths (PCM conversion,",
        "tool loop, wav decode, metrics) with the model inference stubbed —",
        "absolute numbers are harness overhead, useful for regression drift.",
        "Use `--live` against running sidecars for production-representative",
        "latencies (compare with the W71 `voice_*_latency_seconds` histograms).",
        "",
    ]
    return "\n".join(lines)


def _print_summary(report: dict) -> None:
    meta = report["meta"]
    print(f"voice latency bench: mode={meta['mode']} iterations={meta['iterations']}")
    for stage in SCHEMA_STAGES:
        s = report[stage]
        print(
            f"  {stage:4s} p50={s['p50_ms']:>9.3f} ms  p95={s['p95_ms']:>9.3f} ms"
            f"  mean={s['mean_ms']:>9.3f} ms"
        )


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--iterations", type=int, default=20)
    ap.add_argument("--json", metavar="PATH", help="write the report as JSON")
    ap.add_argument("--md", metavar="PATH", help="write a Markdown summary")
    ap.add_argument(
        "--live",
        action="store_true",
        help="hit the real sidecars (WHISPER_MODEL/LLM_BASE_URL/PIPER_HTTP_URL); "
        "default mode is fully offline",
    )
    args = ap.parse_args(argv)

    try:
        report = asyncio.run(
            run_bench(args.iterations, mode="live" if args.live else "offline")
        )
    except Exception as exc:  # noqa: BLE001 - harness error -> non-zero exit
        print(f"[latency_bench] ERROR: {exc}", file=sys.stderr)
        return 1

    if args.json:
        Path(args.json).write_text(json.dumps(report, indent=2) + "\n")
    if args.md:
        Path(args.md).write_text(render_markdown(report))
    _print_summary(report)
    return 0


if __name__ == "__main__":
    sys.exit(main())
