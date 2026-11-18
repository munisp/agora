"""Model-server watchdog (SPEC-W73 WS-B B2).

Health endpoints can lie: a TTS/LLM sidecar can answer ``/healthz`` from a
wedged inference path. This supervisor probes REAL inference on each
configured model server on an interval — a minimal TTS synthesis for the
voice sidecars, a 1-token generate for ollama — and counts CONSECUTIVE
failures per target (any success resets the counter to zero). On threshold
breach it logs CRITICAL and, only when armed, restarts the container via
the mounted docker socket (unix-socket HTTP, stdlib only).

Safety contract:
- ``VOICE_WATCHDOG_DRY_RUN`` defaults to ``1``: log-only, NEVER restarts.
  Set ``0`` to arm restarts after a dry-run soak (see docs/voice-ops.md).
- The voice-agent-runtime / voice-agent-worker containers are NEVER
  eligible: target entries naming them are dropped at config parse, and the
  docker restart helper re-refuses them (defense in depth).
- The watchdog never exits non-zero because a TARGET failed — it is a
  supervisor, not a gate.

Config (env):
    VOICE_WATCHDOG_TARGETS          csv of name=url (default: piper/mms/xtts/
                                    cosyvoice/ollama on their compose names)
    VOICE_WATCHDOG_INTERVAL_S       seconds between probe rounds (default 30)
    VOICE_WATCHDOG_FAIL_THRESHOLD   consecutive failures before action (3)
    VOICE_WATCHDOG_PROBE_TIMEOUT_S  per-probe timeout (10)
    VOICE_WATCHDOG_DRY_RUN          1 = log-only (default), 0 = restart armed
    VOICE_WATCHDOG_DOCKER_SOCK      docker socket path (/var/run/docker.sock)
    LLM_MODEL / PIPER_VOICE         reused for the ollama/piper probe bodies

Deps: Python stdlib + httpx only. Single file by design (runs from the
runtime image with ``python /ops/voice_watchdog.py``).
"""

from __future__ import annotations

import asyncio
import http.client
import json
import logging
import os
import socket
import sys
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Mapping

import httpx

DEFAULT_TARGETS = (
    "piper=http://piper:5500,"
    "mms=http://mms-tts:5800,"
    "xtts=http://xtts-tts:5810,"
    "cosyvoice=http://cosyvoice-tts:5820,"
    "ollama=http://ollama:11434"
)
DEFAULT_DOCKER_SOCK = "/var/run/docker.sock"

# Containers this watchdog must never restart, no matter what the env says.
PROTECTED_CONTAINERS = frozenset({"voice-agent-runtime", "voice-agent-worker"})

log = logging.getLogger("voice_watchdog")


# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------
@dataclass(frozen=True)
class Target:
    name: str
    url: str


def _env_bool(env: Mapping[str, str], key: str, default: bool) -> bool:
    raw = env.get(key)
    if raw is None:
        return default
    return raw.strip().lower() in ("1", "true", "yes", "on")


def parse_targets(csv: str, logger: logging.Logger = log) -> list[Target]:
    """Parse a `name=url` csv; protected/malformed entries are dropped."""
    targets: list[Target] = []
    for item in (csv or "").split(","):
        item = item.strip()
        if not item:
            continue
        if "=" not in item:
            logger.warning(
                "watchdog target skipped (expected name=url)", extra={"fields": {"entry": item}}
            )
            continue
        name, url = item.split("=", 1)
        name, url = name.strip(), url.strip().rstrip("/")
        if not name or not url:
            logger.warning(
                "watchdog target skipped (empty name/url)", extra={"fields": {"entry": item}}
            )
            continue
        if name in PROTECTED_CONTAINERS:
            logger.warning(
                "watchdog target REFUSED: the voice-agent runtime is never a "
                "restart target",
                extra={"fields": {"target": name}},
            )
            continue
        targets.append(Target(name=name, url=url))
    return targets


@dataclass(frozen=True)
class WatchdogConfig:
    targets: tuple[Target, ...] = ()
    interval_s: float = 30.0
    fail_threshold: int = 3
    probe_timeout_s: float = 10.0
    dry_run: bool = True
    docker_sock: str = DEFAULT_DOCKER_SOCK
    ollama_model: str = "qwen3:8b"
    piper_voice: str = "en_US-lessac-medium"

    @classmethod
    def from_env(
        cls, env: Mapping[str, str] | None = None, logger: logging.Logger = log
    ) -> "WatchdogConfig":
        env = os.environ if env is None else env
        return cls(
            targets=tuple(
                parse_targets(env.get("VOICE_WATCHDOG_TARGETS", DEFAULT_TARGETS), logger)
            ),
            interval_s=float(env.get("VOICE_WATCHDOG_INTERVAL_S", "30")),
            fail_threshold=max(1, int(env.get("VOICE_WATCHDOG_FAIL_THRESHOLD", "3"))),
            probe_timeout_s=float(env.get("VOICE_WATCHDOG_PROBE_TIMEOUT_S", "10")),
            dry_run=_env_bool(env, "VOICE_WATCHDOG_DRY_RUN", True),
            docker_sock=env.get("VOICE_WATCHDOG_DOCKER_SOCK", DEFAULT_DOCKER_SOCK),
            ollama_model=env.get("LLM_MODEL", "qwen3:8b"),
            piper_voice=env.get("PIPER_VOICE", "en_US-lessac-medium"),
        )


# ---------------------------------------------------------------------------
# Probes — REAL inference requests, not /healthz.
# ---------------------------------------------------------------------------
class ProbeError(RuntimeError):
    """A probe reached the target but the response was not usable inference."""


def probe_kind(name: str) -> str:
    """Probe contract by target name (docs/voices-sidecars.md)."""
    n = name.lower()
    if "ollama" in n:
        return "ollama"
    if "piper" in n:
        return "piper"
    if "mms" in n:
        return "mms"
    return "cloning_tts"  # xtts / cosyvoice / any other TTS sidecar


def _require_audio(resp: httpx.Response, target: Target) -> None:
    resp.raise_for_status()
    ctype = resp.headers.get("content-type", "")
    if not resp.content or ("audio" not in ctype and "octet-stream" not in ctype):
        raise ProbeError(
            f"{target.name}: expected audio response, got "
            f"{resp.status_code} content-type={ctype!r} bytes={len(resp.content)}"
        )


async def probe_target(
    client: httpx.AsyncClient, target: Target, cfg: WatchdogConfig
) -> None:
    """One real-inference probe; raises (or times out) on failure."""
    kind = probe_kind(target.name)
    if kind == "ollama":
        resp = await client.post(
            f"{target.url}/api/generate",
            json={
                "model": cfg.ollama_model,
                "prompt": "ping",
                "stream": False,
                "options": {"num_predict": 1},
            },
        )
        resp.raise_for_status()
        if not resp.content:
            raise ProbeError(f"{target.name}: empty generate response")
        return
    if kind == "piper":
        # Piper sidecar contract: POST /speak {"text", "voice"} -> audio/wav.
        resp = await client.post(
            f"{target.url}/speak",
            json={"text": "ping", "voice": cfg.piper_voice},
        )
        _require_audio(resp, target)
        return
    if kind == "mms":
        # mms-tts contract: POST /tts {"text", "lang"} -> audio/wav.
        resp = await client.post(
            f"{target.url}/tts", json={"text": "ping", "lang": "eng"}
        )
        _require_audio(resp, target)
        return
    # Cloning sidecars (xtts / cosyvoice): /tts 404s on an unknown voice_id,
    # so discover an enrolled voice first — the /voices round-trip itself
    # proves the app tier is serving. With an empty registry there is no
    # inference to probe (enrollment is operator-gated); that is logged and
    # treated as alive.
    resp = await client.get(f"{target.url}/voices")
    resp.raise_for_status()
    voices = (resp.json() or {}).get("voices") or []
    if not voices:
        log.info(
            "no enrolled voices; liveness-only probe",
            extra={"fields": {"target": target.name}},
        )
        return
    voice_id = voices[0].get("voice_id") or voices[0].get("id")
    resp = await client.post(
        f"{target.url}/tts",
        json={"text": "ping", "voice_id": voice_id, "language": "en"},
    )
    _require_audio(resp, target)


# ---------------------------------------------------------------------------
# Docker restart over the unix socket (stdlib http.client only).
# ---------------------------------------------------------------------------
class _UnixHTTPConnection(http.client.HTTPConnection):
    """Minimal HTTP-over-unix-socket connection for the docker API."""

    def __init__(self, sock_path: str, timeout: float) -> None:
        super().__init__("localhost", timeout=timeout)
        self._sock_path = sock_path

    def connect(self) -> None:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(self.timeout)
        sock.connect(self._sock_path)
        self.sock = sock


def docker_restart(
    container: str,
    *,
    sock_path: str = DEFAULT_DOCKER_SOCK,
    timeout: float = 10.0,
) -> None:
    """`docker restart <container>` via the unix socket API.

    Hard-refuses protected containers (defense in depth behind the
    config-parse filter). Raises on a non-204 API response.
    """
    if container in PROTECTED_CONTAINERS:
        raise ValueError(f"refusing to restart protected container {container!r}")
    conn = _UnixHTTPConnection(sock_path, timeout)
    try:
        conn.request(
            "POST",
            f"/containers/{container}/restart",
            body=b"",
            headers={"Content-Length": "0"},
        )
        resp = conn.getresponse()
        body = resp.read()
        if resp.status != 204:
            raise RuntimeError(
                f"docker restart {container}: HTTP {resp.status} "
                f"{body.decode(errors='replace')[:200]}"
            )
    finally:
        conn.close()


# ---------------------------------------------------------------------------
# Watchdog core
# ---------------------------------------------------------------------------
@dataclass
class Watchdog:
    """Consecutive-failure tracking + breach handling for all targets."""

    config: WatchdogConfig
    restart_fn: Callable[..., None] = docker_restart
    logger: logging.Logger = log
    _failures: dict[str, int] = field(default_factory=dict)

    def __post_init__(self) -> None:
        for t in self.config.targets:
            self._failures.setdefault(t.name, 0)

    def failure_count(self, name: str) -> int:
        return self._failures.get(name, 0)

    async def check_once(self, client: httpx.AsyncClient) -> dict[str, bool]:
        """Probe every target once; returns {name: ok}. Never raises on a
        target failure (a down sidecar is data, not a supervisor error)."""
        results: dict[str, bool] = {}
        for target in self.config.targets:
            try:
                await probe_target(client, target, self.config)
            except Exception as exc:  # noqa: BLE001 - any failure counts
                results[target.name] = False
                self._record_failure(target, exc)
            else:
                results[target.name] = True
                self._record_success(target)
        return results

    def _record_success(self, target: Target) -> None:
        if self._failures.get(target.name):
            self.logger.info(
                "target recovered",
                extra={"fields": {"target": target.name}},
            )
        self._failures[target.name] = 0

    def _record_failure(self, target: Target, exc: BaseException) -> None:
        count = self._failures.get(target.name, 0) + 1
        self._failures[target.name] = count
        fields: dict[str, Any] = {
            "target": target.name,
            "url": target.url,
            "consecutive_failures": count,
            "threshold": self.config.fail_threshold,
            "error": str(exc)[:300],
        }
        if count >= self.config.fail_threshold:
            self._handle_breach(target, count, fields)
        else:
            self.logger.warning("inference probe failed", extra={"fields": fields})

    def _handle_breach(
        self, target: Target, count: int, fields: dict[str, Any]
    ) -> None:
        # Reset after each breach handling so a wedged container re-alerts
        # every `threshold` failures instead of every interval.
        self._failures[target.name] = 0
        fields = {**fields, "container": target.name}
        if self.config.dry_run:
            self.logger.critical(
                "WATCHDOG BREACH: would restart container (dry-run, no action)",
                extra={"fields": {**fields, "action": "would_restart"}},
            )
            return
        self.logger.critical(
            "WATCHDOG BREACH: restarting container",
            extra={"fields": {**fields, "action": "restart"}},
        )
        try:
            self.restart_fn(target.name, sock_path=self.config.docker_sock)
        except Exception as exc:  # noqa: BLE001 - restart failure is logged
            self.logger.exception(
                "container restart failed",
                extra={"fields": {**fields, "error": str(exc)[:300]}},
            )


async def supervise(config: WatchdogConfig, watchdog: Watchdog | None = None) -> None:
    """Probe loop. Runs forever until cancelled / KeyboardInterrupt."""
    wd = watchdog or Watchdog(config)
    timeout = httpx.Timeout(config.probe_timeout_s)
    async with httpx.AsyncClient(timeout=timeout) as client:
        while True:
            started = time.monotonic()
            await wd.check_once(client)
            elapsed = time.monotonic() - started
            await asyncio.sleep(max(0.0, config.interval_s - elapsed))


class _JsonFormatter(logging.Formatter):
    """Single-line JSON logs (structured events for log shipping)."""

    def format(self, record: logging.LogRecord) -> str:
        payload = {
            "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(record.created)),
            "level": record.levelname.lower(),
            "event": record.getMessage(),
        }
        payload.update(getattr(record, "fields", {}) or {})
        if record.exc_info:
            payload["exception"] = self.formatException(record.exc_info)
        return json.dumps(payload, ensure_ascii=False)


def _configure_logging() -> None:
    handler = logging.StreamHandler(sys.stdout)
    handler.setFormatter(_JsonFormatter())
    log.setLevel(logging.INFO)
    log.addHandler(handler)
    log.propagate = False


def main() -> int:
    _configure_logging()
    cfg = WatchdogConfig.from_env()
    log.info(
        "voice watchdog starting",
        extra={
            "fields": {
                "targets": [t.name for t in cfg.targets],
                "interval_s": cfg.interval_s,
                "fail_threshold": cfg.fail_threshold,
                "probe_timeout_s": cfg.probe_timeout_s,
                "dry_run": cfg.dry_run,
            }
        },
    )
    if not cfg.targets:
        log.warning("no watchdog targets configured; idling")
    try:
        asyncio.run(supervise(cfg))
    except KeyboardInterrupt:
        log.info("voice watchdog stopping")
    return 0


if __name__ == "__main__":
    sys.exit(main())
