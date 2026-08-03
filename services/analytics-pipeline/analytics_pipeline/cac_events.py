"""Pure-python parsing of SPEC-W13 contract §2 FunnelEvent CloudEvents.

Topic ``cac.events`` carries CloudEvents 1.0 envelopes of type
``com.opendesk.cac.FunnelEvent`` with ``data``::

    {event_id, tenant_id, entity_type: "lead|customer|agent",
     entity_id, event_name: "lead_created|contacted|opted_in|qualified|
     converted|first_txn|lost", event_ts, channel, campaign_id, lga_id,
     amount_ngn null, idempotency_key}

Payload keys are read in camelCase *or* snake_case (same tolerance as
mapping.py). Bare (non-enveloped) data payloads are accepted too — the
consumer is not the contract police, the bronze/lakehouse path is.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from datetime import UTC, date, datetime
from decimal import Decimal, InvalidOperation
from typing import Any, Mapping

FUNNEL_EVENT_TYPE = "com.opendesk.cac.FunnelEvent"

EVENT_NAMES = (
    "lead_created",
    "contacted",
    "opted_in",
    "qualified",
    "converted",
    "first_txn",
    "lost",
)

# event_names that increment the conversions rollup counter.
CONVERSION_EVENTS = frozenset({"converted"})
# event_names whose amount_ngn contributes to revenue (LTV / payback margin).
REVENUE_EVENTS = frozenset({"converted", "first_txn"})

_CAMEL_RE = re.compile(r"(?<!^)(?=[A-Z])")


def _snake(name: str) -> str:
    return _CAMEL_RE.sub("_", name).lower()


def _get(data: Mapping[str, Any], key: str, default: Any = None) -> Any:
    if key in data:
        return data[key]
    camel = re.sub(r"_([a-z])", lambda m: m.group(1).upper(), key)
    if camel in data:
        return data[camel]
    return default


def _parse_ts(raw: Any) -> datetime | None:
    """ISO-8601 / epoch seconds / epoch millis -> aware UTC datetime."""
    if raw is None or raw == "":
        return None
    if isinstance(raw, (int, float)):
        # heuristic from mapping.py: millis are >= 1e12
        seconds = raw / 1000.0 if raw >= 1e12 else float(raw)
        return datetime.fromtimestamp(seconds, tz=UTC)
    if isinstance(raw, datetime):
        return raw if raw.tzinfo else raw.replace(tzinfo=UTC)
    if isinstance(raw, str):
        text = raw.strip()
        try:
            dt = datetime.fromisoformat(text.replace("Z", "+00:00"))
        except ValueError:
            return None
        return dt if dt.tzinfo else dt.replace(tzinfo=UTC)
    return None


def _parse_amount(raw: Any) -> Decimal | None:
    if raw is None or raw == "":
        return None
    try:
        return Decimal(str(raw))
    except (InvalidOperation, ValueError):
        return None


@dataclass(frozen=True)
class FunnelEvent:
    event_id: str
    tenant_id: str
    entity_type: str
    entity_id: str
    event_name: str
    event_ts: datetime
    channel: str | None
    campaign_id: str | None
    lga_id: int | None
    amount_ngn: Decimal | None
    idempotency_key: str

    @property
    def day(self) -> date:
        """UTC event day — the rollup partition key."""
        return self.event_ts.astimezone(UTC).date()

    @property
    def is_lead(self) -> bool:
        return self.event_name == "lead_created"

    @property
    def is_conversion(self) -> bool:
        return self.event_name in CONVERSION_EVENTS

    @property
    def revenue_ngn(self) -> Decimal:
        """Amount counted toward revenue (converted/first_txn only)."""
        if self.event_name in REVENUE_EVENTS and self.amount_ngn is not None:
            return self.amount_ngn
        return Decimal("0")


def parse_funnel_event(payload: Mapping[str, Any]) -> FunnelEvent | None:
    """Parse a raw Kafka value into a FunnelEvent; None when unusable.

    Unusable = not a mapping, missing tenant_id/event_name/event_ts, or an
    event_name outside the contract enum (unknown names are dropped, not
    retried forever — the bronze sink keeps the raw copy).
    """
    if not isinstance(payload, Mapping):
        return None
    ce_type = str(payload.get("type", ""))
    data = payload.get("data")
    if isinstance(data, Mapping):
        body: Mapping[str, Any] = data
    else:
        # Bare payload (no envelope) — tolerated like mapping.py does.
        body = payload
    if ce_type and not ce_type.endswith("FunnelEvent"):
        return None

    tenant_id = _get(body, "tenant_id") or payload.get("tenantid")
    event_name = _get(body, "event_name")
    ts = _parse_ts(_get(body, "event_ts") or payload.get("time"))
    if not tenant_id or not event_name or ts is None:
        return None
    event_name = str(event_name)
    if event_name not in EVENT_NAMES:
        return None

    event_id = str(_get(body, "event_id") or payload.get("id") or "")
    idem = _get(body, "idempotency_key") or event_id
    if not idem:
        return None

    lga_raw = _get(body, "lga_id")
    try:
        lga_id = int(lga_raw) if lga_raw is not None and lga_raw != "" else None
    except (TypeError, ValueError):
        lga_id = None

    channel = _get(body, "channel")
    campaign_id = _get(body, "campaign_id")

    return FunnelEvent(
        event_id=event_id,
        tenant_id=str(tenant_id),
        entity_type=str(_get(body, "entity_type", "")),
        entity_id=str(_get(body, "entity_id", "")),
        event_name=event_name,
        event_ts=ts,
        channel=str(channel) if channel else None,
        campaign_id=str(campaign_id) if campaign_id else None,
        lga_id=lga_id,
        amount_ngn=_parse_amount(_get(body, "amount_ngn")),
        idempotency_key=str(idem),
    )
