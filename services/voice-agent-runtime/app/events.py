"""CloudEvents 1.0 envelope helpers (SPEC §4).

Envelope: {specversion, id, source, type, subject, time, tenantid (ext), data}.
Note: the booking-service command consumer requires `tenantid` to be the
tenant **UUID** and `subject` to be the tenant **slug**.
"""

from __future__ import annotations

import uuid
from datetime import datetime, timezone
from typing import Any

SOURCE = "voice-agent-runtime"


def session_lifecycle_data(
    *,
    conversation_id: str,
    channel: str,
    site_slug: str,
    quality: dict[str, Any] | None = None,
    agent_id: str | None = None,
) -> dict[str, Any]:
    """Data payload for SessionStarted/SessionEnded events.

    `quality` (SessionMetrics.quality_payload) is attached to SessionEnded
    only when the session produced data — the key stays absent otherwise so
    downstream consumers (crm-sync-service) can tell "no signals" apart from
    "zero values".

    `agent_id` is the registry-resolved agent id (SPEC-W38 F1). When present
    it is emitted as camelCase ``agentId`` — the conversation-service
    capture consumer's primary key (it also accepts ``agent_id``); absent
    otherwise so unresolved (legacy TENANT_PHONE_MAP / web) calls keep the
    pre-W38 shape.
    """
    data: dict[str, Any] = {
        "conversationId": conversation_id,
        "channel": channel,
        "siteSlug": site_slug,
    }
    if agent_id:
        data["agentId"] = agent_id
    if quality:
        data["quality"] = quality
    return data


def new_cloudevent(
    *,
    type_: str,
    subject: str,
    tenant_uuid: str,
    data: dict[str, Any],
    event_id: str | None = None,
) -> dict[str, Any]:
    eid = event_id or str(uuid.uuid4())
    return {
        "specversion": "1.0",
        "id": eid,
        "source": SOURCE,
        "type": type_,
        "subject": subject,
        "time": datetime.now(timezone.utc)
        .isoformat(timespec="milliseconds")
        .replace("+00:00", "Z"),
        "tenantid": tenant_uuid,
        "data": data,
    }
