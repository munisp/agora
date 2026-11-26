"""GDPR privacy erase consumer (SPEC-W3 §2 innovation 13).

Consumes PrivacyEraseRequested tombstone CloudEvents from
opendesk.privacy.events (published by GdprEraseWorkflow in
notification-worker) and deletes the data subject's conversation turns.
Direct-broker aiokafka consumer (like the transcript indexer) — this is a
streaming path, not Dapr pubsub, so delivery retries are explicit.
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

_EVENT_TYPES = {"PrivacyEraseRequested", "com.opendesk.privacy.PrivacyEraseRequested"}
_MAX_ATTEMPTS = 3


class PrivacyEraseConsumer:
    """Background task: tombstones -> erase_contact_data."""

    def __init__(self, cfg: Config, db: Database, search_index: Any = None) -> None:
        self._cfg = cfg
        self._db = db
        # app.search_index.SearchIndex (optional): mirrors the delete in OpenSearch.
        self._search_index = search_index
        self._task: asyncio.Task | None = None
        self._consumer: Any = None

    def start(self) -> None:
        self._task = asyncio.create_task(self._run(), name="privacy-erase-consumer")
        log.info("privacy erase consumer started", topic=self._cfg.privacy_topic)

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
                    self._cfg.privacy_topic,
                    bootstrap_servers=self._cfg.kafka_brokers,
                    group_id=self._cfg.privacy_group,
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
                log.error("privacy consumer error; retrying", error=str(exc))
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
        """Handle one tombstone. Returns True when the offset may be committed
        (processed or permanently invalid); False to retry later."""
        try:
            env = json.loads(value)
        except (ValueError, UnicodeDecodeError):
            log.error("malformed privacy event; skipping")
            return True  # poison payload — never heals
        if env.get("type") not in _EVENT_TYPES:
            return True  # other privacy event kinds: acknowledge and skip
        data = env.get("data") or {}
        phone = data.get("phone") or None
        email = data.get("email") or None
        try:
            tenant_id = uuid.UUID(str(data.get("tenant_id")))
        except (ValueError, AttributeError, TypeError):
            log.error("privacy event with bad tenant_id; skipping")
            return True
        if not phone and not email:
            log.error("erase event carries neither phone nor email; skipping")
            return True
        for attempt in range(1, _MAX_ATTEMPTS + 1):
            try:
                ids, turns = await self._db.erase_contact_conversations(
                    tenant_id, phone, email
                )
                log.info(
                    "contact data erased (GDPR)",
                    tenant_id=str(tenant_id),
                    conversations=len(ids),
                    turns_deleted=turns,
                )
                if self._search_index is not None and ids:
                    if await self._search_index.delete_conversations(tenant_id, ids) is None:
                        # Erased text must not stay searchable: retry the event.
                        # Postgres side is idempotent, so the replay converges.
                        raise RuntimeError("search index erase failed")
                return True
            except Exception as exc:  # noqa: BLE001
                log.error("erase failed", error=str(exc), attempt=attempt)
                await asyncio.sleep(attempt * 0.5)
        return False
