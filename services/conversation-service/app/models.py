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
