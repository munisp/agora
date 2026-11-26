"""Transcript indexer (SPEC §10).

Consumes CloudEvents from Kafka topic `opendesk.conversation.transcripts`
(direct broker via aiokafka — the indexer is a streaming path, not Dapr)
and bulk-indexes turn documents into the OpenSearch index `conversations`,
conforming to the mapping in infra/opensearch/setup-indices.sh:

    tenant_id, conversation_id, site_slug, channel, role, text,
    audio_url, redacted, ts

`redacted` is set by the `pii-safe` ingest pipeline (default pipeline of the
index), not by this service.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import uuid
from datetime import datetime
from typing import Any

from .config import Config
from .db import Database
from .logging import get_logger
from .search_index import SearchIndex

log = get_logger(__name__)


class TranscriptIndexer:
    def __init__(self, cfg: Config, db: Database, search_index: SearchIndex | None = None) -> None:
        self._cfg = cfg
        self._db = db
        self._search_index = search_index
        self._task: asyncio.Task[None] | None = None
        self._prune_task: asyncio.Task[None] | None = None
        self._stop = asyncio.Event()

    def start(self) -> None:
        self._task = asyncio.create_task(self._run(), name="transcript-indexer")
        if self._search_index is not None and self._cfg.indexer_prune_seconds > 0:
            self._prune_task = asyncio.create_task(self._prune_loop(), name="search-prune")

    async def stop(self) -> None:
        self._stop.set()
        for task in (self._task, self._prune_task):
            if task is not None:
                task.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await task
        self._task = None
        self._prune_task = None

    async def _prune_loop(self) -> None:
        """Periodically drop index docs of conversations deleted outside the
        service's own delete paths (manual SQL, demo resets). First run a
        minute after start so boot is not slowed."""
        assert self._search_index is not None
        delay = 60.0
        while not self._stop.is_set():
            try:
                await asyncio.wait_for(self._stop.wait(), timeout=delay)
                return
            except TimeoutError:
                pass
            try:
                await self._search_index.prune_orphans(self._db)
            except Exception as exc:  # noqa: BLE001 — retry next cycle
                log.error("search prune failed", error=repr(exc))
            delay = float(self._cfg.indexer_prune_seconds)

    async def _run(self) -> None:
        # Lazy imports so the service starts even when indexer deps/OS differ.
        from aiokafka import AIOKafkaConsumer
        from opensearchpy import AsyncOpenSearch

        consumer = AIOKafkaConsumer(
            self._cfg.transcripts_topic,
            bootstrap_servers=self._cfg.kafka_brokers,
            group_id=self._cfg.indexer_group,
            enable_auto_commit=False,
            auto_offset_reset="earliest",
            value_deserializer=lambda b: json.loads(b.decode("utf-8")),
        )
        os_client = AsyncOpenSearch(hosts=[self._cfg.opensearch_addr])

        # Retry connect loop: Kafka/OpenSearch may still be starting.
        while not self._stop.is_set():
            try:
                await consumer.start()
                break
            except Exception as exc:  # KafkaError and friends
                log.warning("indexer consumer start failed, retrying", error=str(exc))
                try:
                    await asyncio.wait_for(self._stop.wait(), timeout=5.0)
                except TimeoutError:
                    continue
        if self._stop.is_set():
            await os_client.close()
            return

        log.info(
            "transcript indexer started",
            topic=self._cfg.transcripts_topic,
            index=self._cfg.conversations_index,
        )
        pending: list[dict[str, Any]] = []
        loop = asyncio.get_running_loop()
        last_flush = loop.time()
        try:
            while not self._stop.is_set():
                try:
                    batch = await asyncio.wait_for(
                        consumer.getmany(timeout_ms=1000, max_records=self._cfg.indexer_bulk_size),
                        timeout=5.0,
                    )
                except TimeoutError:
                    batch = {}

                for _tp, messages in batch.items():
                    for msg in messages:
                        doc = await self._to_doc(msg.value)
                        if doc is not None:
                            pending.append(doc)

                now = loop.time()
                if pending and (
                    len(pending) >= self._cfg.indexer_bulk_size
                    or now - last_flush >= self._cfg.indexer_bulk_flush_seconds
                ):
                    await self._flush(os_client, pending)
                    pending.clear()
                    last_flush = now
                    await consumer.commit()
        except Exception as exc:
            log.error("transcript indexer crashed", error=repr(exc))
            raise
        finally:
            if pending:
                with contextlib.suppress(Exception):
                    await self._flush(os_client, pending)
            await consumer.stop()
            await os_client.close()
            log.info("transcript indexer stopped")

    async def _to_doc(self, event: dict[str, Any]) -> dict[str, Any] | None:
        """Map a ConversationTurn CloudEvent to an index document."""
        data = event.get("data") or {}
        tenant_id = event.get("tenantid") or data.get("tenantId")
        conversation_id = data.get("conversationId")
        role = data.get("role")
        text = data.get("text")
        ts = data.get("ts")
        if not (tenant_id and conversation_id and role and text and ts):
            log.warning("skipping malformed transcript event", event_id=event.get("id"))
            return None

        doc: dict[str, Any] = {
            "tenant_id": tenant_id,
            "conversation_id": conversation_id,
            "role": role,
            "text": text,
            "ts": ts,
        }
        if data.get("audioUrl"):
            doc["audio_url"] = data["audioUrl"]

        # Enrich site_slug/channel from Postgres, and skip turns that were
        # deleted after the event was published (conversation deleted, GDPR
        # erase, retention, demo reset): indexing them would make deleted
        # text searchable again when the topic is replayed or lagging.
        # A failed lookup still indexes (best effort, as before).
        try:
            meta = await self._db.turn_index_meta(
                uuid.UUID(conversation_id), uuid.UUID(tenant_id),
                datetime.fromisoformat(ts), role,
            )
            if meta is None:
                log.info("skipping turn of deleted conversation",
                         conversation_id=conversation_id)
                return None
            if not meta.get("turn_exists"):
                log.info("skipping deleted turn", conversation_id=conversation_id)
                return None
            doc["site_slug"] = meta.get("site_slug")
            doc["channel"] = meta.get("channel")
        except Exception as exc:
            log.warning(
                "conversation meta lookup failed; indexing without enrichment",
                conversation_id=conversation_id,
                error=str(exc),
            )
        return doc

    async def _flush(self, os_client: Any, docs: list[dict[str, Any]]) -> None:
        # The raw bulk API takes alternating action and source lines (the
        # "_op_type"/"_index"-inside-the-doc shape is only for helpers.async_bulk;
        # sent here it was rejected with a 400 and stopped the indexer).
        actions: list[dict[str, Any]] = []
        for d in docs:
            actions.append({"index": {
                "_index": self._cfg.conversations_index,
                "_id": f"{d['conversation_id']}:{d['ts']}:{d['role']}",
            }})
            actions.append(d)
        resp = await os_client.bulk(body=actions)
        if resp.get("errors"):
            failed = [i for i in resp.get("items", []) if "error" in i.get("index", {})]
            log.error("bulk index had failures", failed=len(failed))
        else:
            log.info("bulk indexed transcripts", count=len(docs))
