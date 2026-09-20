"""K9 TenantDeleted cascade consumer (SPEC-W45): consumes
com.opendesk.identity.TenantDeleted CloudEvents from
opendesk.identity.events and purges the tenant's conversation sessions and
history (all turns + conversation rows).

Direct-broker aiokafka consumer (same posture as the privacy-erase
consumer): explicit commits, poison payloads acknowledged-and-skipped,
purge failures retried with backoff and never silently swallowed.
purge_tenant_data is idempotent, so redelivery converges.
"""

from __future__ import annotations

import asyncio
import json
import uuid
from typing import Any

from .config import Config
from .db import Database
from .logging import get_logger

log = get_logger(__name__)

_EVENT_TYPES = {"TenantDeleted", "com.opendesk.identity.TenantDeleted"}
_MAX_ATTEMPTS = 3


class TenantLifecycleConsumer:
    """Background task: TenantDeleted tombstones -> purge_tenant_data."""

    def __init__(self, cfg: Config, db: Database) -> None:
        self._cfg = cfg
        self._db = db
        self._task: asyncio.Task | None = None
        self._consumer: Any = None

    def start(self) -> None:
        self._task = asyncio.create_task(self._run(), name="tenant-lifecycle-consumer")
        log.info(
            "tenant lifecycle consumer started",
            topic=self._cfg.identity_events_topic,
            group=self._cfg.tenant_events_group,
        )

    async def stop(self) -> None:
        if self._task is not None:
            self._task.cancel()
            try:
                await self._task
            except (asyncio.CancelledError, Exception):  # noqa: BLE001
                pass
        if self._consumer is not None:
            try:
                await self._consumer.stop()
            except Exception:  # noqa: BLE001
                pass

    async def _run(self) -> None:
        from aiokafka import AIOKafkaConsumer

        backoff = 2.0
        while True:
            try:
                self._consumer = AIOKafkaConsumer(
                    self._cfg.identity_events_topic,
                    bootstrap_servers=self._cfg.kafka_brokers,
                    group_id=self._cfg.tenant_events_group,
                    enable_auto_commit=False,
                    auto_offset_reset="earliest",
                )
                await self._consumer.start()
                backoff = 2.0
                async for msg in self._consumer:
                    if await self._process(msg.value):
                        await self._consumer.commit()
            except asyncio.CancelledError:
                raise
            except Exception as exc:  # noqa: BLE001 — keep the consumer alive
                log.error("tenant lifecycle consumer error; retrying", error=str(exc))
                await asyncio.sleep(backoff)
                backoff = min(backoff * 2, 30.0)
            finally:
                if self._consumer is not None:
                    try:
                        await self._consumer.stop()
                    except Exception:  # noqa: BLE001
                        pass
                    self._consumer = None

    async def _process(self, value: bytes) -> bool:
        """Handle one event. Returns True when the offset may be committed
        (processed, not-for-us, or permanently invalid); False to retry."""
        try:
            env = json.loads(value)
        except (ValueError, UnicodeDecodeError):
            log.error("malformed identity event; skipping")
            return True  # poison payload — never heals
        if env.get("type") not in _EVENT_TYPES:
            return True  # other identity events (MemberInvited etc.): ack + skip
        data = env.get("data") or {}
        raw_tenant = data.get("tenant_id") or env.get("tenantid")
        try:
            tenant_id = uuid.UUID(str(raw_tenant))
        except (ValueError, AttributeError, TypeError):
            log.error("TenantDeleted with bad tenant_id; skipping",
                      tenant_id=raw_tenant)
            return True
        slug = data.get("tenant_slug") or env.get("subject") or ""
        for attempt in range(1, _MAX_ATTEMPTS + 1):
            try:
                convs, turns = await self._db.purge_tenant_data(tenant_id)
                log.info(
                    "tenant data purged (K9 TenantDeleted cascade)",
                    tenant_id=str(tenant_id),
                    tenant_slug=slug,
                    event_id=env.get("id"),
                    conversations_deleted=convs,
                    turns_deleted=turns,
                )
                return True
            except Exception as exc:  # noqa: BLE001
                log.error("tenant purge failed", error=str(exc), attempt=attempt,
                          tenant_id=str(tenant_id))
                await asyncio.sleep(attempt * 0.5)
        return False
