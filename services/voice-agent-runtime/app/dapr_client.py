"""Minimal Dapr HTTP API client (httpx), mirroring the Go services' daprc
package: service invocation + pub/sub publish against the daprd sidecar."""

from __future__ import annotations

from typing import Any

import httpx

from .logging import get_logger

log = get_logger("dapr")


class DaprError(RuntimeError):
    pass


class DaprClient:
    def __init__(self, base_url: str, timeout_s: float = 15.0) -> None:
        self._base = base_url.rstrip("/")
        self._client = httpx.AsyncClient(timeout=httpx.Timeout(timeout_s))

    async def aclose(self) -> None:
        await self._client.aclose()

    # -- service invocation -------------------------------------------------
    async def invoke_get(
        self,
        app_id: str,
        method: str,
        *,
        params: dict[str, str] | None = None,
        headers: dict[str, str] | None = None,
    ) -> Any:
        """GET /v1.0/invoke/{app_id}/method/{method} (query params supported)."""
        url = f"{self._base}/v1.0/invoke/{app_id}/method/{method.lstrip('/')}"
        resp = await self._client.get(url, params=params, headers=headers)
        if resp.status_code >= 300:
            raise DaprError(
                f"invoke GET {app_id}/{method}: status {resp.status_code}: {resp.text[:512]}"
            )
        if not resp.content:
            return None
        return resp.json()

    async def invoke_post(
        self,
        app_id: str,
        method: str,
        *,
        payload: Any | None = None,
        headers: dict[str, str] | None = None,
    ) -> Any:
        """POST /v1.0/invoke/{app_id}/method/{method}."""
        url = f"{self._base}/v1.0/invoke/{app_id}/method/{method.lstrip('/')}"
        resp = await self._client.post(url, json=payload, headers=headers)
        if resp.status_code >= 300:
            raise DaprError(
                f"invoke POST {app_id}/{method}: status {resp.status_code}: {resp.text[:512]}"
            )
        if not resp.content:
            return None
        return resp.json()

    async def invoke_put(
        self,
        app_id: str,
        method: str,
        *,
        payload: Any | None = None,
        headers: dict[str, str] | None = None,
    ) -> Any:
        """PUT /v1.0/invoke/{app_id}/method/{method} (SPEC-W11 Part C:
        booking-service PUT /v1/contacts/{id}/location)."""
        url = f"{self._base}/v1.0/invoke/{app_id}/method/{method.lstrip('/')}"
        resp = await self._client.put(url, json=payload, headers=headers)
        if resp.status_code >= 300:
            raise DaprError(
                f"invoke PUT {app_id}/{method}: status {resp.status_code}: {resp.text[:512]}"
            )
        if not resp.content:
            return None
        return resp.json()

    # -- pub/sub ------------------------------------------------------------
    async def publish(self, pubsub: str, topic: str, event: dict[str, Any]) -> None:
        """POST /v1.0/publish/{pubsub}/{topic} with a CloudEvents envelope.

        Content-Type application/cloudevents+json makes daprd forward the
        envelope as-is (same convention as the Go services).
        """
        url = f"{self._base}/v1.0/publish/{pubsub}/{topic}"
        resp = await self._client.post(
            url, json=event, headers={"Content-Type": "application/cloudevents+json"}
        )
        if resp.status_code >= 300:
            raise DaprError(
                f"publish {pubsub}/{topic}: status {resp.status_code}: {resp.text[:512]}"
            )

    async def publish_best_effort(
        self, pubsub: str, topic: str, event: dict[str, Any], *, kind: str
    ) -> bool:
        """Publish, logging instead of raising on failure (event outbox)."""
        try:
            await self.publish(pubsub, topic, event)
            return True
        except Exception as exc:  # noqa: BLE001 - logged + counted upstream
            log.warning("dapr publish failed", kind=kind, topic=topic, error=str(exc))
            return False
