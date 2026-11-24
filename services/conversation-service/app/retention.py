"""Data-retention enforcement (NDPA 2023 — docs/compliance/ndpa.md).

The Nigeria Data Protection Act 2023 (like GDPR's storage-limitation
principle) requires personal data to be kept no longer than necessary.
This background sweeper hard-deletes conversation turns older than
``RETENTION_DAYS`` (default 365; the NDPA profile in
infra/privacy/ndpa-profile.env sets 180), running once at startup and then
every ``RETENTION_SWEEP_SECONDS`` (default hourly).

Design notes:

- Per-tenant batches: the sweep enumerates tenant ids and deletes inside a
  tenant-scoped transaction (``app.tenant_id`` set), so FORCE ROW LEVEL
  SECURITY keeps each batch inside its tenant. Enumeration requires a role
  that bypasses RLS (the default opendesk superuser DSN); under an
  RLS-enforced role the sweep finds no tenants and no-ops (documented in
  README — run it with the superuser DSN or a maintenance role).
- Cutoff uses the DATABASE clock (``now()`` in SQL), not the app clock, so
  app-side clock skew can never extend the retention window.
- Orthogonal to GDPR/NDPA erasure: the privacy erase consumer deletes a data
  subject's turns immediately; the sweeper only removes aged rows that
  erasure did not cover. Both hard-delete — no tombstones are resurrected.
- Conversation shells are kept (booking history/analytics referential
  integrity); only turn content is deleted. OpenSearch holds indexed
  transcripts — see docs/compliance/ndpa.md for the indexer caveat.
"""

from __future__ import annotations

import asyncio
from typing import Any

from .config import Config
from .db import Database
from .logging import get_logger

log = get_logger(__name__)


class RetentionSweeper:
    """Background task: hourly hard-delete of turns older than N days."""

    def __init__(self, cfg: Config, db: Database, search_index: Any = None) -> None:
        self._cfg = cfg
        self._db = db
        # app.search_index.SearchIndex (optional): mirrors the delete in OpenSearch.
        self._search_index = search_index
        self._task: asyncio.Task | None = None

    def start(self) -> None:
        self._task = asyncio.create_task(self._run(), name="retention-sweeper")
        log.info(
            "retention sweeper started",
            retention_days=self._cfg.retention_days,
            sweep_seconds=self._cfg.retention_sweep_seconds,
        )

    async def stop(self) -> None:
        if self._task is not None:
            self._task.cancel()
            try:
                await self._task
            except (asyncio.CancelledError, Exception):  # noqa: BLE001
                pass

    async def _run(self) -> None:
        while True:
            try:
                await self.run_once()
            except asyncio.CancelledError:
                raise
            except Exception as exc:  # noqa: BLE001 — sweeper must not die
                log.error("retention sweep failed; will retry next cycle",
                          error=str(exc))
            await asyncio.sleep(self._cfg.retention_sweep_seconds)

    async def run_once(self) -> dict[str, int]:
        """One full sweep: every tenant, batched delete of aged turns.

        Returns {tenant_id: turns_deleted} for observability/tests. A
        failure for one tenant is logged and does not stop the others.
        """
        if self._cfg.retention_days <= 0:
            # 0/negative would delete everything — refuse loudly.
            log.error("retention_days must be >= 1; sweep skipped",
                      retention_days=self._cfg.retention_days)
            return {}
        tenants = await self._db.list_tenant_ids()
        deleted: dict[str, int] = {}
        for tenant_id in tenants:
            try:
                n = await self._db.delete_turns_older_than(
                    tenant_id,
                    self._cfg.retention_days,
                    self._cfg.retention_batch_size,
                )
            except Exception as exc:  # noqa: BLE001 — keep sweeping others
                log.error("retention sweep failed for tenant",
                          tenant_id=str(tenant_id), error=str(exc))
                continue
            if self._search_index is not None:
                # Independent of n: docs can outlive their turns (deleted
                # out of band), and the range delete is cheap when empty.
                await self._search_index.delete_older_than(
                    tenant_id, self._cfg.retention_days
                )
            if n:
                deleted[str(tenant_id)] = n
                log.info("retention sweep deleted aged turns",
                         tenant_id=str(tenant_id), turns_deleted=n,
                         retention_days=self._cfg.retention_days)
        return deleted
