"""Capture primitive (SPEC-W38 F3): post-call structured data extraction.

Consumes SessionEnded CloudEvents from opendesk.conversation.events (own
consumer group `conversation-capture`, sibling of app/quality.py), resolves
the agent for the conversation (the agent id carried on the session
metadata), loads the agent's ACTIVE capture_schemas, reads the full turns
from Postgres, and runs one LLM extraction pass per schema against the
INTEL_LLM_* OpenAI-compatible endpoint (3s timeout, JSON-mode prompt).

Degrade contract (SPEC-W38 F3): on ANY LLM failure/timeout the extraction
is logged and SKIPPED — no capture_records row, no event, no crash (the
offset is committed; a transient LLM outage must not block the group).

On success the record is inserted into capture_records (with
extraction_confidence when the model provides one) and a
com.opendesk.conversation.CaptureExtracted CloudEvent is published to
CAPTURE_TOPIC (default opendesk.conversation.captures) via the same Dapr
pubsub path as transcripts. crm-sync-service consumes that topic.
"""

from __future__ import annotations

import asyncio
import json
import uuid
from typing import Any

import httpx

from .config import Config
from .db import Database, NotFoundError
from .events import cloud_event
from .logging import get_logger

log = get_logger(__name__)

EVENT_TYPE_SESSION_ENDED = "com.opendesk.conversation.SessionEnded"
EVENT_TYPE_CAPTURE_EXTRACTED = "com.opendesk.conversation.CaptureExtracted"

_FIELD_TYPES = ("string", "number", "boolean", "enum")


def schema_fields(schema: dict[str, Any]) -> list[dict[str, Any]]:
    """The declared fields of a capture schema (tolerant of junk)."""
    fields = schema.get("fields")
    if not isinstance(fields, list):
        return []
    return [f for f in fields if isinstance(f, dict) and f.get("key")]


def build_extraction_prompt(fields: list[dict[str, Any]]) -> str:
    """System prompt describing the fields to extract (JSON-mode)."""
    lines = []
    for f in fields:
        desc = f'- "{f["key"]}" ({f.get("type", "string")}): {f.get("label", f["key"])}'
        if f.get("required"):
            desc += " [required]"
        options = f.get("options")
        if isinstance(options, list) and options:
            desc += " one of: " + ", ".join(str(o) for o in options)
        lines.append(desc)
    return (
        "You extract structured data from a phone-call transcript. Answer "
        "with ONLY a JSON object of the form "
        '{"data": {"<key>": <value>, ...}, "confidence": <0.0-1.0>} — no '
        "prose, no fences. Extract these fields (use null when the "
        "transcript does not mention a value):\n" + "\n".join(lines)
    )


def _coerce(value: Any, field_type: str) -> Any:
    """Best-effort type coercion to the declared field type."""
    if value is None:
        return None
    if field_type == "number":
        if isinstance(value, (int, float)) and not isinstance(value, bool):
            return value
        try:
            return float(str(value).replace(",", ""))
        except (ValueError, TypeError):
            return value
    if field_type == "boolean":
        if isinstance(value, bool):
            return value
        s = str(value).strip().lower()
        if s in ("true", "yes", "1"):
            return True
        if s in ("false", "no", "0"):
            return False
        return value
    return str(value) if not isinstance(value, str) else value


def parse_extraction(
    content: str, fields: list[dict[str, Any]]
) -> dict[str, Any] | None:
    """Parse the LLM JSON-mode response -> {"data": {...}, "confidence": f|None}.

    Tolerant of prose/fences around the object and of a bare field object
    (no "data" wrapper). Only declared field keys are kept, coerced to
    their declared types.
    """
    text = content.strip()
    start = text.find("{")
    end = text.rfind("}")
    if start == -1 or end <= start:
        return None
    try:
        parsed = json.loads(text[start:end + 1])
    except json.JSONDecodeError:
        return None
    if not isinstance(parsed, dict):
        return None
    raw = parsed.get("data")
    if not isinstance(raw, dict):
        # bare {"key": value, ...} response (model ignored the wrapper)
        raw = {k: v for k, v in parsed.items() if k != "confidence"}
    confidence = parsed.get("confidence")
    if not isinstance(confidence, (int, float)) or isinstance(confidence, bool):
        confidence = None
    else:
        confidence = max(0.0, min(1.0, float(confidence)))
    data: dict[str, Any] = {}
    for f in fields:
        key = f["key"]
        if key in raw and raw[key] is not None:
            data[key] = _coerce(raw[key], str(f.get("type", "string")))
    return {"data": data, "confidence": confidence}


async def llm_extract_fields(
    transcript: str,
    fields: list[dict[str, Any]],
    cfg: Config,
    *,
    client: httpx.AsyncClient | None = None,
) -> dict[str, Any] | None:
    """One JSON-mode chat completion against the INTEL_LLM_* endpoint.

    Returns {"data", "confidence"} or None on ANY failure (timeout, HTTP
    error, bad JSON) — the caller treats None as "skip, no record".
    """
    payload = {
        "model": cfg.intel_llm_model,
        "messages": [
            {"role": "system", "content": build_extraction_prompt(fields)},
            {"role": "user", "content": transcript[:6000]},
        ],
        "temperature": 0.0,
        "response_format": {"type": "json_object"},
    }
    headers = {}
    if cfg.intel_llm_api_key:
        headers["authorization"] = f"Bearer {cfg.intel_llm_api_key}"
    owns = client is None
    client = client or httpx.AsyncClient(
        timeout=httpx.Timeout(cfg.intel_llm_timeout_s)
    )
    try:
        resp = await client.post(
            f"{cfg.intel_llm_base_url.rstrip('/')}/chat/completions",
            json=payload,
            headers=headers,
        )
        if resp.status_code >= 300:
            log.warning("capture llm http error", status=resp.status_code)
            return None
        body = resp.json()
        content = (
            (body.get("choices") or [{}])[0].get("message", {}).get("content") or ""
        )
        return parse_extraction(content, fields)
    except Exception as exc:  # noqa: BLE001 — degrade: skip, no record
        log.warning("capture llm extraction failed", error=str(exc))
        return None
    finally:
        if owns:
            await client.aclose()


def build_capture_event(
    record: dict[str, Any], env: dict[str, Any]
) -> dict[str, Any]:
    """Pure builder for the CaptureExtracted CloudEvent (tested directly).

    Data shape per SPEC-W38 F3: {record_id, tenant_id, agent_id,
    conversation_id, schema_id, data}.
    """
    data = env.get("data") or {}
    payload = {
        "record_id": str(record["id"]),
        "tenant_id": str(record["tenant_id"]),
        "agent_id": str(record["agent_id"]),
        "conversation_id": str(record["conversation_id"]),
        "schema_id": str(record["capture_schema_id"]),
        "data": record.get("data") or {},
    }
    return cloud_event(
        EVENT_TYPE_CAPTURE_EXTRACTED,
        subject=str(env.get("subject") or data.get("siteSlug") or ""),
        tenant_id=str(record["tenant_id"]),
        data=payload,
    )


def _transcript_text(turns: list[Any]) -> str:
    return "\n".join(f"{t['role']}: {t['text']}" for t in turns)


def _event_agent_id(data: dict[str, Any]) -> str | None:
    """Agent id carried on the session/conversation metadata (camelCase
    from the voice runtime, snake_case tolerated)."""
    for key in ("agentId", "agent_id"):
        v = data.get(key)
        if v:
            return str(v)
    agent = data.get("agent")
    if isinstance(agent, dict) and agent.get("id"):
        return str(agent["id"])
    return None


class CaptureExtractor:
    """Background task: SessionEnded -> LLM extraction -> capture_records
    -> CaptureExtracted on CAPTURE_TOPIC (own group `conversation-capture`).

    Direct-broker aiokafka consumer (same pattern as CallQualityEnricher) +
    the Dapr pubsub path used for transcripts for the output event.
    """

    def __init__(self, cfg: Config, db: Database, store: Any, publisher: Any,
                 llm_client: httpx.AsyncClient | None = None) -> None:
        self._cfg = cfg
        self._db = db
        self._store = store
        self._publisher = publisher
        self._llm_client = llm_client
        self._task: asyncio.Task | None = None
        self._consumer: Any = None

    def start(self) -> None:
        self._task = asyncio.create_task(self._run(), name="capture-extractor")
        log.info(
            "capture extractor started",
            topic=self._cfg.conversation_events_topic,
            group=self._cfg.capture_group,
            publish_topic=self._cfg.capture_topic,
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
                    self._cfg.conversation_events_topic,
                    bootstrap_servers=self._cfg.kafka_brokers,
                    group_id=self._cfg.capture_group,
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
                log.error("capture extractor error; retrying", error=str(exc))
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
        (captured or deliberately skipped — including LLM failure); False to
        retry later (transient DB/broker failures)."""
        try:
            env = json.loads(value)
        except (ValueError, UnicodeDecodeError):
            log.error("malformed conversation event; skipping")
            return True  # poison payload — never heals
        if not isinstance(env, dict) or env.get("type") != EVENT_TYPE_SESSION_ENDED:
            return True  # other conversation events: acknowledge and skip
        data = env.get("data") or {}
        try:
            conversation_id = uuid.UUID(str(data.get("conversationId")))
            tenant_id = uuid.UUID(str(env.get("tenantid")))
        except (ValueError, AttributeError, TypeError):
            log.error("SessionEnded with bad conversationId/tenantid; skipping")
            return True
        raw_agent_id = _event_agent_id(data)
        if not raw_agent_id:
            # No agent on the session metadata (legacy/TENANT_PHONE_MAP
            # path, web chats) — nothing to resolve, skip by design.
            log.info(
                "SessionEnded carries no agent id; capture skipped",
                conversation_id=str(conversation_id),
            )
            return True
        try:
            agent_id = uuid.UUID(raw_agent_id)
        except (ValueError, AttributeError, TypeError):
            log.error("SessionEnded with bad agent id; skipping",
                      agent_id=raw_agent_id)
            return True

        for attempt in range(1, 4):
            try:
                return await self._capture(env, tenant_id, agent_id, conversation_id)
            except Exception as exc:  # noqa: BLE001
                log.error("capture extraction failed", error=str(exc), attempt=attempt)
                await asyncio.sleep(attempt * 0.5)
        return False

    async def _capture(
        self,
        env: dict[str, Any],
        tenant_id: uuid.UUID,
        agent_id: uuid.UUID,
        conversation_id: uuid.UUID,
    ) -> bool:
        try:
            await self._store.get_agent(agent_id, tenant_id)
        except NotFoundError:
            log.info("capture: agent not found for tenant; skipping",
                     agent_id=str(agent_id))
            return True
        schemas = await self._store.list_capture_schemas(
            tenant_id, agent_id, active_only=True
        )
        if not schemas:
            return True  # agent declares no active capture schema
        turns = await self._db.list_turns(conversation_id, tenant_id)
        if not turns:
            log.info("capture: no turns for conversation; skipping",
                     conversation_id=str(conversation_id))
            return True
        transcript = _transcript_text(turns)
        for schema in schemas:
            fields = schema_fields(schema.get("schema") or {})
            if not fields:
                continue
            extracted = await llm_extract_fields(
                transcript, fields, self._cfg, client=self._llm_client
            )
            if extracted is None:
                # Degrade path (SPEC-W38 F3): LLM down/timeout/garbage —
                # log + skip, NO record, no crash, offset still committed.
                log.warning(
                    "capture llm extraction unavailable; schema skipped",
                    schema_id=str(schema["id"]),
                    conversation_id=str(conversation_id),
                )
                continue
            record = await self._store.insert_capture_record(
                tenant_id,
                schema["id"],
                agent_id,
                conversation_id,
                extracted["data"],
                extraction_confidence=extracted["confidence"],
            )
            event = build_capture_event(record, env)
            await self._publisher.publish_event(self._cfg.capture_topic, event)
            log.info(
                "capture record extracted",
                record_id=str(record["id"]),
                schema_id=str(schema["id"]),
                conversation_id=str(conversation_id),
                fields=len(extracted["data"]),
                confidence=extracted["confidence"],
            )
        return True
