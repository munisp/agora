"""Tiny Dapr HTTP helper (httpx) — service invocation only.

Duplicated per OpenDesk Python service on purpose (no shared top-level
package): sidecar endpoints are stable, so a ~40-line client is cheaper
than a dependency. Mirrors knowledge-service/app/dapr_client.py.
"""

from __future__ import annotations

from typing import Any

import httpx
import structlog

log = structlog.get_logger()


class DaprError(RuntimeError):
    """Raised when the Dapr sidecar returns a non-success status."""

    def __init__(self, message: str, status_code: int | None = None):
        super().__init__(message)
        self.status_code = status_code


class DaprClient:
    def __init__(self, host: str = "localhost", http_port: int = 3500, timeout: float = 10.0):
        self._base = f"http://{host}:{http_port}/v1.0"
        self._client = httpx.AsyncClient(timeout=timeout)

    async def aclose(self) -> None:
        await self._client.aclose()

    async def invoke(
        self,
        app_id: str,
        method: str,
        *,
        json_body: Any | None = None,
        params: dict[str, str] | None = None,
        headers: dict[str, str] | None = None,
        http_method: str | None = None,
    ) -> Any:
        """Invoke `method` on `app_id` via POST (or GET when no body)."""
        verb = http_method or ("GET" if json_body is None else "POST")
        url = f"{self._base}/invoke/{app_id}/method/{method.lstrip('/')}"
        resp = await self._client.request(
            verb, url, json=json_body, params=params, headers=headers
        )
        if resp.status_code >= 400:
            raise DaprError(
                f"invoke {app_id}/{method}: {resp.status_code} {resp.text[:400]}",
                status_code=resp.status_code,
            )
        if not resp.content:
            return None
        ct = resp.headers.get("content-type", "")
        return resp.json() if "json" in ct else resp.text
