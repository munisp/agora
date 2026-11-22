"""SPEC-W106 tests: prosody layer — OLA time-stretch (duration + pitch
preservation), silence pads, linear resample, policy parsing, chain
integration (identity default, stretch, join pads, rate normalization),
and config parsing."""

from __future__ import annotations

import io
import math
import wave

import numpy as np
import pytest

from app.config import Settings, load_settings
from app.tts_providers.chain import FallbackTTS, build_fallback_tts
from app.tts_providers.prosody import (
    MAX_RATE,
    MIN_RATE,
    ProsodyPolicy,
    apply_prosody,
    parse_prosody_map,
    pcm16_join,
    pcm16_pad,
    pcm16_resample_linear,
    pcm16_time_stretch,
    policy_for,
)

RATE = 22050


def _sine_pcm(freq: float = 220.0, seconds: float = 1.0, rate: int = RATE) -> bytes:
    n = int(rate * seconds)
    t = np.arange(n, dtype=np.float32) / rate
    signal = 0.6 * np.sin(2 * np.pi * freq * t)
    return (signal * 32767).astype(np.int16).tobytes()


def _wav(payload: bytes, rate: int = RATE) -> bytes:
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)
        wf.setframerate(rate)
        wf.writeframes(payload)
    return buf.getvalue()


def _n(pcm: bytes) -> int:
    return len(pcm) // 2


def _dominant_freq(pcm: bytes, rate: int = RATE) -> float:
    samples = np.frombuffer(pcm, dtype=np.int16).astype(np.float32)
    spectrum = np.abs(np.fft.rfft(samples))
    freqs = np.fft.rfftfreq(len(samples), d=1.0 / rate)
    spectrum[0] = 0  # ignore DC
    return float(freqs[int(np.argmax(spectrum))])


# ---------------------------------------------------------------------------
# Time stretch
# ---------------------------------------------------------------------------
class TestTimeStretch:
    def test_slow_down_extends_duration(self):
        pcm = _sine_pcm(seconds=1.0)
        out = pcm16_time_stretch(pcm, 0.5, sample_rate=RATE)
        assert _n(out) == pytest.approx(2 * _n(pcm), rel=0.03)

    def test_speed_up_shortens_duration(self):
        pcm = _sine_pcm(seconds=1.0)
        out = pcm16_time_stretch(pcm, 2.0, sample_rate=RATE)
        assert _n(out) == pytest.approx(_n(pcm) // 2, rel=0.05)

    def test_rate_identity_returns_input_unchanged(self):
        pcm = _sine_pcm(seconds=0.5)
        assert pcm16_time_stretch(pcm, 1.0, sample_rate=RATE) is pcm

    def test_rate_clamped(self):
        pcm = _sine_pcm(seconds=1.0)
        out = pcm16_time_stretch(pcm, 0.01, sample_rate=RATE)  # -> MIN_RATE
        assert _n(out) == pytest.approx(_n(pcm) / MIN_RATE, rel=0.03)
        out2 = pcm16_time_stretch(pcm, 99.0, sample_rate=RATE)  # -> MAX_RATE
        assert _n(out2) == pytest.approx(_n(pcm) // MAX_RATE, rel=0.05)

    def test_pitch_preserved(self):
        """The whole point of the stretch: 220 Hz stays ~220 Hz (no pitch
        shift) even at 1.5x speed."""
        pcm = _sine_pcm(freq=220.0, seconds=2.0)
        out = pcm16_time_stretch(pcm, 1.5, sample_rate=RATE)
        assert _dominant_freq(out) == pytest.approx(220.0, rel=0.05)

    def test_short_clip_returned_unchanged(self):
        pcm = b"\x01\x00" * 512  # below _MIN_SAMPLES
        assert pcm16_time_stretch(pcm, 0.5, sample_rate=RATE) is pcm


# ---------------------------------------------------------------------------
# Pads / join / resample
# ---------------------------------------------------------------------------
class TestPcmHelpers:
    def test_pad_exact_length(self):
        assert _n(pcm16_pad(120, RATE)) == int(round(RATE * 0.120))
        assert pcm16_pad(0, RATE) == b""
        assert pcm16_pad(-5, RATE) == b""

    def test_join_pads_between_only(self):
        a = b"\x01\x00" * 100
        b = b"\x02\x00" * 200
        c = b"\x03\x00" * 300
        out = pcm16_join([a, b, c], 100, RATE)
        pad_n = int(round(RATE * 0.1))
        assert _n(out) == 100 + 200 + 300 + 2 * pad_n
        # no trailing pad
        assert out.endswith(c)
        # no leading pad
        assert out.startswith(a)
        assert pcm16_join([a], 100, RATE) == a
        assert pcm16_join([], 100, RATE) == b""
        assert pcm16_join([a, b], 0, RATE) == a + b

    def test_resample_length_math(self):
        pcm = _sine_pcm(seconds=1.0, rate=24000)
        out = pcm16_resample_linear(pcm, 24000, RATE)
        assert _n(out) == int(round(24000 * RATE / 24000)) or _n(out) == pytest.approx(
            RATE, rel=0.001
        )

    def test_resample_identity(self):
        pcm = _sine_pcm(seconds=0.5)
        assert pcm16_resample_linear(pcm, RATE, RATE) is pcm

    def test_resample_preserves_waveform(self):
        pcm = _sine_pcm(freq=100.0, seconds=1.0, rate=24000)
        out = pcm16_resample_linear(pcm, 24000, 48000)
        assert _n(out) == pytest.approx(48000, rel=0.001)
        assert _dominant_freq(out, rate=48000) == pytest.approx(100.0, rel=0.05)


# ---------------------------------------------------------------------------
# Policy parsing
# ---------------------------------------------------------------------------
class TestPolicy:
    def test_parse_map(self):
        policies = parse_prosody_map("pcm:0.95:120,yo:0.90")
        assert policies["pcm"] == ProsodyPolicy(rate=0.95, join_pad_ms=120)
        assert policies["yo"] == ProsodyPolicy(rate=0.90, join_pad_ms=0)

    def test_parse_drops_invalid(self):
        policies = parse_prosody_map("pcm:0.95,bogus,:0.8,yo:notanumber")
        assert list(policies) == ["pcm"]

    def test_parse_clamps_rate(self):
        policies = parse_prosody_map("pcm:0.01")
        assert policies["pcm"].rate == MIN_RATE
        policies = parse_prosody_map("pcm:99")
        assert policies["pcm"].rate == MAX_RATE

    def test_policy_for_fallback(self):
        policies = parse_prosody_map("*:0.95,yo:0.90")
        assert policy_for("yo", policies).rate == 0.90
        assert policy_for("ha", policies).rate == 0.95  # "*" fallback
        assert policy_for("yo", None) is None
        assert policy_for("yo", {}) is None

    def test_apply_prosody_identity_passthrough(self):
        pcm = _sine_pcm(seconds=0.5)
        assert apply_prosody(pcm, ProsodyPolicy(), RATE) is pcm
        assert apply_prosody(pcm, None, RATE) is pcm


# ---------------------------------------------------------------------------
# Chain integration
# ---------------------------------------------------------------------------
class FakeProvider:
    """Minimal TTSProvider: synthesize returns a wav wrapping a sine whose
    frequency encodes nothing — length is what matters here."""

    name = "fake"
    default_voice = "fv"

    def __init__(self, rate: int = RATE):
        self._rate = rate
        self.calls: list[str] = []

    async def synthesize(self, text: str, voice: str, language: str) -> bytes:
        self.calls.append(text)
        return _wav(_sine_pcm(seconds=0.5, rate=self._rate), rate=self._rate)

    async def aclose(self) -> None:
        pass


def _chain(prosody_map=None, provider_rate: int = RATE) -> FallbackTTS:
    return FallbackTTS(
        {"fake": FakeProvider(rate=provider_rate)},
        ["fake"],
        sample_rate=RATE,
        prosody_map=prosody_map,
    )


class TestChainIntegration:
    async def test_default_is_identity(self):
        """No prosody map: output is exactly the provider PCM (pre-W106
        behavior, bit-exact)."""
        chain = _chain(prosody_map=None)
        pcm = await chain.synthesize_pcm("hello there")
        assert pcm == _sine_pcm(seconds=0.5, rate=RATE)

    async def test_stretch_applied_for_language(self):
        chain = _chain(
            prosody_map=parse_prosody_map("pcm:0.5"), provider_rate=RATE
        )
        chain.language = "pcm"
        pcm = await chain.synthesize_pcm("how body")
        assert _n(pcm) == pytest.approx(2 * int(RATE * 0.5), rel=0.05)

    async def test_unknown_language_identity(self):
        chain = _chain(prosody_map=parse_prosody_map("pcm:0.5"))
        chain.language = "en"  # no policy
        pcm = await chain.synthesize_pcm("hello")
        assert pcm == _sine_pcm(seconds=0.5, rate=RATE)

    async def test_rate_mismatch_normalized_when_prosody_on(self):
        """Provider at 24k on a 22050 chain: pre-W106 this only warned
        ("audio may be pitched"); with the prosody layer ON it normalizes."""
        chain = _chain(prosody_map=parse_prosody_map(""), provider_rate=24000)
        pcm = await chain.synthesize_pcm("hello")
        assert _n(pcm) == pytest.approx(int(RATE * 0.5), rel=0.01)

    async def test_rate_mismatch_warn_only_when_prosody_off(self):
        chain = _chain(prosody_map=None, provider_rate=24000)
        pcm = await chain.synthesize_pcm("hello")
        assert _n(pcm) == int(24000 * 0.5)  # untouched, as pre-W106

    async def test_stream_join_pads(self):
        chain = _chain(prosody_map=parse_prosody_map("pcm:1.0:100"))
        chain.language = "pcm"
        chunks = [
            c
            async for c in chain.stream_pcm("First sentence here. Second one follows.")
        ]
        assert len(chunks) == 3  # chunk, pad, chunk
        pad_n = int(round(RATE * 0.1))
        assert _n(chunks[1]) == pad_n
        assert chunks[1] == b"\x00\x00" * pad_n
        full = _n(chunks[0]) + pad_n + _n(chunks[2])
        # cached second run is identical
        again = [
            c
            async for c in chain.stream_pcm("First sentence here. Second one follows.")
        ]
        assert sum(_n(c) for c in again) == full

    async def test_stream_no_pads_when_off(self):
        chain = _chain(prosody_map=None)
        chunks = [
            c
            async for c in chain.stream_pcm("First sentence here. Second one follows.")
        ]
        assert len(chunks) == 2  # no pad segments


# ---------------------------------------------------------------------------
# Config + factory
# ---------------------------------------------------------------------------
class TestConfig:
    def test_defaults_off(self):
        settings = Settings()
        assert settings.tts_prosody is False
        assert settings.tts_prosody_map == ""

    def test_env_override(self, monkeypatch):
        monkeypatch.setenv("TTS_PROSODY", "on")
        monkeypatch.setenv("TTS_PROSODY_MAP", "pcm:0.95:120")
        settings = load_settings()
        assert settings.tts_prosody is True
        assert settings.tts_prosody_map == "pcm:0.95:120"

    def test_factory_wires_prosody(self):
        on = build_fallback_tts(
            Settings(tts_prosody=True, tts_prosody_map="pcm:0.95:120")
        )
        assert on._prosody_map is not None
        assert on._prosody_map["pcm"].rate == 0.95
        off = build_fallback_tts(Settings(tts_prosody=False))
        assert off._prosody_map is None
