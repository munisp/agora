"""Pydantic request/response models for the REST API."""

from __future__ import annotations

import uuid
from datetime import datetime
from typing import Any, Literal

from pydantic import field_validator, BaseModel, Field

Role = Literal["user", "agent", "system", "tool"]
# SPEC-W12 contract §2: "ussd" joins the channel enum (DB CHECK widened at
# startup via Database.ensure_ussd_channel).
# The messaging-gateway bridge records WhatsApp/Telegram chats under their own channel.
Channel = Literal["voice", "chat", "phone", "api", "ussd", "whatsapp", "telegram"]


class ConversationCreate(BaseModel):
    tenant_id: uuid.UUID
    site_slug: str = Field(min_length=1, max_length=128)
    channel: Channel = "voice"
    # GDPR contact marker (SPEC-W3 §2): set from site/session metadata when
    # the caller knows the visitor's phone (or e-mail); enables ?contact=
    # filtering and privacy erasure. Nullable — anonymous sessions stay NULL.
    contact_phone: str | None = Field(default=None, max_length=64)


class Conversation(BaseModel):
    id: uuid.UUID
    tenant_id: uuid.UUID
    site_slug: str
    channel: str
    contact_phone: str | None = None
    started_at: datetime
    ended_at: datetime | None = None
    # 2026-11-21 (list view only): who the caller said they were, read from the call's own booking tool turn
    # (contact_name / phone arguments), and the time of the last turn. Browser voice calls carry no
    # contact_phone and are never closed (ended_at stays NULL), so the dashboard showed every caller as
    # "Not given" and 0 call minutes. Display-only: the stored contact_phone marker (GDPR) is unchanged.
    contact_name: str | None = None
    caller_phone: str | None = None
    last_activity_at: datetime | None = None
    # 2026-11-23: direction, purpose, length, outcome, mood and the AI summary of a call (also in its CRM note)
    call_details: dict | None = None

    @field_validator("call_details", mode="before")
    @classmethod
    def _parse_call_details(cls, v):  # asyncpg hands JSONB back as text
        if isinstance(v, str):
            import json

            try:
                return json.loads(v)
            except ValueError:
                return None
        return v


class TurnCreate(BaseModel):
    role: Role
    text: str = Field(min_length=1)
    tool_calls: list[dict[str, Any]] | None = None
    audio_url: str | None = None
    # The English a non-English line was made from (the voice receptionist thinks in English and speaks Yoruba,
    # Hausa or Igbo): stored as the line's translation, so it is exact instead of re-translated later.
    translation_en: str | None = Field(default=None, max_length=4000)


class Turn(BaseModel):
    id: uuid.UUID
    conversation_id: uuid.UUID
    seq: int
    role: str
    text: str
    tool_calls: list[dict[str, Any]] | None = None
    # Call-intelligence enrichment (SPEC-W3 §4, innovation 3; nullable).
    sentiment: float | None = None
    intent: str | None = None
    entities: dict[str, Any] | None = None
    # English translation of a non-English line (None: English already, or not translated yet).
    translation_en: str | None = None
    ts: datetime


class ConversationWithTurns(Conversation):
    turns: list[Turn]


class TurnCreated(BaseModel):
    turn: Turn

# ---------------------------------------------------------------------------
# SPEC-W38 F1/F3: agents registry + capture primitive
# ---------------------------------------------------------------------------

# E.164 (ITU-T): "+" followed by 2-15 digits, first non-zero.
E164 = r"^\+[1-9]\d{1,14}$"

AgentStatus = Literal["active", "disabled"]


class AgentCreate(BaseModel):
    name: str = Field(min_length=1, max_length=200)
    slug: str | None = Field(default=None, min_length=1, max_length=128)
    purpose: str | None = None
    phone_number: str | None = Field(default=None, pattern=E164)
    definition: dict[str, Any] = Field(default_factory=dict)


class AgentUpdate(BaseModel):
    """PATCH: only explicitly-provided fields change (model_fields_set)."""

    name: str | None = Field(default=None, min_length=1, max_length=200)
    slug: str | None = Field(default=None, min_length=1, max_length=128)
    purpose: str | None = None
    phone_number: str | None = Field(default=None, pattern=E164)
    status: AgentStatus | None = None
    definition: dict[str, Any] | None = None


class Agent(BaseModel):
    id: uuid.UUID
    tenant_id: uuid.UUID
    name: str
    slug: str
    purpose: str | None = None
    phone_number: str | None = None
    status: str
    definition: dict[str, Any] = Field(default_factory=dict)
    # Only populated by /v1/agents/resolve (LEFT JOIN tenant_slugs): the
    # voice runtime needs the tenant SLUG to bootstrap TenantContext.
    tenant_slug: str | None = None
    created_at: datetime
    updated_at: datetime


class AgentResolved(BaseModel):
    """GET /v1/agents/resolve response (INTERNAL, voice runtime)."""

    agent: Agent
    definition: dict[str, Any] = Field(default_factory=dict)


class CaptureSchemaCreate(BaseModel):
    agent_id: uuid.UUID
    name: str = Field(min_length=1, max_length=200)
    schema: dict[str, Any] = Field(default_factory=lambda: {"fields": []})
    active: bool = True


class CaptureSchemaUpdate(BaseModel):
    """PATCH: only explicitly-provided fields change (model_fields_set)."""

    name: str | None = Field(default=None, min_length=1, max_length=200)
    schema: dict[str, Any] | None = None
    active: bool | None = None


class CaptureSchema(BaseModel):
    id: uuid.UUID
    tenant_id: uuid.UUID
    agent_id: uuid.UUID
    name: str
    schema: dict[str, Any]
    active: bool
    created_at: datetime
    updated_at: datetime


class CaptureRecord(BaseModel):
    id: uuid.UUID
    tenant_id: uuid.UUID
    capture_schema_id: uuid.UUID
    agent_id: uuid.UUID
    conversation_id: uuid.UUID
    data: dict[str, Any]
    extraction_confidence: float | None = None
    created_at: datetime
