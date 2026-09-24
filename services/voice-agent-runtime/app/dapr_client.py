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
    """Dapr HTTP client with optional per-app direct-base overrides.

    SPEC-W46 P-02 (mirrors messaging-gateway ResolveInvokeBase): when a
    direct base is registered for an app-id (e.g. ``booking`` ->
    ``http://booking:7002``), invoke calls go straight to the service and
    skip the voice->daprd->booking hop. A TRANSPORT failure (connect /
    timeout — service unreachable) falls back to the daprd invoke API;
    application-level HTTP errors are returned/raised as-is (daprd would
    surface the same response).
    """

    def __init__(
        self,
        base_url: str,
        timeout_s: float = 15.0,
        *,
        direct_bases: dict[str, str] | None = None,
    ) -> None:
        self._base = base_url.rstrip("/")
        self._direct = {
            app_id: base.rstrip("/")
            for app_id, base in (direct_bases or {}).items()
            if base
        }
        self._client = httpx.AsyncClient(timeout=httpx.Timeout(timeout_s))

    async def aclose(self) -> None:
        await self._client.aclose()

    def _direct_url(self, app_id: str, method: str) -> str | None:
        base = self._direct.get(app_id)
        if base is None:
            return None
        return f"{base}/{method.lstrip('/')}"

    # -- service invocation -------------------------------------------------
    async def _request(
        self,
        verb: str,
        app_id: str,
        method: str,
        *,
        params: dict[str, str] | None = None,
        payload: Any | None = None,
        headers: dict[str, str] | None = None,
    ) -> Any:
        """One invoke round: direct base first (P-02), daprd fallback on
        transport failure. Raises DaprError on HTTP >= 300."""
        kwargs: dict[str, Any] = {"params": params, "headers": headers}
        if verb in ("POST", "PUT"):
            kwargs["json"] = payload
        direct = self._direct_url(app_id, method)
        if direct is not None:
            try:
                resp = await self._client.request(verb, direct, **kwargs)
            except httpx.TransportError as exc:
                log.warning(
                    "direct invoke unreachable; falling back to daprd",
                    app_id=app_id,
                    method=method,
                    error=str(exc)[:200],
                )
            else:
                if resp.status_code >= 300:
                    raise DaprError(
                        f"invoke {verb} {app_id}/{method} (direct): status "
                        f"{resp.status_code}: {resp.text[:512]}"
                    )
                if not resp.content:
                    return None
                return resp.json()
        url = f"{self._base}/v1.0/invoke/{app_id}/method/{method.lstrip('/')}"
        resp = await self._client.request(verb, url, **kwargs)
        if resp.status_code >= 300:
            raise DaprError(
                f"invoke {verb} {app_id}/{method}: status {resp.status_code}: {resp.text[:512]}"
            )
        if not resp.content:
            return None
        return resp.json()

    async def invoke_get(
        self,
        app_id: str,
        method: str,
        *,
        params: dict[str, str] | None = None,
        headers: dict[str, str] | None = None,
    ) -> Any:
        """GET /v1.0/invoke/{app_id}/method/{method} (query params supported)."""
        return await self._request("GET", app_id, method, params=params, headers=headers)

    async def invoke_post(
        self,
        app_id: str,
        method: str,
        *,
        payload: Any | None = None,
        headers: dict[str, str] | None = None,
    ) -> Any:
        """POST /v1.0/invoke/{app_id}/method/{method}."""
        return await self._request("POST", app_id, method, payload=payload, headers=headers)

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
        return await self._request("PUT", app_id, method, payload=payload, headers=headers)

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
