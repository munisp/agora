"""CloudEvents 1.0 envelope per SPEC §4:
{specversion, id, source, type, subject, time, tenantid (ext), data}.
"""

from __future__ import annotations

import uuid
from datetime import UTC, datetime
from typing import Any

SOURCE = "conversation-service"


def cloud_event(
    event_type: str,
    *,
    subject: str,
    tenant_id: str,
    data: dict[str, Any],
) -> dict[str, Any]:
    return {
        "specversion": "1.0",
        "id": str(uuid.uuid4()),
        "source": SOURCE,
        "type": event_type,
        "subject": subject,
        "time": datetime.now(UTC).isoformat(),
        "tenantid": tenant_id,
        "data": data,
    }


def conversation_turn_event(
    *,
    conversation_id: uuid.UUID,
    tenant_id: uuid.UUID,
    site_slug: str,
    role: str,
    text: str,
    ts: datetime,
    audio_url: str | None = None,
    redacted: bool = False,
) -> dict[str, Any]:
    """ConversationTurn event for opendesk.conversation.transcripts (SPEC §4).

    ``redacted`` marks that ``text`` has already been PII-redacted
    (SPEC-W34 GF3) so downstream consumers know not to expect raw PII.
    """
    data: dict[str, Any] = {
        "conversationId": str(conversation_id),
        "tenantId": str(tenant_id),
        "role": role,
        "text": text,
        "ts": ts.isoformat(),
        "redacted": redacted,
    }
    if audio_url:
        data["audioUrl"] = audio_url
    return cloud_event(
        "com.opendesk.conversation.ConversationTurn",
        subject=site_slug,
        tenant_id=str(tenant_id),
        data=data,
    )
