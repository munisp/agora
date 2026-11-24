"""Keeps the OpenSearch `conversations` index in step with Postgres deletes.

The transcript indexer (app/indexer.py) only ever adds documents, so every
path that deletes turns or conversations in Postgres must also delete them
from the index, or deleted text stays searchable:

- GDPR erase (privacy consumer + POST /internal/gdpr/erase): the subject's
  conversations' turns are deleted (shells kept), so their docs go too.
- TenantDeleted cascade: every doc of the tenant.
- Retention sweep: docs older than RETENTION_DAYS.
- Anything deleted outside the service (manual SQL, demo resets): the
  periodic `prune_orphans` reconciliation drops docs whose conversation row
  no longer exists.

Every call is best effort: a failure is logged and reported as None and
never fails the Postgres delete that already happened. The next prune run
converges whatever a failed call left behind (except text of erased turns
in still-existing shells; erase retries cover those).
"""

from __future__ import annotations

import uuid
from typing import Any

from .config import Config
from .logging import get_logger

log = get_logger(__name__)


class SearchIndex:
    def __init__(self, cfg: Config) -> None:
        self._cfg = cfg
        self._client: Any = None

    def _os(self) -> Any:
        if self._client is None:
            from opensearchpy import AsyncOpenSearch

            self._client = AsyncOpenSearch(hosts=[self._cfg.opensearch_addr])
        return self._client

    async def close(self) -> None:
        if self._client is not None:
            try:
                await self._client.close()
            except Exception:  # noqa: BLE001
                pass
            self._client = None

    async def _delete_by_query(self, query: dict[str, Any], what: str, **ctx: Any) -> int | None:
        try:
            resp = await self._os().delete_by_query(
                index=self._cfg.conversations_index,
                body={"query": query},
                params={"conflicts": "proceed", "refresh": "true"},
            )
        except Exception as exc:  # noqa: BLE001 — never fail the caller
            log.error("search index delete failed", what=what, error=str(exc), **ctx)
            return None
        deleted = int(resp.get("deleted", 0))
        if deleted:
            log.info("search index docs deleted", what=what, deleted=deleted, **ctx)
        return deleted

    async def delete_conversations(
        self, tenant_id: uuid.UUID, conversation_ids: list[uuid.UUID] | list[str]
    ) -> int | None:
        ids = [str(i) for i in conversation_ids]
        if not ids:
            return 0
        total = 0
        for start in range(0, len(ids), 500):  # terms query size stays small
            n = await self._delete_by_query(
                {"bool": {"filter": [
                    {"term": {"tenant_id": str(tenant_id)}},
                    {"terms": {"conversation_id": ids[start:start + 500]}},
                ]}},
                "conversations", tenant_id=str(tenant_id),
            )
            if n is None:
                return None
            total += n
        return total

    async def delete_tenant(self, tenant_id: uuid.UUID) -> int | None:
        return await self._delete_by_query(
            {"term": {"tenant_id": str(tenant_id)}}, "tenant", tenant_id=str(tenant_id)
        )

    async def delete_older_than(self, tenant_id: uuid.UUID, days: int) -> int | None:
        if days <= 0:
            return 0
        return await self._delete_by_query(
            {"bool": {"filter": [
                {"term": {"tenant_id": str(tenant_id)}},
                {"range": {"ts": {"lt": f"now-{int(days)}d"}}},
            ]}},
            "retention", tenant_id=str(tenant_id), days=days,
        )

    async def _indexed_conversations(self) -> dict[str, set[str]] | None:
        """{tenant_id: {conversation_id, ...}} for every doc in the index,
        paged with a composite aggregation (no size cap)."""
        out: dict[str, set[str]] = {}
        after: dict[str, Any] | None = None
        try:
            while True:
                comp: dict[str, Any] = {
                    "size": 1000,
                    "sources": [
                        {"t": {"terms": {"field": "tenant_id"}}},
                        {"c": {"terms": {"field": "conversation_id"}}},
                    ],
                }
                if after:
                    comp["after"] = after
                resp = await self._os().search(
                    index=self._cfg.conversations_index,
                    body={"size": 0, "aggs": {"pairs": {"composite": comp}}},
                )
                agg = resp["aggregations"]["pairs"]
                for b in agg["buckets"]:
                    out.setdefault(b["key"]["t"], set()).add(b["key"]["c"])
                after = agg.get("after_key")
                if not agg["buckets"] or not after:
                    return out
        except Exception as exc:  # noqa: BLE001
            log.error("search index scan failed; prune skipped", error=str(exc))
            return None

    async def prune_orphans(self, db: Any) -> dict[str, int] | None:
        """Delete docs whose conversation row no longer exists in Postgres.

        Tenant-scoped lookups (db.existing_conversation_ids runs under
        app.tenant_id), so it works with the RLS-enforced login role.
        Returns {tenant_id: docs_deleted} for tenants with orphans, or None
        when the index could not be read.
        """
        indexed = await self._indexed_conversations()
        if indexed is None:
            return None
        result: dict[str, int] = {}
        for tenant, conv_ids in indexed.items():
            try:
                tid = uuid.UUID(tenant)
            except ValueError:
                log.warning("search index doc with bad tenant_id", tenant_id=tenant)
                continue
            try:
                existing = await db.existing_conversation_ids(tid, list(conv_ids))
            except Exception as exc:  # noqa: BLE001 — keep going for other tenants
                log.error("prune lookup failed", tenant_id=tenant, error=str(exc))
                continue
            gone = sorted(conv_ids - existing)
            if not gone:
                continue
            n = await self.delete_conversations(tid, gone)
            if n:
                result[tenant] = n
                log.info("pruned search docs of deleted conversations",
                         tenant_id=tenant, conversations=len(gone), docs=n)
        return result
