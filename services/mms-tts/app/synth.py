"""MMS-TTS synthesis backends.

Two modes, selected by MMS_MOCK (default "1"):

- mock: deterministic sine-based wav, keyed by language (pitch) and text
  (duration proportional to text length). No model downloads, no torch —
  the heavy deps are imported lazily and only in real mode.
- real: facebook/mms-tts-{lang} VITS checkpoints via transformers, loaded
  lazily per language and gated by the MMS_LANGS env allow-list.

Both modes return RIFF/WAV bytes, 16-bit PCM mono. Mock mode is fixed at
22050 Hz; real mode uses each checkpoint's native sampling rate.
"""

from __future__ import annotations

import hashlib
import io
import threading
from typing import Dict, Tuple

import numpy as np
import soundfile as sf

MOCK_SAMPLE_RATE = 22050

SUPPORTED_LANGS = ("eng", "pcm", "yor", "ibo", "hau")

# Static voice catalog (MMS checkpoints are single-speaker; Meta does not
# publish a gender per checkpoint, so gender is reported as "unspecified").
VOICE_CATALOG = {
    "eng": {"languages": ["en"], "labels": ["mms", "vits", "single-speaker", "english"]},
    "pcm": {"languages": ["pcm"], "labels": ["mms", "vits", "single-speaker", "nigerian-pidgin"]},
    "yor": {"languages": ["yo"], "labels": ["mms", "vits", "single-speaker", "yoruba"]},
    "ibo": {"languages": ["ig"], "labels": ["mms", "vits", "single-speaker", "igbo"]},
    "hau": {"languages": ["ha"], "labels": ["mms", "vits", "single-speaker", "hausa"]},
}

# Mock prosody profiles: base pitch (Hz) and speech rate (chars/second) per
# language. Values are arbitrary but stable — the contract is determinism and
# lang-keyed difference, not realism.
MOCK_PROFILES = {
    "eng": {"pitch": 165.0, "rate": 13.0},
    "pcm": {"pitch": 142.0, "rate": 12.0},
    "yor": {"pitch": 178.0, "rate": 10.5},
    "ibo": {"pitch": 184.0, "rate": 10.5},
    "hau": {"pitch": 155.0, "rate": 11.5},
}

MAX_DURATION_S = 30.0
MIN_DURATION_S = 0.35


def encode_wav(samples: np.ndarray, sample_rate: int) -> bytes:
    """Encode float32 mono samples as 16-bit PCM RIFF/WAV bytes."""
    buf = io.BytesIO()
    sf.write(buf, samples.astype(np.float32), sample_rate, format="WAV", subtype="PCM_16")
    return buf.getvalue()


def mock_synthesize(text: str, lang: str) -> bytes:
    """Deterministic sine-based wav keyed by (lang, text).

    Pitch comes from the language profile with a small deterministic vibrato
    derived from a hash of the text; duration is proportional to text length
    (clamped). Same input always yields byte-identical output.
    """
    profile = MOCK_PROFILES[lang]
    duration = min(MAX_DURATION_S, max(MIN_DURATION_S, len(text) / profile["rate"]))
    n = int(MOCK_SAMPLE_RATE * duration)
    t = np.arange(n, dtype=np.float32) / MOCK_SAMPLE_RATE

    digest = hashlib.sha256(f"{lang}:{text}".encode("utf-8")).digest()
    seed = int.from_bytes(digest[:4], "big")
    rng = np.random.default_rng(seed)

    base = profile["pitch"] + (seed % 21) - 10  # ±10 Hz deterministic offset
    vibrato = 3.0 * np.sin(2 * np.pi * 5.0 * t)
    phase = 2 * np.pi * np.cumsum(base + vibrato) / MOCK_SAMPLE_RATE

    signal = 0.55 * np.sin(phase)
    signal += 0.25 * np.sin(2 * phase)  # first harmonic for timbre
    # Deterministic low-amplitude noise floor so distinct texts differ.
    signal += 0.02 * rng.standard_normal(n).astype(np.float32)

    # Word-boundary amplitude envelope: slight dip at each space, plus short
    # fade in/out to avoid clicks.
    envelope = np.ones(n, dtype=np.float32)
    for idx, ch in enumerate(text):
        if ch == " ":
            center = int(idx / max(len(text), 1) * n)
            lo, hi = max(0, center - 200), min(n, center + 200)
            envelope[lo:hi] *= 0.55
    fade = min(256, n // 4)
    if fade > 0:
        envelope[:fade] *= np.linspace(0.0, 1.0, fade)
        envelope[-fade:] *= np.linspace(1.0, 0.0, fade)
    signal *= envelope

    peak = float(np.max(np.abs(signal))) or 1.0
    signal = signal / peak * 0.8
    return encode_wav(signal, MOCK_SAMPLE_RATE)


class MMSModelPool:
    """Lazy per-language loader for facebook/mms-tts-{lang} checkpoints.

    torch/transformers are imported inside the loader so mock mode (and the
    test suite) never need them installed.
    """

    def __init__(self, enabled_langs: Tuple[str, ...]):
        self.enabled_langs = tuple(enabled_langs)
        self._cache: Dict[str, Tuple[object, object]] = {}
        self._lock = threading.Lock()

    @property
    def loaded_langs(self) -> Tuple[str, ...]:
        return tuple(sorted(self._cache.keys()))

    def _load(self, lang: str) -> Tuple[object, object]:
        if lang not in self.enabled_langs:
            raise LangNotEnabled(lang, self.enabled_langs)
        with self._lock:
            if lang in self._cache:
                return self._cache[lang]
            # Lazy heavy imports — real mode only.
            import torch  # noqa: F401  (ensures a clear ImportError if missing)
            from transformers import AutoTokenizer, VitsModel

            repo = f"facebook/mms-tts-{lang}"
            tokenizer = AutoTokenizer.from_pretrained(repo)
            model = VitsModel.from_pretrained(repo)
            model.eval()
            self._cache[lang] = (model, tokenizer)
            return self._cache[lang]

    def synthesize(self, text: str, lang: str) -> bytes:
        model, tokenizer = self._load(lang)
        import torch

        inputs = tokenizer(text, return_tensors="pt")
        with torch.no_grad():
            waveform = model(**inputs).waveform
        audio = waveform.squeeze().cpu().numpy().astype(np.float32)
        sample_rate = int(getattr(model.config, "sampling_rate", 16000))
        return encode_wav(audio, sample_rate)


class LangNotEnabled(RuntimeError):
    def __init__(self, lang: str, enabled: Tuple[str, ...]):
        super().__init__(
            f"language '{lang}' model is not enabled on this instance; "
            f"enabled MMS_LANGS={','.join(enabled)}"
        )
        self.lang = lang
        self.enabled = enabled
