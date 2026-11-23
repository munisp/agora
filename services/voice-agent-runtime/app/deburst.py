"""Remove the YarnGPT end-of-line burst from Nigerian-language speech (2026-11-23).

The naija voices (YarnGPT on the GPU host) end most lines with a ~0.12-0.14 s loud burst right after the last word,
heard on calls as a sharp bleep after every Yorùbá / Hausa / Igbo sentence. The film pipeline already removed it
(newapps-qa demos/leadway/yarn_audio.py, end_burst_trim + deburst_pieces); this is the same method for live calls,
on 16-bit mono PCM:

1. split the audio at silences (>= 80 ms under -60 dBFS);
2. drop stray ticks (< 40 ms alone in a silence) and lone bursts (< 0.3 s, full scale, louder than the speech);
3. cut each piece's end burst: the last 0.16 s peaking >= 3 dB above the median of the speech before it, or a blip
   after the piece's last short gap; then fade each piece 10 ms in / 40 ms out.

Fail-safe: any error returns the audio unchanged.
"""

from __future__ import annotations

import numpy as np

_QUIET = 33  # |sample| below this is silence (-60 dBFS)


def _db(seg: np.ndarray) -> float:
    if seg.size == 0:
        return -180.0
    rms = float(np.sqrt(np.mean(seg.astype(np.float64) ** 2)))
    return 20.0 * np.log10(rms / 32768.0 + 1e-9)


def end_burst_samples(a: np.ndarray, sr: int) -> int:
    """How many samples to cut from the end of one spoken piece (0 = ends softly, keep it whole)."""
    n = a.size
    win = max(1, sr // 50)
    tail0 = n - int(0.16 * sr)
    before = [d for d in (_db(a[i:i + win]) for i in range(max(0, n - int(0.6 * sr)), max(0, tail0), win)) if d > -40]
    if not before or tail0 <= 0:
        return 0
    ref = sorted(before)[len(before) // 2]
    step = max(1, win // 2)
    levels = [(i, _db(a[i:i + win])) for i in range(max(0, n - int(0.45 * sr)), n - win + 1, step)]
    quiet = [i for i, d in levels if d < -38]
    if quiet:
        last_gap_end = quiet[-1] + win
        after = n - last_gap_end
        if 0 < after <= int(0.18 * sr) and any(d > -32 for i, d in levels if i >= last_gap_end):
            return min(n, after + int(0.01 * sr))
    tail = [(i, _db(a[i:i + win])) for i in range(tail0, n - win + 1, step)]
    if not tail or max(d for _, d in tail) < ref + 3:
        return 0
    first = next(i for i, d in tail if d >= ref + 3)
    return min(n, n - first + int(0.02 * sr))


def _pieces(a: np.ndarray, sr: int) -> list[tuple[int, int]]:
    """Spoken pieces [start, end) separated by >= 80 ms of silence."""
    loud = np.abs(a) >= _QUIET
    if not loud.any():
        return []
    gap = int(0.08 * sr)
    idx = np.flatnonzero(loud)
    out, start, prev = [], int(idx[0]), int(idx[0])
    for i in idx[1:]:
        i = int(i)
        if i - prev > gap:
            out.append((start, prev + 1))
            start = i
        prev = i
    out.append((start, prev + 1))
    return out


def deburst_pcm(pcm: bytes, sample_rate: int) -> bytes:
    try:
        a = np.frombuffer(pcm[: len(pcm) - (len(pcm) % 2)], dtype=np.int16).astype(np.int32)
        if a.size < sample_rate // 5:
            return pcm
        sr = sample_rate
        pieces = _pieces(a, sr)
        if not pieces:
            return pcm
        speech = sorted(_db(a[s:e]) for s, e in pieces)[len(pieces) // 2]
        kept = []
        for s, e in pieces:
            length = e - s
            seg = a[s:e]
            if length < int(0.04 * sr) or (length < int(0.3 * sr) and int(np.max(np.abs(seg))) >= 32000
                                           and _db(seg) > speech):
                a[s:e] = 0  # stray tick / lone burst
                continue
            kept.append((s, e))
        for s, e in kept:
            cut = end_burst_samples(a[s:e], sr) if (e - s) > int(0.3 * sr) else 0
            end = e - cut
            if cut:
                a[end:e] = 0
            fi, fo = int(0.01 * sr), int(0.04 * sr)
            k = min(fi, end - s)
            if k > 0:
                a[s:s + k] = (a[s:s + k] * np.arange(k) / fi).astype(np.int32)
            k = min(fo, end - s)
            if k > 0:
                a[end - k:end] = (a[end - k:end] * (np.arange(k)[::-1] / fo)).astype(np.int32)
        return np.clip(a, -32768, 32767).astype(np.int16).tobytes()
    except Exception:  # noqa: BLE001 - never lose the audio over a clean-up step
        return pcm
