"""Write-back via the graph-service internal API (SPEC-W29 §3 WS-A).

This is the ONLY write path — graph-ml never writes FalkorDB directly, so
audit/validation/tenant-match enforcement live in graph-service (§4 gate 3).
Payloads are POSTed with the ``X-Internal-Token`` header, chunked at 500
items, strictly per-tenant (a chunk never mixes tenants).
"""

from __future__ import annotations

import logging
from typing import Any, Iterable, Protocol

import httpx

log = logging.getLogger(__name__)

SCORES_PATH = "/v1/graph/internal/scores"
RECOMMENDATIONS_PATH = "/v1/graph/internal/recommendations"
DEFAULT_CHUNK_SIZE = 500


def chunked(items: list[dict[str, Any]], size: int = DEFAULT_CHUNK_SIZE) -> Iterable[list[dict[str, Any]]]:
    if size <= 0:
        raise ValueError("chunk size must be positive")
    for start in range(0, len(items), size):
        yield items[start : start + size]


class WritebackClient(Protocol):
    def post_scores(self, tenant_id: str, scores: list[dict[str, Any]]) -> int: ...

    def post_recommendations(self, tenant_id: str, recommendations: list[dict[str, Any]]) -> int: ...

    def close(self) -> None: ...


class HttpWritebackClient:
    """httpx POSTer for the graph-service internal score/recommendation API."""

    def __init__(
        self,
        base_url: str,
        internal_token: str,
        chunk_size: int = DEFAULT_CHUNK_SIZE,
        timeout_s: float = 30.0,
        client: httpx.Client | None = None,
    ) -> None:
        if not internal_token:
            raise ValueError("INTERNAL_TOKEN is required for write-back")
        self._chunk_size = chunk_size
        self._owns_client = client is None
        self._client = client or httpx.Client(base_url=base_url, timeout=timeout_s)
        self._headers = {"X-Internal-Token": internal_token}

    def close(self) -> None:
        if self._owns_client:
            self._client.close()

    def _post_chunked(self, path: str, tenant_id: str, key: str, items: list[dict[str, Any]]) -> int:
        if not tenant_id:
            raise ValueError("tenant_id is required on every write-back")
        # Defense in depth: a chunk must never carry another tenant's rows.
        foreign = [i for i in items if i.get("tenant_id") not in (None, tenant_id)]
        if foreign:
            raise ValueError(
                f"cross-tenant write-back refused: {len(foreign)} items not in {tenant_id}"
            )
        written = 0
        for chunk in chunked(items, self._chunk_size):
            body = {"tenant_id": tenant_id, key: chunk}
            resp = self._client.post(path, json=body, headers=self._headers)
            resp.raise_for_status()
            written += len(chunk)
            log.info(
                "write-back chunk posted", extra={"path": path, "tenant_id": tenant_id, "items": len(chunk)}
            )
        return written

    def post_scores(self, tenant_id: str, scores: list[dict[str, Any]]) -> int:
        return self._post_chunked(SCORES_PATH, tenant_id, "scores", scores)

    def post_recommendations(self, tenant_id: str, recommendations: list[dict[str, Any]]) -> int:
        return self._post_chunked(RECOMMENDATIONS_PATH, tenant_id, "recommendations", recommendations)
