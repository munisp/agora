"""Tests for ops/voice_watchdog.py (SPEC-W73 WS-B B6 d-h).

(d) consecutive failures increment, success resets;
(e) threshold breach in DRY_RUN=1 -> would-restart log, NO docker call;
(f) DRY_RUN=0 -> exactly one restart call to the right container;
(g) probe timeout counted as failure;
(h) the voice-agent runtime containers are never targets, even if listed.

All offline: probes run against httpx.MockTransport; the docker socket
client is a fake callable; nothing touches a real socket or daemon.
"""

from __future__ import annotations

import logging
import sys
from pathlib import Path

import httpx
import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "ops"))

import voice_watchdog as vw  # noqa: E402

WAV = b"RIFF$\x00\x00\x00WAVEfmt " + b"\x00" * 32  # dummy audio payload


def _cfg(**overrides) -> vw.WatchdogConfig:
    base = dict(
        targets=(vw.Target("piper", "http://piper:5500"),),
        interval_s=30.0,
        fail_threshold=3,
        probe_timeout_s=10.0,
        dry_run=True,
    )
    base.update(overrides)
    return vw.WatchdogConfig(**base)


def _client(handler) -> httpx.AsyncClient:
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


def _ok(_req: httpx.Request) -> httpx.Response:
    return httpx.Response(200, content=WAV, headers={"content-type": "audio/wav"})


def _fail(_req: httpx.Request) -> httpx.Response:
    return httpx.Response(500, content=b"boom")


class _RestartRecorder:
    def __init__(self) -> None:
        self.calls: list[tuple] = []

    def __call__(self, container, **kwargs) -> None:
        self.calls.append((container, kwargs))


# (d) consecutive failures increment, success resets -------------------------
async def test_d_consecutive_failures_increment_and_success_resets():
    wd = vw.Watchdog(_cfg(fail_threshold=5))
    async with _client(_fail) as client:
        for expected in (1, 2, 3):
            await wd.check_once(client)
            assert wd.failure_count("piper") == expected
    async with _client(_ok) as client:
        results = await wd.check_once(client)
    assert results == {"piper": True}
    assert wd.failure_count("piper") == 0


# (e) threshold breach in DRY_RUN=1 -> would-restart log, NO docker call -----
async def test_e_dry_run_breach_logs_would_restart_and_never_calls_docker(caplog):
    restart = _RestartRecorder()
    wd = vw.Watchdog(_cfg(fail_threshold=2, dry_run=True), restart_fn=restart)
    with caplog.at_level(logging.CRITICAL, logger="voice_watchdog"):
        async with _client(_fail) as client:
            await wd.check_once(client)
            assert restart.calls == []
            await wd.check_once(client)
    assert restart.calls == []  # dry-run: the docker socket is never touched
    breaches = [
        r for r in caplog.records
        if r.levelno == logging.CRITICAL and "would restart" in r.getMessage()
    ]
    assert len(breaches) == 1
    assert breaches[0].fields["target"] == "piper"
    assert breaches[0].fields["action"] == "would_restart"


# (f) DRY_RUN=0 -> exactly one restart call to the right container -----------
async def test_f_armed_breach_calls_restart_once_for_the_right_container():
    restart = _RestartRecorder()
    wd = vw.Watchdog(_cfg(fail_threshold=2, dry_run=False), restart_fn=restart)
    async with _client(_fail) as client:
        await wd.check_once(client)
        assert restart.calls == []  # below threshold
        await wd.check_once(client)
        assert len(restart.calls) == 1
        container, kwargs = restart.calls[0]
        assert container == "piper"  # the failing target's container
        assert "sock_path" in kwargs
        # Counter reset after the breach: one more failure stays below the
        # threshold -> still exactly one restart call.
        await wd.check_once(client)
        assert len(restart.calls) == 1


# (g) probe timeout counted as failure ---------------------------------------
async def test_g_probe_timeout_counts_as_failure():
    def _slow(_req: httpx.Request) -> httpx.Response:
        raise httpx.ReadTimeout("sidecar wedged")

    wd = vw.Watchdog(_cfg(fail_threshold=5))
    async with _client(_slow) as client:
        results = await wd.check_once(client)
    assert results == {"piper": False}
    assert wd.failure_count("piper") == 1


# (h) never targets the runtime container even if listed ---------------------
def test_h_runtime_containers_are_never_targets():
    env = {
        "VOICE_WATCHDOG_TARGETS": (
            "voice-agent-runtime=http://voice-agent-runtime:7006,"
            "voice-agent-worker=http://voice-agent-worker:8081,"
            "piper=http://piper:5500"
        )
    }
    cfg = vw.WatchdogConfig.from_env(env)
    names = [t.name for t in cfg.targets]
    assert names == ["piper"]
    # Defense in depth: the docker helper hard-refuses even if called directly.
    with pytest.raises(ValueError):
        vw.docker_restart("voice-agent-runtime", sock_path="/nonexistent.sock")
    with pytest.raises(ValueError):
        vw.docker_restart("voice-agent-worker", sock_path="/nonexistent.sock")


# Supporting contract checks ---------------------------------------------------
def test_defaults_cover_all_five_sidecars_and_are_dry_run():
    cfg = vw.WatchdogConfig.from_env({})
    assert [t.name for t in cfg.targets] == [
        "piper",
        "mms",
        "xtts",
        "cosyvoice",
        "ollama",
    ]
    assert cfg.dry_run is True
    assert cfg.interval_s == 30.0
    assert cfg.fail_threshold == 3
    assert cfg.probe_timeout_s == 10.0


def test_dry_run_env_parsing():
    assert vw.WatchdogConfig.from_env({"VOICE_WATCHDOG_DRY_RUN": "0"}).dry_run is False
    assert vw.WatchdogConfig.from_env({"VOICE_WATCHDOG_DRY_RUN": "1"}).dry_run is True
    assert vw.WatchdogConfig.from_env({}).dry_run is True


async def test_cloning_sidecar_probe_discovers_enrolled_voice():
    seen: list[tuple[str, dict | None]] = []

    def handler(req: httpx.Request) -> httpx.Response:
        if req.url.path == "/voices":
            seen.append(("voices", None))
            return httpx.Response(200, json={"voices": [{"voice_id": "v-1"}]})
        body = req.read()
        seen.append(("tts", body))
        return httpx.Response(
            200, content=WAV, headers={"content-type": "audio/wav"}
        )

    cfg = _cfg(targets=(vw.Target("cosyvoice", "http://cosyvoice-tts:5820"),))
    wd = vw.Watchdog(cfg)
    async with _client(handler) as client:
        results = await wd.check_once(client)
    assert results == {"cosyvoice": True}
    assert seen[0][0] == "voices"
    assert b'"v-1"' in seen[1][1]  # real /tts inference probe with the voice


async def test_ollama_probe_uses_tiny_generate():
    seen: list[bytes] = []

    def handler(req: httpx.Request) -> httpx.Response:
        assert req.url.path == "/api/generate"
        seen.append(req.read())
        return httpx.Response(200, json={"response": "pong"})

    cfg = _cfg(targets=(vw.Target("ollama", "http://ollama:11434"),))
    wd = vw.Watchdog(cfg)
    async with _client(handler) as client:
        results = await wd.check_once(client)
    assert results == {"ollama": True}
    assert b'"num_predict": 1' in seen[0] or b'"num_predict":1' in seen[0]
