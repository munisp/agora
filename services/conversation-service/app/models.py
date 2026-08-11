"""Pydantic request/response models for the REST API."""

from __future__ import annotations

import uuid
from datetime import datetime
from typing import Any, Literal

from pydantic import BaseModel, Field

Role = Literal["user", "agent", "system", "tool"]
# SPEC-W12 contract §2: "ussd" joins the channel enum (DB CHECK widened at
# startup via Database.ensure_ussd_channel).
Channel = Literal["voice", "chat", "phone", "api", "ussd"]


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


class TurnCreate(BaseModel):
    role: Role
    text: str = Field(min_length=1)
    tool_calls: list[dict[str, Any]] | None = None
    audio_url: str | None = None


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
