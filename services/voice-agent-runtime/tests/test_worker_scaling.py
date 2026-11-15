"""Worker-plane scaling (VOICE-SCALING §2): prewarming + load gating.

Skipped when the livekit SDKs are not installed (unit-test env); the pure
prewarm/load logic is exercised against the real WorkerOptions when they are.
"""

from __future__ import annotations

import pytest

lw = pytest.importorskip(
    "app.livekit_worker", reason="livekit SDK not installed in this env"
)
from app.config import Settings  # noqa: E402


@pytest.fixture()
def _clean_prewarm_cache():
    lw._PREWARMED.clear()
    yield
    lw._PREWARMED.clear()


def test_worker_options_scaling_fields():
    opts = lw.build_worker_options(Settings())
    assert opts.num_idle_processes == 2
    assert opts.load_threshold == pytest.approx(0.7)
    assert callable(opts.load_fnc)
    assert callable(opts.prewarm_fnc)
    assert 0.0 <= opts.load_fnc() <= 1.0


def test_worker_options_env_overrides():
    settings = Settings(agent_idle_processes=5, load_threshold=0.5)
    opts = lw.build_worker_options(settings)
    assert opts.num_idle_processes == 5
    assert opts.load_threshold == pytest.approx(0.5)


def test_cpu_load_fnc_returns_unit_interval():
    load = lw.cpu_load_fnc()
    assert 0.0 <= load <= 1.0


def test_prewarm_loads_models_into_cache(monkeypatch, _clean_prewarm_cache):
    class _FakeSTT:
        def __init__(self):
            self.loaded = False

        def preload_sync(self):
            self.loaded = True

    class _FakeTTS:
        def __init__(self):
            self.phrase = None

        async def synthesize_pcm(self, text):
            self.phrase = text
            return b"\x00\x00"

    fake_stt, fake_tts = _FakeSTT(), _FakeTTS()
    monkeypatch.setattr(lw, "_build_stt", lambda settings: fake_stt)
    monkeypatch.setattr(lw, "_build_tts", lambda settings: fake_tts)

    prewarm = lw.make_prewarm_fnc(Settings(preload_models=True))
    prewarm(object())

    assert fake_stt.loaded
    assert fake_tts.phrase == lw.PREWARM_PHRASE
    assert lw._PREWARMED["stt"] is fake_stt
    assert lw._PREWARMED["tts"] is fake_tts


def test_prewarm_disabled_is_noop(monkeypatch, _clean_prewarm_cache):
    monkeypatch.setattr(
        lw,
        "_build_stt",
        lambda settings: pytest.fail("STT built despite PRELOAD_MODELS=false"),
    )
    prewarm = lw.make_prewarm_fnc(Settings(preload_models=False))
    prewarm(object())
    assert lw._PREWARMED == {}


def test_prewarm_failure_degrades_to_lazy(monkeypatch, _clean_prewarm_cache):
    def _boom(settings):
        raise RuntimeError("model cache missing")

    class _FakeTTS:
        async def synthesize_pcm(self, text):
            return b"\x00\x00"

    monkeypatch.setattr(lw, "_build_stt", _boom)
    monkeypatch.setattr(lw, "_build_tts", lambda settings: _FakeTTS())

    prewarm = lw.make_prewarm_fnc(Settings(preload_models=True))
    prewarm(object())  # must not raise

    assert "stt" not in lw._PREWARMED
    assert "tts" in lw._PREWARMED


def test_prewarm_fnc_survives_spawn_pickling():
    """Job processes start with "spawn", which pickles prewarm_fnc: a nested closure failed every job."""
    import pickle

    from app.config import Settings
    from app.livekit_worker import make_prewarm_fnc

    fn = pickle.loads(pickle.dumps(make_prewarm_fnc(Settings(preload_models=False))))
    assert fn(None) is None  # preload off: returns without loading anything


def test_prewarm_releases_clients_bound_to_its_own_loop():
    """The prewarm loop closes after asyncio.run(): its HTTP clients must not outlive it."""
    import asyncio

    from app.livekit_worker import _warmup_then_release

    class Chain:
        closed = False
        order: list = []

        async def synthesize_pcm(self, text):
            return b"\x00\x00"

        async def aclose(self):
            Chain.closed = True

    asyncio.run(_warmup_then_release(Chain()))
    assert Chain.closed
