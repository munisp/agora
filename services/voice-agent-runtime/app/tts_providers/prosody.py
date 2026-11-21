"""Runtime prosody layer (SPEC-W106 C1) — deterministic DSP for the voice
chain's language-quality complaints:

- **"too fast"**: :func:`pcm16_time_stretch` slows (or quickens) speech
  WITHOUT a pitch shift — overlap-add (OLA) time-scale modification with a
  Hann window at a COLA hop (frame/4) plus lightweight waveform-similarity
  alignment (WSOLA-lite: each analysis frame is nudged within ±hop/2 to
  maximize dot-product alignment with the synthesized tail, which removes
  the phase-beat artifacts plain OLA produces on periodic/voiced audio).
  Honest quality limit: this is NOT full WSOLA/phase-vocoder grade — clean
  for small adjustments, artifacts grow beyond ~15%; the docs recommend
  staying inside [0.85, 1.15] (the clamp [0.5, 2.0] is only a sanity
  bound).
- **"choppy"**: :func:`pcm16_join` inserts real silence between streamed
  sentence chunks so prosodic boundaries breathe.
- **pitch artifacts**: :func:`pcm16_resample_linear` normalizes a provider's
  sample rate to the chain rate — the case the chain previously only warned
  about ("audio may be pitched").

Per-language policies come from ``TTS_PROSODY_MAP``
(``"pcm:0.95:120,yo:0.95:120"`` = rate 0.95 with a 120 ms join pad). The
whole layer is OFF unless ``TTS_PROSODY`` is set (house opt-in idiom, same
as VOICEPRINTS / INTEL_LLM): with no map every function here is an
identity path and pre-W106 audio is bit-identical.
"""

from __future__ import annotations

import math
from dataclasses import dataclass

import numpy as np

from ..logging import get_logger

log = get_logger("tts.prosody")

MIN_RATE = 0.5
MAX_RATE = 2.0
_IDENTITY_EPS = 1e-3
_MIN_SAMPLES = 1024  # shorter clips are returned unchanged (honest no-op)


def _clamp_rate(rate: float) -> float:
    return min(MAX_RATE, max(MIN_RATE, float(rate)))


def pcm16_time_stretch(
    pcm: bytes, rate: float, *, sample_rate: int, frame_ms: float = 25.0
) -> bytes:
    """Time-scale 16-bit mono PCM by ``rate`` without shifting pitch.

    ``rate`` is speech speed: output duration = input duration / rate
    (0.95 => ~5% slower/longer). OLA Hann/COLA with per-frame waveform
    alignment (WSOLA-lite): each analysis position is searched within
    ±hs/2 for the offset whose first 3*hs samples best dot-align with the
    already-synthesized tail — this removes the hop-rate beat artifacts
    plain OLA produces on voiced (periodic) speech. rate ~1.0 or clips
    below _MIN_SAMPLES are returned unchanged.
    """
    rate = _clamp_rate(rate)
    if abs(rate - 1.0) < _IDENTITY_EPS:
        return pcm
    samples = np.frombuffer(pcm, dtype=np.int16).astype(np.float32)
    n = int(samples.shape[0])
    if n < _MIN_SAMPLES:
        return pcm
    frame = max(64, int(sample_rate * frame_ms / 1000.0))
    hs = frame // 4                      # synthesis hop (COLA for Hann)
    ha = max(1, int(round(hs * rate)))   # analysis hop
    window = np.hanning(frame).astype(np.float32)
    n_frames = 1 + max(0, (n - frame) // ha)
    out_len = frame + hs * max(0, n_frames - 1)
    out = np.zeros(out_len, dtype=np.float32)
    norm = np.zeros(out_len, dtype=np.float32)
    delta = hs // 2
    overlap = 3 * hs
    for i in range(n_frames):
        o = i * hs
        pos = min(i * ha, n - frame)
        if i > 0:
            # WSOLA-lite: align this frame with the synthesized tail.
            ref = out[o : o + overlap]
            best_pos, best_score = pos, -np.inf
            lo = max(0, pos - delta)
            hi = min(n - frame, pos + delta)
            for cand in range(lo, hi + 1):
                score = float(
                    np.dot(ref, samples[cand : cand + overlap])
                )
                if score > best_score:
                    best_score, best_pos = score, cand
            pos = best_pos
        out[o : o + frame] += samples[pos : pos + frame] * window
        norm[o : o + frame] += window
    np.maximum(norm, 1e-8, out=norm)
    out /= norm
    peak = float(np.max(np.abs(out))) or 1.0
    if peak > 32767.0:
        out *= 32767.0 / peak
    return out.astype(np.int16).tobytes()


def pcm16_pad(pad_ms: float, sample_rate: int) -> bytes:
    """Silence of exactly ``pad_ms`` milliseconds (>= 0)."""
    n = max(0, int(round(sample_rate * pad_ms / 1000.0)))
    return b"\x00\x00" * n


def pcm16_join(chunks: list[bytes], pad_ms: float, sample_rate: int) -> bytes:
    """Join PCM chunks with a silence pad BETWEEN chunks (never trailing)."""
    if not chunks:
        return b""
    pad = pcm16_pad(pad_ms, sample_rate) if pad_ms > 0 else b""
    out = [chunks[0]]
    for chunk in chunks[1:]:
        if pad:
            out.append(pad)
        out.append(chunk)
    return b"".join(out)


def pcm16_resample_linear(pcm: bytes, src_rate: int, dst_rate: int) -> bytes:
    """Linear-interpolation resample of 16-bit mono PCM.

    Deliberately simple and deterministic (numpy interp) — adequate for the
    sidecar rate spread (e.g. 24 kHz cosyvoice/longcat onto a 22.05 kHz
    chain) and HONEST about it: not an anti-aliased resampler.
    """
    if src_rate == dst_rate:
        return pcm
    samples = np.frombuffer(pcm, dtype=np.int16).astype(np.float32)
    if samples.shape[0] == 0:
        return b""
    n_out = int(round(samples.shape[0] * dst_rate / float(src_rate)))
    if n_out <= 0:
        return b""
    x_old = np.arange(samples.shape[0], dtype=np.float32)
    x_new = np.linspace(0.0, samples.shape[0] - 1.0, n_out)
    out = np.interp(x_new, x_old, samples)
    return out.astype(np.int16).tobytes()


@dataclass(frozen=True)
class ProsodyPolicy:
    """Per-language prosody: speech ``rate`` + ``join_pad_ms`` between
    streamed sentence chunks. Identity is rate=1.0, join_pad_ms=0."""

    rate: float = 1.0
    join_pad_ms: int = 0

    @property
    def is_identity(self) -> bool:
        return abs(self.rate - 1.0) < _IDENTITY_EPS and self.join_pad_ms <= 0


def parse_prosody_map(raw: str | None) -> dict[str, ProsodyPolicy]:
    """``"pcm:0.95:120,yo:0.95"`` -> ``{"pcm": ProsodyPolicy(0.95, 120), …}``.

    Fields: ``lang:rate[:join_pad_ms]`` — the rate field is REQUIRED (a
    language-only entry would be a useless identity policy). Invalid entries
    are dropped with a warning (parse_chain idiom); rates are clamped to
    [MIN_RATE, MAX_RATE].
    """
    out: dict[str, ProsodyPolicy] = {}
    for part in (raw or "").split(","):
        part = part.strip()
        if not part:
            continue
        fields = part.split(":")
        lang = fields[0].strip().lower()
        try:
            if len(fields) < 2:
                raise ValueError("rate field required")
            rate = float(fields[1])
            pad = int(fields[2]) if len(fields) > 2 else 0
            if not lang or not math.isfinite(rate) or pad < 0:
                raise ValueError("bad prosody entry")
        except (ValueError, IndexError):
            log.warning("TTS_PROSODY_MAP: dropping invalid entry", entry=part)
            continue
        out[lang] = ProsodyPolicy(rate=_clamp_rate(rate), join_pad_ms=pad)
    return out


def policy_for(
    language: str, policies: dict[str, ProsodyPolicy] | None
) -> ProsodyPolicy | None:
    """Policy for ``language`` (case-folded), ``"*"`` fallback, else None."""
    if not policies:
        return None
    lang = (language or "").strip().lower()
    return policies.get(lang) or policies.get("*")


def apply_prosody(
    pcm: bytes, policy: ProsodyPolicy, sample_rate: int
) -> bytes:
    """Chain hook: time-stretch per policy (join pads are applied by the
    stream path, which owns chunk boundaries)."""
    if policy is None or abs(policy.rate - 1.0) < _IDENTITY_EPS:
        return pcm
    return pcm16_time_stretch(pcm, policy.rate, sample_rate=sample_rate)
