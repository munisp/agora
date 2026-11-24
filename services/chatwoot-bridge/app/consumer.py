"""Outbound sync: OpenDesk transcript events -> Chatwoot messages
(SPEC-W118 §4).

Consumes ConversationTurn CloudEvents from Kafka (direct broker via
aiokafka — the W10 TranscriptIndexer idiom, own consumer group, commit
after processing => at-least-once). Per event:

- SKIP data.origin == "chatwoot" — the echo of a turn the bridge itself
  appended from a Chatwoot agent reply. THIS IS THE INBOUND LOOP BREAKER.
- SKIP roles outside {user, agent} (system/tool are internal).
- SKIP unmapped/disabled tenants (per-tenant opt-in, not an error).
- First turn of a conversation: create the Chatwoot contact (identifier
  od-<conversation_id>) + conversation, store the link.
- Post the message with source_id "od-<cloudevent id>" and record it in
  the sent registry (the webhook drops echoes of our own writes).

Turn text arrives ALREADY PII-redacted (W34 GF3 — the Dapr publish path
redacts before the event lands on the topic). Human agents therefore see
exactly what the CRM's OpenSearch index sees. Deliberate NDPA posture,
not data loss.

Residual (documented in SPEC §8): a crash between the Chatwoot POST and
the registry insert can double-post ONE message on redelivery.
"""

from __future__ import annotations

import asyncio
import contextlib
import uuid
from typing import Any

from .client import ChatwootError
from .config import Settings
from .conversations import ConversationNotFound, ConversationServiceError
from .logging import get_logger
from .store import Database

log = get_logger(__name__)

FORWARDED_ROLES = ("user", "agent")


class BridgeConsumer:
    def __init__(
        self,
        cfg: Settings,
        store: Database,
        chatwoot: Any,
        conv_client: Any,
    ) -> None:
        self._cfg = cfg
        self._store = store
        self._chatwoot = chatwoot
        self._conv = conv_client
        self._task: asyncio.Task[None] | None = None
        self._stop = asyncio.Event()

    def start(self) -> None:
        self._task = asyncio.create_task(self._run(), name="chatwoot-bridge-consumer")

    async def stop(self) -> None:
        self._stop.set()
        if self._task is not None:
            self._task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._task
            self._task = None

    async def _run(self) -> None:
        from aiokafka import AIOKafkaConsumer  # lazy: boot without the dep

        import json

        consumer = AIOKafkaConsumer(
            self._cfg.transcripts_topic,
            bootstrap_servers=self._cfg.brokers,
            group_id=self._cfg.bridge_group,
            enable_auto_commit=False,
            auto_offset_reset="earliest",
            value_deserializer=lambda b: json.loads(b.decode("utf-8")),
        )
        while not self._stop.is_set():
            try:
                await consumer.start()
                break
            except Exception as exc:  # broker still starting
                log.warning("bridge consumer start failed, retrying", error=str(exc))
                try:
                    await asyncio.wait_for(self._stop.wait(), timeout=5.0)
                except TimeoutError:
                    continue
        if self._stop.is_set():
            return

        log.info(
            "chatwoot bridge consumer started",
            topic=self._cfg.transcripts_topic,
            group=self._cfg.bridge_group,
        )
        try:
            while not self._stop.is_set():
                try:
                    batch = await asyncio.wait_for(
                        consumer.getmany(
                            timeout_ms=1000,
                            max_records=self._cfg.consumer_batch_size,
                        ),
                        timeout=5.0,
                    )
                except TimeoutError:
                    batch = {}
                for _tp, messages in batch.items():
                    for msg in messages:
                        try:
                            outcome = await self.process_event(msg.value)
                            if outcome.startswith("error"):
                                # Poison protection: log + skip (a permanent
                                # failure must not stall the partition).
                                log.error(
                                    "bridge event failed, skipping",
                                    outcome=outcome,
                                    event_id=msg.value.get("id"),
                                )
                        except Exception as exc:  # noqa: BLE001
                            log.error(
                                "bridge event raised, skipping",
                                error=str(exc),
                                event_id=(msg.value or {}).get("id")
                                if isinstance(msg.value, dict)
                                else None,
                            )
                await consumer.commit()
        finally:
            await consumer.stop()
            log.info("chatwoot bridge consumer stopped")

    async def process_event(self, event: dict[str, Any]) -> str:
        """One transcript event -> zero or one Chatwoot message. Returns a
        machine-readable outcome (test-pinned)."""
        if not isinstance(event, dict):
            return "skipped_malformed"
        data = event.get("data") or {}
        if data.get("origin") == "chatwoot":
            return "skipped_origin"
        role = data.get("role")
        tenant_id = event.get("tenantid") or data.get("tenantId")
        conversation_id = data.get("conversationId")
        text = data.get("text")
        event_id = event.get("id")
        if not (tenant_id and conversation_id and text and event_id and role):
            return "skipped_malformed"
        if role not in FORWARDED_ROLES:
            return "skipped_role"
        tenant_id = str(tenant_id)
        conversation_id = str(conversation_id)

        mapping = await self._store.get_tenant(tenant_id)
        if mapping is None or not mapping.get("enabled", True):
            return "skipped_unmapped"

        source_id = f"od-{event_id}"
        if await self._store.sent_lookup(tenant_id, source_id):
            return "skipped_duplicate"

        link = await self._store.get_link(tenant_id, conversation_id)
        if link is None:
            try:
                conv = await self._conv.get_conversation(conversation_id, tenant_id)
            except ConversationNotFound:
                return "skipped_no_conversation"
            try:
                link = await self._create_link(tenant_id, conversation_id, mapping, conv)
            except (ChatwootError, ConversationServiceError) as exc:
                log.error("link creation failed", error=str(exc),
                          conversation_id=conversation_id)
                return "error_link"

        message_type = "incoming" if role == "user" else "outgoing"
        try:
            msg = await self._chatwoot.create_message(
                mapping["api_access_token"],
                mapping["account_id"],
                link["chatwoot_conversation_id"],
                content=text,
                message_type=message_type,
                source_id=source_id,
            )
        except ChatwootError as exc:
            log.error("chatwoot message post failed", error=str(exc),
                      conversation_id=conversation_id)
            return "error_post"

        await self._store.sent_insert(
            tenant_id, source_id, int(msg.get("id", 0))
        )
        return "posted"

    async def _create_link(
        self,
        tenant_id: str,
        conversation_id: str,
        mapping: dict[str, Any],
        conv: dict[str, Any],
    ) -> dict[str, Any]:
        token = mapping["api_access_token"]
        account_id = mapping["account_id"]
        inbox_id = mapping["inbox_id"]
        channel = conv.get("channel") or "chat"
        phone = conv.get("contact_phone") or None
        short = conversation_id.split("-")[0]
        name = f"OpenDesk {channel} {phone or short}"
        contact_id, contact_source_id = await self._chatwoot.create_contact(
            token,
            account_id,
            inbox_id=inbox_id,
            name=name,
            identifier=f"od-{conversation_id}",
            phone_number=phone,
        )
        cw_conv_id = await self._chatwoot.create_conversation(
            token,
            account_id,
            source_id=contact_source_id,
            inbox_id=inbox_id,
            opendesk_conversation_id=conversation_id,
        )
        link = await self._store.insert_link(
            tenant_id,
            conversation_id,
            chatwoot_conversation_id=cw_conv_id,
            chatwoot_contact_id=contact_id,
            contact_source_id=contact_source_id,
        )
        log.info(
            "chatwoot link created",
            tenant_id=tenant_id,
            conversation_id=conversation_id,
            chatwoot_conversation_id=cw_conv_id,
        )
        return link
