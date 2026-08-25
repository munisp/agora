"""Minimal Dapr HTTP API client (httpx-based): pub/sub publish.

Mirrors the tiny net/http helper used by the Go services; no Dapr SDK needed.
"""

from __future__ import annotations

from typing import Any

import httpx

from .logging import get_logger

log = get_logger(__name__)


class DaprError(RuntimeError):
    """Raised when the Dapr sidecar (or the invoked app) returns non-2xx."""

    def __init__(self, message: str, status_code: int | None = None):
        super().__init__(message)
        self.status_code = status_code


class DaprClient:
    def __init__(self, host: str, port: int, pubsub_name: str = "pubsub-kafka") -> None:
        self._base = f"http://{host}:{port}"
        self._pubsub = pubsub_name
        self._client = httpx.AsyncClient(timeout=15.0)

    async def close(self) -> None:
        await self._client.aclose()

    async def invoke(
        self,
        app_id: str,
        method: str,
        *,
        params: dict[str, str] | None = None,
        headers: dict[str, str] | None = None,
    ) -> Any:
        """GET-invoke `method` on `app_id` via the sidecar (service
        invocation building block). Raises DaprError on non-2xx so callers
        can map 404 vs outage (mirrors analytics-pipeline's client)."""
        url = f"{self._base}/v1.0/invoke/{app_id}/method/{method.lstrip('/')}"
        resp = await self._client.get(url, params=params, headers=headers)
        if resp.status_code >= 400:
            raise DaprError(
                f"invoke {app_id}/{method}: {resp.status_code} {resp.text[:400]}",
                status_code=resp.status_code,
            )
        if not resp.content:
            return None
        ct = resp.headers.get("content-type", "")
        return resp.json() if "json" in ct else resp.text

    async def publish_event(self, topic: str, event: dict[str, Any]) -> None:
        """Publish a CloudEvents envelope to a pubsub component topic.

        Uses application/cloudevents+json so daprd forwards the envelope as-is.
        Raises on non-2xx so callers can decide (log/retry).
        """
        url = f"{self._base}/v1.0/publish/{self._pubsub}/{topic}"
        resp = await self._client.post(
            url,
            json=event,
            headers={"Content-Type": "application/cloudevents+json"},
        )
        if resp.status_code >= 300:
            raise RuntimeError(
                f"dapr publish {self._pubsub}/{topic} failed: "
                f"status={resp.status_code} body={resp.text[:256]}"
            )
