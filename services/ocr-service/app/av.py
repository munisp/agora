"""Hand-rolled clamd zINSTREAM client (SPEC-W59 P1).

~60 lines, stdlib asyncio only — the same "a ~60-line client is cheaper
than a dependency" idiom as dapr_client.py; requirements.txt untouched.
clamd/clamav:1.4.3 sidecar on the internal network (no unix-socket volume
coupling, no PyPI clamd — unmaintained).

Protocol: `zINSTREAM\0`, then chunks framed as 4-byte big-endian length +
payload (2 MiB frames), zero-length terminator. Reply is one line:
`stream: OK` / `stream: <signature> FOUND` / `... ERROR`. An ERROR
(including `INSTREAM size limit exceeded`) is a scan ERROR — the fail
posture rules apply; it is NEVER treated as clean (SPEC-W59 P3).
"""

from __future__ import annotations

import asyncio
import struct
from dataclasses import dataclass

_CHUNK = 2 * 1024 * 1024  # 2 MiB zINSTREAM frames


class AvUnavailable(RuntimeError):
    """clamd unreachable / timeout / protocol or scan error."""


class InfectedError(RuntimeError):
    """Scan verdict FOUND — the route layer maps this to HTTP 415."""

    def __init__(self, signature: str):
        super().__init__(f"rejected by malware scan: {signature}")
        self.signature = signature


@dataclass(frozen=True)
class Clean:
    """Scan verdict: no signature matched."""


@dataclass(frozen=True)
class Infected:
    """Scan verdict: malware signature matched (name only — no bytes)."""

    signature: str


def _parse_reply(reply: bytes) -> Clean | Infected:
    text = reply.decode("utf-8", "replace").strip()
    if not text.startswith("stream:"):
        raise AvUnavailable(f"unexpected clamd reply: {text[:200]!r}")
    body = text[len("stream:"):].strip()
    if body == "OK":
        return Clean()
    if body.endswith(" FOUND"):
        return Infected(signature=body[: -len(" FOUND")].strip())
    raise AvUnavailable(f"clamd scan error: {text[:200]}")


class AvClient:
    """One TCP connection per scan (clamd handles concurrency server-side)."""

    def __init__(self, host: str, port: int = 3310, timeout_s: float = 30.0):
        self._host = host
        self._port = port
        self._timeout = timeout_s

    async def aclose(self) -> None:
        """Connections are per-scan; nothing persistent to close."""

    async def scan(self, data: bytes) -> Clean | Infected:
        try:
            return await asyncio.wait_for(self._scan(data), timeout=self._timeout)
        except (TimeoutError, OSError, EOFError, ValueError) as exc:
            raise AvUnavailable(f"clamd scan failed: {exc}") from exc

    async def _scan(self, data: bytes) -> Clean | Infected:
        reader, writer = await asyncio.open_connection(self._host, self._port)
        try:
            writer.write(b"zINSTREAM\0")
            for off in range(0, len(data), _CHUNK):
                chunk = data[off : off + _CHUNK]
                writer.write(struct.pack(">I", len(chunk)) + chunk)
            writer.write(struct.pack(">I", 0))
            await writer.drain()
            reply = await reader.readline()
        finally:
            writer.close()
        if not reply:
            raise AvUnavailable("clamd closed the connection without a reply")
        return _parse_reply(reply)
