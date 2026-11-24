"""SPEC-W117: OpenSearch-backed advanced search over the `conversations`
turn index (mapping: infra/opensearch/setup-indices.sh; fed by the W10
TranscriptIndexer).

Tenant isolation is enforced INSIDE the query (mandatory tenant_id term
filter) — never post-filtered client-side. OpenSearch outages answer a
loud 503 (fail-closed); there is deliberately no silent Postgres LIKE
fallback, which would return different semantics under the same URL.
"""

from __future__ import annotations

import uuid
from datetime import datetime
from typing import Annotated, Any

from fastapi import APIRouter, Depends, HTTPException, Query, Request, status

from .logging import get_logger
from .routes import _require_tenant

log = get_logger(__name__)

router = APIRouter()

#: roles the turn writer emits (routes.py Turn model)
ALLOWED_ROLES = ("user", "agent", "tool", "system")
MAX_LIMIT = 100
MAX_WINDOW = 10000  # OpenSearch index.max_result_window default


def _parse_ts(raw: str, name: str) -> str:
    """Validate an ISO-8601 timestamp; returns the original string (the
    index `ts` field is a date — OpenSearch parses it). 400 on junk."""
    candidate = raw.strip()
    if candidate.endswith("Z"):
        candidate = candidate[:-1] + "+00:00"
    try:
        datetime.fromisoformat(candidate)
    except ValueError:
        raise HTTPException(
            status.HTTP_400_BAD_REQUEST, f"{name} must be ISO-8601"
        ) from None
    return raw


def build_search_query(
    *,
    tenant_id: str,
    q: str | None = None,
    channel: str | None = None,
    site_slug: str | None = None,
    role: str | None = None,
    conversation_id: str | None = None,
    from_ts: str | None = None,
    to_ts: str | None = None,
    limit: int,
    offset: int,
) -> dict[str, Any]:
    """Pure OpenSearch query builder (unit-tested without a cluster).

    The tenant_id term filter is ALWAYS the first filter clause — that is
    the isolation boundary and must never be conditional."""
    filters: list[dict[str, Any]] = [{"term": {"tenant_id": tenant_id}}]
    if channel:
        filters.append({"term": {"channel": channel}})
    if site_slug:
        filters.append({"term": {"site_slug": site_slug}})
    if role:
        filters.append({"term": {"role": role}})
    if conversation_id:
        filters.append({"term": {"conversation_id": conversation_id}})
    if from_ts or to_ts:
        rng: dict[str, Any] = {}
        if from_ts:
            rng["gte"] = from_ts
        if to_ts:
            rng["lte"] = to_ts
        filters.append({"range": {"ts": rng}})

    query: dict[str, Any] = {"bool": {"filter": filters}}
    if q and q.strip():
        query["bool"]["must"] = [
            {"match": {"text": {"query": q.strip(), "operator": "and",
                                "fuzziness": "AUTO"}}}
        ]
    else:
        query["bool"]["must"] = [{"match_all": {}}]

    return {
        "size": limit,
        "from": offset,
        "track_total_hits": True,
        "sort": [{"ts": {"order": "desc"}}, {"_doc": {"order": "desc"}}],
        "query": query,
        "_source": ["conversation_id", "site_slug", "channel", "role",
                    "text", "ts"],
    }


@router.get("/v1/conversations/search")
async def search_conversations(
    request: Request,
    tenant_id: Annotated[uuid.UUID, Depends(_require_tenant)],
    q: str | None = Query(default=None, max_length=500),
    channel: str | None = Query(default=None, max_length=40),
    site_slug: str | None = Query(default=None, max_length=120),
    role: str | None = Query(default=None),
    conversation_id: str | None = Query(default=None),
    from_ts: str | None = Query(default=None),
    to_ts: str | None = Query(default=None),
    limit: int = Query(default=25, ge=1),
    offset: int = Query(default=0, ge=0),
) -> dict[str, Any]:
    if limit > MAX_LIMIT:
        limit = MAX_LIMIT
    if limit + offset > MAX_WINDOW:
        raise HTTPException(
            status.HTTP_400_BAD_REQUEST,
            f"limit+offset must be <= {MAX_WINDOW} (result window)",
        )
    if role is not None and role not in ALLOWED_ROLES:
        raise HTTPException(
            status.HTTP_400_BAD_REQUEST,
            f"role must be one of {list(ALLOWED_ROLES)}",
        )
    if conversation_id is not None:
        try:
            uuid.UUID(conversation_id)
        except ValueError:
            raise HTTPException(
                status.HTTP_400_BAD_REQUEST, "conversation_id must be a uuid"
            ) from None
    if from_ts is not None:
        from_ts = _parse_ts(from_ts, "from_ts")
    if to_ts is not None:
        to_ts = _parse_ts(to_ts, "to_ts")
    if from_ts and to_ts and from_ts > to_ts:
        raise HTTPException(
            status.HTTP_400_BAD_REQUEST, "from_ts must be <= to_ts"
        )

    # Fail-closed (SPEC §3): the client lives on app.state (lifespan-built);
    # None => OpenSearch was never configured for this process.
    os_client = getattr(request.app.state, "search_os", None)
    if os_client is None:
        raise HTTPException(
            status.HTTP_503_SERVICE_UNAVAILABLE, "search_unavailable"
        )

    cfg = request.app.state.cfg
    body = build_search_query(
        tenant_id=str(tenant_id),
        q=q,
        channel=channel,
        site_slug=site_slug,
        role=role,
        conversation_id=conversation_id,
        from_ts=from_ts,
        to_ts=to_ts,
        limit=limit,
        offset=offset,
    )
    try:
        resp = await os_client.search(index=cfg.conversations_index, body=body)
    except Exception as exc:  # noqa: BLE001 — loud 503, never a fake list
        log.error("conversations.search_failed", error=str(exc)[:200])
        raise HTTPException(
            status.HTTP_503_SERVICE_UNAVAILABLE, "search_unavailable"
        ) from exc

    hits = [
        {
            "conversation_id": src.get("conversation_id"),
            "site_slug": src.get("site_slug"),
            "channel": src.get("channel"),
            "role": src.get("role"),
            "text": src.get("text"),
            "ts": src.get("ts"),
        }
        for src in (h.get("_source", {}) for h in resp.get("hits", {}).get("hits", []))
    ]
    total = resp.get("hits", {}).get("total", {})
    return {
        "hits": hits,
        "total": total.get("value", 0) if isinstance(total, dict) else int(total or 0),
        "limit": limit,
        "offset": offset,
    }
